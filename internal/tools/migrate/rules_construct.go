package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strings"
)

// The construct rule: New(model, store, tools...) and its builder methods become
// New(model, journal, options...), which returns an error; Build becomes New.
//
// A builder chain on the constructor (agent.New(...).WithMaxTurns(3)) becomes options of the
// one call, in order. Builder statements on a variable that follow its construction
// (a := agent.New(...); a.Use(mw)) are folded into it while nothing else has used the variable
// in between; a builder call on an agent that is already in use is reported, since the old
// methods changed the agent in place while With returns a copy.

// builderOpts maps the old builder methods to the options that replace them.
var builderOpts = map[string]string{
	"Use":                   "WithMiddleware",
	"UseTool":               "WithToolMiddleware",
	"WithSampling":          "WithSampling",
	"WithToolChoice":        "WithToolChoice",
	"WithSystemPrompt":      "WithSystemPrompt",
	"WithSystemPromptFunc":  "WithSystemPromptFunc",
	"WithMaxTurns":          "WithMaxTurns",
	"WithTokenBudget":       "WithTokenBudget",
	"SetMaxConcurrency":     "WithMaxConcurrency",
	"WithApproverVerifiers": "WithApproverVerifiers",
	"WithToolErrorRedactor": "WithToolErrorRedactor",
}

// construction is a statement that builds an agent into a variable, which later builder
// statements may be folded into.
type construction struct {
	obj  types.Object
	emit func(extra []string) // re-emits the construction with extra options
}

// builderStmt is a statement that calls builder methods on an agent variable.
type builderStmt struct {
	obj    types.Object
	opts   []string
	folded bool
}

// isBuilder reports whether call is a call of an old builder method on an *agent.Agent.
func (c *Ctx) isBuilder(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if _, ok := builderOpts[sel.Sel.Name]; !ok {
		return false
	}
	r, ok := c.refOf(sel)
	if !ok || !r.is(agentPath, "Agent", sel.Sel.Name) {
		return false
	}
	// the old methods return *Agent alone; With and the new API never have these names
	if sig, ok := c.Info.TypeOf(call.Fun).(*types.Signature); ok && sig.Results().Len() != 1 {
		return false
	}
	return true
}

// isOldNew reports whether call is the old agent.New(model, store, tools...).
func (c *Ctx) isOldNew(call *ast.CallExpr) bool {
	r, ok := c.callee(call)
	if !ok || !r.is(agentPath, "", "New") || len(call.Args) < 2 {
		return false
	}
	if sig, ok := c.Info.TypeOf(call.Fun).(*types.Signature); ok && sig.Results().Len() == 1 {
		return true
	}
	if call.Ellipsis.IsValid() {
		// New(model, store, tools...) or New(model, j, opts...)
		if sl, ok := c.Info.TypeOf(call.Args[len(call.Args)-1]).(*types.Slice); ok {
			return hasMethods(sl.Elem(), "Call")
		}
	}
	if c.singleValue(call) {
		return true // the new New returns an error too, so a use as one value is the old New
	}
	if as, ok := c.Parent(1).(*ast.AssignStmt); ok && len(as.Lhs) == 2 {
		return false // the old New returned one value
	}
	// unresolved against the new New: old if it passes a store, or a tool
	if t := c.Info.TypeOf(call.Args[1]); t != nil && t != types.Typ[types.Invalid] && !isJournalType(t) {
		return true
	}
	for _, a := range call.Args[2:] {
		if hasMethods(c.Info.TypeOf(a), "Call") {
			return true
		}
	}
	return false
}

