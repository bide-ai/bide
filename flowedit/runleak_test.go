package flowedit

import (
	"context"
	"encoding/json"
	"errors"
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

// The only text of an error the page may hold is the file's (storeFailure is the one door a store's error goes through; this
// guard sees only calls of Error(), so an fmt.Errorf("...%w", err) whose text reached a reply would escape it, and must not exist): every call of Error() in the files that build the
// replies carries a comment saying it is the text of a file and not of a store, so a new one has to say what it
// is, in review, before it ships.
func TestOnlyFileTextIsTakenFromErrors(t *testing.T) {
	for _, name := range []string{"run.go", "runpath.go", "affected.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			continue // a file that does not exist in this change
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Error" || len(call.Args) != 0 {
				return true
			}
			line := fset.Position(call.Pos()).Line
			if !strings.Contains(lines[line-1], "// text of the file, not of a store") {
				t.Errorf("%s:%d: Error() is called without saying it is the text of a file: use storeFailure for a store's error", name, line)
			}
			return true
		})
	}
}
