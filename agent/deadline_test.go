package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// testClock is a settable clock for deadline tests.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

var deadlineT0 = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

// deadlineRun drives one run of a fresh agent with a gated "charge" tool, at the clock's time.
func deadlineRun(clk *testClock, store *Journal, runID string, first bool, pol *ApprovalPolicy, vf ApproverVerifierFor, charged *int) (Message, error) {
	turns := [][]Emit{textTurn("done")}
	if first {
		turns = [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}
	}
	charge := &countingTool{name: "charge", approval: pol, calls: charged}
	opts := []Option{WithTools(charge)}
	if vf != nil {
		opts = append(opts, WithApproverVerifiers(vf))
	}
	a, err := New(&scriptModel{turns: turns}, store, opts...)
	if err != nil {
		return Message{}, err
	}
	res, err := a.Run(contextWithClock(context.Background(), clk.now), runID, UserText("pay"))
	if err != nil {
		return Message{}, err
	}
	return res.Message, nil
}

// toolResultOf is the recorded result of call c1.
func toolResultOf(t *testing.T, store *Journal, runID string) Record {
	t.Helper()
	r, ok := hasStep(t, store, runID, ToolResultStep("c1"))
	if !ok {
		t.Fatalf("no result for c1")
	}
	return r
}

// An m-of-n gate with a deadline records it once, measured from the request, and expires when the
// clock reaches it with too few approvals: the expiry is the recorded tally (not passed, Expired),
// the tool never runs, the model reads ExpiredResult, a later decision is refused, and
// EvaluateGate reports the expiry.
func TestDeadline_MofnExpires(t *testing.T) {
	store := memJournal()
	clk := &testClock{t: deadlineT0}
	pol := &ApprovalPolicy{Need: 2, Approvers: abc, Timeout: time.Hour}
	vf := fakeVerifiers(abc...)
	var charged int
	if _, err := deadlineRun(clk, store, "r1", true, pol, vf, &charged); !isApprovalPending(err) {
		t.Fatalf("first run = %v; want a pause", err)
	}
	r, ok := hasStep(t, store, "r1", ApprovalDeadlineStep("c1"))
	if !ok {
		t.Fatal("no deadline recorded on the first pause")
	}
	dl, at, ok := decodeDeadline(r, "c1")
	if !ok || !at.Equal(deadlineT0.Add(time.Hour)) || dl.TimeoutMS != time.Hour.Milliseconds() {
		t.Fatalf("deadline %+v (%v); want an hour after the request", dl, at)
	}
	clk.t = deadlineT0.Add(30 * time.Minute)
	approveAt(t, clk, store, "alice") // on time, but one of two
	clk.t = deadlineT0.Add(time.Hour)
	if _, err := deadlineRun(clk, store, "r1", false, pol, vf, &charged); err != nil {
		t.Fatalf("run at the deadline = %v; want it to end with the call denied", err)
	}
	if charged != 0 {
		t.Fatalf("the tool ran %d times on an expired gate", charged)
	}
	tr, ok := hasStep(t, store, "r1", ApprovalTallyStep("c1"))
	if !ok {
		t.Fatal("no tally recorded for the expiry")
	}
	tally, err := decodeTally("r1", tr)
	if err != nil || !tally.Expired || tally.Passed() || tally.Approved != 1 || tally.ExpiresAt != dl.ExpiresAt {
		t.Fatalf("recorded tally %+v (%v); want the expiry with alice's approval and the deadline", tally, err)
	}
	if res := toolResultOf(t, store, "r1"); string(res.Result) != ExpiredResult || !res.IsError {
		t.Fatalf("result %s; want %s", res.Result, ExpiredResult)
	}
	err = decideAs(contextWithClock(context.Background(), clk.now), store, "r1", "c1", "bob", true,
		fakeSign("bob", ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), "bob", true, testDecidedAt)))
	if !errors.Is(err, ErrApprovalExpired) || !errors.Is(err, ErrConfig) {
		t.Fatalf("SubmitDecision after the deadline = %v; want ErrApprovalExpired", err)
	}
	recs, _ := store.History(context.Background(), "r1")
	st := EvaluateGate(recs, subjectOf(t, store, "r1", "c1"), pol, vf)
	if !st.Expired || !st.Denied || st.Status != GateDecided || !st.Deadline.Equal(at) {
		t.Fatalf("EvaluateGate = %+v; want decided, denied, expired, with the deadline", st)
	}
}

