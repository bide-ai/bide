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
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
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

// guardProblems lists the ways the files named in only let an error's text through.
func guardProblems(fset *token.FileSet, files []*ast.File, info *types.Info, srcs map[string][]byte, only map[string]bool) []string {
	var problems []string
	for _, f := range files {
		name := fset.Position(f.Pos()).Filename
		if !only[name] {
			continue
		}
		lines := strings.Split(string(srcs[name]), "\n")
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Name.Name == "storeFailure" {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pos := fset.Position(call.Pos())
				marked := strings.Contains(lines[pos.Line-1], fileMark)
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || marked {
					return true
				}
				if sel.Sel.Name == "Error" && len(call.Args) == 0 && isError(info, sel.X) {
					problems = append(problems, fmt.Sprintf("%s:%d: Error() is called on an error without saying it is the text of a file: use storeFailure for a store's error", name, pos.Line))
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || !formatters[pkg.Name][sel.Sel.Name] {
					return true
				}
				for _, a := range call.Args {
					if isError(info, a) {
						problems = append(problems, fmt.Sprintf("%s:%d: %s.%s formats a value of an error type: use storeFailure for a store's error", name, pos.Line, pkg.Name, sel.Sel.Name))
						break
					}
				}
				if pkg.Name == "fmt" && sel.Sel.Name == "Errorf" && len(call.Args) > 0 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && strings.Contains(lit.Value, "%w") {
						problems = append(problems, fmt.Sprintf("%s:%d: fmt.Errorf wraps an error with %%w: its text would reach a reply", name, pos.Line))
					}
				}
				if pkg.Name == "errors" {
					problems = append(problems, fmt.Sprintf("%s:%d: errors.%s joins errors: its text would reach a reply", name, pos.Line, sel.Sel.Name))
				}
				return true
			})
		}
	}
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
	"errors"
	"fmt"
)

var _ = errors.New

type wrapped struct{ cause error }

func (w wrapped) Error() string { return "w" }

func storeFailure(what string, err error) string { return fmt.Sprintf("%s (%v)", what, err) }

`
	bad := map[string]string{
		"Sprintf %v":            `func f(err error) string { return fmt.Sprintf("%v", err) }`,
		"Sprint":                `func f(err error) string { return fmt.Sprint(err) }`,
		"Errorf %w":             `func f(err error) error { return fmt.Errorf("read: %w", err) }`,
		"Errorf %s":             `func f(err error) error { return fmt.Errorf("read: %s", err) }`,
		"Fprintf":               `func f(err error) { fmt.Fprintf(nil, "%v", err) }`,
		"Join":                  `func f(err error) error { return errors.Join(err, nil) }`,
		"Error()":               `func f(err error) string { return "x" + err.Error() }`,
		"an error named cause":  `func f(cause error) string { return fmt.Sprint(cause) }`,
		"an error named e":      `func f(e error) string { return fmt.Sprintf("%v", e) }`,
		"a field":               `func f(x wrapped) string { return fmt.Sprint(x.cause) }`,
		"a call that returns":   `func g() error { return nil }; func f() string { return fmt.Sprint(g()) }`,
		"a concrete error type": `func f(x wrapped) string { return fmt.Sprint(x) }`,
		"a pointer to one":      `func f(x *wrapped) string { return fmt.Sprint(x) }`,
		"Error() of a field":    `func f(x wrapped) string { return x.cause.Error() }`,
	}
	for name, body := range bad {
		srcs := map[string][]byte{"x.go": []byte(head + body + "\n")}
		fset, files, info := typeCheck(t, srcs)
		if got := guardProblems(fset, files, info, srcs, map[string]bool{"x.go": true}); len(got) == 0 {
			t.Errorf("%s: the guard let it pass", name)
		}
	}
	ok := map[string]string{
		"a class":       `func f(err error) string { return storeFailure("x", err) }`,
		"a marked file": "func f(err error) string { return fmt.Sprint(err.Error()) " + fileMark + "\n}",
		"no error":      `func f(n int) string { return fmt.Sprintf("%d", n) }`,
		"a string":      `func f(cause string) string { return fmt.Sprint(cause) }`,
		"errors.New":    `func f() error { return errors.New("x") }`,
		"errors.Is":     `func f(err error) bool { return errors.Is(err, errors.ErrUnsupported) }`,
	}
	for name, body := range ok {
		srcs := map[string][]byte{"x.go": []byte(head + body + "\n")}
		fset, files, info := typeCheck(t, srcs)
		if got := guardProblems(fset, files, info, srcs, map[string]bool{"x.go": true}); len(got) != 0 {
			t.Errorf("%s: the guard refused it: %q", name, got)
		}
	}
}
