package runview

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A signed decision the gate's recorded expiry did not read (its write landed after the expiry) is
// shown with agent.ReasonAfterExpiry, so the page does not show an on-time approval next to an
// expiry with no explanation.
func TestDecisionWrittenAfterExpiryIsLabelled(t *testing.T) {
	tally, _ := json.Marshal(agent.ApprovalTally{Need: 1, Approvers: []string{"bob"}, Expired: true})
	recs := []agent.Record{
		{Name: "@llm/0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "refund", Args: json.RawMessage(`{}`)}}}},
		{Name: agent.ApprovalTallyStep("c1"), Kind: agent.StepValue, Result: tally},
		{Name: "approval:c1:bob:late", Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{Approver: "bob", ApproverAlg: "ed25519", Signature: []byte{1}, DecidedAt: "2026-10-15T09:59:59.999Z", RecordedAt: "2026-10-15T10:00:00.500Z"}},
	}
	v := Build(context.Background(), "r1", recs, nil)
	if len(v.Gates) != 1 || len(v.Gates[0].Decisions) != 1 || v.Gates[0].Decisions[0].Note != agent.ReasonAfterExpiry {
		t.Fatalf("gates = %+v; want bob's decision labelled %q", v.Gates, agent.ReasonAfterExpiry)
	}
}
