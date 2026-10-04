package main

import (
	"fmt"
	"go/ast"
	"strings"
)

// The run rule: the Run API under its final names (docs/design/api-v1.md, item 1).
//
//   - a.Run(ctx, id, text) and a.RunSaga(ctx, id, text), which returned the answer, become
//     a.Run(ctx, id, agent.UserText(text)) (with agent.WithSaga() for a saga), which returns a
//     *Result: the answer is its Message. RunResult and RunSagaResult become the same call (their
//     Result is now non-nil whatever the error, once the run ID is valid).
//   - a.Stream(ctx, id, text) and StreamSaga become a.Stream(ctx, id, agent.UserText(text)[,
//     agent.WithSaga()]); a stream's Final() becomes Result(), whose Message is the answer.
//   - Session.Send(ctx, text) and SendOnce(ctx, key, text) take agent.UserText(text) and return a
//     *Result.
//   - agent.RunTyped[T](ctx, a, id, text) and RunTypedNative become the method
//     a.RunTyped[T](ctx, id, agent.UserText(text)[, agent.WithOutputMode(agent.OutputNative)]),
//     which also returns the run's *Result.
//   - The transitional names become the final ones: RunMessage, ResumeRun, StreamMessage,
//     RunTypedMessage, SendMessage, SendMessageOnce; AgentStream and AgentEvent become RunStream
//     and RunEvent; audit.Record becomes audit.RecordStream, which returns the run's *Result.
//
// Where the old call's answer was assigned, the rewrite takes it from the Result: after the
// error check that follows the call when there is one (the Result is non-nil once err is nil),
// and otherwise behind a nil check (a run ID that fails ValidateRunID, or a session turn refused
// before it starts, has no Result).

// renames maps a transitional method of an agent type to its final name.
var renames = map[string]map[string]string{
	"Agent":   {"RunMessage": "Run", "ResumeRun": "Resume", "StreamMessage": "Stream", "RunTypedMessage": "RunTyped"},
	"Session": {"SendMessage": "Send", "SendMessageOnce": "SendOnce"},
}

func visitRun(c *Ctx, n ast.Node) {
	switch n := n.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		e := n.(ast.Expr)
		for old, nw := range map[string]string{"AgentStream": "RunStream", "AgentEvent": "RunEvent"} {
			if c.isTypeName(e, agentPath, old) {
				replaceName(c, e, nw)
				c.Count()
			}
		}
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if _, isCall := c.Parent(1).(*ast.CallExpr); !isCall || c.Parent(1).(*ast.CallExpr).Fun != sel {
				if _, isIdx := c.Parent(1).(*ast.IndexExpr); !isIdx {
					if r, ok := c.refOf(sel); ok && r.Pkg == agentPath && r.Recv == "Agent" && oldEntry[r.Name] {
						c.Manual(sel, "%s is removed and not called here (a method value): use Run with agent.UserText and the options it needs", r.Name)
					}
				}
			}
		}
	case *ast.CallExpr:
		visitRunCall(c, n)
	}
}

// oldEntry names the string entry points of an Agent the rule rewrites.
var oldEntry = map[string]bool{"RunSaga": true, "RunResult": true, "RunSagaResult": true, "StreamSaga": true}

// replaceName replaces the name in an identifier or selector reference.
func replaceName(c *Ctx, e ast.Expr, name string) {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		c.Ed.Replace(e.Sel, name)
	case *ast.Ident:
		c.Ed.Replace(e, name)
	}
}