// Approvals recorded by the deadline win even if no drive counts them until after it: the count
// comes first, and a passing count is recorded, never an expiry.
func TestDeadline_OnTimeApprovalsWinAfterTheDeadline(t *testing.T) {
	store := memJournal()
	clk := &testClock{t: deadlineT0}
	pol := &ApprovalPolicy{Need: 2, Approvers: abc, Timeout: time.Hour}
	vf := fakeVerifiers(abc...)
	var charged int
	_, _ = deadlineRun(clk, store, "r1", true, pol, vf, &charged)
	clk.t = deadlineT0.Add(10 * time.Minute)
	approveAt(t, clk, store, "alice")
	approveAt(t, clk, store, "bob")
	clk.t = deadlineT0.Add(3 * time.Hour)
	if _, err := deadlineRun(clk, store, "r1", false, pol, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("run after the deadline with quorum in on time = %v, ran %d; want the call run once", err, charged)
	}
}

// A decision recorded after the deadline never counts (ReasonLate), even before any drive recorded
// the expiry: the gate expires instead of passing on it.
func TestDeadline_LateDecisionDoesNotCount(t *testing.T) {
	store := memJournal()
	clk := &testClock{t: deadlineT0}
	pol := &ApprovalPolicy{Need: 1, Approvers: abc, Timeout: time.Hour}
	vf := fakeVerifiers(abc...)
	var charged int
	_, _ = deadlineRun(clk, store, "r1", true, pol, vf, &charged)
	s := subjectOf(t, store, "r1", "c1")
	writeRaw(t, store, "r1", "approval:c1:alice:late", Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
		Approver: "alice", ApproverAlg: fakeAlg, Signature: fakeSign("alice", ApprovalDecisionBytes(s, "alice", true, testDecidedAt)),
		DecidedAt: FormatTimestamp(testDecidedAt), RecordedAt: FormatTimestamp(deadlineT0.Add(2 * time.Hour)),
	}})
	recs, _ := store.History(context.Background(), "r1")
	st := EvaluateGate(recs, s, pol, vf)
	var late bool
	for _, c := range st.Checks {
		late = late || c.Reason == ReasonLate
	}
	if st.Status != GateWaiting || st.Tally.Approved != 0 || !late {
		t.Fatalf("EvaluateGate with a late approval = %+v; want waiting, not counted (ReasonLate)", st)
	}
	clk.t = deadlineT0.Add(3 * time.Hour)
	if _, err := deadlineRun(clk, store, "r1", false, pol, vf, &charged); err != nil || charged != 0 {
		t.Fatalf("run after the deadline = %v, ran %d; want the expiry", err, charged)
	}
	tr, _ := hasStep(t, store, "r1", ApprovalTallyStep("c1"))
	if tally, _ := decodeTally("r1", tr); !tally.Expired || len(tally.Records) != 0 {
		t.Fatalf("tally %+v; want an expiry that read no late record", tally)
	}
}

