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

// unreadableJournal serves the agent's own three reads of a run's history (its Load, its Load again
// once run:start is written, and its read-back of the end markers after run:complete), then fails
// every later read of that run, as a SQL store does when its connection drops between the run and
// the eval's read.
type unreadableJournal struct {
	agent.Durable
	reads sync.Map // runID -> *atomic.Int32
}

var errReadFailed = fmt.Errorf("history unavailable: %w", agent.ErrStorage)

func (j *unreadableJournal) History(ctx context.Context, runID string) ([]agent.Record, error) {
	n, _ := j.reads.LoadOrStore(runID, new(atomic.Int32))
	if n.(*atomic.Int32).Add(1) > 3 {
		return nil, errReadFailed
	}
	return j.Durable.History(ctx, runID)
}

// A run whose journal cannot be read has no known trajectory. Its trajectory metrics must not pass
// on the empty trajectory (zero steps is within any step limit) nor fail it: the run is unscored for
// them, and the output carries the read failure rather than look like a clean run. The run itself succeeded, so Err stays nil and
// NoError still passes: the failure is the trajectory's, in TraceErr.
func TestAgentRunner_UnreadableJournalFailsTrajectoryMetrics(t *testing.T) {
	store := &unreadableJournal{Durable: agent.NewMemStore()}
	a := agent.New(&echoModel{}, store)
	run := mustRunner(t, a, store, "unreadable")

	out := run(context.Background(), "hello")
	if out.Err != nil {
		t.Fatalf("out.Err = %v, want nil: the run itself succeeded", out.Err)
	}
	if !errors.Is(out.TraceErr, errReadFailed) || !errors.Is(out.TraceErr, agent.ErrStorage) {
		t.Fatalf("out.TraceErr = %v, want the journal read failure", out.TraceErr)
	}

	rep := mustRun(t, context.Background(), run, []eval.Case{{Name: "c", Input: "hello"}}, []eval.Metric{
		eval.NoError(),
		eval.MaxSteps(5),
		eval.ToolOrder(),
		eval.CalledTool("lookup"),
	}, eval.Options{Runs: 2})
	if s := rep.Overall["no_error"]; s.Passes != 2 || s.Scored != 2 || s.Unscored != 0 {
		t.Errorf("no_error = %+v, want 2 of 2 scored runs passed: each run succeeded", s)
	}
	for _, name := range []string{"max_steps:5", "tool_order:", "called:lookup"} {
		if s := rep.Overall[name]; s.Passes != 0 || s.Scored != 0 || s.Unscored != 2 {
			t.Errorf("%s = %+v, want both runs unscored: their trajectory was never read", name, s)
		}
	}
}

// A trajectory metric leaves unscored any output whose TraceErr is set, even when a RunFunc also
// filled in a Trace that would otherwise pass it.
func TestTrajectoryMetrics_UnscoredOnTraceErr(t *testing.T) {
	trace := eval.Trajectory{Steps: 1, ToolCalls: []string{"lookup"}}
	for _, m := range []eval.Metric{eval.CalledTool("lookup"), eval.MaxSteps(1), eval.ToolOrder("lookup")} {
		if pass, err := m.Fn(context.Background(), eval.Case{}, eval.RunOutput{Trace: trace}); !pass || err != nil {
			t.Fatalf("%s = (%v, %v) on a trajectory it should pass", m.Name, pass, err)
		}
		if _, err := m.Fn(context.Background(), eval.Case{}, eval.RunOutput{Trace: trace, TraceErr: errReadFailed}); !errors.Is(err, errReadFailed) {
			t.Errorf("%s scored a run whose trajectory was not read (err %v)", m.Name, err)
		}
	}
}
