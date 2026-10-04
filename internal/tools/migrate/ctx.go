package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Import paths of the packages the rules know.
const (
	bidePath        = "github.com/bide-ai/bide"
	agentPath       = bidePath + "/agent"
	agenttestPath   = bidePath + "/agent/agenttest"
	auditPath       = bidePath + "/audit"
	planPath        = bidePath + "/plan"
	mcpPath         = bidePath + "/mcp"
	mcptoolsPath    = bidePath + "/mcptools"
	chaosPath       = bidePath + "/chaos"
	sqlitePath      = bidePath + "/store/sqlite"
	postgresPath    = bidePath + "/store/postgres"
	storetestPath   = bidePath + "/agent/storetest"
	durabletestPath = bidePath + "/agent/durabletest"
)

// Rule rewrites one class of old API use. Visit is called for every node of a file, children
// before parents, with the node on top of c's stack.
type Rule struct {
	Name  string
	Doc   string
	Visit func(c *Ctx, n ast.Node)
	// Plan, when set, is called for every node in a first pass that only analyses the file, and
	// Planned once that pass is over.
	Plan    func(c *Ctx, n ast.Node)
	Planned func(c *Ctx)
}

// Finding is a site a rule could not rewrite, which needs a person.
type Finding struct {
	Pos  token.Position
	Rule string
	Msg  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s: %s", f.Pos.Filename, f.Pos.Line, f.Pos.Column, f.Rule, f.Msg)
}

// Run holds what one migration run found and did.
type Run struct {
	Findings []Finding
	Counts   map[string]int // rule -> sites rewritten
	// Docs is set for documentation snippets, whose errors are handled with log.Fatal where the
	// snippet's function returns none (a snippet stands for a program's code).
	Docs     bool
	specDone map[types.Object]bool // tool types the tools rule gave a Spec method (or reported)
}

// Ctx is a rule's view of the file it is rewriting.
type Ctx struct {
	Pkg   *Package
	File  *File
	Info  *types.Info
	Ed    *Editor
	run   *Run
	rule  string
	stack []ast.Node

	imports map[string]string            // import path -> local name, as the file imports it
	added   map[string]string            // import path -> local name, for imports a rule needs
	used    map[ast.Node]map[string]bool // names a rule introduced, per function (fresh names)

	jplan       map[*types.Var]*jvar       // the journal rule's local store variables
	constructs  map[ast.Stmt]*construction // the construct rule's agent constructions
	builders    map[ast.Stmt]*builderStmt  // the construct rule's builder statements
	assigns     map[*ast.AssignStmt]bool   // definitions claimErr made assignments
	durableVars map[types.Object]bool      // variables and fields declared agent.Durable
}

func newCtx(run *Run, f *File) *Ctx {
	c := &Ctx{Pkg: f.Pkg, File: f, Info: f.Pkg.Info, Ed: newEditor(f), run: run,
		imports: map[string]string{}, added: map[string]string{}, used: map[ast.Node]map[string]bool{},
		jplan: map[*types.Var]*jvar{}, constructs: map[ast.Stmt]*construction{}, builders: map[ast.Stmt]*builderStmt{}, assigns: map[*ast.AssignStmt]bool{}, durableVars: map[types.Object]bool{}}
	for _, is := range f.AST.Imports {
		path, _ := strconv.Unquote(is.Path.Value)
		name := ""
		if is.Name != nil {
			name = is.Name.Name
		} else if pn, ok := c.Info.Implicits[is].(*types.PkgName); ok {
			name = pn.Name()
		} else {
			name = filepath.Base(path)
		}
		c.imports[path] = name
	}
	return c
}

// Node returns the node being visited.
func (c *Ctx) Node() ast.Node { return c.stack[len(c.stack)-1] }

// Parent returns the i-th ancestor of the node being visited (1: its parent), or nil.
func (c *Ctx) Parent(i int) ast.Node {
	if k := len(c.stack) - 1 - i; k >= 0 {
		return c.stack[k]
	}
	return nil
}

// IsTest reports whether the file is a test file.
func (c *Ctx) IsTest() bool { return strings.HasSuffix(c.File.Path, "_test.go") }

// InAgent reports whether the file belongs to package agent itself (or its in-package tests),
// where agent's names are unqualified.
func (c *Ctx) InAgent() bool { return c.Pkg.Path == agentPath && c.Pkg.Name == "agent" }

// Count records one site rewritten by the current rule.
func (c *Ctx) Count() { c.run.Counts[c.rule]++ }

