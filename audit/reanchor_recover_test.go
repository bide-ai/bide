package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

var _ agent.Reanchorer = (*audit.AuditedStore)(nil)

// A decision another process writes straight into the database is under no signed head until the
// application writes to the run again. With RecoverLoop over the application's AuditedStore, a
// pass re-signs the waiting run, and CheckDecisionCoverage finds the decision covered.
func TestRecoverLoop_SignsADecisionAnotherProcessWrote(t *testing.T) {
	g := newGate(3, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	inner := agent.NewMemStore()
	log := audit.NewMemAnchorLog()
	as, err := audit.NewAuditedStore(inner, edS(g.logPriv), log)
	if err != nil {
		t.Fatal(err)
	}
	app := agenttest.MustJournal(as)      // the application: signs what it writes
	other := agenttest.MustJournal(inner) // `bide approve`: writes the database, signs nothing
	g.store = app
	wantPaused(t, g.run(), 0)

	g.store = other
	g.decide(t, "alice", true, false)
	g.store = app
	recs, _ := app.History(context.Background(), gateRun)
	decisionAt := -1
	for i, r := range recs {
		if r.Kind == agent.StepApproval {
			decisionAt = i
		}
	}
	largest := func() int {
		n := 0
		for _, e := range log.Entries() {
			if e.RunID == gateRun {
				n = max(n, e.STH.Size)
			}
		}
		return n
	}
	if decisionAt < 0 || largest() > decisionAt {
		t.Fatalf("setup: the decision is at %d and the largest signed head covers %d records; want it outside", decisionAt, largest())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.RecoverLoop(ctx, app, func(context.Context, string, agent.RunStart) error { return g.run() },
			agent.WithRecoverInterval(10*time.Millisecond))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for largest() <= decisionAt && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if largest() <= decisionAt {
		t.Fatalf("the decision at %d is still under no signed head (largest covers %d)", decisionAt, largest())
	}

	var heads []audit.SignedTreeHead
	for _, e := range log.Entries() {
		heads = append(heads, e.STH)
	}
	final, _ := app.History(context.Background(), gateRun)
	rep, err := audit.CheckDecisionCoverage(gateRun, final, heads, edV(g.logPub), audit.CoverageBound{MaxRecords: 1})
	if err != nil || !rep.OK() || len(rep.Decisions) != 1 || rep.Decisions[0].Status != audit.CoverageCovered {
		t.Fatalf("coverage after the pass: %+v, %v", rep, err)
	}
}