// optionText returns the option that replaces one builder call, built from its arguments' text.
func (c *Ctx) optionText(call *ast.CallExpr) string {
	sel := call.Fun.(*ast.SelectorExpr)
	opt := c.A() + builderOpts[sel.Sel.Name]
	args := make([]string, len(call.Args))
	for i, a := range call.Args {
		args[i] = c.Ed.Text(a)
	}
	ell := ""
	if call.Ellipsis.IsValid() {
		ell = "..."
	}
	switch sel.Sel.Name {
	case "WithMaxTurns", "WithTokenBudget", "SetMaxConcurrency":
		// the builders clamped a negative value to 0 (unbounded); the options refuse one
		if tv := c.Info.Types[call.Args[0]]; tv.Value != nil {
			if v, ok := constant.Int64Val(tv.Value); ok && v < 0 {
				args[0] = "0"
			}
		} else {
			args[0] = "max(" + args[0] + ", 0)"
		}
	case "WithSystemPromptFunc":
		// the builder took func(ctx) string; the option takes func(ctx, RunInfo) (string, error)
		if lit, ok := ast.Unparen(call.Args[0]).(*ast.FuncLit); ok && len(lit.Type.Params.List) == 1 {
			args[0] = c.promptFuncLit(lit)
			break
		}
		ctx := c.fresh("pctx")
		args[0] = fmt.Sprintf("func(%s %sContext, _ %sRunInfo) (string, error) { return %s(%s), nil }", ctx, c.Q("context"), c.A(), args[0], ctx)
	}
	return opt + "(" + strings.Join(args, ", ") + ell + ")"
}

// chain unwinds a builder chain from its outermost call: the builder calls, innermost first, and
// the expression they are called on.
func (c *Ctx) chain(top *ast.CallExpr) ([]*ast.CallExpr, ast.Expr) {
	var calls []*ast.CallExpr
	var e ast.Expr = top
	for {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok || !c.isBuilder(call) {
			break
		}
		calls = append([]*ast.CallExpr{call}, calls...)
		e = call.Fun.(*ast.SelectorExpr).X
	}
	return calls, e
}

// isChainTop reports whether call (on top of the stack) is not the receiver of a further
// builder call.
func (c *Ctx) isChainTop(call *ast.CallExpr) bool {
	sel, ok := c.Parent(1).(*ast.SelectorExpr)
	if !ok || sel.X != call {
		return true
	}
	outer, ok := c.Parent(2).(*ast.CallExpr)
	return !ok || outer.Fun != sel || !c.isBuilder(outer)
}

func visitConstruct(c *Ctx, n ast.Node) {
	switch n := n.(type) {
	case *ast.BlockStmt:
		foldBuilders(c, n.List)
	case *ast.CaseClause:
		foldBuilders(c, n.Body)
	case *ast.CommClause:
		foldBuilders(c, n.Body)
	case *ast.CallExpr:
		if r, ok := c.callee(n); ok && r.is(agentPath, "", "Build") {
			switch f := ast.Unparen(n.Fun).(type) {
			case *ast.SelectorExpr:
				c.Ed.Replace(f.Sel, "New")
			case *ast.Ident:
				c.Ed.Replace(f, "New")
			}
			c.Count()
			return
		}
		switch {
		case c.isBuilder(n):
			if !c.isChainTop(n) {
				return
			}
			calls, root := c.chain(n)
			var opts []string
			for _, b := range calls {
				opts = append(opts, c.optionText(b))
			}
			if call, ok := ast.Unparen(root).(*ast.CallExpr); ok && c.isOldNew(call) {
				emitConstruct(c, n, call, opts)
				return
			}
			if call, ok := ast.Unparen(root).(*ast.CallExpr); ok && c.isMustNew(call) {
				// a construction an earlier run of the rule made: the options join it
				c.Ed.Replace(n, appendCallArgs(c.Ed.Text(call), opts))
				c.Count()
				return
			}
			if id, ok := ast.Unparen(root).(*ast.Ident); ok {
				if obj, ok := c.Info.Uses[id].(*types.Var); ok {
					if stmt := c.builderStatement(n, obj); stmt != nil {
						c.builders[stmt] = &builderStmt{obj: obj, opts: opts}
						return
					}
				}
			}
			// The builders changed their receiver in place, With returns a copy: only a receiver no
			// one else holds (a call's result) may take the copy instead.
			if _, fresh := ast.Unparen(root).(*ast.CallExpr); fresh && c.IsTest() {
				c.Ed.Replace(n, c.helper("Must")+"("+c.Ed.Text(root)+".With("+strings.Join(opts, ", ")+"))")
				c.Count()
				return
			}
			c.Manual(n, "builder methods on an agent built elsewhere: pass the options to agent.New, or use a.With (which returns a copy)")
		case c.isOldNew(n):
			if !c.isChainTop(n) {
				return // the builder chain above it emits the construction
			}
			emitConstruct(c, n, n, nil)
		}
	}
}

