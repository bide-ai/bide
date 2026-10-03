package construct

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func tool() agent.Tool { return nil }

func TestConstruct(t *testing.T) {
	m := agent.NewScriptedModel(agent.TextTurn("done"))
	a := agent.New(m, agent.NewMemStore(), tool())
	_ = a

	b := agent.New(m, agent.NewMemStore()).WithMaxTurns(3).Use(nil).WithSystemPromptFunc(func(ctx context.Context) string { return "p" })
	_ = b

	c := agent.New(m, agent.NewMemStore(), tool(), tool())
	c.SetMaxConcurrency(-1)
	c.WithSystemPrompt("sys")
	_ = c

	limit := 7
	e := agent.New(m, agent.NewMemStore())
	_ = limit + 1
	e.WithMaxTurns(limit) // a variable nothing changes between: folded
	_ = e

	j, _ := agent.NewJournal(agent.NewMemStore())
	d, err := agent.Build(m, j, agent.WithMaxTurns(2))
	_, _ = d, err
}

func helper(n int) *agent.Agent {
	return agent.New(agent.NewScriptedModel(), agent.NewMemStore()).WithMaxTurns(n)
}
