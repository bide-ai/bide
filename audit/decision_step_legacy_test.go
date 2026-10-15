package audit_test

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// F3 (legacy double records): alice's one decision is journaled twice, under its identity name and
// under a signature-derived (legacy) name, before the quorum. The gate counts her once; the
// evidence verifier must agree.
func TestLegacyPlusIdentityRecordsOfOneDecisionVerify(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	wantPaused(t, g.run(), 0)
	g.decide(t, "alice", true, false)
	recs, _ := g.store.History(ctx, gateRun)
	for _, r := range recs {
		if r.Kind == agent.StepApproval && r.Approver() == "alice" {
			if _, err := journaltest.Put(ctx, g.store, gateRun, "approval:c1:alice:legacy"+"0000000000000000000000000000000000000000000000000000000000000000"[:56], r); err != nil {
				t.Fatal(err)
			}
		}
	}
	wantPaused(t, g.run(), 1) // one approver: alice counts once
	g.decide(t, "bob", true, false)
	if err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("run at quorum: %v charged %d", err, g.charged)
	}
	v, err := audit.VerifyApprovals(evidenceFor(t, g), "c1", g.policy, g.resolver(), edV(g.logPub))
	if rerr := reportErr(v.OK, err); rerr != nil || !v.OK {
		t.Fatalf("verdict %+v err %v", v, err)
	}
}
