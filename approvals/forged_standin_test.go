package approvals_test

import (
	"context"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
)

// A record that does not verify, with the same approver, verdict, scheme and decided_at, must not
// stand in for the approver's genuine decision ("a bad record never locks an approver out").
func TestDecision_AForgedRecordDoesNotStandInForTheGenuineDecision(t *testing.T) {
	r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	at := time.Date(2026, 10, 13, 12, 0, 1, 0, time.UTC)
	// Recorded without a decision check (the agent API allows it; the gate ignores it as a bad signature).
	if err := agent.SubmitDecision(context.Background(), r.app, agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: "alice",
		Approved: true, DecidedAt: at, Alg: audit.AlgEd25519, Signature: make([]byte, 64)}); err != nil {
		t.Fatal(err)
	}
	status, out := r.post(r.decision("alice", true))
	t.Logf("status %d: recorded=%v counted=%v reason=%v record=%v", status, out["recorded"], out["counted"], out["reason"], out["record"])
	if out["counted"] != true {
		t.Fatalf("the genuine decision was not recorded or counted: the forged record stood in for it")
	}
}

// A replay of a recorded decision is found by its bytes, whatever name its record has, so it replays as itself:
// not written again, found by its bytes, and counted.
func TestDecision_AReplayIsFoundByItsBytes(t *testing.T) {
	r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	first := r.decision("alice", true)
	if status, out := r.post(first); status != 200 || out["recorded"] != true {
		t.Fatalf("first: %d %v", status, out)
	}
	status, out := r.post(first)
	if status != 200 || out["recorded"] != false || out["counted"] != true {
		t.Fatalf("replay: %d %v", status, out)
	}
}
