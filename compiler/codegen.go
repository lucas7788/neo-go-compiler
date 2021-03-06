package compiler

import (
	"bytes"
	"encoding/binary"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"log"

	"encoding/json"
	"github.com/ontio/neo-go-compiler/vm"
	"github.com/ontio/ontology/common"
	"math/rand"
	"strconv"
	"strings"
)

const mainIdent = "Main"

type codegen struct {
	// Information about the program with all its dependencies.
	buildInfo *buildInfo

	// prog holds the output buffer
	prog *bytes.Buffer

	// Type information
	typeInfo *types.Info

	// A mapping of func identifiers with their scope.
	funcs map[string]*funcScope

	// Current funcScope being converted.
	scope *funcScope

	// Label table for recording jump destinations.
	l []int
}

// newLabel creates a new label to jump to
func (c *codegen) newLabel() (l int) {
	l = len(c.l)
	c.l = append(c.l, -1)
	return
}

func (c *codegen) setLabel(l int) {
	c.l[l] = c.pc() + 1
}

// pc return the program offset off the last instruction.
func (c *codegen) pc() int {
	return c.prog.Len() - 1
}

func (c *codegen) emitLoadConst(t types.TypeAndValue) {
	switch typ := t.Type.Underlying().(type) {
	case *types.Basic:
		switch typ.Kind() {
		case types.Int, types.UntypedInt, types.Int64:
			if t.Value != nil {
				val, _ := constant.Int64Val(t.Value)
				emitInt(c.prog, val)
			}

		case types.String, types.UntypedString:
			val := constant.StringVal(t.Value)
			emitString(c.prog, val)
		case types.Bool, types.UntypedBool:
			val := constant.BoolVal(t.Value)
			emitBool(c.prog, val)
		case types.Byte:
			val, _ := constant.Int64Val(t.Value)
			b := byte(val)
			emitBytes(c.prog, []byte{b})
		default:
			log.Fatalf("compiler don't know how to convert this basic type: %v", t)
		}
	case *types.Struct:
		//it's struct array sovled outside
		log.Fatalf("compiler don't know how to convert Struct this constant: %v", t)
	case *types.Array:
		log.Fatalf("compiler don't know how to convert Array this constant: %v", t)
	case *types.Slice:
		log.Fatalf("compiler don't know how to convert Slice this constant: %v", t)
	default:
		log.Fatalf("compiler don't know how to convert this constant: %v", t)
	}
}

func (c *codegen) emitLoadLocal(name string) {
	pos := c.scope.loadLocal(name)
	if pos < 0 {
		log.Fatalf("cannot load local variable with position: %d", pos)
	}
	c.emitLoadLocalPos(pos)
}

func (c *codegen) emitLoadLocalPos(pos int) {
	emitOpcode(c.prog, vm.Ofromaltstack)
	emitOpcode(c.prog, vm.Odup)
	emitOpcode(c.prog, vm.Otoaltstack)

	emitInt(c.prog, int64(pos))
	emitOpcode(c.prog, vm.Opickitem)
}

func (c *codegen) emitStoreLocal(pos int) {
	emitOpcode(c.prog, vm.Ofromaltstack)
	emitOpcode(c.prog, vm.Odup)
	emitOpcode(c.prog, vm.Otoaltstack)

	if pos < 0 {
		log.Fatalf("invalid position to store local: %d", pos)
	}

	emitInt(c.prog, int64(pos))
	emitInt(c.prog, 2)
	emitOpcode(c.prog, vm.Oroll)
	emitOpcode(c.prog, vm.Osetitem)
}

func (c *codegen) emitLoadField(i int) {
	emitInt(c.prog, int64(i))
	emitOpcode(c.prog, vm.Opickitem)
}

func (c *codegen) emitStoreStructField(i int) {
	emitInt(c.prog, int64(i))
	emitOpcode(c.prog, vm.Orot)
	emitOpcode(c.prog, vm.Osetitem)
}

func (c *codegen) emitStoreMapSet(value int, key int, m int) {
	for _, i := range []int{m, key, value} {
		c.emitLoadLocalPos(i)
	}
	emitOpcode(c.prog, vm.Osetitem)
}

// convertGlobals will traverse the AST and only convert global declarations.
// If we call this in convertFuncDecl then it will load all global variables
// into the scope of the function.
func (c *codegen) convertGlobals(f *ast.File) {
	ast.Inspect(f, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncDecl:
			return false
		case *ast.GenDecl:
			ast.Walk(c, n)
		}
		return true
	})
}

