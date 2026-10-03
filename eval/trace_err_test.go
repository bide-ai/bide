package eval_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/eval"
)

// unreadableStore fails every load, as a SQL store does when its connection drops between the run
// and the eval's read. The agent runs over a Journal on the healthy store; the eval reads the same
// runs through a Journal on this wrapper.
type unreadableStore struct{ agent.Store }

var errReadFailed = fmt.Errorf("history unavailable: %w", agent.ErrStorage)

func (unreadableStore) Load(context.Context, string, int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) { yield(agent.Entry{}, errReadFailed) }
}

// A run whose journal cannot be read has no known trajectory. Its trajectory metrics must not pass
// on the empty trajectory (zero steps is within any step limit) nor fail it: the run is unscored for
// them, and the output carries the read failure rather than look like a clean run. The run itself succeeded, so Err stays nil and
// NoError still passes: the failure is the trajectory's, in TraceErr.
func TestAgentRunner_UnreadableJournalFailsTrajectoryMetrics(t *testing.T) {
	mem := agent.NewMemStore()
	a := agenttest.MustNew(&echoModel{}, agenttest.MustJournal(mem))
	run := mustRunner(t, a, agenttest.MustJournal(unreadableStore{mem}), "unreadable")

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
