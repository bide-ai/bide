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

// Conform maps a certificate-bound gate's records (the identity tallies, identity records, exec holds,
// phase and re-ask notes) to their gate by the encoded key, whatever suffix they carry; those of a gate
// the run never reached diverge.
func TestConform_CertificateBoundGateRecords(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers(opsFinRisk...)
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, sodGate(2), vf, &refunds)
	_, err := f.Run(ctx, j, "r3", "order-42", asRequester("ops"))
	pend := asPending(t, err)
	as["finance"].decide(t, j, pend, true)
	as["risk"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r3", "order-42"); err != nil {
		t.Fatal(err)
	}
	put := func(name string) {
		t.Helper()
		if _, err := journaltest.Put(ctx, j, "r3", name, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"node:refund"} {
		for _, name := range []string{
			agent.ApprovalIdentTallyStep(key, 0), agent.ApprovalIdentTallyStep(key, 2), agent.ApprovalIdentityStep(key, 1),
			agent.ApprovalExecHoldStep(key, 0), agent.ApprovalPauseStep(key, 3), agent.ApprovalIdentReaskStep(key, 1),
		} {
			put(name)
		}
	}
	ok, diffs, err := f.Conform(ctx, j, "r3")
	if err != nil || !ok {
		t.Fatalf("Conform = %v %v %v", ok, diffs, err)
	}
	for _, name := range []string{agent.ApprovalIdentTallyStep("node:ghost", 1), agent.ApprovalExecHoldStep("node:ghost", 0), agent.ApprovalIdentityStep("node:ghost", 0)} {
		put(name)
	}
	ok, diffs, err = f.Conform(ctx, j, "r3")
	if err != nil || ok || len(diffs) != 3 {
		t.Fatalf("Conform = %v %v %v", ok, diffs, err)
	}
}
