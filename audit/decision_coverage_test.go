package audit_test

import (
	"context"
	"errors"
	"slices"
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

	// A head that does not verify, or is another run's, is ignored. One the log key signed for this
	// run that is not this journal's is divergent, and fails the check.
	for name, h := range map[string]audit.SignedTreeHead{"signature": badSig, "run": otherRun} {
		t.Run(name, func(t *testing.T) {
			rep, err := audit.CheckDecisionCoverage(gateRun, cg.recs, []audit.SignedTreeHead{h}, v, audit.CoverageBound{MaxRecords: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.RejectedHeads) != 1 || !strings.Contains(rep.RejectedHeads[0].Reason, name) || len(rep.DivergentHeads) != 0 {
				t.Fatalf("rejected %+v divergent %+v, want one rejected head naming %q", rep.RejectedHeads, rep.DivergentHeads, name)
			}
			if rep.Decisions[0].Status == audit.CoverageCovered {
				t.Fatalf("a head that does not belong covered a decision: %v", statusOf(rep))
			}
		})
	}
	t.Run("root", func(t *testing.T) {
		rep, err := audit.CheckDecisionCoverage(gateRun, cg.recs, []audit.SignedTreeHead{fork, cg.whole}, v, audit.CoverageBound{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.DivergentHeads) != 1 || !strings.Contains(rep.DivergentHeads[0].Reason, "root") || rep.OK() {
			t.Fatalf("divergent %+v ok=%v, want one divergent head and a failure", rep.DivergentHeads, rep.OK())
		}
		if rep.Heads != 1 || statusOf(rep)[0] != audit.CoverageCovered {
			t.Fatalf("the matching head still counts: heads %d, statuses %v", rep.Heads, statusOf(rep))
		}
	})
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

// A decision removed from the export does not vanish: every signed head that reaches past it no
// longer matches, which is divergence, and the check fails.
func TestCheckDecisionCoverage_DeletedDecisionFails(t *testing.T) {
	cg := newCoverageGate(t)
	a := cg.decision[0]
	tampered := slices.Delete(slices.Clone(cg.recs), a, a+1)
	rep, err := audit.CheckDecisionCoverage(gateRun, tampered, []audit.SignedTreeHead{cg.before, cg.afterAlice, cg.whole}, edV(cg.logPub), audit.CoverageBound{MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || len(rep.DivergentHeads) != 2 {
		t.Fatalf("ok=%v, divergent %+v; want both heads that reach past the deletion divergent", rep.OK(), rep.DivergentHeads)
	}
}

// An export cut before the decisions is shorter than the heads the log signed: the heads that
// reach past it are divergent, so a truncated export fails and says why.
func TestCheckDecisionCoverage_TruncatedExportFails(t *testing.T) {
	cg := newCoverageGate(t)
	rep, err := audit.CheckDecisionCoverage(gateRun, cg.recs[:cg.decision[0]], []audit.SignedTreeHead{cg.before, cg.afterAlice, cg.whole}, edV(cg.logPub), audit.CoverageBound{MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || len(rep.DivergentHeads) != 2 || !strings.Contains(rep.DivergentHeads[0].Reason, "out of range") {
		t.Fatalf("ok=%v, divergent %+v", rep.OK(), rep.DivergentHeads)
	}
}

// A decision with no recorded time (an unsigned agent.Approve decision records none) is flagged,
// and under an age bound it is over the bound: it cannot be shown to be inside it.
func TestCheckDecisionCoverage_NoRecordedTime(t *testing.T) {
	cg := newCoverageGate(t)
	recs := append(slices.Clone(cg.recs), agent.Record{Kind: agent.StepApproval, ToolUseID: "x", Approved: false})
	heads := []audit.SignedTreeHead{cg.whole}
	rep, err := audit.CheckDecisionCoverage(gateRun, recs, heads, edV(cg.logPub), audit.CoverageBound{})
	if err != nil {
		t.Fatal(err)
	}
	last := rep.Decisions[len(rep.Decisions)-1]
	if last.Status != audit.CoveragePending || last.Note != audit.NoteNoRecordedTime {
		t.Fatalf("without an age bound: %q note %q, want pending with the note", last.Status, last.Note)
	}
	rep, _ = audit.CheckDecisionCoverage(gateRun, recs, heads, edV(cg.logPub), audit.CoverageBound{MaxAge: time.Second, Now: time.Now().Add(1000 * time.Hour)})
	last = rep.Decisions[len(rep.Decisions)-1]
	if last.Status != audit.CoverageOverdue || last.Basis != audit.BasisAge {
		t.Fatalf("under an age bound: %q basis %q, want overdue on age", last.Status, last.Basis)
	}
}
