package plan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestPlanNoAdapterImports locks the ports-and-adapters boundary for the plan
// surface: plan lowers onto the CORE runtime and may import the core
// (github.com/bide-ai/bide), but it must never drag a concrete adapter into
// its runtime import graph. Dependencies point INWARD. If this fails, an adapter
// import crept into plan transitively and the hexagon is rotting.
//
// This mirrors the core's TestCoreHasNoAdapterImports (see
// /Users/dayna/code/go-agents/architecture_test.go): go list -deps reports the
// non-test dependency graph, so this checks what plan actually pulls at runtime.
func TestPlanNoAdapterImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/bide-ai/bide/plan").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	deps := string(out)

	// Adapter packages: plan may depend on the core's ports, never on a concrete
	// adapter. Importing the core root (github.com/bide-ai/bide) is allowed and
	// is intentionally NOT in this list; only its adapter subtrees are forbidden.
	forbidden := []string{
		"bide-ai/bide/model/", // provider adapters
		"bide-ai/bide/store/", // persistence adapters
		"bide-ai/bide/trace",  // OTel adapter
		"bide-ai/bide/middleware",
		"bide-ai/bide/govern", // gsm-backed governor (Tier-2 edge)
	}
	for _, bad := range forbidden {
		if strings.Contains(deps, bad) {
			t.Errorf("plan imports adapter %q -- plan must lower onto the core's ports, not pull adapters", bad)
		}
	}

	// Also assert plan pulls no heavy infrastructure transitively.
	for _, infra := range []string{"opentelemetry", "modernc.org/sqlite", "jackc/pgx", "temporal", "weaviate", "blackwell-systems/gsm"} {
		if strings.Contains(deps, infra) {
			t.Errorf("plan transitively depends on infrastructure %q -- the plan surface must stay dependency-light", infra)
		}
	}
}

// TestNoNewExecutor is the machine-checkable form of the substrate invariant
// (see docs/guides/flows.md): plan introduces NO
// new executor. plan is a surface over agent.Step control flow; Flow.Run walks
// the topology one node at a time and drives each as a durable step, inheriting
// the core's at-most-once/halt/resume guarantees. A scheduler, goroutine pool,
// channel-based fan-out, or errgroup IN plan would constitute a second execution
// model and break that inheritance.
//
// Concurrent agent.Do in the CORE runtime is fine: the core owns execution. What
// is forbidden here is a competing executor living inside the plan package
// itself. So this scans plan's non-test source for the concurrency primitives
// that would signal one.
//
// It matches only real CODE, never comments or string literals. Each file is
// parsed with go/parser and scanned at the token level (go/scanner via the
// parsed AST's identifiers and calls), so documentation prose that names these
// tokens does not trip the guard. This matters concretely: plan/doc.go states the
// package runs "with no goroutines or errgroup", which is PROSE and must not fail
// this test; a naive grep over raw file bytes would give a false positive.
func TestNoNewExecutor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read plan dir: %v", err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		// Parse WITHOUT comments in the AST; then walk only executable syntax
		// (identifiers, selectors, calls, go statements) so prose in doc comments and
		// string literals cannot match.
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.GoStmt:
				// Any `go f(...)` launches a goroutine: a second executor.
				t.Errorf("%s: plan launches a goroutine (go statement) -- plan must add no executor", name)
			case *ast.Ident:
				// errgroup or a sync.WaitGroup identifier used in code.
				if node.Name == "WaitGroup" {
					t.Errorf("%s: plan references sync.WaitGroup -- plan must add no executor", name)
				}
			case *ast.SelectorExpr:
				// errgroup.Group / errgroup.WithContext etc. surface as a selector on the
				// errgroup package identifier.
				if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == "errgroup" {
					t.Errorf("%s: plan uses errgroup.%s -- plan must add no executor", name, node.Sel.Name)
				}
			case *ast.CallExpr:
				// make(chan ...) creates a channel for cross-goroutine coordination.
				if isMakeChan(node) {
					t.Errorf("%s: plan creates a channel (make(chan ...)) -- plan must add no executor", name)
				}
			}
			return true
		})
	}
}

// isMakeChan reports whether call is `make(chan ...)`, the channel-construction
// form that signals cross-goroutine coordination. It matches on syntax only, so a
// channel type named in a comment or string is never seen.
func isMakeChan(call *ast.CallExpr) bool {
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "make" || len(call.Args) == 0 {
		return false
	}
	_, isChan := call.Args[0].(*ast.ChanType)
	return isChan
}
