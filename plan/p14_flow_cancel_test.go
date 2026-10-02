package plan

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A flow's cancellation (D1), beyond the review's test: a read-only node after Cancel does not run,
// and a flow whose last node ran while Cancel landed reads the end markers back after its
// run:complete and reports the first, run:cancelled, as Status does.
func TestP14_FlowCancel(t *testing.T) {
	for _, c := range []struct {
		name string
		opts []NodeOption
	}{{"read-only node", []NodeOption{ReadOnly()}}, {"side-effect node", nil}} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			j, err := agent.NewJournal(agent.NewMemStore())
			if err != nil {
				t.Fatal(err)
			}
			fired := 0
			b := New[int, int]("cancel-flow")
			first := b.Step("first", func(ctx context.Context, n int) (int, error) {
				if err := agent.Cancel(ctx, j, "r", "stop"); err != nil {
					t.Errorf("Cancel = %v", err)
				}
				return n, nil
			}, ReadOnly())
			second := b.Step("second", func(_ context.Context, n int) (int, error) { fired++; return n, nil }, c.opts...)
			b.Edge(first, second)
			flow, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := flow.Run(ctx, j, "r", 1); !errors.Is(err, agent.ErrRunCancelled) {
				t.Fatalf("flow.Run = %v, want ErrRunCancelled", err)
			}
			if fired != 0 {
				t.Fatalf("the node after Cancel ran %d times", fired)
			}
		})
	}
	t.Run("cancel during the last node", func(t *testing.T) {
		ctx := context.Background()
		j, err := agent.NewJournal(agent.NewMemStore())
		if err != nil {
			t.Fatal(err)
		}
		b := New[int, int]("cancel-last")
		b.Step("only", func(ctx context.Context, n int) (int, error) {
			if err := agent.Cancel(ctx, j, "r", "stop"); err != nil {
				t.Errorf("Cancel = %v", err)
			}
			return n, nil
		})
		flow, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		_, err = flow.Run(ctx, j, "r", 1)
		st, _ := agent.Status(ctx, j, "r")
		if !errors.Is(err, agent.ErrRunCancelled) || st.State != agent.RunCancelled {
			t.Fatalf("flow.Run = %v, Status %s; want ErrRunCancelled and cancelled", err, st.State)
		}
		if _, err := flow.Run(ctx, j, "r", 1); !errors.Is(err, agent.ErrRunCancelled) {
			t.Fatalf("a later drive of the cancelled flow = %v, want ErrRunCancelled", err)
		}
	})
}
