package eval_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/eval"
)

// TestRun_StatisticalPassRate confirms the harness reports a pass-rate distribution with a Wilson
// confidence interval: a RunFunc that alternates output yields a deterministic 50% over 4 runs, and
// the 95% CI for 2/4 is wide (roughly [15%, 85%]), which is the point of reporting intervals.
func TestRun_StatisticalPassRate(t *testing.T) {
	var n int64
	stochastic := func(_ context.Context, _ string) eval.RunOutput {
		if atomic.AddInt64(&n, 1)%2 == 0 {
			return eval.RunOutput{Final: agent.UserText("APPROVED")}
		}
		return eval.RunOutput{Final: agent.UserText("DENIED")}
	}
	cases := []eval.Case{{Name: "kyc_case", Input: "applicant 42"}}
	metrics := []eval.Metric{eval.NoError(), eval.Contains("APPROVED")}

	rep := mustRun(t, context.Background(), stochastic, cases, metrics, eval.Options{Runs: 4})

	if rep.RunsPerCase != 4 || rep.TotalRuns != 4 {
		t.Fatalf("runs=%d total=%d, want 4,4", rep.RunsPerCase, rep.TotalRuns)
	}
	ap := rep.Overall["contains:APPROVED"]
	if ap.Passes != 2 || ap.Rate != 0.5 {
		t.Fatalf("contains:APPROVED = %d/%d rate %v, want 2/4 rate 0.5", ap.Passes, ap.Scored, ap.Rate)
	}
	if !(ap.CILow < 0.25 && ap.CIHigh > 0.75) {
		t.Fatalf("expected a wide 95%% CI for 2/4, got [%.2f,%.2f]", ap.CILow, ap.CIHigh)
	}
	if rep.Overall["no_error"].Rate != 1.0 {
		t.Fatalf("no_error rate = %v, want 1.0", rep.Overall["no_error"].Rate)
	}
}

// Reports key metrics by name, so two metrics sharing a name would leave one's results in the
// report under the other's name and drop the rest. Run refuses the metric set instead.
func TestRun_DuplicateMetricNameIsAnError(t *testing.T) {
	run := func(_ context.Context, _ string) eval.RunOutput { return eval.RunOutput{Final: agent.UserText("ok")} }
	always := eval.Custom("ok", func(context.Context, eval.Case, eval.RunOutput) (bool, error) { return true, nil })
	never := eval.Custom("ok", func(context.Context, eval.Case, eval.RunOutput) (bool, error) { return false, nil })
	rep, err := eval.Run(context.Background(), run, []eval.Case{{Name: "c", Input: "x"}},
		[]eval.Metric{always, never}, eval.Options{Runs: 2})
	if err == nil {
		t.Fatalf("two metrics named %q ran; the report keeps one: %+v", "ok", rep.Overall)
	}
}

// judgeModel is a stub judge returning a fixed verdict so the Judge metric is testable offline.
type judgeModel struct{ verdict string }

func (m judgeModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: m.verdict}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

func TestJudge(t *testing.T) {
	cases := []eval.Case{{Name: "c", Input: "in"}}
	run := func(_ context.Context, _ string) eval.RunOutput {
		return eval.RunOutput{Final: agent.UserText("some answer")}
	}
	pass := mustRun(t, context.Background(), run, cases,
		[]eval.Metric{eval.Judge("rubric", judgeModel{"PASS"}, "is it fine")}, eval.Options{Runs: 1})
	if pass.Overall["rubric"].Rate != 1.0 {
		t.Fatalf("judge PASS should score 1.0, got %v", pass.Overall["rubric"].Rate)
	}
	fail := mustRun(t, context.Background(), run, cases,
		[]eval.Metric{eval.Judge("rubric", judgeModel{"FAIL: off topic"}, "is it fine")}, eval.Options{Runs: 1})
	if fail.Overall["rubric"].Rate != 0.0 {
		t.Fatalf("judge FAIL should score 0.0, got %v", fail.Overall["rubric"].Rate)
	}
}

// toolModel calls the "lookup" tool on the first turn, then answers, so the run has a two-step
// trajectory with one tool call, exercising the trajectory metrics.
type toolModel struct{}

func (toolModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	if len(req.Messages) <= 1 {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "done"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// TestTrajectoryMetrics drives a real Agent with a tool and confirms the harness scores the agent's
// behavior (tool called, step count) from the journal, not just the final text.
func TestTrajectoryMetrics(t *testing.T) {
	store := agenttest.MemJournal()
	lookup := agent.MustFunc("lookup", "look something up", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	a := agenttest.MustNew(toolModel{}, store, agent.WithTools(lookup))
	run := mustRunner(t, a, store, "traj")
	cases := []eval.Case{{Name: "with_tool", Input: "go"}}

	rep := mustRun(t, context.Background(), run, cases, []eval.Metric{
		eval.NoError(),
		eval.CalledTool("lookup"),
		eval.MaxSteps(2),
		eval.MaxSteps(1),
		eval.ToolOrder("lookup"),
	}, eval.Options{Runs: 3, Concurrency: 2})

	if rep.Overall["no_error"].Rate != 1.0 {
		t.Fatalf("expected no errors, got %v", rep.Overall["no_error"].Rate)
	}
	if rep.Overall["called:lookup"].Rate != 1.0 {
		t.Fatalf("expected the lookup tool to be called every run, got %v", rep.Overall["called:lookup"].Rate)
	}
	if rep.Overall["max_steps:2"].Rate != 1.0 {
		t.Fatalf("expected <=2 steps every run, got %v", rep.Overall["max_steps:2"].Rate)
	}
	if rep.Overall["max_steps:1"].Rate != 0.0 {
		t.Fatalf("run took 2 turns, so max_steps:1 should fail every run, got %v", rep.Overall["max_steps:1"].Rate)
	}
	if rep.Overall["tool_order:lookup"].Rate != 1.0 {
		t.Fatalf("expected tool_order:lookup to hold, got %v", rep.Overall["tool_order:lookup"].Rate)
	}
}

// mustRun is eval.Run for a context that is never cancelled.
func mustRun(t testing.TB, ctx context.Context, run eval.RunFunc, cases []eval.Case, metrics []eval.Metric, opts eval.Options) eval.Report {
	t.Helper()
	rep, err := eval.Run(ctx, run, cases, metrics, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// mustRunner is eval.AgentRunner for a non-nil agent and store.
func mustRunner(t testing.TB, a *agent.Agent, store *agent.Journal, prefix string) eval.RunFunc {
	t.Helper()
	run, err := eval.AgentRunner(a, store, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// mustCompare is eval.Compare for two reports that Run returned.
func mustCompare(t testing.TB, old, new eval.Report) eval.Comparison {
	t.Helper()
	cmp, err := eval.Compare(old, new)
	if err != nil {
		t.Fatal(err)
	}
	return cmp
}
