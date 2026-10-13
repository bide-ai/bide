package plan

// Adversarial review of #244 (decision times). The test reproduces a finding and fails on the
// reviewed branch.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// TestReview_StaleSeatNotLostUnderV3: SubmitDecision, the design doc (design-mofn-approval.md),
// plan-flows.md and SubmitDecision's godoc say that when an approver's identical decision was
// journaled before a flow gate's request, "their seat is lost for this gate". Under v3 the
// decision's journal name hashes decided_at, so the approver re-signs at another time, the new
// record gets a new name, and it counts: the seat is not lost. The error tells the operator to
// give up on (or cancel) a gate the approver can still pass alone.
func TestReview_StaleSeatNotLostUnderV3(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	h := sha256.New()
	h.Write([]byte{1})
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len("ed25519"))))
	h.Write([]byte("ed25519"))
	at := agent.FormatTimestamp(d.DecidedAt)
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(at))))
	h.Write([]byte(at))
	h.Write(d.Signature)
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund:ops:"+hex.EncodeToString(h.Sum(nil)), rec); err != nil {
		t.Fatal(err)
	}
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	serr := agent.SubmitDecision(ctx, j, d)
	if serr == nil || !strings.Contains(serr.Error(), "seat is lost") {
		t.Fatalf("SubmitDecision = %v; want the stale-decision error", serr)
	}
	// ops re-signs the same approval a second later.
	later := d.DecidedAt.Add(time.Second)
	d.DecidedAt = later
	d.Signature = ed25519.Sign(as["ops"].priv, agent.ApprovalDecisionBytes(pend.Subject(), "ops", true, later))
	if err := agent.SubmitDecision(ctx, j, d); err != nil {
		t.Fatalf("re-signed SubmitDecision: %v", err)
	}
	if _, err := f.Run(ctx, j, "r1", "order-42"); err == nil && refunds == 1 {
		t.Fatalf("the error said %q, but ops passed the gate alone by re-signing at another time", serr)
	}
}

// TestReview_DecisionCheckCountsEarlyRecord: WithDecisionCheck's "already decided" check reads
// every decision on the call, including one journaled before a flow gate's request, which the
// gate never counts. So ops's re-signed approval (which, without the check, is recorded and
// passes the gate, see TestReview_StaleSeatNotLostUnderV3) is refused with ErrAlreadyDecided,
// "the approver's earlier valid decision already counts", when it does not count. The pre-flight
// and the gate disagree about which decisions count.
func TestReview_DecisionCheckCountsEarlyRecord(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund:ops:early", rec); err != nil {
		t.Fatal(err)
	}
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	later := d.DecidedAt.Add(time.Second)
	d.DecidedAt = later
	d.Signature = ed25519.Sign(as["ops"].priv, agent.ApprovalDecisionBytes(pend.Subject(), "ops", true, later))
	if err := agent.SubmitDecision(ctx, j, d, agent.WithDecisionCheck(vf)); err != nil {
		t.Fatalf("SubmitDecision with WithDecisionCheck = %v; but the early decision does not count, and the same call without the check passes the gate", err)
	}
}
