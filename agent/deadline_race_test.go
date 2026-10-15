package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// injectBefore runs inject once, just before the first Insert of name: a write that lands between
// the gate's read of the journal and its own write.
type injectBefore struct {
	Store
	name   string
	inject func()
	once   sync.Once
}

func (s *injectBefore) Unwrap() Store { return s.Store }

func (s *injectBefore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == s.name {
		s.once.Do(s.inject)
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// A decision stamped before the deadline whose write lands after the gate read the journal, but
// before the gate recorded its outcome, does not count: the expiry, recorded first, is the outcome,
// and the call never runs. It fails closed, and every reader says why the on-time decision did not
// count (ReasonAfterExpiry), so the journal does not look contradictory.
func TestDeadline_OnTimeQuorumRacingExpiry(t *testing.T) {
	inner := NewMemStore()
	store := mustJournal(inner)
	clk := &testClock{t: deadlineT0}
	pol := &ApprovalPolicy{Need: 2, Approvers: abc, Timeout: time.Hour}
	vf := fakeVerifiers(abc...)
	var charged int
	if _, err := deadlineRun(clk, store, "r1", true, pol, vf, &charged); !isApprovalPending(err) {
		t.Fatalf("first run = %v; want a pause", err)
	}
	clk.t = deadlineT0.Add(30 * time.Minute)
	approveAt(t, clk, store, "alice")

	// bob decides 1 ms before the deadline; his write lands after the gate's read.
	onTime := &testClock{t: deadlineT0.Add(time.Hour - time.Millisecond)}
	racy := mustJournal(&injectBefore{Store: inner, name: ApprovalTallyStep("c1"), inject: func() {
		approveAt(t, onTime, mustJournal(inner), "bob")
	}})
	clk.t = deadlineT0.Add(time.Hour)
	if _, err := deadlineRun(clk, racy, "r1", false, pol, vf, &charged); err != nil {
		t.Fatalf("run: %v", err)
	}
	tr, _ := hasStep(t, store, "r1", ApprovalTallyStep("c1"))
	tally, _ := decodeTally("r1", tr)
	recs, _ := store.History(context.Background(), "r1")
	onTimeYes := 0
	for _, r := range recs {
		if IsApprovalDecision(r, "c1") && r.Approved && !lateFor(r.RecordedAt(), deadlineT0.Add(time.Hour)) {
			onTimeYes++
		}
	}
	t.Logf("on-time approvals in the journal: %d; recorded tally: expired=%v approved=%d; charged=%d", onTimeYes, tally.Expired, tally.Approved, charged)
	if onTimeYes != 2 || !tally.Expired || tally.Approved != 1 || charged != 0 {
		t.Fatalf("on-time approvals %d, tally %+v, ran %d; want the race to end as the expiry, with alice's approval counted", onTimeYes, tally, charged)
	}
	st := EvaluateGate(recs, subjectOf(t, store, "r1", "c1"), pol, vf)
	var labelled bool
	for _, c := range st.Checks {
		labelled = labelled || c.Approver == "bob" && !c.Counted && c.Reason == ReasonAfterExpiry
	}
	if !st.Expired || !labelled {
		t.Fatalf("EvaluateGate = %+v; want expired, with bob's decision labelled %q", st, ReasonAfterExpiry)
	}
}
