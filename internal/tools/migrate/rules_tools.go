package main

import (
	"fmt"
	"go/ast"
	"go/types"
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

// addSpecMethod adds a Spec method after the Name method of a type that implements the old Tool
// method set and has no Spec.
func addSpecMethod(c *Ctx, fd *ast.FuncDecl) {
	if fd.Recv == nil || fd.Name.Name != "Name" || len(fd.Recv.List) != 1 {
		return
	}
	obj, ok := c.Info.Defs[fd.Name].(*types.Func)
	if !ok {
		return
	}
	recvT := obj.Type().(*types.Signature).Recv().Type()
	ptr := recvT
	if _, isPtr := recvT.(*types.Pointer); !isPtr {
		ptr = types.NewPointer(recvT)
	}
	if !hasMethods(ptr, "Name", "Description", "ArgsSchema", "Safety", "Call") || hasMethods(ptr, "Spec") {
		return
	}
	recv := "t"
	if names := fd.Recv.List[0].Names; len(names) > 0 && names[0].Name != "_" {
		recv = names[0].Name
	}
	A := c.A()
	recvDecl := recv + " " + c.Ed.Orig(fd.Recv.List[0].Type)
	var body string
	if hasMethods(ptr, "Unwrap") {
		body = fmt.Sprintf("s := %s.Unwrap().Spec()\ns.Name, s.Description, s.Input, s.Safety = %s.Name(), %s.Description(), %s.ArgsSchema(), %s.Safety()\nreturn s", recv, recv, recv, recv, recv)
	} else {
		body = fmt.Sprintf("return %sToolSpec{Name: %s.Name(), Description: %s.Description(), Input: %s.ArgsSchema(), Safety: %s.Safety()}", A, recv, recv, recv, recv)
	}
	c.Ed.InsertAfter(fd, fmt.Sprintf("\n\n// Spec describes the tool to the agent (see %sTool).\nfunc (%s) Spec() %sToolSpec {\n%s\n}\n", A, recvDecl, A, body))
	c.Count()
}
