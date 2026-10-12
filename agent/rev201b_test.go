package agent_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

type rv struct{ pub ed25519.PublicKey }

func (rv) Alg() agent.Alg                { return "ed25519" }
func (v rv) Verify(m, s []byte) bool     { return ed25519.Verify(v.pub, m, s) }
func (v rv) KeyIDs() []string            { return []string{"k:" + string(v.pub[:4])} }

// An m-of-n agent call whose model-chosen ID is node:iter:2:x is decidable via SubmitDecision.
func TestRev201b_AgentIterShapedIDMofN(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	priv := ed25519.NewKeyFromSeed(make([]byte, 32))
	vf := func(id string) (agent.ApproverVerifier, bool) { return rv{priv.Public().(ed25519.PublicKey)}, id == "ops" }
	ran := 0
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { ran++; return "ok", nil },
		agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}))
	newAgent := func() *agent.Agent {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("node:iter:2:x", "refund", `{}`), agenttest.TextTurn("done"))
		return agenttest.MustNew(m, j, agent.WithTools(tool), agent.WithApproverVerifiers(vf))
	}
	_, err := newAgent().Run(ctx, "r", agent.UserText("go"))
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first run: %v", err)
	}
	sig := ed25519.Sign(priv, agent.ApprovalDecisionBytes(pend.Subject(), "ops", true))
	if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "r", ToolUseID: pend.ToolUseID, ApproverID: "ops", Approved: true, Alg: "ed25519", Signature: sig}); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); err != nil || ran != 1 {
		t.Fatalf("after approval: err=%v ran=%d", err, ran)
	}
}

// A decision on a node key in a run with no start record yet (a flow before its first drive) is refused.
func TestRev201b_NoStartYetRefused(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	if err := agent.Approve(ctx, j, "fresh", "node:refund", true); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Approve before any start: %v", err)
	}
	if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "fresh", ToolUseID: "node:iter:2:refund", ApproverID: "ops", Approved: true, Alg: "ed25519", Signature: make([]byte, 64)}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("SubmitDecision before any start: %v", err)
	}
}
