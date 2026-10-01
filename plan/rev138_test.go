package plan

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// rev138: Cancel accepts a flow's run (it has run:start), returns nil and Status reports it
// RunCancelled, but the flow's drive never reads run:cancelled: a node after the Cancel still fires
// its effect, and the flow then writes run:complete and returns its output.
func TestRev138_FlowIgnoresCancel(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	j, err := agent.NewJournal(mem)
	if err != nil {
		t.Fatal(err)
	}
	fired := 0
	b := New[int, string]("cancel-flow")
	first := b.Step("first", func(ctx context.Context, n int) (int, error) {
		if err := agent.Cancel(ctx, j, "r", "stop"); err != nil {
			t.Errorf("Cancel = %v, want nil (the flow is started and not over)", err)
		}
		return n, nil
	})
	charge := b.Step("charge", func(_ context.Context, n int) (string, error) { fired++; return "charged", nil })
	b.Edge(first, charge)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	out, err := flow.Run(ctx, j, "r", 5)
	st, serr := agent.Status(ctx, j, "r")
	t.Logf("flow.Run = %q, %v; Status = %+v, %v; charge fired %d", out, err, st, serr, fired)
	if fired != 0 {
		t.Errorf("the charge node fired %d times after Cancel returned nil", fired)
	}
	if !errors.Is(err, agent.ErrRunCancelled) {
		t.Errorf("flow.Run = %q, %v; want ErrRunCancelled (Status says %s)", out, err, st.State)
	}
	// A later drive of the cancelled flow returns the output: two readers disagree on the run's end.
	out2, err2 := flow.Run(ctx, j, "r", 5)
	if err2 == nil {
		t.Errorf("re-drive of a cancelled flow = %q, nil; Status = %s", out2, st.State)
	}
}
