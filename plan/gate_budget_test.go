package plan

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// The gate's store budget (docs/design/api-v1.md, the counting-store table): a node with no gate
// costs nothing; a gate's first pause costs 1 Get (the tally), 2 Inserts (the request and the
// pause note) and 1 Load (the decisions); a pause again the same (both Inserts find their record,
// and the note is written if a crash lost it); the evaluation that reaches the outcome adds the tally's Step (2 Get: the tally and its
// marker key, as a retry-safe Step reads them; 1 Insert); and a decided gate 1 Get.
func TestGate_StoreBudget(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops", "finance")
	cs := agenttest.NewCountingStore(agent.NewMemStore())
	j := agenttest.MustJournal(cs)
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance"}}, vf, &refunds)
	in := refund{Order: "order-42", Amount: 120}
	if _, err := f.Run(ctx, j, "r1", "order-42"); err == nil {
		t.Fatal("no pause")
	}
	eval := func() (agenttest.Counts, error) {
		cs.Reset()
		err := runGate(ctx, j, "r1", f.core, f.core.byName["refund"], "node:refund", in)
		return cs.Counts(), err
	}
	want := func(name string, c agenttest.Counts, get, insert, load int) {
		t.Helper()
		if c.Get != get || c.Insert != insert || c.Load != load {
			t.Errorf("%s: %d Get, %d Insert, %d Load; want %d, %d, %d (%v)", name, c.Get, c.Insert, c.Load, get, insert, load, c.Names)
		}
	}
	cs.Reset()
	if err := runGate(ctx, j, "r2", f.core, f.core.byName["lookup"], "node:lookup", "order-42"); err != nil {
		t.Fatal(err)
	}
	want("an ungated node", cs.Counts(), 0, 0, 0)

	c, _ := eval()
	want("a pause again", c, 1, 2, 1)
	as["ops"].decide(t, j, &agent.ApprovalPending{RunRef: agent.RunRef{RunID: "r1"}, ToolUseID: "node:refund", ToolName: "refund", Args: []byte(`{"order":"order-42","amount":120}`)}, true)
	as["finance"].decide(t, j, &agent.ApprovalPending{RunRef: agent.RunRef{RunID: "r1"}, ToolUseID: "node:refund", ToolName: "refund", Args: []byte(`{"order":"order-42","amount":120}`)}, true)
	c, err := eval()
	if err != nil {
		t.Fatal(err)
	}
	want("the outcome", c, 3, 2, 1)
	c, _ = eval()
	// The plain tally, then two point reads that tell a bare gate from a certificate-bound one (an identity
	// tally of generation 0, the first note): never a Load or an Insert.
	want("a decided gate", c, 3, 0, 0)

	// The first pause, in a run whose journal is open (as Run's has been by then).
	if _, err := journaltest.Put(ctx, j, "r3", "flow:digest", agent.Record{Kind: agent.StepValue, Result: []byte(`"x"`)}); err != nil {
		t.Fatal(err)
	}
	c, _ = func() (agenttest.Counts, error) {
		cs.Reset()
		err := runGate(ctx, j, "r3", f.core, f.core.byName["refund"], "node:refund", in)
		return cs.Counts(), err
	}()
	want("a first pause", c, 1, 2, 1)

	// A decided SingleApproval gate: Approve's record and the request it must follow.
	single := refundFlow(t, agent.SingleApproval(), nil, &refunds)
	pend := asPending(t, func() error { _, err := single.Run(ctx, j, "r5", "order-42"); return err }())
	if err := agent.Approve(ctx, j, pend.RunID, pend.ToolUseID, true); err != nil {
		t.Fatal(err)
	}
	cs.Reset()
	if err := runGate(ctx, j, "r5", single.core, single.core.byName["refund"], "node:refund", in); err != nil {
		t.Fatal(err)
	}
	want("a decided single gate", cs.Counts(), 2, 0, 0)
}
