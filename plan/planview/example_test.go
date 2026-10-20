package planview_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/plan/planview"
	"github.com/bide-ai/bide/theme"
)

type Order struct{ Amount int }
type Receipt struct{ Status string }

// A program checks a flow config against its own Registry: the type mismatch on the edge from
// classify to done is found only with the Registry (bide view -config checks structure only).
// planview.Handler serves the same check as the config page.
func Example() {
	reg := plan.NewRegistry()
	_ = reg.RegisterStep("classify", func(_ context.Context, o Order) (Order, error) { return o, nil })
	_ = reg.RegisterStep("done", func(_ context.Context, s string) (Receipt, error) { return Receipt{Status: s}, nil })
	config := []byte(`{"version": 1, "flow": "orders",
	  "nodes": [{"name": "classify", "block": "classify"}, {"name": "done", "block": "done"}],
	  "wiring": [{"edge": ["classify", "done"]}]}`)

	if planview.Check(os.Stdout, "orders.json", config, nil) {
		return
	}
	planview.Check(os.Stdout, "orders.json", config, reg)
	// Output:
	// orders.json:classify->done: edge "classify" -> "done" connects planview_test.Order to string (types must match exactly)
}

// A CI step compares a flow config with the version on the main branch and fails when an approval
// gate was weakened. With each version's Registry (Version.Registry) the summary line also gives
// the digest each version's runs journal.
func ExampleCheckDiff() {
	old := []byte(`{"version": 1, "flow": "orders",
	  "nodes": [{"name": "classify", "block": "classify"},
	            {"name": "pay", "block": "pay", "approval": {"need": 2, "approvers": ["ops", "finance"]}}],
	  "wiring": [{"edge": ["classify", "pay"]}]}`)
	newer := []byte(`{"version": 1, "flow": "orders",
	  "nodes": [{"name": "classify", "block": "classify"},
	            {"name": "pay", "block": "pay", "approval": {"need": 1, "approvers": ["ops", "finance"]}}],
	  "wiring": [{"edge": ["classify", "pay"]}]}`)

	weakened, err := planview.CheckDiff(os.Stdout, planview.Version{Name: "main:orders.json", Data: old}, planview.Version{Name: "orders.json", Data: newer})
	if err == nil && weakened {
		fmt.Println("fail the build")
	}
	// Output:
	// orders.json: changed node pay: approval 2 of 2 -> 1 of 2 (weakened)
	// orders.json: approval policy weakened: pay: approval 2 of 2 -> 1 of 2
	// orders.json: 1 change from main:orders.json (nodes: 0 added, 0 removed, 1 changed; edges: 0 added, 0 removed, 0 changed); digest not computed: the check is structural (the digest commits to the blocks' Go types; see plan/planview)
	// fail the build
}

// A program that serves the config page in its own look passes a theme that passed the checks, and
// may make it the only theme the page has. The page's stylesheet is generated from the checked
// values; the host cannot pass CSS.
func ExampleWithThemeLock() {
	th, _, err := theme.Parse([]byte(`{"bide_theme": 1, "name": "acme-night", "base": "dark",
	  "surface": {"bg": "#0e1116", "accent": "#79b8ff"}}`))
	if err != nil {
		fmt.Println(err)
		return
	}
	config := []byte(`{"version": 1, "flow": "orders", "nodes": [{"name": "a", "block": "a"}], "wiring": []}`)
	h := planview.Handler("orders.json", config, nil, planview.WithThemes(th), planview.WithThemeLock("acme-night"))

	r := httptest.NewRequest("GET", "http://127.0.0.1/assets/style.css", nil)
	r.Host = "127.0.0.1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	fmt.Println(strings.Contains(w.Body.String(), `--bide-lock: "acme-night";`))
	// Output: true
}
