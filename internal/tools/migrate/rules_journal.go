package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

// The journal rule: the Durable interface is gone, and everything that took one takes a
// *Journal. A Durable type becomes *Journal; a store (MemStore, a SQL store, an AuditedStore)
// passed where a Durable was expected becomes a Journal over it; a store's Do, History and
// Journal shims are replaced by its journal's.
//
// A local variable holding a store that is used only as a journal is retyped (its definition
// builds the journal), so its uses keep their text; one also used as a store gets a journal
// variable beside it, defined right after it, and its journal uses name that.

// jvar is the plan for one local store variable.
type jvar struct {
	v          *types.Var
	decl       ast.Stmt // the statement defining it
	rhs        ast.Expr // its value, when the definition defines it alone
	fn         ast.Node // the function it is local to
	after      ast.Stmt // the statement to define a journal variable after
	jUses      int      // uses as a journal
	otherUses  int      // any other use
	retype     bool     // the variable becomes the journal
	name       string   // the journal variable, when not retyped
	existing   string   // a journal variable over it that the code already defines
	reassigned bool     // assigned again after its definition
	journalOps []ast.Node
}

// planJournal is the journal rule's first pass: it finds local store variables and counts how
// each is used.
func planJournal(c *Ctx, n ast.Node) {
	switch d := n.(type) {
	case *ast.Field:
		if namesDurable(d.Type) {
			for _, name := range d.Names {
				c.durableVars[c.Info.Defs[name]] = true
			}
		}
	case *ast.ValueSpec:
		if d.Type != nil && namesDurable(d.Type) {
			for _, name := range d.Names {
				c.durableVars[c.Info.Defs[name]] = true
			}
		}
	}
	id, ok := n.(*ast.Ident)
	if !ok {
		return
	}
	if v, ok := c.Info.Defs[id].(*types.Var); ok && isStoreLike(v.Type()) && !isInterface(v.Type()) {
		switch d := c.Parent(1).(type) {
		case *ast.AssignStmt:
		case *ast.ValueSpec:
			if len(d.Values) == 0 {
				return // declared without a value: assigned later, so no journal can follow the declaration
			}
		default:
			return // a parameter, a range variable, a field
		}
		stmt, depth := c.stmtInList()
		if stmt == nil {
			return
		}
		p := &jvar{v: v, decl: stmt, fn: c.funcNode(), after: stmt}
		if next := c.nextStmt(stmt, depth); next != nil && isTerminatingErrCheck(next, "err") {
			p.after = next
		}
		// a journal variable an earlier run of the rule defined after it
		for k, next := 0, c.nextStmt(p.after, depth); next != nil && k < 2; k, next = k+1, c.nextStmt(next, depth) {
			if name := c.journalOver(next, v); name != "" {
				p.existing = name
				break
			}
		}
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE && len(s.Lhs) == 1 && len(s.Rhs) == 1 && s.Lhs[0] == id {
				p.rhs = s.Rhs[0]
			}
		case *ast.DeclStmt:
			if gd, ok := s.Decl.(*ast.GenDecl); ok && len(gd.Specs) == 1 {
				if vs, ok := gd.Specs[0].(*ast.ValueSpec); ok && len(vs.Names) == 1 && len(vs.Values) == 1 && vs.Type == nil {
					p.rhs = vs.Values[0]
				}
			}
		}
		c.jplan[v] = p
		return
	}
	v, ok := c.Info.Uses[id].(*types.Var)
	if !ok {
		return
	}
	p := c.jplan[v]
	if p == nil {
		return
	}
	if as, ok := c.Parent(1).(*ast.AssignStmt); ok && as.Tok == token.ASSIGN {
		for _, l := range as.Lhs {
			if l == id {
				p.reassigned = true
				return
			}
		}
	}
	if journalUse(c, id) {
		p.jUses++
	} else {
		p.otherUses++
	}
}