// Manual records a site the current rule could not rewrite.
func (c *Ctx) Manual(n ast.Node, format string, args ...any) {
	c.run.Findings = append(c.run.Findings, Finding{Pos: c.Pkg.Fset.Position(n.Pos()), Rule: c.rule, Msg: fmt.Sprintf(format, args...)})
}

// Q returns the qualifier ("name.") for a package's identifiers in this file, adding an import
// when the file has none; "" in the package itself.
func (c *Ctx) Q(path string) string {
	if path == c.Pkg.Path {
		return "" // the package itself (an external test package's own path included)
	}
	if n, ok := c.imports[path]; ok {
		if n == "." {
			return ""
		}
		return n + "."
	}
	if n, ok := c.added[path]; ok {
		return n + "."
	}
	name := pkgName(path)
	taken := map[string]bool{}
	for _, n := range c.imports {
		taken[n] = true
	}
	for _, n := range c.added {
		taken[n] = true
	}
	base := name
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s%d", base, i)
	}
	c.added[path] = name
	return name + "."
}

// pkgName is the package name of an import path the rules emit.
func pkgName(path string) string { return filepath.Base(path) }

// A is the qualifier for package agent.
func (c *Ctx) A() string { return c.Q(agentPath) }

// ---------------------------------------------------------------------------
// What an expression refers to
// ---------------------------------------------------------------------------

// ref names a package-level object (Recv == "") or a method or field of a named type.
type ref struct{ Pkg, Recv, Name string }

func (r ref) is(pkg, recv, name string) bool { return r.Pkg == pkg && r.Recv == recv && r.Name == name }

// refOf returns what an identifier or selector refers to. It works on code that no longer
// type-checks: an unresolved selector is named by its operand's type (or package), and an
// unresolved identifier in package p by p.
func (c *Ctx) refOf(e ast.Expr) (ref, bool) {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return c.refOf(e.X)
	case *ast.IndexExpr:
		return c.refOf(e.X)
	case *ast.IndexListExpr:
		return c.refOf(e.X)
	case *ast.Ident:
		if o := c.Info.Uses[e]; o != nil {
			if o.Pkg() == nil || o.Parent() != o.Pkg().Scope() {
				return ref{}, false // universe, or a local
			}
			return ref{Pkg: o.Pkg().Path(), Name: o.Name()}, true
		}
		if c.Info.Defs[e] != nil {
			return ref{}, false
		}
		return ref{Pkg: c.Pkg.Path, Name: e.Name}, true // unresolved: assume this package
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if pn, ok := c.Info.Uses[id].(*types.PkgName); ok {
				return ref{Pkg: pn.Imported().Path(), Name: e.Sel.Name}, true
			}
		}
		if sel := c.Info.Selections[e]; sel != nil {
			o := sel.Obj()
			if o.Pkg() == nil {
				return ref{}, false
			}
			recv := ""
			if f, ok := o.(*types.Func); ok {
				if sig, ok := f.Type().(*types.Signature); ok && sig.Recv() != nil {
					recv = namedName(sig.Recv().Type())
				}
			} else if sel.Kind() == types.FieldVal {
				recv = namedName(sel.Recv())
			}
			return ref{Pkg: o.Pkg().Path(), Recv: recv, Name: o.Name()}, true
		}
		t := c.Info.TypeOf(e.X)
		if call, ok := ast.Unparen(e.X).(*ast.CallExpr); ok && (t == nil || t == types.Typ[types.Invalid]) {
			// a call that now returns an error too, used as one value: the checker gives it no
			// type, so take its function's first result
			if sig, ok := c.Info.TypeOf(call.Fun).(*types.Signature); ok && sig.Results().Len() > 0 {
				t = sig.Results().At(0).Type()
			}
		}
		if t != nil {
			if tu, ok := t.(*types.Tuple); ok && tu.Len() > 0 {
				t = tu.At(0).Type() // a call that now returns an error too, used as one value
			}
			if n := named(t); n != nil && n.Obj().Pkg() != nil {
				return ref{Pkg: n.Obj().Pkg().Path(), Recv: n.Obj().Name(), Name: e.Sel.Name}, true
			}
		}
	}
	return ref{}, false
}

// callee returns what a call calls.
func (c *Ctx) callee(call *ast.CallExpr) (ref, bool) { return c.refOf(call.Fun) }

// named returns t's named type, through one pointer, or nil.
func named(t types.Type) *types.Named {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if a, ok := t.(*types.Alias); ok {
		t = types.Unalias(a)
	}
	n, _ := t.(*types.Named)
	return n
}

func namedName(t types.Type) string {
	if n := named(t); n != nil {
		return n.Obj().Name()
	}
	return ""
}

