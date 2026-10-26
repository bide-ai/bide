package flowedit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"iter"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// leak is an error as a driver gives it when a connection fails: it holds a password, a host and a path.
var leak = errors.New(`dial tcp postgres://admin:hunter2@db.internal.example:5432/orders?sslrootcert=/etc/secret/ca.pem: connection refused`)

var leakWords = []string{"hunter2", "db.internal", "postgres://", "/etc/secret", "sslrootcert", "5432"}

// poisonStore fails the reads failOn selects, with the leaking error.
type poisonStore struct {
	agent.Store
	failOn func(op, name string) bool
}

func (p poisonStore) Get(ctx context.Context, run, name string) (agent.Entry, bool, error) {
	if p.failOn("get", name) {
		return agent.Entry{}, false, leak
	}
	return p.Store.Get(ctx, run, name)
}

func (p poisonStore) Load(ctx context.Context, run string, after int64) iter.Seq2[agent.Entry, error] {
	if p.failOn("load", "") {
		return func(yield func(agent.Entry, error) bool) { yield(agent.Entry{}, leak) }
	}
	return p.Store.Load(ctx, run, after)
}

func (p poisonStore) Runs(ctx context.Context, f agent.RunFilter) iter.Seq2[string, error] {
	if p.failOn("runs", "") {
		return func(yield func(string, error) bool) { yield("", leak) }
	}
	l, _ := agent.Capability[agent.Lister](p.Store)
	return l.Runs(ctx, f)
}

// failSource is a Source whose History fails with the leaking error.
type failSource struct{}

func (failSource) Runs(context.Context) ([]string, error)                  { return []string{"r1"}, nil }
func (failSource) History(context.Context, string) ([]agent.Record, error) { return nil, leak }

// An error from the store never reaches a reply as text, whichever read fails: the first entry, the topology, the digest,
// an end marker, a node's result, an attempt, an approval, a Switch's choice, or the history of an exported journal.
// Each reply is searched for the password, the host and the path of the error.
func TestRunReplyNeverHoldsAStoreError(t *testing.T) {
	flow, err := plan.Load[int, string]([]byte(loopRunCfg), loopRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	base := agent.NewMemStore()
	j, _ := agent.NewJournal(base)
	if _, err := flow.Run(context.Background(), j, "r1", 1); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(op, name string) bool{
		"every get":       func(op, n string) bool { return op == "get" },
		"the first entry": func(op, n string) bool { return op == "load" },
		"the topology":    func(op, n string) bool { return op == "get" && n == "flow:topology" },
		"the digest":      func(op, n string) bool { return op == "get" && n == "flow:digest" },
		"an end marker":   func(op, n string) bool { return op == "get" && strings.HasPrefix(n, "run:c") },
		"a node result":   func(op, n string) bool { return op == "get" && strings.HasPrefix(n, "node:") },
		"an attempt":      func(op, n string) bool { return op == "get" && strings.HasPrefix(n, "attempt:") },
		"an approval":     func(op, n string) bool { return op == "get" && strings.HasPrefix(n, "approval-pending:") },
		"a switch choice": func(op, n string) bool { return op == "get" && strings.HasPrefix(n, "switch:") },
		"nothing fails":   func(op, n string) bool { return false },
	}
	for name, fail := range cases {
		ps := poisonStore{Store: base, failOn: fail}
		pj, _ := agent.NewJournal(ps)
		// (the Journal caches nothing across these runs of the server: a new one is made for each)
		for _, typed := range []bool{true, false} {
			s := loopServer(t, loopRunCfg, runview.JournalSource{J: pj}, typed)
			for _, reload := range []bool{false, true} {
				body, _ := json.Marshal(map[string]any{"text": loopRunCfg, "reload": reload})
				out := call(s.Handler(), s, "/api/run", string(body), nil).Body.String()
				for _, w := range leakWords {
					if strings.Contains(out, w) {
						t.Errorf("%s (typed=%v): the reply holds %q: %s", name, typed, w, out)
					}
				}
			}
		}
	}
	// An exported journal whose history fails.
	s := runServer(t, runCfg, failSource{}, "r1", true)
	body, _ := json.Marshal(map[string]any{"text": runCfg})
	if out := call(s.Handler(), s, "/api/run", string(body), nil).Body.String(); strings.Contains(out, "hunter2") || strings.Contains(out, "db.internal") || !strings.Contains(out, "read error") {
		t.Errorf("a failing history: %s", out)
	}
}

// storeFailure is the one door a store's error goes through to a reply. These files build the replies, and in them
// an error value may not be formatted or wrapped into text: no fmt.Errorf, Sprintf, Sprint, Fprintf and the like with
// an argument whose type is an error (whatever it is called: err, cause, e, a struct's field), no %w, no errors.Join,
// and no call of Error() on an error, unless the line carries "// text of the file, not of a store" (a parse error of
// the user's file: not the store's). The check is by type: the package is type-checked with go/types against the export
// data `go list -export` builds, as internal/tools/docsnip does. An error held in an `any` is not seen by type; the
// formatters taking one are flagged all the same when an argument is `any` holding the result of a call of Err().
const fileMark = "// text of the file, not of a store"

var formatters = map[string]map[string]bool{
	"fmt":    {"Errorf": true, "Sprintf": true, "Sprint": true, "Sprintln": true, "Fprintf": true, "Fprint": true, "Fprintln": true, "Printf": true, "Print": true, "Println": true},
	"errors": {"Join": true},
}

var (
	exportsOnce sync.Once
	exportFiles map[string]string
	exportErr   error
)

// loadExports builds the export data of this package's imports and returns the file of each import path.
func loadExports(t *testing.T) map[string]string {
	t.Helper()
	exportsOnce.Do(func() {
		out, err := exec.Command("go", "list", "-e", "-export", "-deps", "-json=ImportPath,Export").Output()
		if err != nil {
			exportErr = fmt.Errorf("go list -export: %w", err)
			return
		}
		exportFiles = map[string]string{}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var p struct{ ImportPath, Export string }
			if err := dec.Decode(&p); err != nil {
				break
			}
			if p.Export != "" {
				exportFiles[p.ImportPath] = p.Export
			}
		}
	})
	if exportErr != nil {
		t.Fatal(exportErr)
	}
	return exportFiles
}

