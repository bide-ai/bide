package manual

import (
	"context"
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
