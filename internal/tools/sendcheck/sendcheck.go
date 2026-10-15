package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A finding is a goroutine that hands a result to another goroutine (a channel send or close) and
// also has a deferred call that is not a plain signal: a deferred decrement, release or unlock runs
// after the result is sent, so the receiver can act on state the goroutine has not yet put right.
type finding struct {
	Pos    token.Position
	Key    string // file|enclosing function|deferred call
	Sends  []string
	Defers []string // the deferred calls and the bookkeeping after a send that make it unsafe
	// Unresolved is a go statement of a function value the scan could not find a body for.
	Unresolved bool
}

func exprString(fset *token.FileSet, n ast.Node) string {
	var b bytes.Buffer
	printer.Fprint(&b, fset, n)
	s := strings.Join(strings.Fields(b.String()), " ")
	if len(s) > 60 {
		s = s[:60] + "..."
	}
	return s
}

// shallow visits the nodes of body that are not inside a nested function literal.
func shallow(body ast.Node, visit func(ast.Node)) {
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok && n != body {
			return false
		}
		visit(n)
		return true
	})
}

// benign defers signal or clean up without exposing state to the receiver: waiting-group Done,
// cancel functions, closing a channel (the signal itself, last), helper markers.
func benign(call string) bool {
	switch {
	case strings.HasSuffix(call, ".Done()"), strings.HasPrefix(call, "cancel"), strings.HasPrefix(call, "close("),
		strings.HasSuffix(call, "t.Helper()"), strings.HasSuffix(call, ".Stop()"):
		return true
	}
	return false
}

// pkgIndex is what the scan knows of the functions declared in the repository, by name (the same
// name in several packages, or on several types, is several candidates: the analysis takes all of
// them, so an ambiguous name is never skipped).
type pkgIndex struct {
	decls map[string][]*ast.FuncDecl
	// sends[name] is true when some function called name sends on a channel or closes one, directly or
	// through the functions it calls (to a fixed depth).
	sends map[string]bool
}

func newIndex(fset *token.FileSet, decls map[string][]*ast.FuncDecl) *pkgIndex {
	ix := &pkgIndex{decls: decls, sends: map[string]bool{}}
	// A fixed point over the call graph, by name: a function sends if its body has a send or close, or
	// calls a function that sends.
	for changed, round := true, 0; changed && round < 8; round++ {
		changed = false
		for name, ds := range decls {
			if ix.sends[name] {
				continue
			}
			// A name counts as a sender only if every function of that name in the repository sends:
			// the analysis goes by name, and a name shared with an unrelated function that does not
			// send (Done, Close, Write, Error, ...) must not make every call of it a send.
			all := true
			for _, d := range ds {
				if !takesChan(d) || !ix.bodySends(d.Body) {
					all = false
					break
				}
			}
			if all {
				ix.sends[name] = true
				changed = true
			}
		}
	}
	return ix
}

// takesChan reports whether d has a channel parameter: a helper that is handed the channel it
// sends on. A function that sends on a channel it owns (a field, a package variable) is not a
// handoff of the caller's result, and a name-based scan cannot tell those apart from the callers'.
func takesChan(d *ast.FuncDecl) bool {
	if d.Type.Params == nil {
		return false
	}
	for _, f := range d.Type.Params.List {
		if _, ok := f.Type.(*ast.ChanType); ok {
			return true
		}
	}
	return false
}

// directSend reports whether n is itself a channel send or a close.
func directSend(n ast.Node) (string, bool) {
	switch x := n.(type) {
	case *ast.SendStmt:
		return "send", true
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "close" && len(x.Args) == 1 {
			return "close", true
		}
	}
	return "", false
}

func calleeName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// bodySends reports whether body, including the function literals in it, has a send or close or
// calls a function that does.
func (ix *pkgIndex) bodySends(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		if _, ok := directSend(n); ok {
			found = true
		} else if c, ok := n.(*ast.CallExpr); ok && ix.sends[calleeName(c)] {
			found = true
		}
		return !found
	})
	return found
}