// isNamed reports whether t (through one pointer) is the named type pkg.name.
func isNamed(t types.Type, pkg, name string) bool {
	n := named(t)
	return n != nil && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

// isString reports whether e's type is a string (or an untyped string constant).
func (c *Ctx) isString(e ast.Expr) bool {
	t := c.Info.TypeOf(e)
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

// hasMethods reports whether t's method set (or *t's) has every named method.
func hasMethods(t types.Type, names ...string) bool {
	if t == nil {
		return false
	}
	ms := types.NewMethodSet(t)
	if _, ok := t.(*types.Pointer); !ok {
		if _, ok := t.Underlying().(*types.Interface); !ok {
			ms = types.NewMethodSet(types.NewPointer(t))
		}
	}
	for _, n := range names {
		found := false
		for i := range ms.Len() {
			if ms.At(i).Obj().Name() == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// expected returns the type the context of the node being visited (an expression) requires, when
// it is an argument, an assigned value, a returned value or a composite literal element; nil
// otherwise.
func (c *Ctx) expected(e ast.Expr) types.Type {
	switch p := c.Parent(1).(type) {
	case *ast.CallExpr:
		sig, _ := c.Info.TypeOf(p.Fun).(*types.Signature)
		if sig == nil {
			return nil
		}
		for i, a := range p.Args {
			if a != e {
				continue
			}
			n := sig.Params().Len()
			var t types.Type
			switch {
			case sig.Variadic() && i >= n-1:
				t = sig.Params().At(n - 1).Type()
				if !p.Ellipsis.IsValid() {
					if sl, ok := t.(*types.Slice); ok {
						t = sl.Elem()
					}
				}
			case i < n:
				t = sig.Params().At(i).Type()
			}
			if t != nil && t == types.Typ[types.Invalid] {
				return c.declaredParam(p, min(i, n-1))
			}
			return t
		}
	case *ast.AssignStmt:
		if p.Tok != token.ASSIGN || len(p.Lhs) != len(p.Rhs) {
			return nil
		}
		for i, r := range p.Rhs {
			if r == e {
				return c.Info.TypeOf(p.Lhs[i])
			}
		}
	case *ast.ValueSpec:
		if p.Type != nil {
			if t := c.Info.TypeOf(p.Type); t != nil && t != types.Typ[types.Invalid] {
				return t
			}
			if namesDurable(p.Type) {
				return durableMarker
			}
		}
	case *ast.ReturnStmt:
		sig := c.funcSig()
		if sig == nil || sig.Results().Len() != len(p.Results) {
			return nil
		}
		for i, r := range p.Results {
			if r != e {
				continue
			}
			if t := sig.Results().At(i).Type(); t != types.Typ[types.Invalid] {
				return t
			}
			var ft *ast.FuncType
			switch f := c.funcNode().(type) {
			case *ast.FuncDecl:
				ft = f.Type
			case *ast.FuncLit:
				ft = f.Type
			}
			if ft != nil && ft.Results != nil {
				k := 0
				for _, field := range ft.Results.List {
					n := max(len(field.Names), 1)
					if i < k+n && namesDurable(field.Type) {
						return durableMarker
					}
					k += n
				}
			}
		}
	case *ast.KeyValueExpr:
		if p.Value != e {
			return nil
		}
		if lit, ok := c.Parent(2).(*ast.CompositeLit); ok {
			if st, ok := underlyingStruct(c.Info.TypeOf(lit)); ok {
				if k, ok := p.Key.(*ast.Ident); ok {
					for i := range st.NumFields() {
						if st.Field(i).Name() == k.Name {
							return st.Field(i).Type()
						}
					}
				}
			}
			if m, ok := c.Info.TypeOf(lit).Underlying().(*types.Map); ok {
				return m.Elem()
			}
		}
	case *ast.CompositeLit:
		t := c.Info.TypeOf(p)
		if t == nil {
			return nil
		}
		switch u := t.Underlying().(type) {
		case *types.Slice:
			return u.Elem()
		case *types.Array:
			return u.Elem()
		case *types.Struct:
			for i, el := range p.Elts {
				if el == e && i < u.NumFields() {
					return u.Field(i).Type()
				}
			}
		}
	}
	return nil
}

func underlyingStruct(t types.Type) (*types.Struct, bool) {
	if t == nil {
		return nil, false
	}
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	s, ok := t.Underlying().(*types.Struct)
	return s, ok
}

// ---------------------------------------------------------------------------
// Statements, functions and scopes
// ---------------------------------------------------------------------------

// stmtInList returns the innermost ancestor statement that sits directly in a statement list (a
// block or a case body), where statements can be inserted before or after it, and its index in
// the stack; nil if the node is not inside one (a package-level declaration).
func (c *Ctx) stmtInList() (ast.Stmt, int) {
	for i := len(c.stack) - 1; i > 0; i-- {
		s, ok := c.stack[i].(ast.Stmt)
		if !ok {
			continue
		}
		switch p := c.stack[i-1].(type) {
		case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause:
			return s, i
		case *ast.LabeledStmt:
			_ = p
		}
		if _, ok := c.stack[i].(*ast.FuncLit); ok {
			return nil, -1
		}
	}
	return nil, -1
}

// nextStmt returns the statement after s in its list, or nil.
func (c *Ctx) nextStmt(s ast.Stmt, depth int) ast.Stmt {
	var list []ast.Stmt
	switch p := c.stack[depth-1].(type) {
	case *ast.BlockStmt:
		list = p.List
	case *ast.CaseClause:
		list = p.Body
	case *ast.CommClause:
		list = p.Body
	}
	for i, x := range list {
		if x == s && i+1 < len(list) {
			return list[i+1]
		}
	}
	return nil
}

// funcSig returns the signature of the innermost function around the node being visited.
func (c *Ctx) funcSig() *types.Signature {
	for i := len(c.stack) - 1; i >= 0; i-- {
		switch f := c.stack[i].(type) {
		case *ast.FuncLit:
			s, _ := c.Info.TypeOf(f).(*types.Signature)
			return s
		case *ast.FuncDecl:
			if o := c.Info.Defs[f.Name]; o != nil {
				s, _ := o.Type().(*types.Signature)
				return s
			}
			return nil
		}
	}
	return nil
}

// funcNode returns the innermost function (FuncDecl or FuncLit) around the node being visited.
func (c *Ctx) funcNode() ast.Node {
	for i := len(c.stack) - 1; i >= 0; i-- {
		switch f := c.stack[i].(type) {
		case *ast.FuncLit, *ast.FuncDecl:
			return f
		}
	}
	return nil
}

// scopeAt returns the innermost scope at pos.
func (c *Ctx) scopeAt(pos token.Pos) *types.Scope {
	return c.Pkg.Types.Scope().Innermost(pos)
}

// lookup finds name in scope at pos, considering only objects declared before pos.
func (c *Ctx) lookup(name string, pos token.Pos) types.Object {
	for s := c.scopeAt(pos); s != nil; s = s.Parent() {
		if o := s.Lookup(name); o != nil && (o.Pos() < pos || s == c.Pkg.Types.Scope() || s.Parent() == nil) {
			return o
		}
	}
	return nil
}

// fresh returns a name based on base that names nothing in the enclosing function or package
// and that no rule has introduced in this file.
func (c *Ctx) fresh(base string) string { return c.freshIn(c.funcNode(), base) }

// testVar returns the name of a *testing.T, *testing.B, *testing.F or testing.TB in scope at pos.
func (c *Ctx) testVar(pos token.Pos) string {
	for s := c.scopeAt(pos); s != nil && s != c.Pkg.Types.Scope(); s = s.Parent() {
		names := s.Names()
		sort.Strings(names)
		for _, n := range names {
			o, ok := s.Lookup(n).(*types.Var)
			if !ok || o.Pos() > pos || n == "_" {
				continue
			}
			t := o.Type()
			if isNamed(t, "testing", "T") || isNamed(t, "testing", "B") || isNamed(t, "testing", "F") || isNamed(t, "testing", "TB") {
				return n
			}
		}
	}
	return ""
}

// errDeclared reports whether a variable err of type error is in scope at pos within the
// innermost function.
func (c *Ctx) errDeclared(pos token.Pos) bool {
	o, ok := c.lookup("err", pos).(*types.Var)
	return ok && o.Type().String() == "error" && o.Parent() != c.Pkg.Types.Scope()
}

// onErr returns the statement that handles a non-nil err in the enclosing function: t.Fatal
// in a test, a return of err from a function whose last result is an error, log.Fatal in main,
// and panic otherwise, which is reported for a person to review.
func (c *Ctx) onErr(pos token.Pos, err string) string {
	if t := c.testVar(pos); t != "" {
		return fmt.Sprintf("if %s != nil {\n%s.Fatal(%s)\n}", err, t, err)
	}
	if sig := c.funcSig(); sig != nil && sig.Results().Len() > 0 {
		res := sig.Results()
		if last := res.At(res.Len() - 1).Type(); last.String() == "error" {
			var zs []string
			for i := range res.Len() - 1 {
				zs = append(zs, c.zero(res.At(i).Type()))
			}
			zs = append(zs, err)
			return fmt.Sprintf("if %s != nil {\nreturn %s\n}", err, strings.Join(zs, ", "))
		}
	}
	if c.Pkg.Name == "main" || c.run.Docs {
		return fmt.Sprintf("if %s != nil {\n%sFatal(%s)\n}", err, c.Q("log"), err)
	}
	// The old call could not fail here; the new one panics where it fails, which a library
	// function's caller does not expect: rewritten, and reported.
	c.run.Findings = append(c.run.Findings, Finding{Pos: c.Pkg.Fset.Position(pos), Rule: c.rule,
		Msg: "the error the new call returns is handled with panic(err), as this function returns no error: return or handle it"})
	return fmt.Sprintf("if %s != nil {\npanic(%s)\n}", err, err)
}

// typeString prints t as this file names it.
func (c *Ctx) typeString(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string {
		if p.Path() == c.Pkg.Path && !strings.HasSuffix(c.Pkg.Name, "_test") {
			return ""
		}
		return strings.TrimSuffix(c.Q(p.Path()), ".")
	})
}

// zero returns the zero value of t as an expression.
func (c *Ctx) zero(t types.Type) string {
	switch u := t.Underlying().(type) {
	case *types.Basic:
		switch {
		case u.Info()&types.IsBoolean != 0:
			return "false"
		case u.Info()&types.IsString != 0:
			return `""`
		case u.Info()&types.IsNumeric != 0:
			return "0"
		}
		return "nil"
	case *types.Struct, *types.Array:
		return c.typeString(t) + "{}"
	case *types.TypeParam:
		return "*new(" + c.typeString(t) + ")"
	}
	if _, ok := t.(*types.TypeParam); ok {
		return "*new(" + c.typeString(t) + ")"
	}
	return "nil"
}

// isTerminatingErrCheck reports whether s is "if err != nil { ... }" whose body ends the
// function or the test (a return, a t.Fatal, log.Fatal or panic).
func isTerminatingErrCheck(s ast.Stmt, err string) bool {
	is, ok := s.(*ast.IfStmt)
	if !ok || is.Init != nil || is.Else != nil {
		return false
	}
	be, ok := is.Cond.(*ast.BinaryExpr)
	if !ok || be.Op != token.NEQ {
		return false
	}
	if x, ok := be.X.(*ast.Ident); !ok || x.Name != err {
		return false
	}
	if y, ok := be.Y.(*ast.Ident); !ok || y.Name != "nil" {
		return false
	}
	if len(is.Body.List) == 0 {
		return false
	}
	return terminates(is.Body.List[len(is.Body.List)-1])
}

// terminates reports whether s ends the function or the test: a return, a panic, or a call
// of Fatal, Fatalf, FailNow, Skip, Skipf or SkipNow, or os.Exit.
func terminates(s ast.Stmt) bool {
	switch s := s.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return s.Tok == token.CONTINUE || s.Tok == token.BREAK || s.Tok == token.GOTO
	case *ast.ExprStmt:
		call, ok := s.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			return f.Name == "panic"
		case *ast.SelectorExpr:
			switch f.Sel.Name {
			case "Fatal", "Fatalf", "Fatalln", "FailNow", "Skip", "Skipf", "SkipNow", "Exit", "Panic", "Panicf":
				return true
			}
		}
	}
	return false
}

// durableMarker stands for the old agent.Durable where the code names it but it no longer
// resolves (code partly migrated): it is a type isDurableType recognizes.
var durableMarker = types.NewNamed(types.NewTypeName(token.NoPos, types.NewPackage(agentPath, "agent"), "Durable", nil), types.NewInterfaceType(nil, nil), nil)

// namesDurable reports whether a type expression names agent.Durable (or Durable).
func namesDurable(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name == "Durable"
	case *ast.Ident:
		return e.Name == "Durable"
	}
	return false
}

// declaredParam returns durableMarker when the i-th parameter of the function call calls is
// declared in this package with the type agent.Durable, which no longer resolves; nil otherwise.
func (c *Ctx) declaredParam(call *ast.CallExpr, i int) types.Type {
	var obj types.Object
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		obj = c.Info.Uses[f]
	case *ast.SelectorExpr:
		obj = c.Info.Uses[f.Sel]
	}
	if obj == nil || obj.Pkg() != c.Pkg.Types {
		return nil
	}
	for _, f := range c.Pkg.Files {
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Pos() != obj.Pos() {
				continue
			}
			k := 0
			for _, field := range fd.Type.Params.List {
				n := max(len(field.Names), 1)
				if i < k+n {
					if namesDurable(field.Type) {
						return durableMarker
					}
					return nil
				}
				k += n
			}
		}
	}
	return nil
}