func (c *codegen) convertFuncDecl(file *ast.File, decl *ast.FuncDecl) {
	var (
		f  *funcScope
		ok bool
	)

	f, ok = c.funcs[decl.Name.Name]
	if ok {
		c.setLabel(f.label)
	} else {
		f = c.newFunc(decl)
	}
	c.scope = f
	//this function has bugs
	//1. var a = someFunc()  will judged as void assignment
	//2. how to solve the  a:= someFunc() ; someFunc()
	ast.Inspect(decl, c.scope.analyzeVoidCalls) // @OPTIMIZE

	// All globals copied into the scope of the function need to be added
	// to the stack size of the function.
	emitInt(c.prog, f.stackSize()+countGlobals(file))
	emitOpcode(c.prog, vm.Onewarray)
	emitOpcode(c.prog, vm.Otoaltstack)

	// We need to handle methods, which in Go, is just syntactic sugar.
	// The method receiver will be passed in as first argument.
	// We check if this declaration has a receiver and load it into scope.
	//
	// FIXME: For now we will hard cast this to a struct. We can later finetune this
	// to support other types.
	if decl.Recv != nil {
		for _, arg := range decl.Recv.List {
			ident := arg.Names[0]
			// Currently only method receives for struct types is supported.
			_, ok := c.typeInfo.Defs[ident].Type().Underlying().(*types.Struct)
			if !ok {
				log.Fatal("method receives for non-struct types is not yet supported")
			}
			l := c.scope.newLocal(ident.Name)
			c.emitStoreLocal(l)
		}
	}

	// Load the arguments in scope.
	for _, arg := range decl.Type.Params.List {
		name := arg.Names[0].Name // for now.
		l := c.scope.newLocal(name)
		c.emitStoreLocal(l)
	}
	// Load in all the global variables in to the scope of the function.
	// This is not necessary for syscalls and appcall.
	if !isSyscall(f.name) && !isAppcall(f.name) {
		c.convertGlobals(file)
	}

	ast.Walk(c, decl.Body)

	// If this function returs the void (no return stmt) we will cleanup its junk on the stack.
	if !hasReturnStmt(decl) {
		emitOpcode(c.prog, vm.Ofromaltstack)
		emitOpcode(c.prog, vm.Odrop)
		emitOpcode(c.prog, vm.Oret)
	}
}