// isSend reports whether the statement-level node n hands a result over: a send, a close, or a call
// of a function that sends (the channel may be passed to it or reached some other way).
func (ix *pkgIndex) isSend(n ast.Node) (string, bool) {
	if k, ok := directSend(n); ok {
		return k, true
	}
	if c, ok := n.(*ast.CallExpr); ok && ix.sends[calleeName(c)] {
		return "call " + calleeName(c), true
	}
	return "", false
}

// bookkeeping reports whether n gives back something the goroutine held: a decrement, an unlock, a
// release, a receive that frees a slot, a flag cleared, an entry deleted.
func bookkeeping(n ast.Node) (string, bool) {
	switch x := n.(type) {
	case *ast.IncDecStmt:
		if x.Tok == token.DEC {
			return "decrement", true
		}
	case *ast.AssignStmt:
		if x.Tok == token.SUB_ASSIGN {
			return "decrement", true
		}
		if x.Tok == token.ASSIGN && len(x.Lhs) == 1 && len(x.Rhs) == 1 {
			if id, ok := x.Rhs[0].(*ast.Ident); ok && (id.Name == "false" || id.Name == "nil") {
				if _, plain := x.Lhs[0].(*ast.Ident); !plain { // a field or element: shared state, not a local
					return "reset", true
				}
			}
		}
	case *ast.ExprStmt:
		if u, ok := x.X.(*ast.UnaryExpr); ok && u.Op == token.ARROW {
			if _, call := u.X.(*ast.CallExpr); !call && slotLike(u.X) {
				return "receive", true // <-slots: a slot given back
			}
		}
		if c, ok := x.X.(*ast.CallExpr); ok {
			switch name := calleeName(c); name {
			case "Unlock", "RUnlock", "Release", "release", "Leave", "leave", "Free", "free":
				return name, true
			case "Add":
				if len(c.Args) == 1 {
					if u, ok := c.Args[0].(*ast.UnaryExpr); ok && u.Op == token.SUB {
						return "Add(-n)", true
					}
				}
			case "delete":
				return "delete", true
			}
		}
	}
	return "", false
}

// slotLike reports whether a channel expression names a slot pool: receiving from it gives a slot
// back. Receiving from other channels (a gate, a start signal, ctx.Done()) waits, it gives nothing.
func slotLike(e ast.Expr) bool {
	var name string
	switch x := e.(type) {
	case *ast.Ident:
		name = x.Name
	case *ast.SelectorExpr:
		name = x.Sel.Name
	}
	name = strings.ToLower(name)
	for _, w := range []string{"sem", "slot", "token", "pool", "limit", "permit", "hooks"} {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

func terminates(s ast.Stmt) bool {
	switch x := s.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return x.Tok == token.BREAK || x.Tok == token.CONTINUE || x.Tok == token.GOTO
	case *ast.ExprStmt:
		if c, ok := x.X.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "panic" {
				return true
			}
		}
	}
	return false
}

// falls reports whether s hands a result over on some path that continues past s: s is a send, or a
// compound statement with a branch that sends and then neither returns nor jumps.
func (ix *pkgIndex) falls(s ast.Stmt) (string, bool) {
	switch x := s.(type) {
	case *ast.SendStmt:
		return "send", true
	case *ast.ExprStmt:
		return ix.isSend(x.X)
	case *ast.AssignStmt:
		for _, r := range x.Rhs {
			if c, ok := r.(*ast.CallExpr); ok {
				if k, ok := ix.isSend(c); ok {
					return k, true
				}
			}
		}
	case *ast.BlockStmt:
		return ix.listFalls(x.List)
	case *ast.IfStmt:
		if k, ok := ix.listFalls(x.Body.List); ok {
			return k, true
		}
		if x.Else != nil {
			return ix.falls(x.Else)
		}
	case *ast.ForStmt:
		return ix.listFalls(x.Body.List)
	case *ast.RangeStmt:
		return ix.listFalls(x.Body.List)
	case *ast.LabeledStmt:
		return ix.falls(x.Stmt)
	case *ast.SwitchStmt:
		return ix.clausesFall(x.Body)
	case *ast.TypeSwitchStmt:
		return ix.clausesFall(x.Body)
	case *ast.SelectStmt:
		return ix.clausesFall(x.Body)
	}
	return "", false
}

