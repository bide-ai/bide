package plan

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// A decision recorded before the request (by a writer the API no longer allows, e.g. a run
// journaled before the fix) must not leave the gate undecidable: after the pause, Approve must
// take effect, or refuse loudly.
func TestRev201_StaleSingleDecisionBlocksGate(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, agent.SingleApproval(), nil, &refunds)
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund", agent.Record{Kind: agent.StepApproval, ToolUseID: "node:refund", Approved: true}); err != nil {
		t.Fatal(err)
	}
	_, err := f.Run(ctx, j, "r1", "order-42")
	asPending(t, err)
	aerr := agent.Approve(ctx, j, "r1", "node:refund", true)
	_, err = f.Run(ctx, j, "r1", "order-42")
	t.Logf("Approve after pause: %v; Run: %v; refunds=%d", aerr, err, refunds)
	if aerr == nil && refunds == 0 {
		t.Fatalf("Approve returned nil but the gate stays paused forever")
	}
	if !errors.Is(aerr, agent.ErrConfig) || !strings.Contains(aerr.Error(), "recorded before the gate's request") || refunds != 0 {
		t.Fatalf("Approve after a stale decision = %v (refunds %d); want ErrConfig naming it, and the node not run", aerr, refunds)
	}
}

// An m-of-n decision before the request is refused (SubmitDecision), as is a decision on a later
// loop iteration's key before that iteration reaches its gate.
func TestRev201_DecisionsBeforeRequestRefused(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	for _, key := range []string{"node:refund", "node:iter:2:refund"} {
		err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "r1", ToolUseID: key, ApproverID: "ops", Approved: true, DecidedAt: testDecidedAt, Alg: "ed25519", Signature: make([]byte, 64)})
		if !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("SubmitDecision(%s) before the request = %v; want ErrConfig", key, err)
		}
		if err := agent.Approve(ctx, j, "r1", key, true); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("Approve(%s) before the request = %v; want ErrConfig", key, err)
		}
	}
}

// The m-of-n form: a pre-request decision stored under its canonical name (ed25519 signatures are
// deterministic) takes the name of the approver's identical decision after the pause, so that one
// is not recorded and SubmitDecision says so (ErrConfig), pointing the approver at re-signing at a
// new decided_at, which counts (TestReview_StaleSeatNotLostUnderV3).
func TestRev201_StaleMofNDecisionBlocksGate(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if _, err := journaltest.Put(ctx, j, "r1", agent.DecisionStep(d), rec); err != nil {
		t.Fatal(err)
	}
	_, err := f.Run(ctx, j, "r1", "order-42")
	pend := asPending(t, err)
	serr := agent.SubmitDecision(ctx, j, d)
	_, err = f.Run(ctx, j, "r1", "order-42")
	t.Logf("subject args %s; SubmitDecision: %v; Run: %v; refunds=%d", pend.Args, serr, err, refunds)
	if serr == nil && refunds == 0 {
		t.Fatalf("SubmitDecision returned nil but its decision never counts")
	}
	if !errors.Is(serr, agent.ErrConfig) || !strings.Contains(serr.Error(), "sign the decision again at a new decided_at") {
		t.Fatalf("SubmitDecision after a stale decision = %v; want ErrConfig saying to re-sign", serr)
	}
}

// With a stale decision on record for one approver, the gate can still pass with the others.
func TestRev201_StaleSeatOthersCanPass(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops", "finance")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops", "finance"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	_, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund:ops:stale", rec); err != nil {
		t.Fatal(err)
	}
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	as["finance"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("Run with finance's approval = %v, refunds %d; want the node run once", err, refunds)
	}
}

// A stale decision under the previous, signature-derived name still takes the name of the
// approver's identical decision after the pause, as before the upgrade: the identical resubmission
// is not recorded and SubmitDecision says so; re-signing at a new decided_at counts.
func TestRev201_LegacyNamedStaleDecisionStillBlocksTheIdenticalOne(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if _, err := journaltest.Put(ctx, j, "r1", legacyDecisionStep(d), rec); err != nil {
		t.Fatal(err)
	}
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	if err := agent.SubmitDecision(ctx, j, d); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("SubmitDecision of the identical decision = %v; want ErrConfig", err)
	}
}

// legacyDecisionStep is the signature-derived name journals written before the decision step was
// keyed by identity hold their decisions under.
func legacyDecisionStep(d agent.Decision) string {
	h := sha256.New()
	h.Write([]byte{1})
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(d.Alg))))
	h.Write([]byte(d.Alg))
	at := agent.FormatTimestamp(d.DecidedAt)
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(at))))
	h.Write([]byte(at))
	h.Write(d.Signature)
	return "approval:" + url.QueryEscape(d.ToolUseID) + ":" + d.ApproverID + ":" + hex.EncodeToString(h.Sum(nil))
}

// In a flow gate a decision journaled before the request is held, but not verified: it cannot count.
func TestRev201_HeldDecisionStepFlagsAPreRequestRecord(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", `{"order":"order-42","amount":120}`, true)
	if _, err := journaltest.Put(ctx, j, "r1", agent.DecisionStep(d), rec); err != nil {
		t.Fatal(err)
	}
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	_, held, verified, err := agent.HeldDecisionStep(ctx, j, d, vf)
	if err != nil || !held || verified {
		t.Fatalf("held %v verified %v %v; want held and not verified", held, verified, err)
	}
}
