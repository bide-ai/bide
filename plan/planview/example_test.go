package planview_test

import (
	"context"
	"os"

	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/plan/planview"
)

type Order struct{ Amount int }
type Receipt struct{ Status string }

// A program checks a flow config against its own Registry: the type mismatch on the edge from
// classify to done is found only with the Registry (bide-view -config checks structure only).
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