// builderStatement returns the statement that is the builder chain top alone (an expression
// statement, or obj = chain), or nil.
func (c *Ctx) builderStatement(top *ast.CallExpr, obj types.Object) ast.Stmt {
	switch p := c.Parent(1).(type) {
	case *ast.ExprStmt:
		return p
	case *ast.AssignStmt:
		if p.Tok == token.ASSIGN && len(p.Lhs) == 1 && len(p.Rhs) == 1 && p.Rhs[0] == top {
			if id, ok := p.Lhs[0].(*ast.Ident); ok && c.Info.Uses[id] == obj {
				return p
			}
		}
	}
	return nil
}

// emitConstruct replaces node (the old New call, or the builder chain on it) with the new
// construction: New(model, journal, WithTools(tools...), opts...).
func emitConstruct(c *Ctx, node ast.Expr, call *ast.CallExpr, opts []string) {
	model := c.Ed.Text(call.Args[0])
	journal := c.Ed.Text(call.Args[1]) // the journal rule made it a journal
	var all []string
	if len(call.Args) > 2 {
		var tools []string
		for _, t := range call.Args[2:] {
			tools = append(tools, c.Ed.Text(t))
		}
		ell := ""
		if call.Ellipsis.IsValid() {
			ell = "..."
		}
		all = append(all, c.A()+"WithTools("+strings.Join(tools, ", ")+ell+")")
	}
	all = append(all, opts...)
	text := func(extra []string) string {
		args := append([]string{model, journal}, append(append([]string(nil), all...), extra...)...)
		if s := strings.Join(args, ", "); len(s) <= 90 && !strings.Contains(s, "\n") {
			return s
		}
		return "\n" + strings.Join(args, ",\n") + ",\n"
	}
	c.Count()
	// the statement it defines or assigns, for folding later builder statements
	var stmt ast.Stmt
	var obj types.Object
	switch p := c.Parent(1).(type) {
	case *ast.AssignStmt:
		if len(p.Lhs) == 1 && len(p.Rhs) == 1 {
			if id, ok := p.Lhs[0].(*ast.Ident); ok {
				stmt, obj = p, c.objOf(id)
			}
		}
	case *ast.ValueSpec:
		if len(p.Names) == 1 && len(p.Values) == 1 {
			if ds, ok := c.Parent(3).(*ast.DeclStmt); ok {
				stmt, obj = ds, c.objOf(p.Names[0])
			}
		}
	}
	if c.IsTest() {
		emit := func(extra []string) { c.Ed.Replace(node, c.helper("MustNew")+"("+text(extra)+")") }
		emit(nil)
		if stmt != nil && obj != nil {
			c.constructs[stmt] = &construction{obj: obj, emit: emit}
		}
		return
	}
	if stmt != nil && obj != nil {
		if as, ok := stmt.(*ast.AssignStmt); ok && (as.Tok == token.DEFINE || as.Tok == token.ASSIGN) {
			lhs := c.Ed.Text(as.Lhs[0])
			tok := ":="
			if as.Tok == token.DEFINE {
				c.claimErr(as.Pos())
			}
			decl := ""
			if as.Tok == token.ASSIGN {
				tok = "="
				if !c.errDeclared(as.Pos()) {
					decl = "var err error\n"
				}
			}
			onErr := c.onErr(as.Pos(), "err")
			emit := func(extra []string) {
				c.Ed.Replace(as, fmt.Sprintf("%s%s, err %s %sNew(%s)\n%s", decl, lhs, tok, c.A(), text(extra), onErr))
			}
			emit(nil)
			c.constructs[stmt] = &construction{obj: obj, emit: emit}
			return
		}
	}
	name, ok := c.fallible(node, c.A()+"New("+text(nil)+")", "", "ag")
	if ok {
		c.Ed.Replace(node, name)
	}
}

// objOf returns the object an identifier defines or uses.
func (c *Ctx) objOf(id *ast.Ident) types.Object {
	if o := c.Info.Defs[id]; o != nil {
		return o
	}
	return c.Info.Uses[id]
}

