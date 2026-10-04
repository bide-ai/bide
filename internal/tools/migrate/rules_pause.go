package main

import (
	"go/ast"
	"strings"
)

// The pause rule: the pause contract and the verbs under their final names (docs/design/api-v1.md,
// item 6, and section 13).
//
//   - The aliases PendingApproval, Interrupted, Awaiting, Sleeping and ResumeHalt become
//     ApprovalPending, InterruptPending, SignalPending, TimerPending and OutcomeUnknown.
//   - ResolveHaltRef becomes ResolveHalt; the old ResolveHalt(ctx, j, runID, toolUseID, result,
//     isError, opts...) and ResolveStepHalt(..., name, ...) become
//     ResolveHalt(ctx, j, agent.HaltRef{RunID: runID, Op: agent.OpRef{Kind: agent.OpTool, ID:
//     toolUseID}, Cause: agent.HaltCrashed}, agent.Outcome{Result: result, IsError: isError},
//     opts...) (OpStep for a step), as the old functions built them.
//   - Resume[T] (the old name of AnswerInterrupt) and the channel's Send[T] (Enqueue's) become
//     their new names.
//   - The journal verbs Step, Parallel, Signal, Enqueue and AnswerInterrupt become generic
//     methods of *Journal: agent.Step(ctx, j, runID, ...) becomes j.Step(ctx, runID, ...).
//   - The context decorators ContextWithIdentity, ContextWithWaker and ContextWithClock become
//     the run options WithIdentity, WithWaker and WithClock where the decorated context is the
//     context of a run entry point call; elsewhere the site is reported.

var pauseAliases = map[string]string{
	"PendingApproval": "ApprovalPending",
	"Interrupted":     "InterruptPending",
	"Awaiting":        "SignalPending",
	"Sleeping":        "TimerPending",
	"ResumeHalt":      "OutcomeUnknown",
}

// verbMethods are the journal verbs that become methods of *Journal, with the old names that
// become them.
var verbMethods = map[string]string{
	"Step": "Step", "Parallel": "Parallel", "Signal": "Signal", "Enqueue": "Enqueue",
	"AnswerInterrupt": "AnswerInterrupt", "Resume": "AnswerInterrupt", "Send": "Enqueue",
}

var decorators = map[string]string{
	"ContextWithIdentity": "WithIdentity",
	"ContextWithWaker":    "WithWaker",
	"ContextWithClock":    "WithClock",
}

// runEntries are the methods whose first argument is a run's context and whose trailing
// arguments are run options, by receiver.
var runEntries = map[string]map[string]bool{
	"Agent":   {"Run": true, "Resume": true, "Stream": true, "RunTyped": true, "RunMessage": true, "ResumeRun": true, "StreamMessage": true, "RunTypedMessage": true},
	"Session": {"Send": true, "SendOnce": true, "SendMessage": true, "SendMessageOnce": true},
}

func visitPause(c *Ctx, n ast.Node) {
	switch n := n.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		e := n.(ast.Expr)
		for old, nw := range pauseAliases {
			if c.isTypeName(e, agentPath, old) {
				replaceName(c, e, nw)
				c.Count()
				return
			}
		}
		if c.isTypeName(e, agentPath, "ResolveHaltRef") {
			replaceName(c, e, "ResolveHalt")
			c.Count()
		}
	case *ast.CallExpr:
		visitPauseCall(c, n)
	}
}