func isInterface(t types.Type) bool {
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

// journalMethods are the methods that, called on a store, call a journal: the Durable shims, and
// the journal verbs, which an earlier pass made methods of *Journal.
var journalMethods = map[string]bool{"Do": true, "History": true, "Journal": true,
	"Step": true, "Parallel": true, "Signal": true, "Enqueue": true, "AnswerInterrupt": true}

// journalUse reports whether the store expression e (on top of c's stack) is used as a journal:
// where a Durable is expected, or as the receiver of a Durable shim method.
func journalUse(c *Ctx, e ast.Expr) bool {
	if sel, ok := c.Parent(1).(*ast.SelectorExpr); ok && sel.X == e {
		return journalMethods[sel.Sel.Name]
	}
	return wantsJournal(c.expected(e)) && !auditedStoreArg(c, e)
}

// wantsJournal reports whether a value of type t is expected where a journal goes: the old
// Durable, or a *Journal (code already partly migrated, where a store passed to a function that
// now takes a *Journal no longer type-checks).
func wantsJournal(t types.Type) bool { return isDurableType(t) || isJournalType(t) }

// auditedStoreArg reports whether e is the store argument of audit.NewAuditedStore, which the
// rewrite makes a store wrapper: it takes a Store, not a Durable.
func auditedStoreArg(c *Ctx, e ast.Expr) bool {
	call, ok := c.Parent(1).(*ast.CallExpr)
	if !ok || len(call.Args) == 0 || call.Args[0] != e {
		return false
	}
	r, ok := c.callee(call)
	return ok && r.is(auditPath, "", "NewAuditedStore")
}

// finishJournalPlan decides each variable's plan once the first pass has seen every use.
func finishJournalPlan(c *Ctx) {
	for _, p := range c.jplan {
		if p.jUses == 0 {
			delete(c.jplan, p.v)
			continue
		}
		if p.reassigned {
			// a journal defined after the definition would wrap its first value only: each use
			// builds its own Journal over the value it has then (journalFor)
			delete(c.jplan, p.v)
			continue
		}
		if p.existing != "" {
			p.name = p.existing
			continue
		}
		if p.otherUses == 0 && p.rhs != nil {
			p.retype = true
			continue
		}
		p.name = c.freshIn(p.fn, "j")
	}
}

// visitJournal is the journal rule's rewrite pass.
func visitJournal(c *Ctx, n ast.Node) {
	if e, ok := n.(ast.Expr); ok && c.isTypeName(e, agentPath, "Durable") {
		if c.inInterfaceEmbed() {
			c.Manual(n, "an interface embeds agent.Durable, which is removed: embed the methods it needs")
			return
		}
		c.Ed.Replace(n, "*"+c.A()+"Journal")
		c.Count()
		return
	}
	switch n := n.(type) {
	case ast.Stmt:
		journalDecl(c, n)
		return
	case *ast.CallExpr:
		if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Do" && len(n.Args) == 4 {
			if t := c.Info.TypeOf(sel.X); isStoreLike(t) || isJournalType(t) || isDurableType(t) || c.durableTyped(sel.X) {
				journalDo(c, n, sel)
				return
			}
		}
		// x.Journal() on a store: the journal itself
		if sel, ok := n.Fun.(*ast.SelectorExpr); ok && len(n.Args) == 0 && sel.Sel.Name == "Journal" && isStoreLike(c.Info.TypeOf(sel.X)) {
			if text, ok := c.journalFor(sel.X); ok {
				c.Ed.Replace(n, text)
				c.Count()
			}
			return
		}
	case *ast.SelectorExpr:
		if !isStoreLike(c.Info.TypeOf(n.X)) {
			break
		}
		switch n.Sel.Name {
		case "History", "Step", "Parallel", "Signal", "Enqueue", "AnswerInterrupt":
			if text, ok := c.journalFor(n.X); ok {
				c.Ed.Replace(n.X, text)
				c.Count()
			}
			return
		}
	}
	e, ok := n.(ast.Expr)
	if !ok {
		return
	}
	if sel, ok := c.Parent(1).(*ast.SelectorExpr); ok && sel.X == e {
		return // a receiver: handled at the selector
	}
	if auditedStoreArg(c, e) {
		if t := c.Info.TypeOf(e); isJournalType(t) || isDurableType(t) {
			c.Ed.Replace(e, c.Ed.Text(e)+".Store()")
			c.Count()
		}
		return
	}
	if !wantsJournal(c.expected(e)) {
		return
	}
	t := c.Info.TypeOf(e)
	switch {
	case isJournalType(t) || isDurableType(t):
		return
	case isStoreLike(t):
		text, ok := c.journalFor(e)
		if ok && text != c.Ed.Text(e) {
			c.Ed.Replace(e, text)
		}
		c.Count()
	case named(t) != nil:
		c.Manual(e, "a %s is used as an agent.Durable, which is removed: make it an agent.Store (wrap the store, not the journal) and pass a Journal over it", c.typeString(t))
	}
}

// journalDecl rewrites the definition of a planned store variable.
func journalDecl(c *Ctx, s ast.Stmt) {
	for _, p := range c.jplan {
		switch {
		case p.retype && p.decl == s:
			if c.IsTest() {
				text := c.helper("MustJournal") + "(" + c.Ed.Text(p.rhs) + ")"
				if c.isMemStoreCall(p.rhs) {
					text = c.helper("MemJournal") + "()"
				}
				c.Ed.Replace(p.rhs, text)
				c.Count()
				continue
			}
			call := c.A() + "NewJournal(" + c.Ed.Text(p.rhs) + ")"
			c.claimErr(s.Pos())
			if as, ok := s.(*ast.AssignStmt); ok {
				c.Ed.Replace(as, c.Ed.Text(as.Lhs[0])+", err := "+call+"\n"+c.onErr(as.Pos(), "err"))
			} else {
				c.Ed.Replace(s, p.v.Name()+", err := "+call+"\n"+c.onErr(s.Pos(), "err"))
			}
			c.Count()
		case !p.retype && p.existing == "" && p.after == s:
			var text string
			if c.IsTest() {
				text = "\n" + p.name + " := " + c.helper("MustJournal") + "(" + p.v.Name() + ")"
			} else {
				c.claimErr(s.End())
				text = "\n" + p.name + ", err := " + c.A() + "NewJournal(" + p.v.Name() + ")\n" + c.onErr(s.End(), "err")
			}
			c.Ed.InsertAfter(s, text)
			c.Count()
		}
	}
}

