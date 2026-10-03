package run

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

type answer struct{ X int }

func checkRun(t *testing.T, a *agent.Agent, s *agent.Session) {
	ctx := context.Background()
	out, err := a.Run(ctx, "r1", "hi")
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Text()

	msg, err := a.RunSaga(ctx, "r2", "book")
	if err == nil || msg.Text() != "" {
		t.Fatal(msg)
	}

	if _, err := a.RunResult(ctx, "r3", "x"); err != nil {
		t.Fatal(err)
	}
	res, err := a.RunMessage(ctx, "r4", agent.UserText("y"), agent.WithSaga())
	_, _ = res, err
	_, _ = a.ResumeRun(ctx, "r4")

	st := a.StreamSaga(ctx, "r5", "z")
	var events []agent.AgentEvent
	for e := range st.Events() {
		events = append(events, e)
	}
	final, err := st.Final()
	_, _ = final, err

	turn, err := s.Send(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	_ = turn
	_, _ = s.SendMessage(ctx, agent.UserText("again"))

	v, err := agent.RunTyped[answer](ctx, a, "r6", "q")
	_, _ = v, err
	w, err := agent.RunTypedNative[*answer](ctx, a, "r7", "q")
	_, _ = w, err
	x, _, err := a.RunTypedMessage[answer](ctx, "r8", agent.UserText("q"))
	_, _ = x, err

	m, err := audit.Record(&audit.EventLog{}, a.Stream(ctx, "r9", "s"), nil)
	_, _ = m, err
}

func typed(ctx context.Context, a *agent.Agent) (answer, error) {
	return agent.RunTyped[answer](ctx, a, "r", "q")
}
