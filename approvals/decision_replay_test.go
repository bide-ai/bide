package approvals_test

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
)

// R1: findDecision's first step matches a record by bytes alone, with no verification. A request
// whose own signature is junk, equal to a junk record already journaled, is answered 200 with a
// receipt built from the junk record instead of being refused as invalid.
func TestJunkReplayedByItsBytesIsRefused(t *testing.T) {
	r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	at := time.Date(2026, 10, 13, 12, 0, 1, 0, time.UTC)
	junk := make([]byte, 64)
	if err := agent.SubmitDecision(context.Background(), r.app, agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: "alice",
		Approved: true, DecidedAt: at, Alg: audit.AlgEd25519, Signature: junk}); err != nil {
		t.Fatal(err)
	}
	b := r.decision("alice", true)
	b.Signature = base64.StdEncoding.EncodeToString(junk)
	status, out := r.post(b)
	if status == 200 {
		t.Fatalf("a junk signature is answered 200: counted=%v reason=%v receipt=%v", out["counted"], out["reason"], out["receipt"] != nil)
	}
}
