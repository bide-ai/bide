package eval_test

import (
	"context"
	"sync/atomic"
	"testing"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/eval"
)

// TestRun_StatisticalPassRate confirms the harness reports a pass-rate distribution over runs, not
// a single verdict: a RunFunc that alternates output every call yields a deterministic 50% rate
// over 4 runs (2 of 4), regardless of goroutine scheduling, because the alternation is by an atomic
// counter's parity.
func TestRun_StatisticalPassRate(t *testing.T) {
	var n int64
	stochastic := func(_ context.Context, _ string) eval.RunOutput {
		// even call -> "APPROVED", odd -> "DENIED". Over 4 concurrent calls: exactly 2 of each.
		if atomic.AddInt64(&n, 1)%2 == 0 {
			return eval.RunOutput{Final: agent.UserText("APPROVED")}
		}
		return eval.RunOutput{Final: agent.UserText("DENIED")}
	}
	cases := []eval.Case{{Name: "kyc_case", Input: "applicant 42"}}
	metrics := []eval.Metric{eval.NoError(), eval.Contains("APPROVED")}

	rep := eval.Run(context.Background(), stochastic, cases, metrics, eval.Options{Runs: 4})

	if rep.RunsPerCase != 4 {
		t.Fatalf("runs per case = %d, want 4", rep.RunsPerCase)
	}
	if ne := rep.Overall["no_error"]; ne.Rate != 1.0 {
		t.Fatalf("no_error rate = %v, want 1.0", ne.Rate)
	}
	if ap := rep.Overall["contains:APPROVED"]; ap.Passes != 2 || ap.Rate != 0.5 {
		t.Fatalf("contains:APPROVED = %d/%d rate %v, want 2/4 rate 0.5", ap.Passes, ap.Runs, ap.Rate)
	}
}

// judgeModel is a stub judge that returns a fixed verdict, so the Judge metric is testable offline.
type judgeModel struct{ verdict string }

func (m judgeModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: m.verdict}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

// TestJudge confirms the LLM-as-judge metric passes on a PASS verdict and fails on FAIL.
func TestJudge(t *testing.T) {
	cases := []eval.Case{{Name: "c", Input: "in"}}
	run := func(_ context.Context, _ string) eval.RunOutput {
		return eval.RunOutput{Final: agent.UserText("some answer")}
	}

	pass := eval.Run(context.Background(), run, cases,
		[]eval.Metric{eval.Judge("rubric", judgeModel{"PASS"}, "is it fine")}, eval.Options{Runs: 1})
	if pass.Overall["rubric"].Rate != 1.0 {
		t.Fatalf("judge PASS should score 1.0, got %v", pass.Overall["rubric"].Rate)
	}

	fail := eval.Run(context.Background(), run, cases,
		[]eval.Metric{eval.Judge("rubric", judgeModel{"FAIL: off topic"}, "is it fine")}, eval.Options{Runs: 1})
	if fail.Overall["rubric"].Rate != 0.0 {
		t.Fatalf("judge FAIL should score 0.0, got %v", fail.Overall["rubric"].Rate)
	}
}

// echoModel returns a fixed final answer, so AgentRunner can be tested end to end offline.
type echoModel struct{}

func (echoModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: "done"}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

// TestAgentRunner drives a real Agent through the harness, giving each run a unique runID.
func TestAgentRunner(t *testing.T) {
	a := agent.New(echoModel{}, agent.NewMemStore())
	run := eval.AgentRunner(a, "eval")
	cases := []eval.Case{{Name: "greet", Input: "hi"}, {Name: "ask", Input: "what"}}

	rep := eval.Run(context.Background(), run, cases,
		[]eval.Metric{eval.NoError(), eval.Contains("done")}, eval.Options{Runs: 2, Concurrency: 2})

	if rep.Overall["no_error"].Rate != 1.0 || rep.Overall["contains:done"].Rate != 1.0 {
		t.Fatalf("expected all runs to succeed and contain 'done', got %+v", rep.Overall)
	}
	if len(rep.Cases) != 2 {
		t.Fatalf("expected 2 case reports, got %d", len(rep.Cases))
	}
}
