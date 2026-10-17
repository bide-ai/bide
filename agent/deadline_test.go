package agent

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sleepModel calls the "wait" tool on the first turn, then answers once it sees the tool result.
type sleepModel struct{}

func (sleepModel) Stream(_ context.Context, req Request) (*Stream, error) {
	hasToolResult := false
	for _, m := range req.Messages {
		if m.Role == RoleTool {
			hasToolResult = true
		}
	}
	ch := make(chan Emit, 2)
	if !hasToolResult {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "w", Name: "wait", ArgsFragment: []byte(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
	} else {
		ch <- Emit{Event: TextDelta{Text: "done"}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	}
	close(ch)
	return NewStream(ch), nil
}

// waitTool sleeps one hour (durably) then reports it waited. ReadOnly, so it is retry-safe.
func waitTool() Tool {
	return MustFunc("wait", "wait an hour", func(ctx context.Context, _ struct{}) (string, error) {
		if err := Sleep(ctx, "w1", time.Hour); err != nil {
			return "", err
		}
		return "waited", nil
	}, WithSafety(Safety{ReadOnly: true}))
}

// TestSleep_PausesAndResumes confirms a durable timer pauses the run, keeps the same wake time
// across resumes (at-most-once), stays paused before it is due, and resumes after.
func TestSleep_PausesAndResumes(t *testing.T) {
	var clk int64 = 1000 // seconds since epoch, controllable
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	ctx := contextWithClock(context.Background(), now)

	a := mustNew(sleepModel{}, memJournal(), WithTools(waitTool()))

	// First run: the wait tool sleeps, so the run pauses with *TimerPending at now+1h.
	_, err := a.Run(ctx, "r1", UserText("go"))
	var slp *TimerPending
	if !errors.As(err, &slp) {
		t.Fatalf("expected *TimerPending, got %v", err)
	}
	wake := time.Unix(1000+3600, 0)
	if !slp.FireAt.Equal(wake) {
		t.Fatalf("wake time = %s, want %s", slp.FireAt, wake)
	}

	// Resume 30 minutes in: still before the wake time, and the wake time did NOT move (at-most-once).
	atomic.StoreInt64(&clk, 1000+1800)
	_, err = a.Run(ctx, "r1", UserText("go"))
	if !errors.As(err, &slp) {
		t.Fatalf("should still be sleeping at +30m, got %v", err)
	}
	if !slp.FireAt.Equal(wake) {
		t.Fatalf("wake time drifted to %s on resume; it must stay %s", slp.FireAt, wake)
	}

	// At the wake time: the run resumes and completes.
	atomic.StoreInt64(&clk, 1000+3600)
	res, err := a.Run(ctx, "r1", UserText("go"))
	if err != nil {
		t.Fatalf("run should resume once due, got %v", err)
	}
	msg := res.Message
	if msg.Text() != "done" {
		t.Fatalf("resumed run should finish, got %q", msg.Text())
	}
}

// TestMemWaker_FiresDueRun confirms the reference waker resumes a sleeping run once its timer is due.
func TestMemWaker_FiresDueRun(t *testing.T) {
	var clk int64 = 1000
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	a := mustNew(sleepModel{}, memJournal(), WithTools(waitTool()))

	var w *MemWaker
	w = NewMemWaker(func(ctx context.Context, runID string) error {
		rctx := contextWithWaker(contextWithClock(ctx, now), w) // resumed run can reschedule if it sleeps again
		_, err := a.Run(rctx, runID, UserText("go"))
		return err
	})
	ctx := contextWithWaker(contextWithClock(context.Background(), now), w)

	// First run pauses and registers a wake with the waker.
	if _, err := a.Run(ctx, "r1", UserText("go")); !errorsIsSleeping(err) {
		t.Fatalf("expected the run to sleep, got %v", err)
	}

	// Not due yet: Fire resumes nothing.
	if n, err := w.Fire(context.Background(), now()); n != 0 || err != nil {
		t.Fatalf("nothing should fire before the wake time, fired %d err %v", n, err)
	}

	// Advance past the wake time and Fire: the run resumes to completion.
	atomic.StoreInt64(&clk, 1000+3600)
	n, err := w.Fire(context.Background(), now())
	if err != nil || n != 1 {
		t.Fatalf("waker should resume exactly one run, fired %d err %v", n, err)
	}
	// The resumed run completed: a replay now returns the final answer with no pause.
	res, err := a.Run(context.Background(), "r1", UserText("go"), WithClock(now))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "done" {
		t.Fatalf("run should be complete after the waker fired it, got %q err %v", msg.Text(), err)
	}
}

func errorsIsSleeping(err error) bool {
	var slp *TimerPending
	return errors.As(err, &slp)
}

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

// A recorded expiry is final whatever the deciding process's clock says: SubmitDecision with a clock
// still before the deadline refuses, and Approve on a one-decision gate whose expiry holds the
// decision's name reports it.
func TestDeadline_RecordedExpiryRefusesASkewedClock(t *testing.T) {
	early := &testClock{t: deadlineT0.Add(10 * time.Minute)}
	t.Run("m-of-n", func(t *testing.T) {
		store := memJournal()
		clk := &testClock{t: deadlineT0}
		pol := &ApprovalPolicy{Need: 2, Approvers: abc, Timeout: time.Hour}
		vf := fakeVerifiers(abc...)
		var charged int
		_, _ = deadlineRun(clk, store, "r1", true, pol, vf, &charged)
		clk.t = deadlineT0.Add(2 * time.Hour)
		_, _ = deadlineRun(clk, store, "r1", false, pol, vf, &charged)
		sig := fakeSign("bob", ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), "bob", true, testDecidedAt))
		if err := decideAs(contextWithClock(context.Background(), early.now), store, "r1", "c1", "bob", true, sig); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("SubmitDecision with a clock before the deadline after the recorded expiry = %v; want ErrApprovalExpired", err)
		}
	})
	t.Run("single", func(t *testing.T) {
		store := memJournal()
		clk := &testClock{t: deadlineT0}
		pol := SingleApproval()
		pol.Timeout = time.Hour
		var charged int
		_, _ = deadlineRun(clk, store, "r1", true, pol, nil, &charged)
		clk.t = deadlineT0.Add(2 * time.Hour)
		_, _ = deadlineRun(clk, store, "r1", false, pol, nil, &charged)
		if err := Approve(contextWithClock(context.Background(), early.now), store, "r1", "c1", true); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("Approve with a clock before the deadline after the recorded expiry = %v; want ErrApprovalExpired", err)
		}
	})
}

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

