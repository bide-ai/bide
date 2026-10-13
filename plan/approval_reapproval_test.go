package plan

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// A plan flow gate whose recorded tally passed on decisions that predate bide.approval.v3 (no
// decision format) does not run its node on that tally: it asks again (PendingReapproval), and
// once the approver signs under v3 the node runs once.
func TestFlowGate_PreV3PassedTallyReapproval(t *testing.T) {
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
	if pend.Reason != agent.PendingReapproval || refunds != 0 {
		t.Fatalf("drive on a pre-v3 passed tally: reason %q, refunds %d; want a re-approval pause and no refund", pend.Reason, refunds)
	}
	as["ops"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("drive after v3 re-approval = %v, refunds %d; want the node run once", err, refunds)
	}
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("a later drive = %v, refunds %d", err, refunds)
	}
}