func (c *codegen) Visit(node ast.Node) ast.Visitor {
	switch n := node.(type) {

	// General declarations.
	// var (
	//     x = 2
	// )

	case *ast.RangeStmt:
		// this need the opcode "KEYS" support
		var (
			fstart = c.newLabel()
			fend   = c.newLabel()
			tmpVal = ""
		)

		rnd := strconv.Itoa(rand.Intn(100))

		//tmp var names
		_keys := "_keys_" + rnd
		_values := "_values_" + rnd
		_arraylen := "_arraylen_" + rnd
		_i := "_i_" + rnd

		obj := n.Key.(*ast.Ident).Obj
		tmpKey := obj.Name
		collect := obj.Decl.(*ast.AssignStmt)

		for _, l := range collect.Lhs {
			switch l.(type) {
			case *ast.Ident:
				tmpVal = l.(*ast.Ident).Name
			}
		}

		m := collect.Rhs[0].(*ast.UnaryExpr).X.(*ast.Ident).Name

		c.emitLoadLocal(m)
		emitOpcode(c.prog, vm.OKeys)
		l := c.scope.newLocal(_keys)
		c.emitStoreLocal(l)

		c.emitLoadLocal(m)
		emitOpcode(c.prog, vm.OValues)
		l = c.scope.newLocal(_values)
		c.emitStoreLocal(l)

		c.emitLoadLocalPos(l)
		emitOpcode(c.prog, vm.Oarraysize)
		l = c.scope.newLocal(_arraylen)
		c.emitStoreLocal(l)

		// initializer
		emitInt(c.prog, 0)
		i := c.scope.newLocal(_i)
		c.emitStoreLocal(i)

		c.setLabel(fstart)
		c.emitLoadLocal(_i)
		c.emitLoadLocal(_arraylen)
		emitOpcode(c.prog, vm.Olt)
		emitJmp(c.prog, vm.Ojmpifnot, int16(fend))

		k := c.scope.newLocal(tmpKey)
		c.emitLoadLocal(_keys)
		c.emitLoadLocal(_i)
		emitOpcode(c.prog, vm.Opickitem)
		c.emitStoreLocal(k)

		v := c.scope.newLocal(tmpVal)
		c.emitLoadLocal(_values)
		c.emitLoadLocal(_i)
		emitOpcode(c.prog, vm.Opickitem)
		c.emitStoreLocal(v)

		//walk body
		ast.Walk(c, n.Body)

		//walk post
		c.emitLoadLocal(_i)
		emitInt(c.prog, 1)
		emitOpcode(c.prog, vm.Oadd)
		idx := c.scope.loadLocal(_i)
		c.emitStoreLocal(idx)
		// Jump back to condition.
		emitJmp(c.prog, vm.Ojmp, int16(fstart))
		c.setLabel(fend)

		return nil

	case *ast.MapType: //make(map[ktype][vtype]) do nothing
		return nil

	case *ast.GenDecl:
		for _, spec := range n.Specs {
			switch t := spec.(type) {
			case *ast.ValueSpec:
				for i, val := range t.Values {
					ast.Walk(c, val)
					l := c.scope.newLocal(t.Names[i].Name)
					c.emitStoreLocal(l)
				}
			}
		}
		return nil

	case *ast.AssignStmt:
		for i := 0; i < len(n.Lhs); i++ {
			switch t := n.Lhs[i].(type) {
			case *ast.Ident:
				switch n.Tok {
				case token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN:
					c.emitLoadLocal(t.Name)
					ast.Walk(c, n.Rhs[0]) // can only add assign to 1 expr on the RHS
					c.convertToken(n.Tok)
					l := c.scope.loadLocal(t.Name)
					c.emitStoreLocal(l)
				default:
					ast.Walk(c, n.Rhs[i])
					l := c.scope.loadLocal(t.Name)
					c.emitStoreLocal(l)
				}

			case *ast.SelectorExpr:
				switch expr := t.X.(type) {
				case *ast.Ident:
					ast.Walk(c, n.Rhs[i])
					typ := c.typeInfo.ObjectOf(expr).Type().Underlying()
					if strct, ok := typ.(*types.Struct); ok {
						c.emitLoadLocal(expr.Name)            // load the struct
						i := indexOfStruct(strct, t.Sel.Name) // get the index of the field
						c.emitStoreStructField(i)             // store the field
					}
				default:
					log.Fatal("nested selector assigns not supported yet")
				}
			case *ast.IndexExpr: //arr[i] = j or map[key] = value

				//get collection
				collection := n.Lhs[i].(*ast.IndexExpr).X.(*ast.Ident).Name
				lc := c.scope.loadLocal(collection)
				c.emitLoadLocalPos(lc)

				//get key
				switch n.Lhs[i].(type) {
				case *ast.IndexExpr:

					idx := n.Lhs[i].(*ast.IndexExpr).Index
					switch idx.(type) {
					case *ast.Ident:
						indexName := idx.(*ast.Ident).Name
						ipos := c.scope.loadLocal(indexName)
						c.emitLoadLocalPos(ipos)

					case *ast.BasicLit:
						tmp := idx.(*ast.BasicLit)
						c.emitLoadConst(c.typeInfo.Types[tmp])
					}
				case *ast.BasicLit:
					tmp := n.Lhs[i].(*ast.BasicLit)
					c.emitLoadConst(c.typeInfo.Types[tmp])
				}

				//get value
				switch n.Rhs[i].(type) {
				case *ast.Ident:
					value := n.Rhs[i].(*ast.Ident).Name
					lv := c.scope.loadLocal(value)
					c.emitLoadLocalPos(lv)
				case *ast.BasicLit:
					tmp := n.Rhs[i].(*ast.BasicLit)
					c.emitLoadConst(c.typeInfo.Types[tmp])
				}
				emitOpcode(c.prog, vm.Osetitem)
				//c.emitStoreMapSet(lv,ipos,lc)
			}
		}
		return nil

	case *ast.ReturnStmt:
		if len(n.Results) > 1 {
			log.Fatal("multiple returns not supported.")
		}

		// @OPTIMIZE: We could skip these 3 instructions for each return statement.
		// To be backwards compatible we will put them them in.
		// See issue #65 (https://github.com/CityOfZion/neo-go/issues/65)
		l := c.newLabel()
		emitJmp(c.prog, vm.Ojmp, int16(l))
		c.setLabel(l)

		if len(n.Results) > 0 {
			ast.Walk(c, n.Results[0])
		}

		emitOpcode(c.prog, vm.Onop) // @OPTIMIZE
		emitOpcode(c.prog, vm.Ofromaltstack)
		emitOpcode(c.prog, vm.Odrop) // Cleanup the stack.
		emitOpcode(c.prog, vm.Oret)
		return nil

	case *ast.IfStmt:
		lIf := c.newLabel()
		lElse := c.newLabel()
		if n.Cond != nil {
			ast.Walk(c, n.Cond)
			emitJmp(c.prog, vm.Ojmpifnot, int16(lElse))
		}

		c.setLabel(lIf)
		ast.Walk(c, n.Body)

		if n.Else != nil {
			// emitJmp(c.prog, vm.Ojmp, int16(lEnd))
		}
		c.setLabel(lElse)
		if n.Else != nil {
			ast.Walk(c, n.Else)
		}
		return nil

	case *ast.BasicLit:
		c.emitLoadConst(c.typeInfo.Types[n])
		return nil

	case *ast.Ident:
		if isIdentBool(n) {
			c.emitLoadConst(makeBoolFromIdent(n, c.typeInfo))
		} else {
			c.emitLoadLocal(n.Name)
		}
		return nil

	case *ast.CompositeLit:
		var typ types.Type

		switch t := n.Type.(type) {
		case *ast.Ident:
			typ = c.typeInfo.ObjectOf(t).Type().Underlying()
		case *ast.SelectorExpr:
			typ = c.typeInfo.ObjectOf(t.Sel).Type().Underlying()
		default:
			ln := len(n.Elts)
			// ByteArrays need a different approach then normal arrays.
			if isByteArray(n, c.typeInfo) {
				c.convertByteArray(n)
				return nil
			}
			for i := ln - 1; i >= 0; i-- {
				switch n.Elts[i].(type) {
				case *ast.BasicLit:
					c.emitLoadConst(c.typeInfo.Types[n.Elts[i]])
				case *ast.Ident:
					if isIdentBool(n.Elts[i].(*ast.Ident)) {
						c.emitLoadConst(makeBoolFromIdent(n.Elts[i].(*ast.Ident), c.typeInfo))
					} else {
						c.emitLoadLocal((n.Elts[i].(*ast.Ident).Name))
					}
				}

			}
			emitInt(c.prog, int64(ln))
			emitOpcode(c.prog, vm.Opack)
			return nil
		}

		switch typ.(type) {
		case *types.Struct:
			c.convertStruct(n)
		}

		return nil

	case *ast.BinaryExpr:
		switch n.Op {
		case token.LAND:
			ast.Walk(c, n.X)
			emitJmp(c.prog, vm.Ojmpifnot, int16(len(c.l)-1))
			ast.Walk(c, n.Y)
			return nil

		case token.LOR:
			ast.Walk(c, n.X)
			emitJmp(c.prog, vm.Ojmpif, int16(len(c.l)-2))
			ast.Walk(c, n.Y)
			return nil

		default:
			// The AST package will try to resolve all basic literals for us.
			// If the typeinfo.Value is not nil we know that the expr is resolved
			// and needs no further action. e.g. x := 2 + 2 + 2 will be resolved to 6.
			// NOTE: Constants will also be automagically resolved be the AST parser.
			// example:
			// const x = 10
			// x + 2 will results into 12
			tinfo := c.typeInfo.Types[n]
			if tinfo.Value != nil {
				c.emitLoadConst(tinfo)
				return nil
			}

			ast.Walk(c, n.X)
			ast.Walk(c, n.Y)
			//"+" for string concat
			if tinfo.Type.Underlying().String() == "string" {
				switch n.Op {
				case token.ADD:
					emitOpcode(c.prog, vm.Ocat)
				}
				return nil
			}

			c.convertToken(n.Op)
			return nil
		}

	case *ast.CallExpr:
		var (
			f         *funcScope
			ok        bool
			numArgs   = len(n.Args)
			isBuiltin = isBuiltin(n.Fun)
		)
		switch fun := n.Fun.(type) {
		case *ast.Ident:
			f, ok = c.funcs[fun.Name]
			if !ok && !isBuiltin {
				log.Fatalf("could not resolve function %s", fun.Name)
			}
		case *ast.SelectorExpr:
			// If this is a method call we need to walk the AST to load the struct locally.
			// Otherwise this is a function call from a imported package and we can call it
			// directly.
			if c.typeInfo.Selections[fun] != nil {
				ast.Walk(c, fun.X)
				// Dont forget to add 1 extra argument when its a method.
				numArgs++
			}
			f, ok = c.funcs[fun.Sel.Name]
			if !ok {
				log.Fatalf("could not resolve function %s", fun.Sel.Name)
			}
		case *ast.ArrayType:
			// For now we will assume that there is only 1 argument passed which
			// will be a basic literal (string kind). This only to handle string
			// to byte slice conversions. E.G. []byte("foobar")
			switch n.Args[0].(type) {
			case *ast.BasicLit:
				arg := n.Args[0].(*ast.BasicLit)
				c.emitLoadConst(c.typeInfo.Types[arg])
				return nil
			case *ast.SelectorExpr:
				log.Fatalf("could not resolve type %v", n.Args[0])
			}
		default:
			log.Fatalf("not recongized...")

		}

		// Handle the arguments

		//add a special solution for appcall
		//appcall should not push the first argument (contract address) to the stack
		var appcallContractAddr []byte = nil
		if f != nil && isAppcall(f.name) {
			contractAddress := n.Args[0]
			switch contractAddress.(type) {
			case *ast.Ident:
				//get the real bytes[]
				//todo support the var indent case of the contract address
				log.Fatal("not support a variable contract address now")
			case *ast.BasicLit:
				addrStr := contractAddress.(*ast.BasicLit).Value
				addr, err := common.AddressFromBase58(strings.Replace(addrStr, "\"", "", -1))
				if err != nil {
					addr, err = common.AddressFromHexString(strings.Replace(addrStr, "\"", "", -1))
					if err != nil {
						log.Fatal("not a valid base58 or Hex format address")
					}
				}
				appcallContractAddr = addr[:]
			default:
				log.Fatal("not a supported address type")
			}
			//remove the first argument
			n.Args = n.Args[1:]
			numArgs -= 1
		}

		for _, arg := range n.Args {
			ast.Walk(c, arg)
		}
		// Do not swap for builtin functions.
		if !isBuiltin {
			if numArgs == 2 {
				emitOpcode(c.prog, vm.Oswap)
			}
			if numArgs == 3 {
				emitInt(c.prog, 2)
				emitOpcode(c.prog, vm.Oxswap)
			}

			if numArgs > 3 {
				halfP := int(numArgs / 2)
				for i := 0; i < halfP; i++ {
					saveto := numArgs - 1 - i
					emitInt(c.prog, int64(saveto))
					emitOpcode(c.prog, vm.Opick)

					emitInt(c.prog, int64(i+1))
					emitOpcode(c.prog, vm.Opick)

					emitInt(c.prog, int64(saveto+2))
					emitOpcode(c.prog, vm.Oxswap)
					emitOpcode(c.prog, vm.Odrop)

					emitInt(c.prog, int64(i+1))
					emitOpcode(c.prog, vm.Oxswap)
					emitOpcode(c.prog, vm.Odrop)
				}
			}
		}

		// c# compiler adds a NOP (0x61) before every function call. Dont think its relevant
		// and we could easily removed it, but to be consistent with the original compiler I
		// will put them in. ^^
		emitOpcode(c.prog, vm.Onop)

		// Check builtin first to avoid nil pointer on funcScope!

		if isBuiltin {
			// Use the ident to check, builtins are not in func scopes.
			// We can be sure builtins are of type *ast.Ident.
			c.convertBuiltin(n)
		} else if isSyscall(f.name) {
			c.convertSyscall(f.name)
		} else if isAppcall(f.name) {
			c.convertAppcall(f.name, appcallContractAddr)
		} else {
			emitCall(c.prog, vm.Ocall, int16(f.label))
		}

		// If we are not assigning this function to a variable we need to drop
		// (cleanup) the top stack item. It's not a void but you get the point \o/.

		if _, ok := c.scope.voidCalls[n]; ok {
			if f.decl.Type.Results != nil {
				emitOpcode(c.prog, vm.Odrop)
			}
		}
		return nil

	case *ast.SelectorExpr:
		switch t := n.X.(type) {
		case *ast.Ident:
			typ := c.typeInfo.ObjectOf(t).Type().Underlying()
			if strct, ok := typ.(*types.Struct); ok {
				c.emitLoadLocal(t.Name) // load the struct
				i := indexOfStruct(strct, n.Sel.Name)
				c.emitLoadField(i) // load the field
			}

		case *ast.IndexExpr: //array[struct]
			collectionName := n.X.(*ast.IndexExpr).X.(*ast.Ident).Name //get collection name
			idx := n.X.(*ast.IndexExpr).Index
			switch idx.(type) {
			case *ast.BasicLit:
				idxvalue := idx.(*ast.BasicLit).Value
				c.emitLoadLocal(collectionName)
				switch idx.(*ast.BasicLit).Kind {
				case token.INT: //index is int
					intvalue, err := strconv.Atoi(idxvalue)
					if err != nil {
						log.Fatal(err.Error())
					}
					emitInt(c.prog, int64(intvalue))
				case token.STRING: //index is string (map)
					emitString(c.prog, idxvalue)

				default:
					log.Fatal("not supported index type!")
				}
				emitOpcode(c.prog, vm.Opickitem) //get the collection in collection
				fieldname := n.Sel.Name
				typ := c.typeInfo.ObjectOf(n.X.(*ast.IndexExpr).X.(*ast.Ident)).Type().Underlying()

				baseElemtyp := typ.(*types.Slice).Elem().Underlying()

				if strct, ok := baseElemtyp.(*types.Struct); ok {
					i := indexOfStruct(strct, fieldname)
					emitInt(c.prog, int64(i))
				} else {
					log.Fatal("not a struct type")
				}
				emitOpcode(c.prog, vm.Opickitem)
			default:
				log.Fatal("nested selectors IndexExpr not supported yet")
			}
		case *ast.SelectorExpr:
			ast.Walk(c, n.X.(*ast.SelectorExpr).X)
			typ := c.typeInfo.ObjectOf(n.X.(*ast.SelectorExpr).X.(*ast.Ident)).Type().Underlying()
			fieldname := n.X.(*ast.SelectorExpr).Sel.Name

			if strct, ok := typ.(*types.Struct); ok {
				i := indexOfStruct(strct, fieldname)
				emitInt(c.prog, int64(i))
			} else {
				log.Fatal("not a struct type")
			}
			emitOpcode(c.prog, vm.Opickitem)

			//todo replace with the real index of struct
			emitInt(c.prog, int64(1))

			emitOpcode(c.prog, vm.Opickitem)

		default:
			log.Fatal("nested selectors not supported yet")
		}
		return nil

	case *ast.UnaryExpr:
		ast.Walk(c, n.X)
		c.convertToken(n.Op)
		return nil

	case *ast.IncDecStmt:
		ast.Walk(c, n.X)
		c.convertToken(n.Tok)

		// For now only identifiers are supported for (post) for stmts.
		// for i := 0; i < 10; i++ {}
		// Where the post stmt is ( i++ )
		if ident, ok := n.X.(*ast.Ident); ok {
			pos := c.scope.loadLocal(ident.Name)
			c.emitStoreLocal(pos)
		}
		return nil

	case *ast.IndexExpr:
		// Walk the expression, this could be either an Ident or SelectorExpr.
		// This will load local whatever X is.
		ast.Walk(c, n.X)

		switch n.Index.(type) {
		case *ast.BasicLit:
			t := c.typeInfo.Types[n.Index]
			switch typ := t.Type.Underlying().(type) {
			case *types.Basic:
				switch typ.Kind() {
				case types.Int, types.UntypedInt, types.Int64:
					val, _ := constant.Int64Val(t.Value)
					c.emitLoadField(int(val))
				case types.String:
					val := constant.StringVal(t.Value)
					emitString(c.prog, val)
					emitOpcode(c.prog, vm.Opickitem)

				}
			}

		case *ast.Ident: //todo  for loop a[i] == b[i] case, need more test
			pos := c.scope.loadLocal(n.Index.(*ast.Ident).Name)
			c.emitLoadLocalPos(pos) // get i
			emitOpcode(c.prog, vm.Opickitem)

		default:
			ast.Walk(c, n.Index)
			emitOpcode(c.prog, vm.Opickitem) // just pickitem here
		}
		return nil

	case *ast.ForStmt:
		var (
			fstart = c.newLabel()
			fend   = c.newLabel()
		)

		// Walk the initializer and condition.
		ast.Walk(c, n.Init)

		// Set label and walk the condition.
		c.setLabel(fstart)
		ast.Walk(c, n.Cond)

		// Jump if the condition is false
		emitJmp(c.prog, vm.Ojmpifnot, int16(fend))

		// Walk body followed by the iterator (post stmt).
		ast.Walk(c, n.Body)
		ast.Walk(c, n.Post)

		// Jump back to condition.
		emitJmp(c.prog, vm.Ojmp, int16(fstart))
		c.setLabel(fend)

		return nil

	// We dont really care about assertions for the core logic.
	// The only thing we need is to please the compiler type checking.
	// For this to work properly, we only need to walk the expression
	// not the assertion type.
	case *ast.TypeAssertExpr:
		ast.Walk(c, n.X)
		return nil
	}
	return c
}