// R117-1. The late-error rule is "an error returned after the deadline has an unknown outcome",
// but callTool detects it by tctx.Err(), which is set only when the context's timer goroutine has
// run. A tool that honors its deadline from ctx.Deadline() (its own client timeout, a conn
// deadline, a poll loop) and returns an error once the deadline has passed, before that timer
// callback has run, is recorded as an ordinary failure. For a side effect that tells the model
// the call failed, inviting a second charge.
//
// GOMAXPROCS(1) makes it deterministic: the tool busy-waits past the deadline without yielding,
// so the timer callback cannot run before callTool reads tctx.Err().
//
// The busy-wait needs the wall clock (in a synctest bubble time.Now would not advance while the
// tool spins), and on the wall clock the 2ms deadline can pass while the call is dispatched: the
// base handler then rightly refuses the call as not started, the tool never runs, and the attempt
// says nothing about the late rule. Such an attempt is checked to be exactly that refusal and run
// again; the test fails if no attempt in 100 reaches the tool.
func TestR117_LateErrorBeforeTheTimerFiresIsRecordedAsAFailure(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for range 100 {
		var calls atomic.Int32
		charge := MustFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
			calls.Add(1)
			dl, _ := ctx.Deadline()
			for time.Now().Before(dl) { // the provider call is in flight until the deadline
			}
			return "", errors.New("gateway: client timeout awaiting response")
		}, WithTimeout(2*time.Millisecond))
		store := memJournal()
		m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
		_, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
		rec, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
		if calls.Load() == 0 {
			// Not reached: the deadline passed before dispatch. That is a known failure, recorded.
			if err != nil || !ok || !rec.IsError || !strings.Contains(string(rec.Result), "not started") {
				t.Fatalf("a call that never reached its tool: Run err = %v, result %s (recorded %v); want it recorded as not started", err, rec.Result, ok)
			}
			continue
		}
		if ok {
			t.Fatalf("the side effect's error after its deadline was recorded as a failure (%s, is_error %v); Run err = %v; want nothing recorded and ErrToolOutcomeUnknown",
				rec.Result, rec.IsError, err)
		}
		if !errors.Is(err, ErrToolOutcomeUnknown) {
			t.Fatalf("Run: err = %v, want ErrToolOutcomeUnknown", err)
		}
		return
	}
	t.Fatal("no attempt in 100 reached the tool before its deadline")
}