// A one-decision gate with a deadline: no Approve by the deadline, and the gate records its expiry
// as the decision; Approve after it is refused. An Approve on time is the decision, even if the run
// is driven after the deadline.
func TestDeadline_Single(t *testing.T) {
	pol := SingleApproval()
	pol.Timeout = time.Hour
	t.Run("expires", func(t *testing.T) {
		store := memJournal()
		clk := &testClock{t: deadlineT0}
		var charged int
		_, _ = deadlineRun(clk, store, "r1", true, pol, nil, &charged)
		clk.t = deadlineT0.Add(time.Hour)
		if _, err := deadlineRun(clk, store, "r1", false, pol, nil, &charged); err != nil || charged != 0 {
			t.Fatalf("run at the deadline = %v, ran %d", err, charged)
		}
		r, ok := hasStep(t, store, "r1", approvalStep("c1"))
		if !ok || r.Approved || string(r.Result) != expiredDecision {
			t.Fatalf("decision record %+v; want the gate's expiry", r)
		}
		if err := Approve(contextWithClock(context.Background(), clk.now), store, "r1", "c1", true); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("Approve after the deadline = %v; want ErrApprovalExpired", err)
		}
		if res := toolResultOf(t, store, "r1"); string(res.Result) != ExpiredResult {
			t.Fatalf("result %s; want %s", res.Result, ExpiredResult)
		}
	})
	t.Run("approved on time", func(t *testing.T) {
		store := memJournal()
		clk := &testClock{t: deadlineT0}
		var charged int
		_, _ = deadlineRun(clk, store, "r1", true, pol, nil, &charged)
		clk.t = deadlineT0.Add(59 * time.Minute)
		if err := Approve(contextWithClock(context.Background(), clk.now), store, "r1", "c1", true); err != nil {
			t.Fatal(err)
		}
		clk.t = deadlineT0.Add(5 * time.Hour)
		if _, err := deadlineRun(clk, store, "r1", false, pol, nil, &charged); err != nil || charged != 1 {
			t.Fatalf("run after the deadline on an on-time Approve = %v, ran %d", err, charged)
		}
	})
	t.Run("late decision reads as the expiry", func(t *testing.T) {
		store := memJournal()
		clk := &testClock{t: deadlineT0}
		var charged int
		_, _ = deadlineRun(clk, store, "r1", true, pol, nil, &charged)
		writeRaw(t, store, "r1", approvalStep("c1"), Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{RecordedAt: FormatTimestamp(deadlineT0.Add(2 * time.Hour))}})
		clk.t = deadlineT0.Add(3 * time.Hour)
		if _, err := deadlineRun(clk, store, "r1", false, pol, nil, &charged); err != nil || charged != 0 {
			t.Fatalf("run on a late Approve = %v, ran %d; want the expiry", err, charged)
		}
	})
}

// The deadline is recorded once: a redeploy that changes the policy's Timeout does not move it.
func TestDeadline_FixedOnce(t *testing.T) {
	store := memJournal()
	clk := &testClock{t: deadlineT0}
	vf := fakeVerifiers(abc...)
	var charged int
	_, _ = deadlineRun(clk, store, "r1", true, &ApprovalPolicy{Need: 1, Approvers: abc, Timeout: time.Hour}, vf, &charged)
	clk.t = deadlineT0.Add(2 * time.Hour)
	longer := &ApprovalPolicy{Need: 1, Approvers: abc, Timeout: 24 * time.Hour}
	if _, err := deadlineRun(clk, store, "r1", false, longer, vf, &charged); err != nil || charged != 0 {
		t.Fatalf("run under a longer timeout after the recorded deadline = %v, ran %d; want the expiry", err, charged)
	}
}

