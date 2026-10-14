package plan

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// REVIEW PROOF 5: Conform reports a flow run whose gate re-asked over a weak tally
// (approval-tally:<key>:sod) as diverging: it trims only the ":v3" suffix.
func TestReview_SoDFlow_ConformAcceptsSoDRetally(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers(opsFinRisk...)
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, sodGate(2), vf, &refunds)
	_, err := f.Run(ctx, j, "r1", "order-42", asRequester("ops"))
	pend := asPending(t, err)
	as["ops"].decide(t, j, pend, true)
	as["finance"].decide(t, j, pend, true)
	weak, _ := json.Marshal(agent.ApprovalTally{Need: 2, Approvers: opsFinRisk, Approved: 2, ApprovedBy: []string{"ops", "finance"}, DecisionFormat: agent.DecisionFormat})
	if _, err := journaltest.Put(ctx, j, "r1", agent.ApprovalTallyStep("node:refund"), agent.Record{Kind: agent.StepValue, Result: weak}); err != nil {
		t.Fatal(err)
	}
	_, err = f.Run(ctx, j, "r1", "order-42")
	pend = asPending(t, err)
	as["risk"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil {
		t.Fatal(err)
	}
	ok, diffs, err := f.Conform(ctx, j, "r1")
	if err != nil || !ok {
		t.Fatalf("Conform = %v %v %v", ok, diffs, err)
	}
}

// REVIEW PROOF 6: Conform rejects the run:principal record of any flow run that has a principal.
func TestReview_SoDFlow_ConformAcceptsRunPrincipal(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers(opsFinRisk...)
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, sodGate(2), vf, &refunds)
	_, err := f.Run(ctx, j, "r2", "order-42", asRequester("ops"))
	pend := asPending(t, err)
	as["finance"].decide(t, j, pend, true)
	as["risk"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r2", "order-42"); err != nil {
		t.Fatal(err)
	}
	ok, diffs, err := f.Conform(ctx, j, "r2")
	if err != nil || !ok {
		t.Fatalf("Conform = %v %v %v", ok, diffs, err)
	}
}