// typeCheck parses and type-checks the files (name to source) as one package.
func typeCheck(t *testing.T, srcs map[string][]byte) (*token.FileSet, []*ast.File, *types.Info) {
	t.Helper()
	exports := loadExports(t)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range slices.Sorted(maps.Keys(srcs)) {
		f, err := parser.ParseFile(fset, name, srcs[name], parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if f := exports[path]; f != "" {
			return os.Open(f)
		}
		return nil, fmt.Errorf("package %s was not loaded", path)
	})
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	cfg := types.Config{Importer: imp, Error: func(err error) { t.Errorf("type-checking: %v", err) }}
	if _, err := cfg.Check("flowedit", fset, files, info); err != nil {
		t.Fatalf("type-checking: %v", err)
	}
	return fset, files, info
}

var errorType = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

// isError reports whether the expression's type is, or points to, a type that implements error.
func isError(info *types.Info, e ast.Expr) bool {
	tv, ok := info.Types[e]
	if !ok || tv.Type == nil || tv.IsType() {
		return false
	}
	typ := tv.Type
	if _, isIface := typ.Underlying().(*types.Interface); isIface && !types.Implements(typ, errorType) {
		return false
	}
	return types.Implements(typ, errorType) || types.Implements(types.NewPointer(typ), errorType)
}

