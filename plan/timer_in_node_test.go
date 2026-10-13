package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// agent.Sleep and agent.WaitUntil need the run context an agent supplies to its tools. A plan flow
// does not bind one, so a timer in a node's body (a Step, or a Tool node) fails with ErrConfig.
// This pins the limitation listed in docs/KNOWN-LIMITATIONS.md: when flows gain timers, this test
// fails, and the limitation and the guides change with it.
func TestTimerInNodeBodyIsNotAvailable(t *testing.T) {
	sleep := func(ctx context.Context, in int) (int, error) { return in, agent.Sleep(ctx, "t", time.Hour) }
	until := func(ctx context.Context, in int) (int, error) {
		return in, agent.WaitUntil(ctx, "t", time.Now().Add(time.Hour))
	}
	tool := agent.MustFunc("nap", "sleep", func(ctx context.Context, in int) (int, error) { return in, agent.Sleep(ctx, "t", time.Hour) },
		agent.WithSafety(agent.Safety{ReadOnly: true}))
	for name, build := range map[string]func(b *Builder[int, int]){
		"Step Sleep":     func(b *Builder[int, int]) { b.Step("n", sleep, ReadOnly()) },
		"Step WaitUntil": func(b *Builder[int, int]) { b.Step("n", until, ReadOnly()) },
		"Tool Sleep":     func(b *Builder[int, int]) { b.Tool[int, int]("n", tool) },
	} {
		t.Run(name, func(t *testing.T) {
			b := New[int, int]("timer")
			build(b)
			flow, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			j, err := agent.NewJournal(agent.NewMemStore())
			if err != nil {
				t.Fatal(err)
			}
			_, err = flow.Run(context.Background(), j, "r1", 1)
			var pending *agent.TimerPending
			if errors.As(err, &pending) || !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "Sleep called outside a running agent") {
				t.Fatalf("Run = %v; want ErrConfig \"Sleep called outside a running agent\"", err)
			}
		})
	}
}
