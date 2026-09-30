package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

// An api block lists declarations of one package as its documentation shows them: functions
// and methods without bodies, types, vars and consts. It is compiled with that package
// dot-imported, so names in it resolve to the package's own, and each declaration must match
// the package's current one:
//
//   - a function or method must exist with an identical signature (type parameter names may
//     differ);
//   - an interface, or any type that is not a struct, must be identical to the package's type;
//   - a struct may list a subset of the fields, in any order, each with an identical type;
//   - a var or const must exist with an identical type.

// apiDecl is one declaration of an api block, renamed so it does not collide with the
// dot-imported name it documents.
type apiDecl struct {
	name  string // the documented name
	recv  string // receiver type name, for a method
	ptr   bool   // the method has a pointer receiver
	local *ast.Ident
	pos   token.Pos
}

const apiPrefix = "docsnip_api_"

// prepareAPI renames every declaration of an api block and turns each method into a var of
// its function type (a method cannot be declared on the dot-imported type), returning what to
// compare after type-checking. A function with a body is reported: an api block lists
// declarations, not code.
func prepareAPI(f *ast.File) ([]apiDecl, []string) {
	var (
		decls []apiDecl
		bad   []string
		out   []ast.Decl
		n     int
	)
	rename := func(id *ast.Ident) *ast.Ident {
		n++
		id.Name = fmt.Sprintf("%s%d_%s", apiPrefix, n, id.Name)
		return id
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				bad = append(bad, d.Name.Name)
				out = append(out, d)
				continue
			}
			if d.Recv == nil {
				decls = append(decls, apiDecl{name: d.Name.Name, pos: d.Name.Pos()})
				decls[len(decls)-1].local = rename(d.Name)
				out = append(out, d)
				continue
			}
			recv, ptr := recvName(d.Recv.List[0].Type)
			a := apiDecl{name: d.Name.Name, recv: recv, ptr: ptr, pos: d.Name.Pos()}
			id := ast.NewIdent(d.Name.Name)
			id.NamePos = d.Name.Pos()
			a.local = rename(id)
			decls = append(decls, a)
			out = append(out, &ast.GenDecl{Tok: token.VAR, TokPos: d.Pos(), Specs: []ast.Spec{
				&ast.ValueSpec{Names: []*ast.Ident{id}, Type: d.Type},
			}})
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					decls = append(decls, apiDecl{name: s.Name.Name, pos: s.Name.Pos()})
					decls[len(decls)-1].local = rename(s.Name)
				case *ast.ValueSpec:
					for _, id := range s.Names {
						if id.Name == "_" {
							continue
						}
						decls = append(decls, apiDecl{name: id.Name, pos: id.Pos()})
						decls[len(decls)-1].local = rename(id)
					}
				}
			}
			out = append(out, d)
		default:
			out = append(out, d)
		}
	}
	f.Decls = out
	return decls, bad
}

func recvName(e ast.Expr) (string, bool) {
	ptr := false
	if s, ok := e.(*ast.StarExpr); ok {
		ptr, e = true, s.X
	}
	switch t := e.(type) {
	case *ast.IndexExpr:
		e = t.X
	case *ast.IndexListExpr:
		e = t.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name, ptr
	}
	return "", ptr
}

