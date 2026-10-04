package main

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"
)

// The tool rule: the Tool interface is Spec() ToolSpec and Call (docs/design/api-v1.md, item 7),
// and the tool constructors return errors, with panicking Must twins.
//
//   - Func(name, description, safety, fn, opts...) becomes MustFunc(name, description, fn,
//     WithSafety(safety), opts...) (no WithSafety for the zero Safety{}, the default); it panicked
//     as MustFunc does. CompensatedFunc, SubAgent and RetrievalTool become MustCompensatedFunc,
//     MustSubAgent and MustRetrievalTool, which panic where they did.
//   - SpecOf(t) becomes t.Spec(); t.Name(), t.Description(), t.ArgsSchema() and t.Safety() on an
//     agent.Tool become t.Spec().Name, .Description, .Input and .Safety.
//   - A type that implemented the old method set (Name, Description, ArgsSchema, Safety, Call) and
//     has no Spec gets one, built from those methods (and, for a wrapper with Unwrap() Tool, from
//     the wrapped tool's spec for the rest, as SpecOf read it).
//   - plan's RegisterStep, RegisterJoin2, RegisterJoin3, RegisterTool, RegisterModel and
//     RegisterPredicate become methods of *plan.Registry (section 13).

var mustTwins = map[string]string{"CompensatedFunc": "MustCompensatedFunc", "SubAgent": "MustSubAgent", "RetrievalTool": "MustRetrievalTool"}

var oldToolMethods = map[string]string{"Name": "Name", "Description": "Description", "ArgsSchema": "Input", "Safety": "Safety"}

var registers = map[string]bool{"RegisterStep": true, "RegisterJoin2": true, "RegisterJoin3": true, "RegisterTool": true, "RegisterModel": true, "RegisterPredicate": true}

func visitTools(c *Ctx, n ast.Node) {
	switch n := n.(type) {
	case *ast.FuncDecl:
		addSpecMethod(c, n)
	case *ast.CallExpr:
		visitToolCall(c, n)
	}
}

