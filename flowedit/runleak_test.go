package flowedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"iter"
	"os"
	"strings"
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
// an error among their arguments (an identifier named err, a `...Err` identifier, a call of Err() or Error()), no %w, no
// errors.Join, and no call of Error() at all, unless the line carries "// text of the file, not of a store" (a parse error
// of the user's file: not the store's). The check is by name, since the files are small and the names are the code's own;
// it fails closed on a name it cannot tell, and a planted fmt.Sprintf("%v", err) fails it (see TestTheGuardFindsWhatItShould).
const fileMark = "// text of the file, not of a store"

var formatters = map[string]map[string]bool{
	"fmt":    {"Errorf": true, "Sprintf": true, "Sprint": true, "Sprintln": true, "Fprintf": true, "Fprint": true, "Fprintln": true, "Printf": true, "Print": true, "Println": true},
	"errors": {"Join": true},
}

// errorLike reports whether e is, by its name, an error value: err, a name ending in Err or err, or a call of Err() or Error().
func errorLike(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		n := strings.ToLower(e.Name)
		return n == "err" || strings.HasSuffix(n, "err") || n == "e" || n == "cause"
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Err" || sel.Sel.Name == "Error") {
			return true
		}
	case *ast.SelectorExpr:
		n := strings.ToLower(e.Sel.Name)
		return strings.HasSuffix(n, "err") || n == "cause"
	}
	return false
}

// guardProblems lists the ways the source of a reply-building file lets an error's text through.
func guardProblems(name string, src []byte) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return []string{name + ": " + err.Error()}
	}
	lines := strings.Split(string(src), "\n")
	var problems []string
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
			if !ok {
				return true
			}
			if sel.Sel.Name == "Error" && len(call.Args) == 0 && !marked {
				problems = append(problems, fmt.Sprintf("%s:%d: Error() is called without saying it is the text of a file: use storeFailure for a store's error", name, pos.Line))
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && formatters[pkg.Name][sel.Sel.Name] && !marked {
				for _, a := range call.Args {
					if errorLike(a) {
						problems = append(problems, fmt.Sprintf("%s:%d: %s.%s formats an error value: use storeFailure for a store's error", name, pos.Line, pkg.Name, sel.Sel.Name))
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
			}
			return true
		})
	}
	return problems
}

func TestOnlyFileTextIsTakenFromErrors(t *testing.T) {
	for _, name := range []string{"run.go", "runpath.go", "affected.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			continue // a file that does not exist in this change
		}
		for _, p := range guardProblems(name, src) {
			t.Error(p)
		}
	}
}

// The guard finds what it is for: a store's error formatted, wrapped or joined, or its text taken, in a function that is
// not storeFailure; and lets pass the file's text, marked, a class, and storeFailure itself.
func TestTheGuardFindsWhatItShould(t *testing.T) {
	const head = "package flowedit\n\nimport (\n\t\"errors\"\n\t\"fmt\"\n)\n\nvar _ = fmt.Sprint\nvar _ = errors.New\n\n"
	bad := map[string]string{
		"Sprintf %v":  `func f(err error) string { return fmt.Sprintf("%v", err) }`,
		"Sprint":      `func f(err error) string { return fmt.Sprint(err) }`,
		"Errorf %w":   `func f(err error) error { return fmt.Errorf("read: %w", err) }`,
		"Errorf %s":   `func f(err error) error { return fmt.Errorf("read: %s", err) }`,
		"Fprintf":     `func f(w fmt.Stringer, err error) { fmt.Fprintf(nil, "%v", err) }`,
		"Join":        `func f(err error) error { return errors.Join(err, nil) }`,
		"Error()":     `func f(err error) string { return "x" + err.Error() }`,
		"ctx.Err()":   `func f(ctx interface{ Err() error }) string { return fmt.Sprint(ctx.Err()) }`,
		"a Err name":  `func f(listErr error) string { return fmt.Sprintf("%v", listErr) }`,
		"a field err": `type t struct{ cause error }; func f(x t) string { return fmt.Sprint(x.cause) }`,
	}
	for name, body := range bad {
		if got := guardProblems("x.go", []byte(head+body+"\n")); len(got) == 0 {
			t.Errorf("%s: the guard let it pass", name)
		}
	}
	ok := map[string]string{
		"a class":      `func f(err error) string { return storeFailure("x", err) }`,
		"storeFailure": `func storeFailure(what string, err error) string { return fmt.Sprintf("%s (%v)", what, err) }`,
		"a marked file": `func f(err error) string { return fmt.Sprint(err.Error()) // text of the file, not of a store
}`,
		"no error":   `func f(n int) string { return fmt.Sprintf("%d", n) }`,
		"errors.New": `func f() error { return errors.New("x") }`,
	}
	for name, body := range ok {
		if got := guardProblems("x.go", []byte(head+body+"\n")); len(got) != 0 {
			t.Errorf("%s: the guard refused it: %q", name, got)
		}
	}
}
