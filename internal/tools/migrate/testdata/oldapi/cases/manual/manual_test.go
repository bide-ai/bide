package manual

import (
	"context"
	"encoding/json"
	"flag"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// Each site here needs a person; the tool reports it and leaves it.

type fake struct{}

func (fake) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return fn(ctx)
}
func (fake) History(ctx context.Context, runID string) ([]agent.Record, error) { return nil, nil }

func useJournal(d agent.Durable) error {
	return agent.Approve(context.Background(), d, "r", "c", false)
}

// hooked is a store whose own Do intercepts writes: a Journal over it would never call it.
type hooked struct{ *agent.MemStore }

func (h hooked) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return h.MemStore.Do(ctx, runID, name, fn)
}

type holder struct{ a *agent.Agent }

type waker struct{}

func (waker) Wake() {}

func helper(n int) *agent.Agent {
	return agent.New(agent.NewScriptedModel(), agent.NewMemStore()).WithMaxTurns(n)
}

func TestManual(t *testing.T) {
	_ = useJournal(fake{})                      // a custom Durable
	_ = useJournal(hooked{agent.NewMemStore()}) // a store with a Do of its own
	a := helper(2)
	_ = a
	a.WithMaxTurns(4)                                            // a builder on an agent already in use
	ctx := agent.ContextWithWaker(context.Background(), waker{}) // a decorated context kept in a variable
	if out, err := a.Run(ctx, "r", "x"); err == nil {            // the answer scoped to an if
		t.Log(out)
	}
	f := a.RunSaga // a method value
	_ = f
	h := holder{a: helper(1)}
	h.a.WithMaxTurns(5) // a builder on a field, its result unused: it changed h.a in place
}

// fromHelper gets its old methods from an embedded struct that is not a tool: reported.
type descHelper struct{}

func (descHelper) Description() string         { return "h" }
func (descHelper) ArgsSchema() json.RawMessage { return nil }
func (descHelper) Safety() agent.Safety        { return agent.Safety{} }

type fromHelper struct{ descHelper }

func (fromHelper) Name() string { return "h" }
func (fromHelper) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

var _ agent.Tool = fromHelper{}

// Builder statements whose arguments may not evaluate the same where the agent is built: each
// is reported and left, not folded into the construction.
func TestFoldNeedsStableArguments(t *testing.T) {
	turns := flag.Int("turns", 3, "")
	a := agent.New(agent.NewScriptedModel(), agent.NewMemStore())
	flag.Parse()
	a.WithMaxTurns(*turns) // a pointer read after flag.Parse

	k := 1
	b := agent.New(agent.NewScriptedModel(), agent.NewMemStore())
	k = 2
	b.WithMaxTurns(k) // a variable assigned between

	c := agent.New(agent.NewScriptedModel(), agent.NewMemStore())
	var p = "late"
	c.WithSystemPrompt(p) // a variable declared after the construction

	n := 4
	bump := func() { n++ }
	d := agent.New(agent.NewScriptedModel(), agent.NewMemStore())
	bump()
	d.WithMaxTurns(n) // a variable a closure changes

	e := agent.New(agent.NewScriptedModel(), agent.NewMemStore())
	_, _ = e.Run(context.Background(), "r", "x")
	e.WithMaxTurns(5) // a builder after the agent ran: its first run had no limit
	_, _, _, _ = a, b, c, d
}

// Context decorators around a call that passes its options as a slice: reported (the option
// would have to join the slice).
func TestDecoratorsWithSliceOptions(t *testing.T) {
	a := helper(1)
	opts := []agent.RunOption{agent.WithSaga()}
	_, _ = a.RunMessage(agent.ContextWithWaker(context.Background(), waker{}), "r", agent.UserText("x"), opts...)
}