// A wake whose resume fails transiently (a model provider blip, a store timeout) must be retried
// on a later Fire; otherwise the run stays asleep with nothing left to wake it.
func TestMemWaker_RetriesAFailedResume(t *testing.T) {
	ctx := context.Background()
	calls := 0
	w := NewMemWaker(func(context.Context, string) error {
		calls++
		if calls == 1 {
			return errors.New("model provider unavailable")
		}
		return nil
	})
	due := time.Unix(1000, 0)
	w.Schedule(context.Background(), Wake{RunID: "r1", Name: "nap", FireAt: due})

	if _, err := w.Fire(ctx, due); err == nil {
		t.Fatal("first Fire: want the resume error reported")
	}
	if _, err := w.Fire(ctx, due.Add(time.Second)); err != nil {
		t.Fatalf("second Fire: %v", err)
	}
	if calls != 2 {
		t.Fatalf("resume called %d times, want 2: after one failed resume the wake was dropped and the run would never wake", calls)
	}
	if _, err := w.Fire(ctx, due.Add(2*time.Second)); err != nil || calls != 2 {
		t.Fatalf("after a successful resume, another Fire resumed again (calls=%d, err=%v); want the wake consumed", calls, err)
	}
}

// A resume that ends in a durable pause (the run slept again, or awaits a human) is not a failure:
// the wake is consumed, not retried every tick.
func TestMemWaker_PauseIsNotRetried(t *testing.T) {
	ctx := context.Background()
	for name, pause := range map[string]error{
		"sleeping":         &TimerPending{RunRef: RunRef{RunID: "r1"}, Name: "next"},
		"pending approval": &ApprovalPending{RunRef: RunRef{RunID: "r1"}},
	} {
		calls := 0
		w := NewMemWaker(func(context.Context, string) error { calls++; return pause })
		due := time.Unix(1000, 0)
		w.Schedule(context.Background(), Wake{RunID: "r1", Name: "nap", FireAt: due})
		_, _ = w.Fire(ctx, due)
		_, _ = w.Fire(ctx, due.Add(time.Second))
		if calls != 1 {
			t.Fatalf("%s: resume called %d times, want 1 (a paused run must not be re-woken every tick)", name, calls)
		}
	}
}

// If the run re-registered the same timer for a later time during the failed attempt, that newer
// wake stands; the stale due one is not put back over it.
func TestMemWaker_RetryKeepsARescheduledWake(t *testing.T) {
	ctx := context.Background()
	due := time.Unix(1000, 0)
	later := due.Add(time.Hour)
	var w *MemWaker
	calls := 0
	w = NewMemWaker(func(context.Context, string) error {
		calls++
		w.Schedule(context.Background(), Wake{RunID: "r1", Name: "nap", FireAt: later}) // the run got far enough to re-register its wake, then failed
		return errors.New("store timeout")
	})
	w.Schedule(context.Background(), Wake{RunID: "r1", Name: "nap", FireAt: due})
	_, _ = w.Fire(ctx, due)
	if _, _ = w.Fire(ctx, due.Add(time.Minute)); calls != 1 {
		t.Fatalf("resume called %d times before the rescheduled time, want 1 (the stale wake overwrote the newer one)", calls)
	}
	_, _ = w.Fire(ctx, later)
	if calls != 2 {
		t.Fatalf("resume called %d times at the rescheduled time, want 2", calls)
	}
}

// After cancelling Start's context, a caller can wait for the loop to stop: the channel Start
// returns closes only once a resume that was in flight has returned, so the store it writes to
// can be closed safely afterwards.
func TestMemWaker_StartReportsWhenStopped(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var finished bool
	w := NewMemWaker(func(context.Context, string) error {
		close(entered)
		<-release // a resume still writing its journal
		finished = true
		return nil
	})
	w.Schedule(context.Background(), Wake{RunID: "r1", Name: "nap", FireAt: time.Now().Add(-time.Second)})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := w.Start(ctx, time.Millisecond, nil)
	<-entered
	cancel()
	select {
	case <-stopped:
		t.Fatal("Start reported stopped while a resume was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Start never reported stopped")
	}
	if !finished {
		t.Fatal("stopped closed before the in-flight resume returned")
	}
}