func visitToolCall(c *Ctx, call *ast.CallExpr) {
	fun := ast.Unparen(call.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	r, ok := c.refOf(fun)
	if !ok {
		return
	}
	A := c.A()
	switch {
	case r.Pkg == agentPath && r.Recv == "" && (r.Name == "Func" || r.Name == "CompensatedFunc"):
		if len(call.Args) < 4 || !isNamed(c.Info.TypeOf(call.Args[2]), agentPath, "Safety") {
			return // the new signature
		}
		name := "MustFunc"
		last := 2 // the function
		if r.Name == "CompensatedFunc" {
			name, last = "MustCompensatedFunc", 3 // do, undo
		}
		if !isZeroSafety(call.Args[2]) {
			// the safety option goes before the caller's options, so a WithSafety among them
			// still wins, as it did
			safety := A + "WithSafety(" + c.Ed.Text(call.Args[2]) + ")"
			if call.Ellipsis.IsValid() && len(call.Args) == last+3 {
				// the options are a slice passed on: the safety option leads it
				spread := call.Args[last+2]
				c.Ed.Replace(spread, "append([]"+A+"ToolOption{"+safety+"}, "+c.Ed.Text(spread)+"...)")
			} else {
				c.Ed.InsertAfter(call.Args[last+1], ", "+safety)
			}
		}
		c.Ed.DeleteArg(call, 2)
		replaceName(c, fun, name)
		c.Count()
	case r.Pkg == agentPath && r.Recv == "" && (r.Name == "SubAgent" || r.Name == "RetrievalTool"):
		if sig, ok := c.Info.TypeOf(call.Fun).(*types.Signature); ok && sig.Results().Len() == 2 {
			return // the new constructor, which returns an error
		}
		c.Ed.Replace(fun, A+mustTwins[r.Name])
		c.Count()
	case r.Pkg == agentPath && r.Recv == "" && r.Name == "SpecOf" && len(call.Args) == 1:
		c.Ed.Replace(call, recvText(c, call.Args[0])+".Spec()")
		c.Count()
	case r.Pkg == planPath && r.Recv == "" && registers[r.Name] && len(call.Args) >= 2:
		if t := c.Info.TypeOf(call.Args[0]); t != nil && !isNamed(t, planPath, "Registry") {
			return
		}
		c.Ed.Replace(fun, recvText(c, call.Args[0])+"."+r.Name)
		c.Ed.DeleteArg(call, 0)
		c.Count()
	case len(call.Args) == 0 && oldToolMethods[r.Name] != "":
		sel, ok := fun.(*ast.SelectorExpr)
		if !ok {
			return
		}
		if t := c.Info.TypeOf(sel.X); t != nil && isNamed(t, agentPath, "Tool") {
			c.Ed.Replace(call, recvText(c, sel.X)+".Spec()."+oldToolMethods[r.Name])
			c.Count()
		}
	}
}

// recvText is e's text as a method call's receiver.
func recvText(c *Ctx, e ast.Expr) string {
	if needsParens(e) {
		return "(" + c.Ed.Text(e) + ")"
	}
	return c.Ed.Text(e)
}

// isZeroSafety reports whether e is the literal agent.Safety{} (or Safety{}).
func isZeroSafety(e ast.Expr) bool {
	lit, ok := ast.Unparen(e).(*ast.CompositeLit)
	return ok && len(lit.Elts) == 0
}

// addSpecMethod gives a Spec method to a type that implements the old Tool method set and has
// none, at the first of its old methods (Name, Description, ArgsSchema, Safety) it declares. The
// Spec reports what the old methods did, so a method the type declares keeps overriding:
//
//   - a decorator that embeds an agent.Tool starts from the embedded tool's Spec and sets the
//     fields of the methods it declares (the embedded tool's own old methods become its Spec);
//   - a wrapper with Unwrap() Tool starts from the wrapped tool's Spec, as SpecOf read it;
//   - any other type builds the spec from its four methods.
//
// A type whose old methods come from anywhere else (an embedded struct, a method of another
// file) is reported for a person.
func addSpecMethod(c *Ctx, fd *ast.FuncDecl) {
	if fd.Recv == nil || oldToolMethods[fd.Name.Name] == "" || len(fd.Recv.List) != 1 {
		return
	}
	obj, ok := c.Info.Defs[fd.Name].(*types.Func)
	if !ok {
		return
	}
	recvT := obj.Type().(*types.Signature).Recv().Type()
	base := recvT
	if p, isPtr := recvT.(*types.Pointer); isPtr {
		base = p.Elem()
	}
	nt := named(base)
	if nt == nil {
		return
	}
	ptr := types.NewPointer(base)
	if !hasMethods(ptr, "Call") || hasMethods(ptr, "Spec") {
		return
	}
	if c.run.specDone == nil {
		c.run.specDone = map[types.Object]bool{}
	}
	if c.run.specDone[nt.Obj()] {
		return
	}
	// The embedded agent.Tool, if any: its old methods are gone after the rewrite.
	embed := ""
	if st, ok := base.Underlying().(*types.Struct); ok {
		for i := range st.NumFields() {
			if f := st.Field(i); f.Embedded() && isNamed(f.Type(), agentPath, "Tool") {
				embed = f.Name()
			}
		}
	}
	// The old methods the type declares itself (the ones that keep overriding), and whether every
	// one it has comes from itself or from the embedded tool.
	declared := map[string]*types.Func{}
	for i := range nt.NumMethods() {
		if m := nt.Method(i); oldToolMethods[m.Name()] != "" {
			declared[m.Name()] = m
		}
	}
	for _, name := range []string{"Name", "Description", "ArgsSchema", "Safety"} {
		if declared[name] != nil {
			if c.Pkg.Fset.File(declared[name].Pos()) != c.Pkg.Fset.File(fd.Pos()) {
				c.run.specDone[nt.Obj()] = true
				c.Manual(fd, "%s declares the old Tool methods in more than one file: give it a Spec method (agent.ToolSpec) by hand", nt.Obj().Name())
				return
			}
			continue
		}
		// Not declared: with an embedded agent.Tool it can only be the tool's (another embedded
		// type providing it at the same depth would make it ambiguous).
		if hasMethods(ptr, name) && embed == "" {
			c.run.specDone[nt.Obj()] = true
			c.Manual(fd, "%s gets its %s method from an embedded type other than agent.Tool: give it a Spec method (agent.ToolSpec) by hand", nt.Obj().Name(), name)
			return
		}
	}
	// Generate it once, at the first declared old method in the file.
	for _, m := range declared {
		if m.Pos() < obj.Pos() {
			return
		}
	}
	c.run.specDone[nt.Obj()] = true
	recv := "t"
	if names := fd.Recv.List[0].Names; len(names) > 0 && names[0].Name != "_" {
		recv = names[0].Name
	}
	// The receiver: a pointer if any declared old method has one, so a value keeps the method set
	// it had (a value's promoted methods were the embedded tool's, as its Spec will be).
	typ := strings.TrimPrefix(c.Ed.Orig(fd.Recv.List[0].Type), "*")
	for _, m := range declared {
		if _, isPtr := m.Type().(*types.Signature).Recv().Type().(*types.Pointer); isPtr {
			typ = "*" + typ
			break
		}
	}
	A := c.A()
	var start string
	switch {
	case embed != "":
		start = fmt.Sprintf("s := %s.%s.Spec()", recv, embed)
	case hasMethods(ptr, "Unwrap"):
		// SpecOf read a nil Unwrap() as no wrapped tool
		start = fmt.Sprintf("var s %sToolSpec\nif u := %s.Unwrap(); u != nil {\ns = u.Spec()\n}", A, recv)
	default:
		if len(declared) != 4 {
			c.Manual(fd, "%s implements the old Tool methods only in part: give it a Spec method (agent.ToolSpec) by hand", nt.Obj().Name())
			return
		}
		start = fmt.Sprintf("var s %sToolSpec", A)
	}
	body := []string{start}
	for _, name := range []string{"Name", "Description", "ArgsSchema", "Safety"} {
		if declared[name] != nil {
			body = append(body, fmt.Sprintf("s.%s = %s.%s()", oldToolMethods[name], recv, name))
		}
	}
	body = append(body, "return s")
	c.Ed.InsertAfter(fd, fmt.Sprintf("\n\n// Spec describes the tool to the agent (see %sTool).\nfunc (%s %s) Spec() %sToolSpec {\n%s\n}\n", A, recv, typ, A, strings.Join(body, "\n")))
	c.Count()
}
