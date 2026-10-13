package plan

// Adversarial review of #244 (decision times). The test reproduces a finding and fails on the
// reviewed branch.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// TestReview_StaleSeatNotLostUnderV3: when an approver's identical decision was journaled before a
// flow gate's request, SubmitDecision (and its godoc, design-mofn-approval.md, plan-flows.md and
// Flows.tla) said "their seat is lost for this gate". Under v3 the decision's journal name hashes
// decided_at, so the approver re-signs at another time, the new record gets a new name, and it
// counts: the seat is not lost. The error must not tell the operator to give up on (or cancel) a
// gate the approver can still pass alone, and the re-signed decision must pass the gate.
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
	// The identical decision: it cannot be recorded (its name is taken), and must be reported, but
	// not as a lost seat.
	serr := agent.SubmitDecision(ctx, j, d)
	if !errors.Is(serr, agent.ErrConfig) {
		t.Fatalf("SubmitDecision of the identical early decision = %v; want ErrConfig", serr)
	}
	if strings.Contains(serr.Error(), "seat is lost") {
		t.Errorf("SubmitDecision says %q, but under v3 the approver can sign again at another time", serr)
	}
	// ops re-signs the same approval a second later.
	later := d.DecidedAt.Add(time.Second)
	d.DecidedAt = later
	d.Signature = ed25519.Sign(as["ops"].priv, agent.ApprovalDecisionBytes(pend.Subject(), "ops", true, later))
	if err := agent.SubmitDecision(ctx, j, d); err != nil {
		t.Fatalf("re-signed SubmitDecision: %v", err)
	}
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("Run after the re-signed approval = %v, refunds %d; want the node run once", err, refunds)
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

// TestReview_ConformRejectsRetally (re-attack): a flow run whose gate asked again under v3 records
// its outcome under agent.ApprovalRetallyStep ("approval-tally:<enc>:v3"). Flow.Conform maps an
// "approval-tally:" record to its gate by cutting at the first ':' and looking the rest up as an
// encoded node key, so "<enc>:v3" matches no gate and a correct re-approved run is reported as
// not conforming ("approval record of a gate the run never reached").
func TestReview_ConformRejectsRetally(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	old, _ := json.Marshal(agent.ApprovalTally{Need: 1, Approvers: []string{"ops"}, Approved: 1, ApprovedBy: []string{"ops"}})
	if _, err := journaltest.Put(ctx, j, "r1", agent.ApprovalTallyStep("node:refund"), agent.Record{Kind: agent.StepValue, Result: old}); err != nil {
		t.Fatal(err)
	}
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	as["ops"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("drive after v3 re-approval = %v, refunds %d", err, refunds)
	}
	ok, diffs, err := f.Conform(ctx, j, "r1")
	if err != nil || !ok {
		t.Fatalf("Conform of a re-approved run = %v, %q, %v; want it to conform", ok, diffs, err)
	}
}