func (ix *pkgIndex) clausesFall(b *ast.BlockStmt) (string, bool) {
	for _, c := range b.List {
		var list []ast.Stmt
		switch cc := c.(type) {
		case *ast.CaseClause:
			list = cc.Body
		case *ast.CommClause:
			if cc.Comm != nil {
				if k, ok := ix.falls(cc.Comm); ok { // case ch <- v:
					return k, true
				}
			}
			list = cc.Body
		}
		if k, ok := ix.listFalls(list); ok {
			return k, true
		}
	}
	return "", false
}

// listFalls reports whether a send in list is followed by the end of the list, not by a return or
// a jump, on some path.
func (ix *pkgIndex) listFalls(list []ast.Stmt) (string, bool) {
	for i, s := range list {
		if k, ok := ix.falls(s); ok {
			for _, t := range list[i+1:] {
				if terminates(t) {
					return "", false
				}
			}
			return k, true
		}
	}
	return "", false
}

// afterSend returns the bookkeeping statements that follow a send in list (or in a list it
// contains) on the same path, as text.
func (ix *pkgIndex) afterSend(fset *token.FileSet, list []ast.Stmt, out *[]string) {
	for i, s := range list {
		if k, ok := ix.falls(s); ok {
			for _, t := range list[i+1:] {
				ast.Inspect(t, func(n ast.Node) bool {
					if b, ok := bookkeeping(n); ok {
						*out = append(*out, "after "+k+": "+b+" "+exprString(fset, n))
					}
					return true
				})
			}
		}
		// Statement lists nested in s are checked in their own right.
		ast.Inspect(s, func(n ast.Node) bool {
			if n == s {
				return true
			}
			switch x := n.(type) {
			case *ast.BlockStmt:
				ix.afterSend(fset, x.List, out)
				return false
			case *ast.CaseClause:
				ix.afterSend(fset, x.Body, out)
				return false
			case *ast.CommClause:
				ix.afterSend(fset, x.Body, out)
				return false
			case *ast.FuncLit:
				return false
			}
			return true
		})
	}
}

// analyze returns what makes body unsafe: the sends it makes and the deferred calls that are not
// signals, when it has both (a deferred call runs after any send), and the bookkeeping statements
// that follow a send. Function literals nested in body count as part of it.
func (ix *pkgIndex) analyze(fset *token.FileSet, body *ast.BlockStmt) (sends, late []string) {
	hasSend := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SendStmt:
			sends = append(sends, "send "+exprString(fset, x.Chan))
			hasSend = true
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "close" && len(x.Args) == 1 {
				sends = append(sends, "close "+exprString(fset, x.Args[0]))
				hasSend = true
			} else if ix.sends[calleeName(x)] {
				sends = append(sends, "call "+calleeName(x))
				hasSend = true
			}
		case *ast.GoStmt:
			return false // a goroutine started here is analysed on its own
		}
		return true
	})
	if !hasSend {
		return nil, nil
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GoStmt:
			return false
		case *ast.DeferStmt:
			if c := exprString(fset, x.Call); !benign(c) {
				late = append(late, "defer "+c)
			}
		}
		return true
	})
	ix.afterSend(fset, body.List, &late)
	return sends, late
}

