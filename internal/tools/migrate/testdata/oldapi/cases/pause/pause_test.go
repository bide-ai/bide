package pause

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

type waker struct{}

func (waker) Wake() {}

func checkPause(t *testing.T, a *agent.Agent) {
	ctx := context.Background()
	j, _ := agent.NewJournal(agent.NewMemStore())
	_, err := a.Run(agent.ContextWithWaker(ctx, waker{}), "r", "go")
	var p *agent.PendingApproval
	if errors.As(err, &p) {
		t.Log(p)
	}
	if _, ok := errors.AsType[*agent.ResumeHalt](err); ok {
		_ = agent.ResolveHalt(ctx, j, "r", "c1", "ok", false)
		_ = agent.ResolveStepHalt(ctx, j, "r", "charge", nil, true)
		_ = agent.ResolveHaltRef(ctx, j, agent.HaltRef{RunID: "r"}, agent.Outcome{})
	}
	_ = agent.Signal(ctx, j, "r", "go", 1)
	_ = agent.Send(ctx, j, "r", "ch", "k", "v")
	_ = agent.Resume(ctx, j, "r", "q", "a")
	_ = agent.AnswerInterrupt[string](ctx, j, "r", "q", "a")
	n, err := agent.Step(ctx, j, "r", "count", func(context.Context) (int, error) { return 1, nil })
	_, _ = n, err
}