func (c *codegen) convertSyscall(name string) {
	api, ok := vm.Syscalls[name]
	if !ok {
		log.Fatalf("unknown VM syscall api: %s", name)
	}
	emitSyscall(c.prog, api)
	emitOpcode(c.prog, vm.Onop) // @OPTIMIZE
}

func (c *codegen) convertAppcall(name string, address []byte) {

	emitAppcall(c.prog, address)
	emitOpcode(c.prog, vm.Onop) // @OPTIMIZE
}

func (c *codegen) convertBuiltin(expr *ast.CallExpr) {
	var name string
	switch t := expr.Fun.(type) {
	case *ast.Ident:
		name = t.Name
	case *ast.SelectorExpr:
		name = t.Sel.Name
	}

	switch name {
	case "len":
		arg := expr.Args[0]
		typ := c.typeInfo.Types[arg].Type
		if isStringType(typ) {
			emitOpcode(c.prog, vm.Osize)
		} else {
			emitOpcode(c.prog, vm.Oarraysize)
		}
	case "make":
		l := len(expr.Args)
		if l == 1 { //make(map[ktype]vtype)
			emitOpcode(c.prog, vm.Onewmap)
		} else { //make([]type,length) or make([]type.length,cap)
			emitOpcode(c.prog, vm.Onewarray)
		}
	case "append":
		emitOpcode(c.prog, vm.Oappend)
	case "SHA256":
		emitOpcode(c.prog, vm.Osha256)
	case "SHA1":
		emitOpcode(c.prog, vm.Osha1)
	case "Hash256":
		emitOpcode(c.prog, vm.Ohash256)
	case "Hash160":
		emitOpcode(c.prog, vm.Ohash160)
	case "BytesEquals":
		emitOpcode(c.prog, vm.Oequal)
	case "ToScriptHash":
		switch expr.Args[0].(type) {
		case *ast.BasicLit:
			val := expr.Args[0].(*ast.BasicLit).Value
			bs, err := common.AddressFromBase58(strings.Replace(val, "\"", "", -1))
			if err != nil {
				log.Fatal("ToScriptHash parse failed")
			}
			emitBytes(c.prog, bs[:])
		default:
			log.Fatal("ToScriptHash method only support const string")
		}
	case "Cat":
		emitOpcode(c.prog, vm.Ocat)
	case "[]byte":
		switch expr.Args[0].(type) {
		case *ast.BasicLit:
			val := expr.Args[0].(*ast.BasicLit).Value
			bs := []byte(val)
			emitBytes(c.prog, bs)
		//case *ast.Ident:
		//	agName :=  expr.Args[0].(*ast.Ident).Name
		//	c.emitLoadLocal(agName)
		default:
			log.Fatal("toBytes method only support const string")
		}

	}
}