// scan returns the findings in the Go files below root, test files included.
func scan(root string) ([]finding, error) {
	fset := token.NewFileSet()
	type file struct {
		path string
		f    *ast.File
	}
	var files []file
	decls := map[string][]*ast.FuncDecl{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "node_modules" || n == "testdata" || n == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil // not this tool's to report
		}
		rel, _ := filepath.Rel(root, p)
		files = append(files, file{filepath.ToSlash(rel), f})
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				decls[fd.Name.Name] = append(decls[fd.Name.Name], fd)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ix := newIndex(fset, decls)
	var out []finding
	for _, fl := range files {
		for _, d := range fl.f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			// Function values bound to a name in this function: go name() runs one of them.
			lits := map[string]*ast.FuncLit{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
					for i := range as.Lhs {
						if id, ok := as.Lhs[i].(*ast.Ident); ok {
							if lit, ok := as.Rhs[i].(*ast.FuncLit); ok {
								lits[id.Name] = lit
							}
						}
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				g, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				var bodies []*ast.BlockStmt
				switch fun := g.Call.Fun.(type) {
				case *ast.FuncLit:
					bodies = append(bodies, fun.Body)
				case *ast.Ident:
					if lit := lits[fun.Name]; lit != nil {
						bodies = append(bodies, lit.Body)
					} else {
						for _, c := range decls[fun.Name] {
							bodies = append(bodies, c.Body)
						}
						if len(bodies) == 0 {
							out = append(out, finding{Pos: fset.Position(g.Pos()), Key: fl.path + "|" + fd.Name.Name + "|unresolved go " + fun.Name,
								Unresolved: true})
						}
					}
				case *ast.SelectorExpr: // a method or a package function: every declaration of that name is a candidate
					for _, c := range decls[fun.Sel.Name] {
						bodies = append(bodies, c.Body)
					}
				}
				for _, body := range bodies {
					if sends, late := ix.analyze(fset, body); len(sends) > 0 && len(late) > 0 {
						out = append(out, finding{Pos: fset.Position(g.Pos()), Key: fl.path + "|" + fd.Name.Name + "|" + strings.Join(late, " ; "), Sends: sends, Defers: late})
					}
				}
				return true
			})
		}
	}
	slices.SortFunc(out, func(a, b finding) int { return strings.Compare(a.Key, b.Key) })
	return slices.CompactFunc(out, func(a, b finding) bool { return a.Key == b.Key }), nil
}

// readAllow parses an allowlist: one "key # reason" per line; blank lines and lines starting with #
// are comments. A key without a reason is an error: the reason is the point.
func readAllow(r io.Reader) (map[string]string, []string) {
	allow := map[string]string{}
	var bad []string
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, reason, ok := strings.Cut(line, " # ")
		if !ok || strings.TrimSpace(reason) == "" {
			bad = append(bad, fmt.Sprintf("allowlist line %d: %q has no reason after \" # \"", n, key))
			continue
		}
		allow[strings.TrimSpace(key)] = strings.TrimSpace(reason)
	}
	return allow, bad
}

func check(root string, allowFile string, stderr io.Writer) int {
	finds, err := scan(root)
	if err != nil {
		fmt.Fprintf(stderr, "sendcheck: %v\n", err)
		return 2
	}
	allow := map[string]string{}
	if allowFile != "" {
		f, err := os.Open(allowFile)
		if err != nil {
			fmt.Fprintf(stderr, "sendcheck: %v\n", err)
			return 2
		}
		defer f.Close()
		var bad []string
		allow, bad = readAllow(f)
		for _, b := range bad {
			fmt.Fprintln(stderr, "sendcheck: "+b)
		}
		if len(bad) > 0 {
			return 1
		}
	}
	used := map[string]bool{}
	status := 0
	for _, f := range finds {
		if _, ok := allow[f.Key]; ok {
			used[f.Key] = true
			continue
		}
		status = 1
		if f.Unresolved {
			fmt.Fprintf(stderr, "%s: the goroutine's function cannot be resolved here, so it cannot be checked. Start a function literal or a declared function, "+
				"or, if its order is safe, add %q to the allowlist with the reason.\n", f.Pos, f.Key)
			continue
		}
		fmt.Fprintf(stderr, "%s: a goroutine sends or closes %v and then has %v: the receiver can act before that bookkeeping is done. "+
			"Do the bookkeeping first (release the slot, clear the flag, decrement) and then send; or, if the order is safe, add %q to the allowlist with the reason.\n",
			f.Pos, f.Sends, f.Defers, f.Key)
	}
	for k := range allow {
		if !used[k] {
			status = 1
			fmt.Fprintf(stderr, "sendcheck: the allowlist entry %q matches no goroutine: delete it\n", k)
		}
	}
	return status
}