func visitRunCall(c *Ctx, call *ast.CallExpr) {
	fun := ast.Unparen(call.Fun)
	var typeArgs []ast.Expr
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun, typeArgs = f.X, []ast.Expr{f.Index}
	case *ast.IndexListExpr:
		fun, typeArgs = f.X, f.Indices
	}
	r, ok := c.refOf(fun)
	if !ok {
		return
	}
	A := c.A()
	args := func(from int) []string {
		var out []string
		for _, a := range call.Args[from:] {
			out = append(out, c.Ed.Text(a))
		}
		return out
	}
	userText := func(e ast.Expr) string {
		if c.isString(e) {
			return A + "UserText(" + c.Ed.Text(e) + ")"
		}
		return c.Ed.Text(e)
	}
	sel, _ := fun.(*ast.SelectorExpr)
	switch {
	case r.Pkg == agentPath && renames[r.Recv] != nil && renames[r.Recv][r.Name] != "" && sel != nil:
		c.Ed.Replace(sel.Sel, renames[r.Recv][r.Name])
		c.Count()

	case r.Pkg == agentPath && r.Recv == "Agent" && sel != nil:
		recv := c.Ed.Text(sel.X)
		switch r.Name {
		case "Run", "Stream":
			if len(call.Args) != 3 || !c.isString(call.Args[2]) {
				return // the new Run and Stream
			}
			a := args(0)
			a[2] = userText(call.Args[2])
			text := recv + "." + r.Name + "(" + strings.Join(a, ", ") + ")"
			if r.Name == "Run" {
				reshapeMessage(c, call, text, A)
			} else {
				c.Ed.Replace(call, text)
				c.Count()
			}
		case "RunSaga", "RunResult", "RunSagaResult", "StreamSaga":
			if len(call.Args) != 3 {
				return
			}
			a := args(0)
			a[2] = userText(call.Args[2])
			if strings.Contains(r.Name, "Saga") {
				a = append(a, A+"WithSaga()")
			}
			name := "Run"
			if r.Name == "StreamSaga" {
				name = "Stream"
			}
			text := recv + "." + name + "(" + strings.Join(a, ", ") + ")"
			if r.Name == "RunSaga" {
				reshapeMessage(c, call, text, A)
			} else {
				c.Ed.Replace(call, text)
				c.Count()
			}
		}

	case r.Pkg == agentPath && r.Recv == "Session" && sel != nil:
		recv := c.Ed.Text(sel.X)
		switch {
		case r.Name == "Send" && len(call.Args) == 2 && c.isString(call.Args[1]),
			r.Name == "SendOnce" && len(call.Args) == 3 && c.isString(call.Args[2]):
			a := args(0)
			a[len(a)-1] = userText(call.Args[len(call.Args)-1])
			reshapeMessage(c, call, recv+"."+r.Name+"("+strings.Join(a, ", ")+")", A)
		}

	case r.Pkg == agentPath && (r.Recv == "AgentStream" || r.Recv == "RunStream") && r.Name == "Final" && sel != nil:
		reshapeMessage(c, call, c.Ed.Text(sel.X)+".Result()", A)

	case r.Pkg == auditPath && r.Recv == "" && r.Name == "Record" && len(call.Args) == 3:
		reshapeMessage(c, call, c.Q(auditPath)+"RecordStream("+strings.Join(args(0), ", ")+")", A)

	case r.Pkg == agentPath && r.Recv == "" && (r.Name == "RunTyped" || r.Name == "RunTypedNative"):
		if len(call.Args) != 4 || len(typeArgs) != 1 {
			c.Manual(call, "%s with an unexpected shape: rewrite it as a.RunTyped[T](ctx, runID, input, opts...)", r.Name)
			return
		}
		a := []string{c.Ed.Text(call.Args[0]), c.Ed.Text(call.Args[2]), userText(call.Args[3])}
		if r.Name == "RunTypedNative" {
			a = append(a, A+"WithOutputMode("+A+"OutputNative)")
		}
		recv := c.Ed.Text(call.Args[1])
		if needsParens(call.Args[1]) {
			recv = "(" + recv + ")"
		}
		text := recv + ".RunTyped[" + c.Ed.Text(typeArgs[0]) + "](" + strings.Join(a, ", ") + ")"
		reshapeTyped(c, call, text)
	}
}

// needsParens reports whether e must be parenthesized as a method call's receiver.
func needsParens(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Ident, *ast.SelectorExpr, *ast.CallExpr, *ast.ParenExpr, *ast.IndexExpr:
		return false
	}
	return true
}

