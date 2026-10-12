package agent_test

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// An agent tool call whose model-chosen ID has the shape of a plan node key must stay decidable.
func TestRev201_AgentCallIDLooksLikeNodeKey(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	ran := 0
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { ran++; return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	newAgent := func() *agent.Agent {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("node:x", "refund", `{}`), agenttest.TextTurn("done"))
		return agenttest.MustNew(m, j, agent.WithTools(tool))
	}
	_, err := newAgent().Run(ctx, "r", agent.UserText("go"))
	t.Logf("first run: %v", err)
	if err := agent.Approve(ctx, j, "r", "node:x", true); err != nil {
		t.Fatalf("Approve on an agent call with ID node:x: %v", err)
	}
	if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); err != nil || ran != 1 {
		t.Fatalf("run after approval: err=%v ran=%d", err, ran)
	}
}
