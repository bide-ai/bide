package eval_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/eval"
)

// unreadableJournal serves the agent's own read of a run's history, then fails every later read of
// that run, as a SQL store does when its connection drops between the run and the eval's read.
type unreadableJournal struct {
	agent.Durable
	reads sync.Map // runID -> *atomic.Int32
}

var errReadFailed = fmt.Errorf("history unavailable: %w", agent.ErrStorage)

func (j *unreadableJournal) History(ctx context.Context, runID string) ([]agent.Record, error) {
	n, _ := j.reads.LoadOrStore(runID, new(atomic.Int32))
	if n.(*atomic.Int32).Add(1) > 1 {
		return nil, errReadFailed
	}
	return j.Durable.History(ctx, runID)
}

// A run whose journal cannot be read has no known trajectory. Its trajectory metrics must not pass
// on the empty trajectory (zero steps is within any step limit), and the output must carry the read
// failure rather than look like a clean run.
func TestAgentRunner_UnreadableJournalFailsTrajectoryMetrics(t *testing.T) {
	store := &unreadableJournal{Durable: agent.NewMemStore()}
	a := agent.New(&echoModel{}, store)
	run := eval.AgentRunner(a, store, "unreadable")

	out := run(context.Background(), "hello")
	if !errors.Is(out.Err, errReadFailed) || !errors.Is(out.Err, agent.ErrStorage) {
		t.Fatalf("out.Err = %v, want the journal read failure", out.Err)
	}

	rep := mustRun(t, context.Background(), run, []eval.Case{{Name: "c", Input: "hello"}}, []eval.Metric{
		eval.NoError(),
		eval.MaxSteps(5),
		eval.ToolOrder(),
	}, eval.Options{Runs: 2})
	for _, name := range []string{"no_error", "max_steps:5", "tool_order:"} {
		if s := rep.Overall[name]; s.Passes != 0 {
			t.Errorf("%s passed %d of %d runs whose trajectory was never read", name, s.Passes, s.Runs)
		}
	}
}
