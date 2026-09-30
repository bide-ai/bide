package plan_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

type durOnly struct{ m *agent.MemStore }

func (d durOnly) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return d.m.Do(ctx, runID, name, fn)
}
func (d durOnly) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return d.m.History(ctx, runID)
}

// A flow through a Durable that is not a Journal (the durableStep path): halt, resolve, resume,
// and a relabelled node.
func TestZDurableWrapperPath(t *testing.T) {
	ctx := context.Background()
	d := durOnly{agent.NewMemStore()}
	var calls atomic.Int64
	b := plan.New[int, int]("w")
	a := b.Step("a", func(_ context.Context, n int) (int, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("lost")
		}
		return n + 1, nil
	})
	c := b.Step("c", func(_ context.Context, n int) (int, error) { return n * 10, nil }, plan.ReadOnly())
	b.Edge(a, c)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	_, err = flow.Run(ctx, d, "r", 1)
	t.Logf("d0: %v", err)
	_, err = flow.Run(ctx, d, "r", 1)
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok || calls.Load() != 1 {
		t.Fatalf("d1: %v, calls %d", err, calls.Load())
	}
	if err := agent.ResolveHaltRef(ctx, d, halt.Ref(), agent.Outcome{Result: 2}, agent.WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	out, err := flow.Run(ctx, d, "r", 1)
	if err != nil || out != 20 || calls.Load() != 1 {
		t.Fatalf("after resolve: %d %v calls %d", out, err, calls.Load())
	}
	if ok, diffs, err := flow.Conform(ctx, d, "r"); !ok || err != nil {
		t.Fatalf("conform: %v %v", diffs, err)
	}
	if _, err := flow.Run(ctx, d, "r", 2); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("other input: %v", err)
	}
}