// isTypeName reports whether e (on top of the stack) is a reference to the package-level name
// pkg.name: a qualified selector, or an unqualified identifier in pkg itself, but not the Sel of
// a selector (the selector is the reference).
func (c *Ctx) isTypeName(e ast.Expr, pkg, name string) bool {
	switch e := e.(type) {
	case *ast.Ident:
		if sel, ok := c.Parent(1).(*ast.SelectorExpr); ok && sel.Sel == e {
			return false
		}
		if e.Name != name {
			return false
		}
	case *ast.SelectorExpr:
		if e.Sel.Name != name {
			return false
		}
		if id, ok := e.X.(*ast.Ident); !ok || c.Info.Uses[id] == nil {
			return false
		} else if _, ok := c.Info.Uses[id].(*types.PkgName); !ok {
			return false
		}
	default:
		return false
	}
	r, ok := c.refOf(e)
	return ok && r.is(pkg, "", name)
}

// inInterfaceEmbed reports whether the node on top of the stack is an embedded interface.
func (c *Ctx) inInterfaceEmbed() bool {
	f, ok := c.Parent(1).(*ast.Field)
	if !ok || len(f.Names) != 0 {
		return false
	}
	_, ok = c.Parent(3).(*ast.InterfaceType)
	return ok
}

// freshIn returns a name that names nothing in fn (nor at package level), and that no rule has
// introduced in the file.
func (c *Ctx) freshIn(fn ast.Node, base string) string {
	taken := map[string]bool{}
	if fn != nil {
		ast.Inspect(fn, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				taken[id.Name] = true
			}
			return true
		})
	}
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s%d", base, i)
		}
		if c.used[fn] == nil {
			c.used[fn] = map[string]bool{}
		}
		if !taken[name] && !c.used[fn][name] && c.Pkg.Types.Scope().Lookup(name) == nil && types.Universe.Lookup(name) == nil {
			c.used[fn][name] = true
			return name
		}
	}
}

// journalDo rewrites a raw record write, x.Do(ctx, runID, name, fn), on a store, a journal or a
// Durable: the stores' Do shims are removed and the journal's is unexported. Inside bide, a test
// writes the record through internal/journaltest (package agent's own tests through the
// journal's do); anywhere else there is no raw write, and the site is reported.
func journalDo(c *Ctx, call *ast.CallExpr, sel *ast.SelectorExpr) {
	if !strings.HasPrefix(c.Pkg.Path, bidePath) || !c.IsTest() {
		c.Manual(call, "a raw journal write (Do) is not part of the API: record through a Step, or test with a store wrapper")
		return
	}
	recv := c.Ed.Text(sel.X)
	if isStoreLike(c.Info.TypeOf(sel.X)) {
		text, ok := c.journalFor(sel.X)
		if !ok {
			c.Manual(call, "rewrite this raw journal write by hand")
			return
		}
		recv = text
	}
	args := make([]string, len(call.Args))
	for i, a := range call.Args {
		args[i] = c.Ed.Text(a)
	}
	if c.InAgent() {
		c.Ed.Replace(call, recv+".do("+strings.Join(args, ", ")+")")
	} else {
		c.Ed.Replace(call, c.Q(bidePath+"/internal/journaltest")+"Do("+args[0]+", "+recv+", "+strings.Join(args[1:], ", ")+")")
	}
	c.Count()
}

// durableTyped reports whether e is a variable or field declared with the type agent.Durable,
// which no longer resolves in code that is partly migrated.
func (c *Ctx) durableTyped(e ast.Expr) bool {
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		return c.durableVars[c.Info.Uses[e]]
	case *ast.SelectorExpr:
		if sel := c.Info.Selections[e]; sel != nil {
			return c.durableVars[sel.Obj()]
		}
		return c.durableVars[c.Info.Uses[e.Sel]]
	}
	return false
}

// journalOver returns the variable s defines as a journal over the store variable v
// (j := agenttest.MustJournal(v), or j, err := agent.NewJournal(v)), or "".
func (c *Ctx) journalOver(s ast.Stmt, v *types.Var) string {
	as, ok := s.(*ast.AssignStmt)
	if !ok || as.Tok != token.DEFINE || len(as.Rhs) != 1 {
		return ""
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return ""
	}
	r, ok := c.callee(call)
	if !ok || !(r.is(agenttestPath, "", "MustJournal") || r.is(agentPath, "", "NewJournal") || r.is(agentPath, "", "mustJournal")) {
		return ""
	}
	if id, ok := call.Args[0].(*ast.Ident); !ok || c.Info.Uses[id] != v {
		return ""
	}
	if id, ok := as.Lhs[0].(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
