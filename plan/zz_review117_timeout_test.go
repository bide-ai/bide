package plan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Suspicion (c): a flow's Tool node applies the wrapped tool's ToolSpec.Timeout, as the agent
// does, and an error after the deadline has an unknown outcome.
func TestR117_ToolNodeAppliesTheToolsTimeout(t *testing.T) {
	var sawDeadline bool
	slow := agent.MustFunc("slow", "", func(ctx context.Context, _ int) (int, error) {
		dl, ok := ctx.Deadline()
		sawDeadline = ok && time.Until(dl) < time.Second
		<-ctx.Done()
		return 0, ctx.Err()
	}, agent.WithTimeout(time.Millisecond))
	for name, run := range map[string]func(context.Context, any) (any, error){
		"Builder.Tool": func() func(context.Context, any) (any, error) {
			b := New[int, int]("f")
			b.Tool[int, int]("slow", slow)
			return b.core.nodes[0].run
		}(),
		"RegisterTool": func() func(context.Context, any) (any, error) {
			reg := NewRegistry()
			if err := reg.RegisterTool[int, int]("slow", slow); err != nil {
				t.Fatal(err)
			}
			return reg.blocks["slow"].run
		}(),
	} {
		sawDeadline = false
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // a backstop, far past the tool's
		start := time.Now()
		_, err := run(ctx, 1)
		cancel()
		if time.Since(start) > 4*time.Second {
			t.Errorf("%s: the call ran to the backstop: the tool's timeout was not applied", name)
		}
		if !sawDeadline || !errors.Is(err, agent.ErrToolOutcomeUnknown) {
			t.Errorf("%s: deadline seen %v, err %v; want the tool's deadline applied and ErrToolOutcomeUnknown", name, sawDeadline, err)
		}
	}
}
