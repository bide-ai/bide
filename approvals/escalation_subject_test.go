package approvals_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journaltest"
)

// The escalation instance of a call (the call's ID and "#taint") is gated by the taint policy's gate, not
// by the tool's own, which the handler's WithGatePolicy names by tool. Until the intake decides it by the
// gate its pause note records, it serves no such subject: a decision for it must not be taken under the
// tool's own gate (here the 2-of-3 policy of "charge"), and nothing is recorded.
func TestEscalationSubject_IsNotServedUnderTheToolsOwnGate(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// A pause note for the escalation instance, as an agent with a taint policy would write it.
	wait, _ := json.Marshal(agent.ApprovalWait{ToolUseID: "c1#taint", ToolName: "charge", Need: 1, Approvers: []string{"alice"}})
	if _, err := journaltest.Put(ctx, r.app, runID, agent.ApprovalPendingStep("c1#taint"), agent.Record{Kind: agent.StepValue, Result: wait}); err != nil {
		t.Fatal(err)
	}
	// The call itself is served, as before.
	if status, out := r.get("/pending?run=" + runID + "&call=c1"); status != http.StatusOK || out["state"] != "waiting" {
		t.Fatalf("the call itself: %d %v", status, out)
	}
	if !agent.IsEscalationSubject("c1#taint") || agent.IsEscalationSubject("c1") {
		t.Fatal("IsEscalationSubject")
	}
	if status, out := r.get("/pending?run=" + runID + "&call=c1%23taint"); status != http.StatusNotFound {
		t.Fatalf("the escalation subject was served: %d %v", status, out)
	}
	before := r.records()
	s := agent.ApprovalSubject{RunID: runID, ToolUseID: "c1#taint", ToolName: "charge", Args: r.subject().Args}
	if status, out := r.post(r.decisionOn(s, "alice", true)); status != http.StatusNotFound {
		t.Fatalf("a decision for the escalation subject: %d %v, want it refused", status, out)
	}
	if r.records() != before {
		t.Fatalf("records %d, want %d: nothing is recorded for the refused subject", r.records(), before)
	}
}
