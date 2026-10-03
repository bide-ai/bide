package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
)

// helper returns the call of a test helper of package agenttest: agenttest.MustNew,
// agenttest.MemJournal, agenttest.MustJournal or agenttest.Must. Package agent's own tests cannot
// import agenttest (it imports agent), so there they are the package's unexported test helpers
// mustNew, memJournal, mustJournal, must and answerOf.
func (c *Ctx) helper(name string) string {
	if c.InAgent() {
		if name == "Answer" {
			return "answerOf" // package agent's tests have a type named answer
		}
		return string(name[0]-'A'+'a') + name[1:]
	}
	return c.Q(agenttestPath) + name
}

// fallible returns the text that stands for the value of call, a call (text) returning (T,
// error), where the expression e stood. In a test it is a Must helper (must is the helper to
// wrap with: "" for Must(call)); elsewhere the call is hoisted into the statements before the
// one holding e, with the error handled as the function handles errors, and the result is a
// fresh variable named after base. Where it cannot do either (a package-level declaration), it
// reports the site and returns call unchanged.
//
// When e is the whole right-hand side of a single assignment or definition (x := e), the
// statement itself becomes x, err := call, so no variable is introduced; fallible then returns
// false, and the caller must not replace e.
func (c *Ctx) fallible(e ast.Expr, call, must, base string) (string, bool) {
	if c.IsTest() {
		if must != "" {
			return must, true
		}
		return c.helper("Must") + "(" + call + ")", true
	}
	stmt, depth := c.stmtInList()
	if stmt == nil {
		c.Manual(e, "a constructor that now returns an error, outside a function: handle the error by hand")
		return call, true
	}
	if as, ok := c.Parent(1).(*ast.AssignStmt); ok && ast.Stmt(as) == stmt && len(as.Lhs) == 1 && len(as.Rhs) == 1 && as.Rhs[0] == e {
		lhs := c.Ed.Text(as.Lhs[0])
		switch as.Tok {
		case token.DEFINE:
			c.claimErr(as.Pos())
			c.Ed.Replace(as, fmt.Sprintf("%s, err := %s\n%s", lhs, call, c.onErr(as.Pos(), "err")))
			return "", false
		case token.ASSIGN:
			decl := ""
			if !c.errDeclared(as.Pos()) {
				decl = "var err error\n"
			}
			c.Ed.Replace(as, fmt.Sprintf("%s%s, err = %s\n%s", decl, lhs, call, c.onErr(as.Pos(), "err")))
			return "", false
		}
	}
	if why := c.notHoistable(e, depth); why != "" {
		c.Manual(e, "a constructor that now returns an error, %s: moving the call before the statement would run it when the statement would not; handle the error by hand", why)
		return call, true
	}
	name := c.fresh(base)
	c.claimErr(stmt.Pos())
	c.Ed.InsertBefore(stmt, fmt.Sprintf("%s, err := %s\n%s\n", name, call, c.onErr(stmt.Pos(), "err")))
	return name, true
}

// notHoistable says why e, under the statement at stack depth, is not evaluated every time the
// statement runs, before anything else of it ("" when it is): a call hoisted before the statement
// must run exactly when e would have, and before nothing e followed.
func (c *Ctx) notHoistable(e ast.Expr, depth int) string {
	child := ast.Node(e)
	for j := len(c.stack) - 1; j >= depth; j-- {
		n := c.stack[j]
		if n == child {
			continue
		}
		switch p := n.(type) {
		case *ast.BinaryExpr:
			if (p.Op == token.LAND || p.Op == token.LOR) && p.Y == child {
				return "on the right of " + p.Op.String()
			}
		case *ast.FuncLit:
			return "in a function literal"
		case *ast.IfStmt:
			if j != depth {
				return "in an else-if"
			}
			if p.Init != nil && child != p.Init {
				return "in an if statement after its init statement"
			}
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SelectStmt, *ast.CaseClause, *ast.CommClause:
			if j != depth || !isHeadOf(p, child) {
				return "in a loop, switch or select"
			}
		case *ast.AssignStmt:
			// the left-hand side's index and pointer operands are evaluated first
			for _, l := range p.Lhs {
				if l == child {
					return "on the left of an assignment"
				}
			}
			if len(p.Rhs) > 1 {
				for _, r := range p.Rhs {
					if r == child {
						break
					}
					if hasCall(r) {
						return "after another call of the assignment"
					}
				}
			}
		case *ast.CallExpr:
			// arguments evaluate in order: a call among the earlier ones runs first
			if p.Fun != child && hasCall(p.Fun) {
				return "after the call that computes the function"
			}
			for _, a := range p.Args {
				if a == child {
					break
				}
				if hasCall(a) {
					return "after another argument's call"
				}
			}
		}
		child = n
	}
	return ""
}

// isHeadOf reports whether child is a part of s evaluated once, before anything else of it: a
// switch's tag (with no init statement), a range's operand.
func isHeadOf(s ast.Node, child ast.Node) bool {
	switch s := s.(type) {
	case *ast.SwitchStmt:
		return s.Init == nil && s.Tag == child
	case *ast.RangeStmt:
		return s.X == child
	}
	return false
}