func visitPauseCall(c *Ctx, call *ast.CallExpr) {
	fun := ast.Unparen(call.Fun)
	var typeArgs []ast.Expr
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun, typeArgs = f.X, []ast.Expr{f.Index}
	case *ast.IndexListExpr:
		fun, typeArgs = f.X, f.Indices
	}
	r, ok := c.refOf(fun)
	if !ok || r.Pkg != agentPath || r.Recv != "" {
		if ok && r.Pkg == agentPath && runEntries[r.Recv][r.Name] && len(call.Args) > 0 {
			undecorate(c, call)
		}
		return
	}
	A := c.A()
	text := func(from int) []string {
		var out []string
		for _, a := range call.Args[from:] {
			out = append(out, c.Ed.Text(a))
		}
		return out
	}
	ell := ""
	if call.Ellipsis.IsValid() {
		ell = "..."
	}
	switch r.Name {
	case "ResolveHalt", "ResolveStepHalt":
		if len(call.Args) < 6 || !c.isString(call.Args[2]) {
			return // the new ResolveHalt(ctx, j, ref, outcome, opts...)
		}
		kind := "OpTool"
		if r.Name == "ResolveStepHalt" {
			kind = "OpStep"
		}
		a := text(0)
		ref := A + "HaltRef{RunID: " + a[2] + ", Op: " + A + "OpRef{Kind: " + A + kind + ", ID: " + a[3] + "}, Cause: " + A + "HaltCrashed}"
		out := A + "Outcome{Result: " + a[4] + ", IsError: " + a[5] + "}"
		args := append([]string{a[0], a[1], ref, out}, a[6:]...)
		c.Ed.Replace(call, A+"ResolveHalt("+strings.Join(args, ", ")+ell+")")
		c.Count()
	case "Step", "Parallel", "Signal", "Enqueue", "AnswerInterrupt", "Resume", "Send":
		if len(call.Args) < 3 {
			return
		}
		if r.Name == "Send" && len(call.Args) != 6 {
			return
		}
		if r.Name == "Resume" && len(typeArgs) == 0 && len(call.Args) != 5 {
			return
		}
		c.Ed.Replace(fun, recvText(c, call.Args[1])+"."+verbMethods[r.Name])
		c.Ed.DeleteArg(call, 1)
		c.Count()
	case "ContextWithIdentity", "ContextWithWaker", "ContextWithClock":
		if p, ok := c.Parent(1).(*ast.CallExpr); ok && len(p.Args) > 0 && p.Args[0] == call {
			if pr, ok := c.refOf(p.Fun); ok && pr.Pkg == agentPath && runEntries[pr.Recv][pr.Name] {
				return // the run entry point moves it to an option (undecorate)
			}
		}
		c.Manual(call, "%s is removed: pass %s%s(...) to the run entry point the context reaches (Run, Resume, Stream, RunTyped, Session.Send), or to agent.New for every run", r.Name, A, decorators[r.Name])
	}
}

// nonEmptyIdentity reports whether e is an agent.Identity literal with a field set to a non-empty
// string constant, which WithIdentity accepts.
func (c *Ctx) nonEmptyIdentity(e ast.Expr) bool {
	lit, ok := ast.Unparen(e).(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			el = kv.Value
		}
		if tv, ok := c.Info.Types[el]; ok && tv.Value != nil && tv.Value.ExactString() != `""` {
			return true
		}
	}
	return false
}

// undecorate moves context decorators wrapped around a run entry point's context argument to run
// options: a.Run(agent.ContextWithWaker(ctx, w), id, in) becomes a.Run(ctx, id, in,
// agent.WithWaker(w)).
func undecorate(c *Ctx, call *ast.CallExpr) {
	var opts []string
	ctx := call.Args[0]
	for {
		inner, ok := ast.Unparen(ctx).(*ast.CallExpr)
		if !ok || len(inner.Args) != 2 {
			break
		}
		r, ok := c.refOf(inner.Fun)
		if !ok || r.Pkg != agentPath || decorators[r.Name] == "" {
			break
		}
		if r.Name == "ContextWithIdentity" && !c.nonEmptyIdentity(inner.Args[1]) {
			c.Manual(inner, "WithIdentity refuses an empty identity (ErrConfig), which ContextWithIdentity bound: rewritten; make sure %s is never empty", c.Ed.Orig(inner.Args[1]))
		}
		opts = append([]string{c.A() + decorators[r.Name] + "(" + c.Ed.Text(inner.Args[1]) + ")"}, opts...)
		ctx = inner.Args[0]
	}
	if len(opts) == 0 {
		return
	}
	if call.Ellipsis.IsValid() {
		c.Manual(call, "context decorators around a call that passes its options as a slice: add %s to the slice", strings.Join(opts, ", "))
		return
	}
	var args []string
	args = append(args, c.Ed.Text(ctx))
	for _, a := range call.Args[1:] {
		args = append(args, c.Ed.Text(a))
	}
	args = append(args, opts...)
	c.Ed.Replace(call, c.Ed.Text(call.Fun)+"("+strings.Join(args, ", ")+")")
	c.Count()
}
