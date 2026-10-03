package tools

import (
	"context"
	"encoding/json"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

type in struct{ Q string }

var lookup = agent.Func("lookup", "looks up", agent.Safety{ReadOnly: true},
	func(ctx context.Context, x in) (string, error) { return x.Q, nil })

var pay = agent.Func("pay", "pays", agent.Safety{}, func(ctx context.Context, x in) (int, error) { return 0, nil }, agent.WithTimeout(0))

var book = agent.CompensatedFunc("book", "books", agent.Safety{Idempotent: true},
	func(ctx context.Context, x in) (int, error) { return 0, nil },
	func(ctx context.Context, x in, out int) error { return nil })

func sub(a *agent.Agent) agent.Tool { return agent.SubAgent("helper", "helps", a) }

// legacy implements the old Tool method set.
type legacy struct{ name string }

func (t *legacy) Name() string                { return t.name }
func (t *legacy) Description() string         { return "legacy" }
func (t *legacy) ArgsSchema() json.RawMessage { return nil }
func (t *legacy) Safety() agent.Safety        { return agent.Safety{} }
func (t *legacy) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func names(ts []agent.Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name(), agent.SpecOf(t).Description)
	}
	return out
}

func register(r *plan.Registry) error {
	return plan.RegisterStep(r, "double", func(ctx context.Context, n int) (int, error) { return 2 * n, nil })
}

func withOpts(opts ...agent.ToolOption) agent.Tool {
	return agent.Func("x", "d", agent.Safety{ReadOnly: true}, func(ctx context.Context, x in) (int, error) { return 0, nil }, opts...)
}

// forceSideEffect is a decorator that embeds a tool and overrides its Safety: the override must
// survive (a read-only tool forced to be a side effect must not be re-run on resume).
type forceSideEffect struct{ agent.Tool }

func (forceSideEffect) Safety() agent.Safety { return agent.Safety{} }

// renamed overrides the name and the description, on a pointer receiver, and reads the embedded
// tool's own description.
type renamed struct {
	agent.Tool
	prefix string
}

func (r *renamed) Name() string        { return r.prefix + r.Tool.Name() }
func (r *renamed) Description() string { return "renamed: " + r.Tool.Description() }

// passThrough embeds a tool and overrides nothing: the embedded Spec is its own.
type passThrough struct{ agent.Tool }

var _ = []agent.Tool{forceSideEffect{}, &renamed{}, passThrough{}}