func (c *codegen) convertByteArray(lit *ast.CompositeLit) {
	buf := make([]byte, len(lit.Elts))
	for i := 0; i < len(lit.Elts); i++ {
		t := c.typeInfo.Types[lit.Elts[i]]
		val, _ := constant.Int64Val(t.Value)
		buf[i] = byte(val)
	}
	emitBytes(c.prog, buf)
}

func (c *codegen) convertStruct(lit *ast.CompositeLit) {
	// Create a new structScope to initialize and store
	// the positions of its variables.
	strct, ok := c.typeInfo.TypeOf(lit).Underlying().(*types.Struct)
	if !ok {
		log.Fatalf("the given literal is not of type struct: %v", lit)
	}
	emitOpcode(c.prog, vm.Onop)
	emitInt(c.prog, int64(strct.NumFields()))
	emitOpcode(c.prog, vm.Onewstruct)
	emitOpcode(c.prog, vm.Otoaltstack)

	// We need to locally store all the fields, even if they are not initialized.
	// We will initialize all fields to their "zero" value.
	for i := 0; i < strct.NumFields(); i++ {
		sField := strct.Field(i)

		fieldAdded := false
		// Fields initialized by the program.
		for i, field := range lit.Elts {
			switch field.(type) {
			case *ast.KeyValueExpr: //for struct{fieldname:value} case
				f := field.(*ast.KeyValueExpr)
				fieldName := f.Key.(*ast.Ident).Name
				if sField.Name() == fieldName {
					ast.Walk(c, f.Value)
					pos := indexOfStruct(strct, fieldName)
					c.emitStoreLocal(pos)
					fieldAdded = true
					break
				}
			case *ast.Ident:
				varname := field.(*ast.Ident).Name
				fieldIdx := indexOfStruct(strct, sField.Name())
				if fieldIdx == i {
					emitOpcode(c.prog, vm.Ofromaltstack)
					c.emitLoadLocal(varname)
					emitInt(c.prog, int64(1))
					emitOpcode(c.prog, vm.Oroll)
					emitOpcode(c.prog, vm.Otoaltstack)
					c.emitStoreLocal(i)

					fieldAdded = true
				}

			case *ast.BasicLit: //for struct{value1,value2} case
				fieldIdx := indexOfStruct(strct, sField.Name())
				if fieldIdx == i {
					ast.Walk(c, field)
					c.emitStoreLocal(i)
					fieldAdded = true
				}

			default:
				log.Fatal("not supported struct field type")
			}
		}
		if fieldAdded {
			continue
		}

		typeAndVal := typeAndValueForField(sField)
		c.emitLoadConst(typeAndVal)
		c.emitStoreLocal(i)
	}
	emitOpcode(c.prog, vm.Ofromaltstack)
}