// reshapeMessage replaces call, which returned (Message, error), with text, which returns
// (*Result, error), and rewrites the statement around it to take the answer from the Result.
func reshapeMessage(c *Ctx, call *ast.CallExpr, text, A string) {
	switch p := c.Parent(1).(type) {
	case *ast.ExprStmt, *ast.GoStmt, *ast.DeferStmt:
		c.Ed.Replace(call, text)
		c.Count()
		return
	case *ast.AssignStmt:
		if len(p.Rhs) != 1 || p.Rhs[0] != call || len(p.Lhs) != 2 {
			break
		}
		msg := p.Lhs[0]
		if id, ok := msg.(*ast.Ident); ok && id.Name == "_" {
			c.Ed.Replace(call, text)
			c.Count()
			return
		}
		stmt, depth := c.stmtInList()
		if stmt != ast.Stmt(p) {
			break // not a statement of a list (an if statement's init, say): the answer stays scoped
		}
		res := c.fresh("res")
		errText := c.Ed.Text(p.Lhs[1])
		tok := c.tok(p)
		msgText := c.Ed.Text(msg)
		msgID, _ := msg.(*ast.Ident)
		isNew := tok == ":=" && msgID != nil && c.Info.Defs[msgID] != nil
		head := fmt.Sprintf("%s, %s %s %s", res, errText, tok, text)
		if tok == "=" {
			head = "var " + res + " *" + A + "Result\n" + head // the assignment's other variables exist
		}
		next := c.nextStmt(p, depth)
		if errID, ok := p.Lhs[1].(*ast.Ident); ok && errID.Name != "_" && next != nil && isTerminatingErrCheck(next, errID.Name) && !c.mentionsObj(next, msgID) {
			c.Ed.Replace(p, head)
			assign := " = "
			if isNew {
				assign = " := "
			}
			c.Ed.InsertAfter(next, "\n"+msgText+assign+res+".Message")
			c.Count()
			return
		}
		decl := ""
		if isNew {
			decl = "\nvar " + msgText + " " + A + "Message"
		}
		c.Ed.Replace(p, head+decl+"\nif "+res+" != nil {\n"+msgText+" = "+res+".Message\n}")
		c.Count()
		return
	case *ast.ReturnStmt:
		if len(p.Results) != 1 {
			break
		}
		sig := c.funcSig()
		if sig == nil || sig.Results().Len() != 2 {
			break
		}
		res, err := c.fresh("res"), "err"
		c.Ed.Replace(p, fmt.Sprintf("%s, %s := %s\nif %s != nil {\nreturn %s, %s\n}\nreturn %s.Message, nil",
			res, err, text, err, c.zero(sig.Results().At(0).Type()), err, res))
		c.Count()
		return
	}
	if c.IsTest() {
		// any other use (an if statement's init, an argument): Answer gives back the old pair
		c.Ed.Replace(call, c.helper("Answer")+"("+text+")")
		c.Count()
		return
	}
	c.Manual(call, "the answer of this call is now the Message of the *Result it returns: rewrite the use by hand (to %s)", text)
}

// reshapeTyped replaces call, which returned (T, error), with text, which returns (T, *Result,
// error).
func reshapeTyped(c *Ctx, call *ast.CallExpr, text string) {
	switch p := c.Parent(1).(type) {
	case *ast.ExprStmt, *ast.GoStmt, *ast.DeferStmt:
		c.Ed.Replace(call, text)
		c.Count()
		return
	case *ast.AssignStmt:
		if len(p.Rhs) == 1 && p.Rhs[0] == call && len(p.Lhs) == 2 {
			c.Ed.Replace(p, c.Ed.Text(p.Lhs[0])+", _, "+c.Ed.Text(p.Lhs[1])+" "+c.tok(p)+" "+text)
			c.Count()
			return
		}
	case *ast.ReturnStmt:
		if len(p.Results) == 1 {
			v, err := c.fresh("v"), "err"
			c.Ed.Replace(p, fmt.Sprintf("%s, _, %s := %s\nreturn %s, %s", v, err, text, v, err))
			c.Count()
			return
		}
	}
	c.Manual(call, "RunTyped is now a method that also returns the run's *Result: rewrite the use by hand (to %s)", text)
}

// mentionsObj reports whether n uses the object id defines or uses.
func (c *Ctx) mentionsObj(n ast.Node, id *ast.Ident) bool {
	if id == nil {
		return false
	}
	obj := c.objOf(id)
	return obj != nil && c.mentions(n, obj)
}