// hasCall reports whether e contains a call (whose side effects a hoisted call would now precede).
func hasCall(e ast.Node) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if _, ok := n.(*ast.CallExpr); ok {
			found = true
		}
		return !found
	})
	return found
}

// isJournalType reports whether t is *agent.Journal.
func isJournalType(t types.Type) bool { return isNamed(t, agentPath, "Journal") }

// isDurableType reports whether t is the old agent.Durable interface.
func isDurableType(t types.Type) bool {
	if t == nil {
		return false
	}
	if _, ok := t.(*types.Pointer); ok {
		return false
	}
	return isNamed(t, agentPath, "Durable")
}

// isStoreLike reports whether a value of type t, found where a Durable was expected, becomes a
// Journal over it: a store (Insert, Get and Load), or audit.AuditedStore, which the rewrite makes
// a store wrapper.
func isStoreLike(t types.Type) bool {
	if t == nil || isJournalType(t) || isDurableType(t) || customDurable(t) {
		return false
	}
	if _, ok := t.Underlying().(*types.Interface); ok {
		return isNamed(t, agentPath, "Store")
	}
	return hasMethods(t, "Insert", "Get", "Load") || isNamed(t, auditPath, "AuditedStore")
}

// isMemStoreCall reports whether e is agent.NewMemStore().
func (c *Ctx) isMemStoreCall(e ast.Expr) bool {
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	r, ok := c.callee(call)
	return ok && r.is(agentPath, "", "NewMemStore")
}

// journalFor returns the text of a *Journal over the store e (a store value found where a Durable
// was expected): e itself when its variable is planned to become a journal, the planned journal
// variable, or a Journal built from e (MemJournal for agent.NewMemStore() and MustJournal in a
// test, a hoisted NewJournal elsewhere). ok is false when the statement holding e was rewritten
// in place (see fallible); the caller then leaves e alone.
func (c *Ctx) journalFor(e ast.Expr) (text string, ok bool) {
	if id, isID := ast.Unparen(e).(*ast.Ident); isID {
		if v, isVar := c.Info.Uses[id].(*types.Var); isVar {
			if p := c.jplan[v]; p != nil {
				if p.retype {
					return c.Ed.Text(e), true
				}
				return p.name, true
			}
		}
	}
	if c.IsTest() {
		if c.isMemStoreCall(e) {
			return c.helper("MemJournal") + "()", true
		}
		return c.helper("MustJournal") + "(" + c.Ed.Text(e) + ")", true
	}
	return c.fallible(e, c.A()+"NewJournal("+c.Ed.Text(e)+")", "", "j")
}

// claimErr prepares the scope at pos for a definition "x, err :=" the rewrite inserts there.
// That compiles unless the same scope defines err later, in a statement whose other variables
// are not new ("_, err := f()"), which the new definition would turn into an error: such a
// statement becomes an assignment ("_, err = f()"), and a later "var err error" is removed.
func (c *Ctx) claimErr(pos token.Pos) {
	s := c.scopeAt(pos)
	if s == nil || s == c.Pkg.Types.Scope() {
		return
	}
	obj, ok := s.Lookup("err").(*types.Var)
	if !ok || obj.Pos() < pos {
		return
	}
	fn := c.funcNode()
	if fn == nil {
		return
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE {
				return true
			}
			defines, others := false, false
			for _, l := range n.Lhs {
				id, ok := l.(*ast.Ident)
				if !ok {
					continue
				}
				if c.Info.Defs[id] == obj {
					defines = true
				} else if id.Name != "_" && c.Info.Defs[id] != nil {
					others = true
				}
			}
			if defines && !others && !c.assigns[n] {
				c.assigns[n] = true
				c.Ed.ReplaceRange(n.TokPos, n.TokPos+2, "=")
			}
		case *ast.DeclStmt:
			if gd, ok := n.Decl.(*ast.GenDecl); ok && len(gd.Specs) == 1 {
				if vs, ok := gd.Specs[0].(*ast.ValueSpec); ok && len(vs.Names) == 1 && c.Info.Defs[vs.Names[0]] == obj && len(vs.Values) == 0 {
					c.Ed.Replace(n, "")
				}
			}
		}
		return true
	})
}

// tok returns the assignment token of as as the rewrite leaves it.
func (c *Ctx) tok(as *ast.AssignStmt) string {
	if c.assigns[as] {
		return "="
	}
	return as.Tok.String()
}

// customDurable reports whether t has a Do or History method of its own (or of a type other than
// bide's stores): a test double that intercepted the Durable's writes. A Journal over it would
// never call its Do, so the rewrite reports it instead of wrapping it.
func customDurable(t types.Type) bool {
	if t == nil {
		return false
	}
	pt := t
	if _, ok := t.(*types.Pointer); !ok {
		if _, isIface := t.Underlying().(*types.Interface); isIface {
			return false
		}
		pt = types.NewPointer(t)
	}
	ms := types.NewMethodSet(pt)
	for _, name := range []string{"Do", "History"} {
		sel := ms.Lookup(nil, name)
		if sel == nil {
			continue
		}
		switch sel.Obj().Pkg().Path() {
		case agentPath, sqlitePath, postgresPath, auditPath:
		default:
			return true
		}
	}
	return false
}
