package eval_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/eval"
)

// echoModel answers "echo: <input>" and counts its calls.
type echoModel struct{ calls atomic.Int32 }

func (m *echoModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	m.calls.Add(1)
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: "echo: " + req.Messages[len(req.Messages)-1].Text()}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

// answersItsInput passes when a run's answer is to that run's own case.
var answersItsInput = eval.Metric{Name: "answers-its-input", Fn: func(_ context.Context, c eval.Case, out eval.RunOutput) (bool, error) {
	return out.Err == nil && out.Final.Text() == "echo: "+c.Input, nil
}}

// A second evaluation against the same durable store, as with a SQLite store kept between eval
// runs, must sample the model again and score each answer against its own case.
func TestAgentRunner_EvalsAreIndependentOverOneStore(t *testing.T) {
	store := agenttest.MemJournal()
	cases := []eval.Case{{Name: "a", Input: "alpha"}, {Name: "b", Input: "beta"}}
	opts := eval.Options{Runs: 3, Concurrency: 1}

	first := &echoModel{}
	mustRun(t, context.Background(), mustRunner(t, agenttest.MustNew(first, store), store, "sentiment"), cases, []eval.Metric{answersItsInput}, opts)

	second := &echoModel{}
	rep := mustRun(t, context.Background(), mustRunner(t, agenttest.MustNew(second, store), store, "sentiment"), cases, []eval.Metric{answersItsInput}, opts)
	if n := second.calls.Load(); n != 6 {
		t.Errorf("the second eval called the model %d times, want 6 (it replayed the first eval's recorded answers)", n)
	}
	if s := rep.Overall["answers-its-input"]; s.Passes != s.Scored || s.Unscored != 0 {
		t.Errorf("%d of %d runs were scored against another case's answer:\n%s", s.Scored-s.Passes, s.Scored, strings.TrimSpace(rep.String()))
	}
}