// foldBuilders folds builder statements into the construction of their variable, in a statement
// list whose statements have all been rewritten.
func foldBuilders(c *Ctx, list []ast.Stmt) {
	for i, s := range list {
		if b := c.builders[s]; b != nil && !b.folded {
			c.Manual(s, "builder methods on an agent that was built elsewhere or is already in use: pass the options to agent.New, or use a.With (which returns a copy)")
			continue
		}
		k := c.constructs[s]
		if k == nil {
			continue
		}
		var extra []string
		for _, t := range list[i+1:] {
			if b := c.builders[t]; b != nil && b.obj == k.obj {
				extra = append(extra, b.opts...)
				b.folded = true
				c.Ed.DeleteStmt(t)
				continue
			}
			if c.mentions(t, k.obj) {
				break
			}
		}
		if len(extra) > 0 {
			k.emit(extra)
		}
	}
}

// mentions reports whether n uses obj.
func (c *Ctx) mentions(n ast.Node, obj types.Object) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && c.Info.Uses[id] == obj {
			found = true
		}
		return !found
	})
	return found
}

// promptFuncLit rewrites the function literal func(ctx context.Context) string { ... } that the
// WithSystemPromptFunc builder took as the function the option takes, func(ctx
// context.Context, _ agent.RunInfo) (string, error) { ... }: every return of the literal's own
// body returns a nil error too.
func (c *Ctx) promptFuncLit(lit *ast.FuncLit) string {
	p := lit.Type.Params.List[0]
	param := "_"
	if len(p.Names) == 1 {
		param = p.Names[0].Name
	}
	var rets []*ast.ReturnStmt
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			rets = append(rets, n)
		}
		return true
	})
	// the body's text with each return's value followed by ", nil"
	body := c.Ed.Text(lit.Body)
	base := int(lit.Body.Pos())
	for i := len(rets) - 1; i >= 0; i-- {
		r := rets[i]
		if len(r.Results) != 1 {
			continue
		}
		at := int(r.Results[0].End()) - base
		if at < 0 || at > len(body) || len(c.Ed.Text(lit.Body)) != int(lit.Body.End()-lit.Body.Pos()) {
			// the body has edits of its own, so offsets do not map: wrap the literal instead
			ctx := c.fresh("pctx")
			return fmt.Sprintf("func(%s %sContext, _ %sRunInfo) (string, error) { return (%s)(%s), nil }", ctx, c.Q("context"), c.A(), c.Ed.Text(lit), ctx)
		}
		body = body[:at] + ", nil" + body[at:]
	}
	return fmt.Sprintf("func(%s %s, _ %sRunInfo) (string, error) %s", param, c.Ed.Text(p.Type), c.A(), body)
}

// singleValue reports whether the call on top of the stack is used where one value goes: the
// whole right side of a one-variable assignment, an argument, a receiver, an element.
func (c *Ctx) singleValue(call *ast.CallExpr) bool {
	switch p := c.Parent(1).(type) {
	case *ast.AssignStmt:
		return len(p.Lhs) == 1 && len(p.Rhs) == 1
	case *ast.ValueSpec:
		return len(p.Names) == 1
	case *ast.CallExpr:
		return p.Fun != call && len(p.Args) > 1 // a lone argument may pass both values on: Must(New(...))
	case *ast.SelectorExpr, *ast.KeyValueExpr, *ast.CompositeLit:
		return true
	case *ast.ReturnStmt:
		sig := c.funcSig()
		return sig != nil && sig.Results().Len() == len(p.Results)
	}
	return false
}

// isMustNew reports whether call is agenttest.MustNew (or package agent's test helper mustNew).
func (c *Ctx) isMustNew(call *ast.CallExpr) bool {
	r, ok := c.callee(call)
	return ok && (r.is(agenttestPath, "", "MustNew") || r.is(agentPath, "", "mustNew"))
}

// appendCallArgs adds args to the end of the text of a call.
func appendCallArgs(call string, args []string) string {
	body := strings.TrimRight(call, " \t\n")
	body = strings.TrimSuffix(body, ")")
	trimmed := strings.TrimRight(body, " \t\n")
	sep := ", "
	if strings.HasSuffix(trimmed, ",") {
		body, sep = trimmed, " "
	}
	return body + sep + strings.Join(args, ", ") + ")"
}