func (c *codegen) convertToken(tok token.Token) {
	switch tok {
	case token.ADD_ASSIGN:
		emitOpcode(c.prog, vm.Oadd)
	case token.SUB_ASSIGN:
		emitOpcode(c.prog, vm.Osub)
	case token.MUL_ASSIGN:
		emitOpcode(c.prog, vm.Omul)
	case token.QUO_ASSIGN:
		emitOpcode(c.prog, vm.Odiv)
	case token.ADD:
		emitOpcode(c.prog, vm.Oadd)
	case token.SUB:
		emitOpcode(c.prog, vm.Osub)
	case token.MUL:
		emitOpcode(c.prog, vm.Omul)
	case token.QUO:
		emitOpcode(c.prog, vm.Odiv)
	case token.LSS:
		emitOpcode(c.prog, vm.Olt)
	case token.LEQ:
		emitOpcode(c.prog, vm.Olte)
	case token.GTR:
		emitOpcode(c.prog, vm.Ogt)
	case token.GEQ:
		emitOpcode(c.prog, vm.Ogte)
	case token.EQL:
		//only integer or int64 to emit onumequal
		emitOpcode(c.prog, vm.Onumequal)
	case token.NEQ:
		emitOpcode(c.prog, vm.Onumnotequal)
	case token.DEC:
		emitOpcode(c.prog, vm.Odec)
	case token.INC:
		emitOpcode(c.prog, vm.Oinc)
	case token.NOT:
		emitOpcode(c.prog, vm.Onot)
	default:
		log.Fatalf("compiler could not convert token: %s", tok)
	}
}

