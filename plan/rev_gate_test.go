package plan

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// A SingleApproval recorded before the run ever reaches the gate (node keys are predictable)
// lets the node run without any pause and without an approval-request being recorded, so the
// decision was never bound to the input the node ran with.
func TestRev_SingleApprovalBeforeRequest(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, agent.SingleApproval(), nil, &refunds)
	// Approve refuses a decision on a node key with no request yet; written directly (as a
	// journal's writer could), the decision must still not count.
	if err := agent.Approve(ctx, j, "r1", "node:refund", true); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Approve before the request = %v; want ErrConfig", err)
	}
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund", agent.Record{Kind: agent.StepApproval, ToolUseID: "node:refund", Approved: true}); err != nil {
		t.Fatal(err)
	}
	_, err := f.Run(ctx, j, "r1", "order-42")
	recs, _ := j.History(ctx, "r1")
	var names []string
	for _, r := range recs {
		names = append(names, r.Name)
	}
	t.Logf("Run err=%v refunds=%d records=%v", err, refunds, names)
	if refunds != 0 {
		t.Fatalf("gated node ran %d times on an approval recorded before its request (no subject ever shown)", refunds)
	}
	asPending(t, err)
}

// Resuming a paused run under a flow whose gate was removed or loosened must be refused.
func TestRev_ResumeGateRemoved(t *testing.T) {
	ctx := context.Background()
	_, vf := approvers("ops", "finance", "risk")
	for _, tc := range []struct {
		name string
		pol  *agent.ApprovalPolicy
	}{{"loosened", &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops", "finance", "risk"}}}, {"single", agent.SingleApproval()}} {
		j := agenttest.MemJournal()
		n := 0
		f := refundFlow(t, &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}, vf, &n)
		_, err := f.Run(ctx, j, "r1", "order-42")
		asPending(t, err)
		g := refundFlow(t, tc.pol, vf, &n)
		_, err = g.Run(ctx, j, "r1", "order-42")
		t.Logf("%s: %v", tc.name, err)
		if err == nil || !strings.Contains(err.Error(), "") {
			t.Fatalf("%s: resumed", tc.name)
		}
	}
}

// signedDecision is a's decision on the subject (runID, key, name, args) as SubmitDecision records it.
func signedDecision(a edApprover, runID, key, name, args string, approved bool) (agent.Decision, agent.Record) {
	s := agent.ApprovalSubject{RunID: runID, ToolUseID: key, ToolName: name, Args: []byte(args)}
	sig := ed25519.Sign(a.priv, agent.ApprovalDecisionBytes(s, a.id, approved, testDecidedAt))
	d := agent.Decision{RunID: runID, ToolUseID: key, ApproverID: a.id, Approved: approved, DecidedAt: testDecidedAt, Alg: "ed25519", Signature: sig}
	return d, agent.Record{Kind: agent.StepApproval, ToolUseID: key, Approved: approved, ApproverSignature: &agent.ApproverSignature{Approver: a.id, ApproverAlg: "ed25519", DecidedAt: agent.FormatTimestamp(testDecidedAt), Signature: sig}}
}

// An m-of-n decision signed for the right input but recorded before the gate's request does not
// count, and SubmitDecision refuses one: the gate counts only decisions that follow its request.
func TestRev_MofNDecisionBeforeRequest(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if err := agent.SubmitDecision(ctx, j, d); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("SubmitDecision before the request = %v; want ErrConfig", err)
	}
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund:ops:early", rec); err != nil {
		t.Fatal(err)
	}
	_, err := f.Run(ctx, j, "r1", "order-42")
	pend := asPending(t, err)
	if refunds != 0 || pend.Quorum.Approved != 0 {
		t.Fatalf("refunds %d, quorum %+v; want the early decision not counted", refunds, pend.Quorum)
	}
	// The same decision after the request counts.
	if err := agent.SubmitDecision(ctx, j, d); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("after a decision that follows the request: Run = %v, refunds %d", err, refunds)
	}
}

// In a loop, a decision on a later iteration's key recorded before that iteration's request does
// not count, and SubmitDecision refuses it.
func TestRev_LoopDecisionBeforeIterationRequest(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	pays := 0
	f := loopRefund(t, vf, &pays)
	_, err := f.Run(ctx, j, "r1", 2)
	pend := asPending(t, err)
	d1, rec1 := signedDecision(as["ops"], "r1", "node:iter:1:pay", "pay", "1", true)
	if err := agent.SubmitDecision(ctx, j, d1); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("SubmitDecision for iteration 1 before its request = %v; want ErrConfig", err)
	}
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Aiter%3A1%3Apay:ops:early", rec1); err != nil {
		t.Fatal(err)
	}
	as["ops"].decide(t, j, pend, true) // iteration 0
	_, err = f.Run(ctx, j, "r1", 2)
	pend = asPending(t, err)
	if pend.ToolUseID != "node:iter:1:pay" || pays != 1 || pend.Quorum.Approved != 0 {
		t.Fatalf("paused at %s with %d pays, quorum %+v; want iteration 1 waiting, the early decision not counted", pend.ToolUseID, pays, pend.Quorum)
	}
}

// approval-pending is written on any pause that finds it missing, so a crash between the request
// and the note does not hide the wait from a viewer.
func TestRev_PendingWrittenWhenMissing(t *testing.T) {
	ctx := context.Background()
	_, vf := approvers("ops")
	mem := agent.NewMemStore()
	fs := &failingStore{Store: mem, name: agent.ApprovalPendingStep("node:refund")}
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	if _, err := f.Run(ctx, agenttest.MustJournal(fs), "r1", "order-42"); err == nil {
		t.Fatal("no pause")
	}
	j := agenttest.MustJournal(mem)
	if _, ok, _ := j.Get(ctx, "r1", agent.ApprovalPendingStep("node:refund")); ok {
		t.Fatal("the injected failure did not drop the note")
	}
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	if _, ok, _ := j.Get(ctx, "r1", agent.ApprovalPendingStep("node:refund")); !ok {
		t.Fatal("a later pause did not write the missing approval-pending note")
	}
}