// compareAPI reports every declaration of an api block that does not match pkg.
func compareAPI(pkg *types.Package, info *types.Info, decls []apiDecl, pos func(token.Pos) Finding) []Finding {
	qual := types.RelativeTo(pkg)
	str := func(t types.Type) string { return types.TypeString(t, qual) }
	var out []Finding
	fail := func(d apiDecl, format string, args ...any) {
		f := pos(d.pos)
		name := d.name
		if d.recv != "" {
			name = d.recv + "." + d.name
		}
		f.Msg = fmt.Sprintf("api drift: %s.%s: ", pkg.Name(), name) + fmt.Sprintf(format, args...)
		out = append(out, f)
	}
	for _, d := range decls {
		doc := info.Defs[d.local]
		if doc == nil {
			continue // a type error was reported for it
		}
		if d.recv != "" {
			tn, ok := pkg.Scope().Lookup(d.recv).(*types.TypeName)
			if !ok {
				fail(d, "no type %s in the package", d.recv)
				continue
			}
			var recv types.Type = tn.Type()
			if d.ptr {
				recv = types.NewPointer(recv)
			}
			obj, _, _ := types.LookupFieldOrMethod(recv, false, pkg, d.name)
			m, ok := obj.(*types.Func)
			if !ok {
				fail(d, "no such method")
				continue
			}
			if !types.Identical(doc.Type(), m.Type()) {
				fail(d, "the doc has %s, the code has %s", str(doc.Type()), str(m.Type()))
			}
			continue
		}
		real := pkg.Scope().Lookup(d.name)
		if real == nil || !real.Exported() {
			fail(d, "not in the package")
			continue
		}
		switch doc := doc.(type) {
		case *types.Func:
			if _, ok := real.(*types.Func); !ok {
				fail(d, "the doc declares a func, the code a %s", objKind(real))
			} else if !types.Identical(doc.Type(), real.Type()) {
				fail(d, "the doc has %s, the code has %s", str(doc.Type()), str(real.Type()))
			}
		case *types.TypeName:
			rt, ok := real.(*types.TypeName)
			if !ok {
				fail(d, "the doc declares a type, the code a %s", objKind(real))
				continue
			}
			if msg := compareType(doc.Type(), rt.Type(), str); msg != "" {
				fail(d, "%s", msg)
			}
		case *types.Var, *types.Const:
			if objKind(doc) != objKind(real) {
				fail(d, "the doc declares a %s, the code a %s", objKind(doc), objKind(real))
			} else if !types.Identical(doc.Type(), real.Type()) {
				fail(d, "the doc has type %s, the code has %s", str(doc.Type()), str(real.Type()))
			}
		}
	}
	return out
}

func objKind(o types.Object) string {
	switch o.(type) {
	case *types.Func:
		return "func"
	case *types.TypeName:
		return "type"
	case *types.Var:
		return "var"
	case *types.Const:
		return "const"
	}
	return "declaration"
}

// compareType compares a documented named type with the package's. A generic pair is
// compared after instantiating the package's type with the doc's type parameters.
func compareType(doc, real types.Type, str func(types.Type) string) string {
	dn, _ := types.Unalias(doc).(*types.Named)
	rn, _ := types.Unalias(real).(*types.Named)
	if dn == nil || rn == nil {
		if !types.Identical(doc, real) {
			return fmt.Sprintf("the doc has %s, the code has %s", str(doc), str(real))
		}
		return ""
	}
	du, ru := dn.Underlying(), rn.Underlying()
	if dt, rt := dn.TypeParams(), rn.TypeParams(); dt.Len() != rt.Len() {
		return fmt.Sprintf("the doc has %d type parameters, the code has %d", dt.Len(), rt.Len())
	} else if dt.Len() > 0 {
		args := make([]types.Type, dt.Len())
		for i := range args {
			args[i] = dt.At(i)
		}
		inst, err := types.Instantiate(nil, rn, args, false)
		if err != nil {
			return fmt.Sprintf("cannot compare the generic types: %v", err)
		}
		ru = inst.Underlying()
	}
	ds, dIsStruct := du.(*types.Struct)
	rs, rIsStruct := ru.(*types.Struct)
	if dIsStruct && rIsStruct {
		var diffs []string
		for i := 0; i < ds.NumFields(); i++ {
			df := ds.Field(i)
			var rf *types.Var
			for j := 0; j < rs.NumFields(); j++ {
				if rs.Field(j).Name() == df.Name() {
					rf = rs.Field(j)
				}
			}
			switch {
			case rf == nil:
				diffs = append(diffs, fmt.Sprintf("field %s is not in the code", df.Name()))
			case !types.Identical(df.Type(), rf.Type()):
				diffs = append(diffs, fmt.Sprintf("field %s is %s in the doc, %s in the code", df.Name(), str(df.Type()), str(rf.Type())))
			}
		}
		return strings.Join(diffs, "; ")
	}
	if !types.Identical(du, ru) {
		return fmt.Sprintf("the doc has %s, the code has %s", str(du), str(ru))
	}
	return ""
}