func (c *codegen) newFunc(decl *ast.FuncDecl) *funcScope {
	f := newFuncScope(decl, c.newLabel())
	c.funcs[f.name] = f
	return f
}

// CodeGen is the function that compiles the program to bytecode.
func CodeGen(info *buildInfo, genabi bool) (*bytes.Buffer, *bytes.Buffer, error) {
	pkg := info.program.Package(info.initialPackage)
	c := &codegen{
		buildInfo: info,
		prog:      new(bytes.Buffer),
		l:         []int{},
		funcs:     map[string]*funcScope{},
		typeInfo:  &pkg.Info,
	}
	var abibytes []byte
	// Resolve the entrypoint of the program
	main, mainFile := resolveEntryPoint(mainIdent, pkg)
	if main == nil {
		log.Fatal("could not find func main. did you forgot to declare it?")
	}

	if genabi {
		funcs := MakeAbi(main)
		tmpbytes, err := json.Marshal(funcs)
		if err != nil {
			return nil, nil, err
		}
		abibytes = tmpbytes
	}

	funUsage := analyzeFuncUsage(info.program.AllPackages)

	// Bring all imported functions into scope
	for _, pkg := range info.program.AllPackages {
		for _, f := range pkg.Files {
			c.resolveFuncDecls(f)
		}
	}

	// convert the entry point first
	c.convertFuncDecl(mainFile, main)

	// Generate the code for the program
	for _, pkg := range info.program.AllPackages {
		c.typeInfo = &pkg.Info

		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				switch n := decl.(type) {
				case *ast.FuncDecl:
					// Dont convert the function if its not used. This will save alot
					// of bytecode space.
					if n.Name.Name != mainIdent && funUsage.funcUsed(n.Name.Name) {
						c.convertFuncDecl(f, n)
					}
				}
			}
		}
	}

	c.writeJumps()

	if genabi {
		return c.prog, bytes.NewBuffer(abibytes), nil
	} else {
		return c.prog, nil, nil
	}

}

func (c *codegen) resolveFuncDecls(f *ast.File) {
	for _, decl := range f.Decls {
		switch n := decl.(type) {
		case *ast.FuncDecl:
			if n.Name.Name != mainIdent {
				c.newFunc(n)
			}
		}
	}
}

func (c *codegen) writeJumps() {
	b := c.prog.Bytes()
	for i, op := range b {
		j := i + 1
		switch vm.Opcode(op) {
		case vm.Ojmp, vm.Ojmpifnot, vm.Ojmpif, vm.Ocall:
			index := int16(binary.LittleEndian.Uint16(b[j : j+2]))
			if int(index) > len(c.l) || int(index) < 0 {
				continue
			}
			offset := uint16(c.l[index] - i)
			if offset < 0 {
				log.Fatalf("new offset is negative, table list %v", c.l)
			}
			binary.LittleEndian.PutUint16(b[j:j+2], offset)
		}
	}
}
