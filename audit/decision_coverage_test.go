package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// coverageGate is a 3-of-3 gate paused at asked, with alice's and bob's decisions recorded. Its
// journal is recs; before, afterAlice and full are signed heads over it as it grew.
type coverageGate struct {
	*gate
	recs                      []agent.Record
	decision                  []int // journal positions of the decisions
	before, afterAlice, whole audit.SignedTreeHead
}

func (g *gate) headNow(t *testing.T) audit.SignedTreeHead {
	t.Helper()
	th, err := audit.NewTreeHead(context.Background(), g.store, gateRun, headTime)
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignTreeHead(th, edS(g.logPriv))
	if err != nil {
		t.Fatal(err)
	}
	return sth
}

func newCoverageGate(t *testing.T) *coverageGate {
	t.Helper()
	g := newGate(3, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.clock = func() time.Time { return asked }
	wantPaused(t, g.run(), 0)
	cg := &coverageGate{gate: g, before: g.headNow(t)}
	g.decideAt(t, "alice", asked.Add(time.Second), asked.Add(2*time.Second))
	cg.afterAlice = g.headNow(t)
	g.decideAt(t, "bob", asked.Add(time.Second), asked.Add(3*time.Second))
	cg.whole = g.headNow(t)
	recs, err := g.store.History(context.Background(), gateRun)
	if err != nil {
		t.Fatal(err)
	}
	cg.recs = recs
	for i, r := range recs {
		if r.Kind == agent.StepApproval && r.Approver() != "" {
			cg.decision = append(cg.decision, i)
		}
	}
	if len(cg.decision) != 2 || cg.afterAlice.Size != cg.decision[0]+1 || cg.before.Size != cg.decision[0] || cg.whole.Size != len(recs) {
		t.Fatalf("setup: decisions at %v, head sizes %d/%d/%d, %d records", cg.decision, cg.before.Size, cg.afterAlice.Size, cg.whole.Size, len(recs))
	}
	return cg
}

func statusOf(rep audit.CoverageReport) []string {
	var out []string
	for _, d := range rep.Decisions {
		out = append(out, d.Status)
	}
	return out
}

// A decision under a signed head is covered; one past the largest head is pending until the
// journal has grown MaxRecords past it, and then overdue. Heads are ordered by size, not by the
// order they are given in.
func TestCheckDecisionCoverage_PositionsAndHeadSizes(t *testing.T) {
	cg := newCoverageGate(t)
	n := len(cg.recs)
	a, b := cg.decision[0], cg.decision[1]
	v := edV(cg.logPub)

	// Heads over the journal up to and including alice's decision, given out of order.
	small, tiny := cg.afterAlice, cg.before
	rep, err := audit.CheckDecisionCoverage(gateRun, cg.recs, []audit.SignedTreeHead{small, tiny}, v, audit.CoverageBound{MaxRecords: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(rep); len(got) != 2 || got[0] != audit.CoverageCovered || got[1] != audit.CoveragePending {
		t.Fatalf("statuses %v, want covered then pending", got)
	}
	if rep.Decisions[0].CoveredBySize != a+1 {
		t.Errorf("alice covered by size %d, want the smallest covering head, %d", rep.Decisions[0].CoveredBySize, a+1)
	}
	if rep.Decisions[1].Behind != n-1-b {
		t.Errorf("bob has %d records after it, want %d", rep.Decisions[1].Behind, n-1-b)
	}
	if rep.OK() == false {
		t.Error("a pending decision within the bound is not a finding")
	}

	// With only the head before alice's decision, alice's has bob's record after it: overdue under a
	// bound of one record. Bob's is the journal's last record, so nothing is after it yet.
	rep, err = audit.CheckDecisionCoverage(gateRun, cg.recs, []audit.SignedTreeHead{tiny}, v, audit.CoverageBound{MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(rep); got[0] != audit.CoverageOverdue || rep.Decisions[0].Basis != audit.BasisRecords || got[1] != audit.CoveragePending {
		t.Fatalf("statuses %v basis %q, want alice overdue on records and bob pending", got, rep.Decisions[0].Basis)
	}
	if rep.OK() {
		t.Error("an overdue decision must fail the check")
	}

	// A head over the whole journal covers both, whatever MaxRecords is.
	rep, err = audit.CheckDecisionCoverage(gateRun, cg.recs, []audit.SignedTreeHead{cg.whole}, v, audit.CoverageBound{MaxRecords: 1})
	if err != nil || !rep.OK() || statusOf(rep)[1] != audit.CoverageCovered {
		t.Fatalf("whole-journal head: %v %v", statusOf(rep), err)
	}
}

// The age bound is the weaker basis: it compares the recording process's clock with the checker's,
// so it needs a Now, allows the tolerance, and labels the finding.
func TestCheckDecisionCoverage_Age(t *testing.T) {
	cg := newCoverageGate(t)
	v := edV(cg.logPub)
	heads := []audit.SignedTreeHead{cg.before} // covers neither decision
	bound := audit.CoverageBound{MaxAge: time.Hour, Tolerance: 5 * time.Minute}

	rep, err := audit.CheckDecisionCoverage(gateRun, cg.recs, heads, v, bound) // no Now: age is not checked
	if err != nil || statusOf(rep)[0] != audit.CoveragePending {
		t.Fatalf("no Now: %v %v", statusOf(rep), err)
	}
	bound.Now = asked.Add(2*time.Second + 65*time.Minute) // alice recorded at asked+2s: 65m old, inside 1h + 5m
	rep, _ = audit.CheckDecisionCoverage(gateRun, cg.recs, heads, v, bound)
	if statusOf(rep)[0] != audit.CoveragePending {
		t.Fatalf("inside the tolerance: %v", statusOf(rep))
	}
	bound.Now = asked.Add(2*time.Second + 66*time.Minute)
	rep, _ = audit.CheckDecisionCoverage(gateRun, cg.recs, heads, v, bound)
	if got := statusOf(rep); got[0] != audit.CoverageOverdue || rep.Decisions[0].Basis != audit.BasisAge {
		t.Fatalf("past the tolerance: %v basis %q", got, rep.Decisions[0].Basis)
	}
	if got := statusOf(rep); got[1] != audit.CoverageOverdue {
		t.Fatalf("bob (recorded a second later) %v", got)
	}
}

// A head that does not belong to this journal covers nothing: a bad signature, another run's, one
// whose root is not this journal's prefix, or a size past the journal. Each is listed with why.
func TestCheckDecisionCoverage_RejectsHeadsThatDoNotBelong(t *testing.T) {
	cg := newCoverageGate(t)
	v := edV(cg.logPub)
	good := cg.whole

	badSig := good
	badSig.Signature = append([]byte(nil), good.Signature...)
	badSig.Signature[0] ^= 0xff

	otherRun := good
	otherRun.RunID = "elsewhere"

	// The same steps in another journal, signed by this log's key: its root is not this one's.
	fork, err := audit.SignTreeHead(newCoverageGate(t).whole.TreeHead, edS(cg.logPriv))
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]audit.SignedTreeHead{"signature": badSig, "run": otherRun, "root": fork}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			rep, err := audit.CheckDecisionCoverage(gateRun, cg.recs, []audit.SignedTreeHead{h}, v, audit.CoverageBound{MaxRecords: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.RejectedHeads) != 1 || !strings.Contains(rep.RejectedHeads[0].Reason, name) {
				t.Fatalf("rejected heads %+v, want one naming %q", rep.RejectedHeads, name)
			}
			if rep.Decisions[0].Status == audit.CoverageCovered || rep.OK() {
				t.Fatalf("a head that does not belong covered a decision: %v", statusOf(rep))
			}
		})
	}
	_, err = audit.CheckDecisionCoverage(gateRun, cg.recs, nil, nil, audit.CoverageBound{})
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a nil verifier: %v, want ErrConfig", err)
	}
}

// A journal with no decision has nothing to check, and a run with no heads at all reports every
// decision uncovered (pending, until a bound passes).
func TestCheckDecisionCoverage_NoDecisionsNoHeads(t *testing.T) {
	cg := newCoverageGate(t)
	v := edV(cg.logPub)
	rep, err := audit.CheckDecisionCoverage(gateRun, cg.recs[:cg.decision[0]], nil, v, audit.CoverageBound{MaxRecords: 1})
	if err != nil || len(rep.Decisions) != 0 || !rep.OK() {
		t.Fatalf("no decisions: %+v %v", rep, err)
	}
	rep, _ = audit.CheckDecisionCoverage(gateRun, cg.recs, nil, v, audit.CoverageBound{MaxRecords: 1})
	if got := statusOf(rep); got[0] == audit.CoverageCovered || got[1] == audit.CoverageCovered {
		t.Fatalf("with no heads: %v", got)
	}
}