// The policy journals its timeout as timeout_ms, omitted when zero; Validate refuses a negative or
// sub-millisecond timeout.
func TestDeadline_PolicyEncoding(t *testing.T) {
	for _, p := range []*ApprovalPolicy{{Need: 1, Approvers: abc, Timeout: 90 * time.Minute}, func() *ApprovalPolicy { s := SingleApproval(); s.Timeout = time.Minute; return s }()} {
		b, err := json.Marshal(p)
		if err != nil || !strings.Contains(string(b), `"timeout_ms":`) {
			t.Fatalf("Marshal(%+v) = %s, %v", p, b, err)
		}
		var back ApprovalPolicy
		if err := json.Unmarshal(b, &back); err != nil || back.Timeout != p.Timeout || back.single() != p.single() {
			t.Fatalf("round trip of %s = %+v, %v", b, back, err)
		}
	}
	if b, _ := json.Marshal(&ApprovalPolicy{Need: 1, Approvers: abc}); strings.Contains(string(b), "timeout") {
		t.Fatalf("a policy with no deadline encodes %s; want no timeout_ms", b)
	}
	for _, d := range []time.Duration{-time.Second, time.Microsecond} {
		if err := (ApprovalPolicy{Need: 1, Approvers: abc, Timeout: d}).Validate(); !errors.Is(err, ErrConfig) {
			t.Fatalf("Validate(Timeout %v) = %v; want ErrConfig", d, err)
		}
	}
}

func isApprovalPending(err error) bool {
	var p *ApprovalPending
	return errors.As(err, &p)
}

// approveAt records approverID's signed approval of c1 in run r1 at the clock's time.
func approveAt(t *testing.T, clk *testClock, store *Journal, approverID string) {
	t.Helper()
	sig := fakeSign(approverID, ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), approverID, true, testDecidedAt))
	if err := decideAs(contextWithClock(context.Background(), clk.now), store, "r1", "c1", approverID, true, sig); err != nil {
		t.Fatalf("decideAs(%s): %v", approverID, err)
	}
}

// An expiry tally never passes, whatever its counts say (a tally hand-written with enough
// approvals and the expiry is not a pass), and EvaluateGate reads a recorded expiry tally as the
// gate's expired outcome before the call's result is recorded.
func TestDeadline_ExpiryTallyNeverPasses(t *testing.T) {
	if (ApprovalTally{Need: 1, Approved: 2, Expired: true}).Passed() {
		t.Fatal("an expiry tally passed")
	}
	store := memJournal()
	clk := &testClock{t: deadlineT0}
	pol := &ApprovalPolicy{Need: 2, Approvers: abc, Timeout: time.Hour}
	vf := fakeVerifiers(abc...)
	var charged int
	_, _ = deadlineRun(clk, store, "r1", true, pol, vf, &charged)
	clk.t = deadlineT0.Add(2 * time.Hour)
	_, _ = deadlineRun(clk, store, "r1", false, pol, vf, &charged)
	recs, _ := store.History(context.Background(), "r1")
	cut := recs[:0:0]
	for _, r := range recs {
		if r.Name == ToolResultStep("c1") {
			break
		}
		cut = append(cut, r)
	}
	st := EvaluateGate(cut, subjectOf(t, store, "r1", "c1"), pol, vf)
	if st.Status != GateDecided || !st.Expired || !st.Denied || !st.Recorded {
		t.Fatalf("EvaluateGate on the expiry tally alone = %+v; want decided, denied, expired", st)
	}
}

// A drive of a gate that already recorded its deadline reads it from the journal and writes nothing
// for it.
func TestDeadline_ReplayDoesNotRewriteTheDeadline(t *testing.T) {
	cs := &insertLog{Store: NewMemStore()}
	store := mustJournal(cs)
	clk := &testClock{t: deadlineT0}
	pol := &ApprovalPolicy{Need: 2, Approvers: abc, Timeout: time.Hour}
	vf := fakeVerifiers(abc...)
	var charged int
	_, _ = deadlineRun(clk, store, "r1", true, pol, vf, &charged)
	cs.inserts = nil
	_, _ = deadlineRun(clk, store, "r1", false, pol, vf, &charged)
	for _, n := range cs.inserts {
		if n == ApprovalDeadlineStep("c1") {
			t.Fatal("a later drive wrote the deadline again")
		}
	}
}

// insertLog is a Store that lists the names of the Inserts made through it.
type insertLog struct {
	Store
	inserts []string
}

func (s *insertLog) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	s.inserts = append(s.inserts, name)
	return s.Store.Insert(ctx, runID, name, data)
}