// mentionsError reports whether a value of type t can carry an error: it is one, or holds one (an element, a field, a
// pointee, a key). Formatting such a value formats the error.
func mentionsError(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	if types.Implements(t, errorType) || types.Implements(types.NewPointer(t), errorType) {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Slice:
		return mentionsError(u.Elem(), seen)
	case *types.Array:
		return mentionsError(u.Elem(), seen)
	case *types.Pointer:
		return mentionsError(u.Elem(), seen)
	case *types.Chan:
		return mentionsError(u.Elem(), seen)
	case *types.Map:
		return mentionsError(u.Key(), seen) || mentionsError(u.Elem(), seen)
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			if mentionsError(u.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// toOtherInterface reports whether storing expr in a place of type target turns a value that can carry an error into
// an interface other than error itself: `any`, fmt.Stringer, whatever the code then does with it can format the error.
func toOtherInterface(info *types.Info, expr ast.Expr, target types.Type) bool {
	if target == nil {
		return false
	}
	if _, ok := target.Underlying().(*types.Interface); !ok {
		return false
	}
	if types.Identical(target, types.Universe.Lookup("error").Type()) {
		return false
	}
	return mentionsError(info.TypeOf(expr), map[types.Type]bool{})
}

// isErrorsAs is a call of errors.As, whose target is a pointer to an error type by design: it finds a typed error to
// inspect, and what is then done with the one it finds is checked like any error.
func isErrorsAs(n *ast.CallExpr) bool {
	sel, ok := n.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "errors" && sel.Sel.Name == "As"
}

// guardProblems lists the ways the files named in only let an error's text through: an error formatted, wrapped,
// joined, asserted to another type, its text taken, or an error-carrying value converted (an argument, an assignment, a
// declaration, a return, an element of a literal) to an interface other than error.
func guardProblems(fset *token.FileSet, files []*ast.File, info *types.Info, srcs map[string][]byte, only map[string]bool) []string {
	var problems []string
	for _, f := range files {
		name := fset.Position(f.Pos()).Filename
		if !only[name] {
			continue
		}
		lines := strings.Split(string(srcs[name]), "\n")
		report := func(pos token.Pos, format string, args ...any) {
			p := fset.Position(pos)
			if strings.Contains(lines[p.Line-1], fileMark) {
				return
			}
			problems = append(problems, fmt.Sprintf("%s:%d: ", name, p.Line)+fmt.Sprintf(format, args...))
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Name.Name == "storeFailure" {
				continue
			}
			var enclosing []*types.Signature // the function the node is in: a literal inside a declaration has its own
			if sig, ok := info.Defs[fd.Name].Type().(*types.Signature); ok {
				enclosing = append(enclosing, sig)
			}
			var stack []ast.Node
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if n == nil {
					if top := stack[len(stack)-1]; top != nil {
						if _, isLit := top.(*ast.FuncLit); isLit {
							enclosing = enclosing[:len(enclosing)-1]
						}
					}
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				switch n := n.(type) {
				case *ast.FuncLit:
					sig, _ := info.TypeOf(n).(*types.Signature)
					enclosing = append(enclosing, sig)
				case *ast.TypeAssertExpr:
					if n.Type != nil && mentionsError(info.TypeOf(n.X), map[types.Type]bool{}) {
						report(n.Pos(), "a type assertion on a value that can carry an error: its text would be one call away from a reply")
					}
				case *ast.SendStmt:
					if ch, ok := info.TypeOf(n.Chan).Underlying().(*types.Chan); ok && toOtherInterface(info, n.Value, ch.Elem()) {
						report(n.Pos(), "an error-carrying value is sent on a channel of an interface other than error")
					}
				case *ast.ReturnStmt:
					if sig := enclosing[len(enclosing)-1]; sig != nil && sig.Results().Len() == len(n.Results) {
						for i, r := range n.Results {
							if toOtherInterface(info, r, sig.Results().At(i).Type()) {
								report(r.Pos(), "an error-carrying value is returned as an interface other than error")
							}
						}
					}
				case *ast.AssignStmt:
					if n.Tok == token.ASSIGN && len(n.Lhs) == len(n.Rhs) {
						for i := range n.Rhs {
							if toOtherInterface(info, n.Rhs[i], info.TypeOf(n.Lhs[i])) {
								report(n.Rhs[i].Pos(), "an error-carrying value is assigned to an interface other than error")
							}
						}
					}
				case *ast.ValueSpec:
					if n.Type != nil {
						for _, v := range n.Values {
							if toOtherInterface(info, v, info.TypeOf(n.Type)) {
								report(v.Pos(), "an error-carrying value is declared as an interface other than error")
							}
						}
					}
				case *ast.CompositeLit:
					switch u := info.TypeOf(n).Underlying().(type) {
					case *types.Struct:
						for i, e := range n.Elts {
							if kv, ok := e.(*ast.KeyValueExpr); ok {
								for k := 0; k < u.NumFields(); k++ {
									if id, ok := kv.Key.(*ast.Ident); ok && u.Field(k).Name() == id.Name && toOtherInterface(info, kv.Value, u.Field(k).Type()) {
										report(kv.Value.Pos(), "an error-carrying value is put in a field of an interface other than error")
									}
								}
							} else if i < u.NumFields() && toOtherInterface(info, e, u.Field(i).Type()) {
								report(e.Pos(), "an error-carrying value is put in a field of an interface other than error")
							}
						}
					case *types.Slice, *types.Array, *types.Map:
						var elem types.Type
						switch u := u.(type) {
						case *types.Slice:
							elem = u.Elem()
						case *types.Array:
							elem = u.Elem()
						case *types.Map:
							elem = u.Elem()
						}
						for _, e := range n.Elts {
							if kv, ok := e.(*ast.KeyValueExpr); ok {
								e = kv.Value
							}
							if toOtherInterface(info, e, elem) {
								report(e.Pos(), "an error-carrying value is put in a collection of an interface other than error")
							}
						}
					}
				case *ast.CallExpr:
					// A conversion T(x), and a call: each argument against its parameter.
					if tv := info.Types[n.Fun]; tv.IsType() && len(n.Args) == 1 {
						if toOtherInterface(info, n.Args[0], tv.Type) {
							report(n.Pos(), "an error-carrying value is converted to an interface other than error")
						}
					} else if sig, ok := info.TypeOf(n.Fun).(*types.Signature); ok && !isErrorsAs(n) {
						for i, a := range n.Args {
							var pt types.Type
							switch {
							case sig.Variadic() && i >= sig.Params().Len()-1:
								last := sig.Params().At(sig.Params().Len() - 1).Type().(*types.Slice)
								pt = last.Elem()
								if n.Ellipsis.IsValid() {
									pt = last
								}
							case i < sig.Params().Len():
								pt = sig.Params().At(i).Type()
							}
							if toOtherInterface(info, a, pt) {
								report(a.Pos(), "an error-carrying value is passed as an interface other than error: use storeFailure for a store's error")
							}
						}
					}
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok {
						break
					}
					if sel.Sel.Name == "Error" && len(n.Args) == 0 && isError(info, sel.X) {
						report(n.Pos(), "Error() is called on an error without saying it is the text of a file: use storeFailure for a store's error")
					}
					pkg, ok := sel.X.(*ast.Ident)
					if !ok || !formatters[pkg.Name][sel.Sel.Name] {
						break
					}
					if pkg.Name == "fmt" && sel.Sel.Name == "Errorf" && len(n.Args) > 0 {
						if lit, ok := n.Args[0].(*ast.BasicLit); ok && strings.Contains(lit.Value, "%w") {
							report(n.Pos(), "fmt.Errorf wraps an error with %%w: its text would reach a reply")
						}
					}
					if pkg.Name == "errors" {
						report(n.Pos(), "errors.%s joins errors: its text would reach a reply", sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	slices.Sort(problems)
	return problems
}

func TestOnlyFileTextIsTakenFromErrors(t *testing.T) {
	pkg, err := build.ImportDir(".", 0) // the files of this platform's build, as the compiler sees them
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string][]byte{}
	for _, n := range pkg.GoFiles {
		if srcs[n], err = os.ReadFile(n); err != nil {
			t.Fatal(err)
		}
	}
	fset, files, info := typeCheck(t, srcs)
	only := map[string]bool{}
	for _, n := range []string{"run.go", "runpath.go", "affected.go"} {
		if _, ok := srcs[n]; ok {
			only[n] = true
		}
	}
	if len(only) < 2 {
		t.Fatalf("the files that build replies were not found: %v", only)
	}
	for _, p := range guardProblems(fset, files, info, srcs, only) {
		t.Error(p)
	}
}

// The guard finds what it is for: an error of any name or field, formatted, wrapped or joined, or its text taken, in a
// function that is not storeFailure; and lets pass the file's text, marked, a class, and storeFailure itself.
func TestTheGuardFindsWhatItShould(t *testing.T) {
	const head = `package flowedit

import (
	"encoding/json"
	"errors"
	"fmt"
)

var _ = errors.New
var _ = json.Marshal

type wrapped struct{ cause error }

func (w wrapped) Error() string { return "w" }

func storeFailure(what string, err error) string { return fmt.Sprintf("%s (%v)", what, err) }

`
	bad := map[string]string{
		"Sprintf %v":                `func f(err error) string { return fmt.Sprintf("%v", err) }`,
		"Sprint":                    `func f(err error) string { return fmt.Sprint(err) }`,
		"Errorf %w":                 `func f(err error) error { return fmt.Errorf("read: %w", err) }`,
		"Errorf %s":                 `func f(err error) error { return fmt.Errorf("read: %s", err) }`,
		"Fprintf":                   `func f(err error) { fmt.Fprintf(nil, "%v", err) }`,
		"Join":                      `func f(err error) error { return errors.Join(err, nil) }`,
		"Error()":                   `func f(err error) string { return "x" + err.Error() }`,
		"an error named cause":      `func f(cause error) string { return fmt.Sprint(cause) }`,
		"an error named e":          `func f(e error) string { return fmt.Sprintf("%v", e) }`,
		"a field":                   `func f(x wrapped) string { return fmt.Sprint(x.cause) }`,
		"a call that returns":       `func g() error { return nil }; func f() string { return fmt.Sprint(g()) }`,
		"a concrete error type":     `func f(x wrapped) string { return fmt.Sprint(x) }`,
		"a pointer to one":          `func f(x *wrapped) string { return fmt.Sprint(x) }`,
		"Error() of a field":        `func f(x wrapped) string { return x.cause.Error() }`,
		"via an any variable":       `func f(err error) string { var x any = err; return fmt.Sprint(x) }`,
		"json.Marshal":              `func f(err error) []byte { b, _ := json.Marshal(err); return b }`,
		"in a slice":                `func f(err error) string { return fmt.Sprint([]error{err}) }`,
		"in a struct":               `func f(err error) string { return fmt.Sprint(struct{ e error }{err}) }`,
		"a Stringer assertion":      `func f(err error) string { return err.(fmt.Stringer).String() }`,
		"returned as any":           `func g(err error) any { return err }; func f(err error) string { return fmt.Sprint(g(err)) }`,
		"assigned to any":           `func f(err error) string { var x any; x = err; return fmt.Sprint(x) }`,
		"a map of errors":           `func f(err error) string { return fmt.Sprint(map[string]error{"a": err}) }`,
		"converted":                 `func f(err error) string { return fmt.Sprint(any(err)) }`,
		"a closure":                 `func f(err error) string { return func() any { return err }().(string) }`,
		"sent on a chan any":        `func f(err error) { c := make(chan any, 1); c <- err }`,
		"sent in a slice on a chan": `func f(err error) { c := make(chan any, 1); c <- []error{err} }`,
		"an any slice":              `func f(err error) { _ = []any{err} }`,
		"an any map":                `func f(err error) { _ = map[string]any{"a": err} }`,
		"an any field, unkeyed":     `func f(err error) { _ = struct{ v any }{err} }`,
		"an any field, keyed":       `func f(err error) { _ = struct{ v any }{v: err} }`,
	}
	for name, body := range bad {
		srcs := map[string][]byte{"x.go": []byte(head + body + "\n")}
		fset, files, info := typeCheck(t, srcs)
		if got := guardProblems(fset, files, info, srcs, map[string]bool{"x.go": true}); len(got) == 0 {
			t.Errorf("%s: the guard let it pass", name)
		}
	}
	ok := map[string]string{
		"a class":              `func f(err error) string { return storeFailure("x", err) }`,
		"a marked file":        "func f(err error) string { return fmt.Sprint(err.Error()) " + fileMark + "\n}",
		"no error":             `func f(n int) string { return fmt.Sprintf("%d", n) }`,
		"a string":             `func f(cause string) string { return fmt.Sprint(cause) }`,
		"errors.New":           `func f() error { return errors.New("x") }`,
		"errors.Is":            `func f(err error) bool { return errors.Is(err, errors.ErrUnsupported) }`,
		"an error var":         `func f(err error) error { var e error = err; e = err; return e }`,
		"a func(error)":        `func g(error) {}; func f(err error) { g(err) }`,
		"an error list":        `func f(err error) []error { return []error{err} }`,
		"sent on a chan error": `func f(err error) { c := make(chan error, 1); c <- err }`,
		"errors.As":            `type pe struct{}; func (*pe) Error() string { return "" }; func f(err error) bool { var p *pe; return errors.As(err, &p) }`,
	}
	for name, body := range ok {
		srcs := map[string][]byte{"x.go": []byte(head + body + "\n")}
		fset, files, info := typeCheck(t, srcs)
		if got := guardProblems(fset, files, info, srcs, map[string]bool{"x.go": true}); len(got) != 0 {
			t.Errorf("%s: the guard refused it: %q", name, got)
		}
	}
}