// Two sub-agents of one root each wait on a timer of the same name, one due in an hour and one in
// two. Both wakes resume the root, so the Waker must hold both: if the later one replaced the
// earlier, the first sub-agent would sleep an hour past its wake time.
func TestWaker_SameNamedTimersInTwoSubRunsAreDistinct(t *testing.T) {
	for _, wait := range map[string]func(ctx context.Context, d time.Duration) error{
		"Sleep": func(ctx context.Context, d time.Duration) error { return Sleep(ctx, "nap", d) },
		"AwaitFor": func(ctx context.Context, d time.Duration) error {
			_, _, err := AwaitFor[string](ctx, "nap", d)
			return err
		},
	} {
		store := memJournal()
		t0 := time.Unix(1_000_000, 0)
		var resumed []string
		w := NewMemWaker(func(_ context.Context, runID string) error { resumed = append(resumed, runID); return nil })
		root := withRunContext(contextWithWaker(contextWithClock(context.Background(), func() time.Time { return t0 }), w), store, "root", "", false)
		for i, sub := range []string{"a", "b"} {
			ctx := withRunContext(root, store, SubRunID("root", sub), "", false)
			if err := wait(ctx, time.Duration(i+1)*time.Hour); !IsPause(err) {
				t.Fatalf("sub-run %s: wait = %v, want a pause", sub, err)
			}
		}
		if n, err := w.Fire(context.Background(), t0.Add(time.Hour)); n != 1 || err != nil {
			t.Fatalf("Fire at the first wake = %d, %v; want the root resumed (the second sub-run's timer replaced the first's)", n, err)
		}
		if n, err := w.Fire(context.Background(), t0.Add(2*time.Hour)); n != 1 || err != nil {
			t.Fatalf("Fire at the second wake = %d, %v; want the root resumed again", n, err)
		}
		if len(resumed) != 2 || resumed[0] != "root" || resumed[1] != "root" {
			t.Fatalf("resumed %v, want the root twice", resumed)
		}
	}
}

// RecoverLoop gives a user's error handler a failure only after the run's slot and its in-flight mark
// are released: a handler that waits (here for the next run to be driven, with one slot) must not
// hold up the very work it is waiting for.
func TestRecoverLoop_TheErrorHandlerRunsAfterTheRunsSlotIsReleased(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := memJournal()
	for _, run := range []string{"a", "b"} {
		if _, _, err := j.putEntry(ctx, run, runStartStep, Record{Kind: StepValue, Result: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
	}
	bDriven := make(chan struct{})
	resume := func(_ context.Context, run string, _ RunStart) error {
		if run == "b" {
			select {
			case <-bDriven:
			default:
				close(bDriven)
			}
			return nil
		}
		return errors.New("a fails")
	}
	handled := make(chan error, 1)
	handler := func(error) {
		select {
		case <-bDriven:
			handled <- nil
		case <-time.After(5 * time.Second):
			handled <- errors.New("the handler waited 5s for the next run to be driven: it ran while holding the only slot")
		}
	}
	go func() {
		_ = RecoverLoop(ctx, j, resume, WithRecoverConcurrency(1), WithRecoverInterval(time.Hour), WithRecoverErrors(handler))
	}()
	if err := <-handled; err != nil {
		t.Fatal(err)
	}
}

// A flush called from inside the WithNotifyErrors handler would wait for the very round that is
// calling the handler: it returns an error at once instead. The round finishes, and a flush from
// anywhere else then settles.
func TestNotifier_AFlushFromTheErrorHandlerFailsAtOnceInsteadOfDeadlocking(t *testing.T) {
	var e *notifyEngine
	type outcome struct {
		err  error
		took time.Duration
	}
	got := make(chan outcome, 1)
	handler := func(error) {
		start := time.Now()
		err := e.flush(context.Background()) // no deadline: a deadlock would hang here
		select {
		case got <- outcome{err, time.Since(start)}:
		default:
		}
	}
	sink := &fakeSink{fail: 1000}
	trail := NewMemNotifyTrail()
	e = testEngine(t, memJournal(), sink, trail, resolvedClock(), "p1", WithNotifyErrors(handler), WithNotifyRetry(time.Hour, time.Hour))
	e.w.submit(haltEvent())
	select {
	case o := <-got:
		if !errors.Is(o.err, ErrConfig) || !strings.Contains(o.err.Error(), "WithNotifyErrors") || o.took > time.Second {
			t.Fatalf("flush from the handler = %v after %v, want an ErrConfig naming WithNotifyErrors at once", o.err, o.took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the flush from the handler hung")
	}
	// The round finished: it is no longer in flight, and a flush from outside the handler settles.
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.flush(fctx); errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the round never finished: %v", err)
	}
	if recs, _ := trail.Load(context.Background(), "notify:r"); len(recs) < 2 {
		t.Fatalf("trail = %v: the round did not run", recKinds(recs))
	}
	if sink.count() == 0 {
		t.Fatal("nothing was delivered")
	}
}
