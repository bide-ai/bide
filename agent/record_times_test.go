package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The records that resolve something a person was told about (a single approver's decision,
// ResolveHalt's result, a run's end) note when they were written, in recorded_at. These tests
// specify the time, the clock it comes from, and that nothing reads it.

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

var (
	timesA = time.Date(2026, 10, 14, 9, 30, 15, 123_999_000, time.FixedZone("east", 2*3600)) // 07:30:15.123Z
	timesB = time.Date(2026, 10, 14, 10, 0, 0, 456_000_000, time.UTC)
	timesC = time.Date(2026, 10, 14, 11, 0, 0, 789_000_000, time.UTC)
)

const (
	stampA = "2026-10-14T07:30:15.123Z"
	stampB = "2026-10-14T10:00:00.456Z"
	stampC = "2026-10-14T11:00:00.789Z"
)

// A single approver's decision (Approve, an approval, a denial) carries recorded_at: WithClock's
// clock, else the context's, else the wall clock, in the one canonical form. A repeated call
// keeps the first decision and its time. It is a record with no approver, so it is not an m-of-n
// decision.
func TestRecordTimes_SingleApproval(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if err := Approve(ctx, j, "r", "c1", true, WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	if err := Approve(ctx, j, "r", "c1", true, WithClock(fixedClock(timesB))); err != nil {
		t.Fatal(err)
	}
	if err := Approve(ctx, j, "r", "c2", false, WithClock(fixedClock(timesB))); err != nil {
		t.Fatal(err)
	}
	if err := Approve(contextWithClock(ctx, fixedClock(timesC)), j, "r", "c3", true); err != nil {
		t.Fatal(err)
	}
	// The option beats the context's clock.
	if err := Approve(contextWithClock(ctx, fixedClock(timesC)), j, "r", "c4", true, WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	if err := Approve(ctx, j, "r", "c5", true); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(time.Second)
	for id, want := range map[string]string{"c1": stampA, "c2": stampB, "c3": stampC, "c4": stampA} {
		rec, ok := hasStep(t, j, "r", approvalStep(id))
		if !ok || rec.Kind != StepApproval || rec.RecordedAt() != want {
			t.Errorf("%s: approval %+v (found %v), want recorded_at %q", id, rec, ok, want)
		}
		if rec.Approver() != "" || rec.Signature() != nil || rec.DecidedAt() != "" || IsApprovalDecision(rec, id) {
			t.Errorf("%s: a single approver's decision reads as a signed one: %+v", id, rec)
		}
	}
	if rec, _ := hasStep(t, j, "r", approvalStep("c2")); rec.Approved {
		t.Error("c2: the denial was recorded as an approval")
	}
	rec, _ := hasStep(t, j, "r", approvalStep("c5"))
	at, err := ParseTimestamp(rec.RecordedAt())
	if err != nil || at.Before(before) || at.After(after) {
		t.Errorf("wall clock: recorded_at %q (%v), want a canonical time within the call", rec.RecordedAt(), err)
	}
}

// Approve does not fail for want of a time: a clock with no canonical form records none. A nil
// clock, and a decision check (which belongs to SubmitDecision), are ErrConfig and record nothing.
func TestRecordTimes_SingleApprovalClockFaults(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := Approve(ctx, j, "r", "c1", true, WithClock(fixedClock(far))); err != nil {
		t.Fatalf("Approve with an out-of-range clock = %v, want it recorded without a time", err)
	}
	if rec, ok := hasStep(t, j, "r", approvalStep("c1")); !ok || !rec.Approved || rec.RecordedAt() != "" || rec.ApproverSignature != nil {
		t.Fatalf("approval = %+v (found %v), want an approval with no time", rec, ok)
	}
	for name, opt := range map[string]ApproveOption{
		"nil clock":      WithClock(nil),
		"decision check": WithDecisionCheck(func(string) (ApproverVerifier, bool) { return nil, false }),
	} {
		if err := Approve(ctx, j, "r", "c2", true, opt); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: Approve = %v, want ErrConfig", name, err)
		}
		if _, ok := hasStep(t, j, "r", approvalStep("c2")); ok {
			t.Errorf("%s: a refused Approve recorded a decision", name)
		}
	}
	if err := Approve(ctx, j, "r", "c3", true, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Approve with a nil option = %v, want ErrConfig", err)
	}
}

// ResolveHalt's recorded outcome, for a tool call and for a Step, carries recorded_at: WithClock's
// clock, else the wall clock (never the context's, which is the run's: the resolver is not a drive). The resolver signs nothing, so the record has
// no approver or signature.
func TestRecordTimes_ResolveHalt(t *testing.T) {
	ctx := context.Background()
	stepRef := HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: "charge"}, Cause: HaltCrashed}
	mk := func() *Journal {
		j := markerAt(t, time.Now().UnixMilli())
		jj := mustJournal(j)
		if _, err := jj.do(ctx, "r", stepAttemptStep("charge"), func(context.Context) (Record, error) {
			return Record{Kind: StepAttempt, ToolUseID: "charge", AttemptedAt: time.Now().UnixMilli()}, nil
		}); err != nil {
			t.Fatal(err)
		}
		return jj
	}
	for name, c := range map[string]struct {
		ref   HaltRef
		key   string
		ctx   context.Context
		opts  []ResolveOption
		want  string
		recon bool
	}{
		"tool, WithClock":        {covHalt, ToolResultStep("c1"), ctx, []ResolveOption{WithClock(fixedClock(timesA))}, stampA, false},
		"tool, option beats ctx": {covHalt, ToolResultStep("c1"), contextWithClock(ctx, fixedClock(timesB)), []ResolveOption{WithClock(fixedClock(timesC))}, stampC, false},
		"step, WithClock":        {stepRef, "charge", ctx, []ResolveOption{WithClock(fixedClock(timesA))}, stampA, false},
		"reconciled":             {covHalt, ToolResultStep("c1"), ctx, []ResolveOption{WithEvidence("provider says charged"), WithClock(fixedClock(timesC))}, stampC, true},
	} {
		j := mk()
		if err := ResolveHalt(c.ctx, j, c.ref, Outcome{Result: "charged"}, append(c.opts, WithoutLiveDriverCheck())...); err != nil {
			t.Fatalf("%s: ResolveHalt = %v", name, err)
		}
		rec, ok := hasStep(t, j, "r", c.key)
		if !ok || rec.RecordedAt() != c.want || rec.Reconciled != c.recon {
			t.Errorf("%s: result %+v (found %v), want recorded_at %q, reconciled %v", name, rec, ok, c.want, c.recon)
		}
		if rec.Approver() != "" || rec.Signature() != nil {
			t.Errorf("%s: the resolver's record claims a signature: %+v", name, rec)
		}
		// Resolving again with the same outcome is a no-op that keeps the first time.
		if err := ResolveHalt(contextWithClock(ctx, fixedClock(timesC.Add(time.Hour))), j, c.ref, Outcome{Result: "charged"}, WithoutLiveDriverCheck(), WithClock(fixedClock(timesC.Add(time.Hour)))); err != nil {
			t.Fatalf("%s: second ResolveHalt = %v", name, err)
		}
		if again, _ := hasStep(t, j, "r", c.key); again.RecordedAt() != c.want {
			t.Errorf("%s: a repeated resolution changed recorded_at to %q", name, again.RecordedAt())
		}
	}
	// The wall clock, when nothing names one.
	j := mk()
	before := time.Now().Add(-time.Second)
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	rec, _ := hasStep(t, j, "r", ToolResultStep("c1"))
	if at, err := ParseTimestamp(rec.RecordedAt()); err != nil || at.Before(before) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("wall clock: recorded_at %q (%v)", rec.RecordedAt(), err)
	}
	// A clock with no canonical form records no time, and the resolution still happens.
	j = mk()
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck(), WithClock(fixedClock(far))); err != nil {
		t.Fatalf("ResolveHalt with an out-of-range clock = %v", err)
	}
	if rec, ok := hasStep(t, j, "r", ToolResultStep("c1")); !ok || rec.RecordedAt() != "" || rec.ApproverSignature != nil {
		t.Errorf("out-of-range clock: %+v (found %v), want a result with no time", rec, ok)
	}
}

// The context's clock neither stamps the resolution nor measures a halt's age: WithMinHaltAge
// compares the attempt marker's wall-clock time with the clock WithClock names, else the wall
// clock, as it did before the resolution carried a time.
func TestRecordTimes_ResolveHaltAgeIgnoresContextClock(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(markerAt(t, time.Now().UnixMilli()))
	future := contextWithClock(ctx, fixedClock(time.Now().Add(240*time.Hour)))
	err := ResolveHalt(future, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck(), WithMinHaltAge(time.Hour))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("ResolveHalt with a context clock ten days ahead = %v, want *HaltTooYoung (the age is the wall clock's)", err)
	}
	if _, ok := hasStep(t, j, "r", ToolResultStep("c1")); ok {
		t.Fatal("a refused resolution recorded a result")
	}
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck(), WithMinHaltAge(time.Hour), WithClock(fixedClock(time.Now().Add(2*time.Hour)))); err != nil {
		t.Fatalf("ResolveHalt with WithClock two hours ahead = %v, want it resolved", err)
	}
}

// A run's completion records recorded_at, by the agent's clock, the run's, or the wall clock, and
// Status reports it as EndedAt. A drive that finds the run complete writes no second marker and
// keeps the time.
func TestRecordTimes_RunComplete(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		ctx  context.Context
		opts []Option
		want string
	}{
		"agent clock":   {ctx, []Option{WithClock(fixedClock(timesA))}, stampA},
		"context clock": {contextWithClock(ctx, fixedClock(timesB)), nil, stampB},
	} {
		j := memJournal()
		var n int
		a := mustNew(lookupRefund, j, append([]Option{refundTools(&n, &n, nil)}, c.opts...)...)
		if _, err := a.Run(c.ctx, "r", UserText("go")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rec, ok := hasStep(t, j, "r", runCompleteStep)
		if !ok || rec.RecordedAt() != c.want {
			t.Errorf("%s: run:complete %+v (found %v), want recorded_at %q", name, rec, ok, c.want)
		}
		if st, err := Status(ctx, j, "r"); err != nil || st.State != RunCompleted || st.EndedAt != c.want {
			t.Errorf("%s: Status = %+v, %v, want completed at %q", name, st, err, c.want)
		}
		// A later drive of the finished run keeps the first marker.
		if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil), WithClock(fixedClock(timesC))).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatalf("%s: redrive: %v", name, err)
		}
		if again, _ := hasStep(t, j, "r", runCompleteStep); again.RecordedAt() != c.want {
			t.Errorf("%s: a later drive changed run:complete's time to %q", name, again.RecordedAt())
		}
	}
}

// Cancel records recorded_at on run:cancelled (WithClock, else the context's clock). On a saga it
// stamps the rollback request when requested, and the run:cancelled the finished rollback writes
// when the rollback finished, so Status reports the end, not the request.
func TestRecordTimes_Cancel(t *testing.T) {
	ctx := context.Background()
	j, _ := pcJournal(t)
	pcStart(t, j, "plain", false)
	if err := Cancel(ctx, j, "plain", "stop", WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	pcStart(t, j, "viactx", false)
	if err := Cancel(contextWithClock(ctx, fixedClock(timesB)), j, "viactx", "stop"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"plain": stampA, "viactx": stampB} {
		rec, ok := hasStep(t, j, id, runCancelledStep)
		if !ok || rec.RecordedAt() != want {
			t.Errorf("%s: run:cancelled %+v (found %v), want recorded_at %q", id, rec, ok, want)
		}
		if st, _ := Status(ctx, j, id); st.State != RunCancelled || st.EndedAt != want || st.Terminal != "stop" {
			t.Errorf("%s: Status = %+v, want cancelled at %q", id, st, want)
		}
		var v cancelReason
		if err := json.Unmarshal(rec.Result, &v); err != nil || v.Reason != "stop" {
			t.Errorf("%s: the reason moved: %s (%v)", id, rec.Result, err)
		}
	}
	// Cancelling a cancelled run again keeps the first time.
	if err := Cancel(ctx, j, "plain", "again", WithClock(fixedClock(timesC))); err != nil {
		t.Fatal(err)
	}
	if rec, _ := hasStep(t, j, "plain", runCancelledStep); rec.RecordedAt() != stampA {
		t.Errorf("a second Cancel changed the time to %q", rec.RecordedAt())
	}
	// A saga: the request has its time, and the rollback's end marker its own.
	pcStart(t, j, "saga", true)
	if err := Cancel(ctx, j, "saga", "stop", WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	if req, ok := hasStep(t, j, "saga", runCancelRequestedStep); !ok || req.RecordedAt() != stampA {
		t.Errorf("run:cancel-requested %+v (found %v), want recorded_at %q", req, ok, stampA)
	}
	if st, _ := Status(ctx, j, "saga"); st.State != RunStarted || st.EndedAt != "" {
		t.Errorf("Status of a saga with a pending request = %+v, want started with no end time", st)
	}
	for name, opt := range map[string]CancelOption{"nil clock": WithClock(nil)} {
		if err := Cancel(ctx, j, "plain", "x", opt); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: Cancel = %v, want ErrConfig", name, err)
		}
	}
	if err := Cancel(ctx, j, "plain", "x", nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Cancel with a nil option = %v, want ErrConfig", err)
	}
}

// A saga's rollback writes run:aborted or run:cancelled with the rollback's own time: by the
// drive's clock, once, so aborted_at and recorded_at agree.
func TestRecordTimes_SagaEnd(t *testing.T) {
	ctx := contextWithClock(context.Background(), fixedClock(timesB))
	j := memJournal()
	a := mustNew(turnModel{toolTurn("c1", "boom", `{}`), textTurn("x")}, j, WithTools(failTool("boom")))
	_, err := a.Run(ctx, "r", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("Run = %v, want *SagaAborted", err)
	}
	rec, ok := hasStep(t, j, "r", runAbortedStep)
	if !ok || rec.RecordedAt() != stampB {
		t.Fatalf("run:aborted %+v (found %v), want recorded_at %q", rec, ok, stampB)
	}
	var v abortValue
	if err := json.Unmarshal(rec.Result, &v); err != nil || v.AbortedAt != stampB || v.Reason == "" {
		t.Fatalf("run:aborted value %s: %+v (%v), want aborted_at %q beside the reason", rec.Result, v, err, stampB)
	}
	if st, _ := Status(ctx, j, "r"); st.State != RunAborted || st.EndedAt != stampB {
		t.Fatalf("Status = %+v, want aborted at %q", st, stampB)
	}
}

// The aborted_at and recorded_at of a saga's run:aborted come from one reading of the clock, so
// they agree even when the clock moves between calls.
func TestRecordTimes_SagaEndOneReading(t *testing.T) {
	var n int
	tick := func() time.Time { n++; return timesB.Add(time.Duration(n) * time.Second) }
	ctx := contextWithClock(context.Background(), tick)
	j := memJournal()
	a := mustNew(turnModel{toolTurn("c1", "boom", `{}`), textTurn("x")}, j, WithTools(failTool("boom")))
	if _, err := a.Run(ctx, "r", UserText("go"), WithSaga()); err == nil {
		t.Fatal("want the saga to abort")
	}
	rec, _ := hasStep(t, j, "r", runAbortedStep)
	var v abortValue
	if err := json.Unmarshal(rec.Result, &v); err != nil || v.AbortedAt == "" || rec.RecordedAt() != v.AbortedAt {
		t.Fatalf("aborted_at %q and recorded_at %q differ (%v)", v.AbortedAt, rec.RecordedAt(), err)
	}
}

// Status never supplies a time: an end marker written before they carried one reports none, a
// run:aborted that has only its aborted_at reports that, and a recorded_at that is not canonical
// is not reported.
func TestRecordTimes_StatusNeverInvents(t *testing.T) {
	ctx := context.Background()
	j, _ := pcJournal(t)
	pcStart(t, j, "old", false)
	pcMark(t, j, "old", runCompleteStep, "")
	if st, _ := Status(ctx, j, "old"); st.State != RunCompleted || st.EndedAt != "" {
		t.Errorf("an end marker with no time: Status = %+v, want no EndedAt", st)
	}
	pcStart(t, j, "oldabort", true)
	if _, err := j.put(ctx, "oldabort", runAbortedStep, Record{Kind: StepValue, Result: []byte(`{"reason":"x","aborted_at":"` + stampC + `"}`)}); err != nil {
		t.Fatal(err)
	}
	if st, _ := Status(ctx, j, "oldabort"); st.EndedAt != stampC {
		t.Errorf("a run:aborted with only aborted_at: EndedAt = %q, want %q", st.EndedAt, stampC)
	}
	pcStart(t, j, "oldstr", true)
	pcMark(t, j, "oldstr", runAbortedStep, "step failed")
	if st, _ := Status(ctx, j, "oldstr"); st.State != RunAborted || st.EndedAt != "" || st.Terminal != "step failed" {
		t.Errorf("a run:aborted that is a bare string: Status = %+v", st)
	}
	for i, bad := range []string{"yesterday", "2026-10-14T07:30:15Z", "2026-10-14T07:30:15.123+00:00", " "} {
		id := fmt.Sprintf("bad%d", i)
		pcStart(t, j, id, false)
		if _, err := j.put(ctx, id, runCancelledStep, Record{Kind: StepValue, Result: []byte(`{"reason":"x"}`), ApproverSignature: &ApproverSignature{RecordedAt: bad}}); err != nil {
			t.Fatal(err)
		}
		if st, _ := Status(ctx, j, id); st.State != RunCancelled || st.EndedAt != "" {
			t.Errorf("recorded_at %q: Status = %+v, want cancelled with no EndedAt", bad, st)
		}
	}
}

// No drive, gate, claim or recovery pass reads recorded_at: the same journal driven on with the
// time as written, absent, different, and not a time does and journals the same.
func TestRecordTimes_NotRead(t *testing.T) {
	ctx := context.Background()
	variants := map[string]func(*ApproverSignature) *ApproverSignature{
		"as written": func(s *ApproverSignature) *ApproverSignature { return s },
		"absent":     func(*ApproverSignature) *ApproverSignature { return nil },
		"different": func(*ApproverSignature) *ApproverSignature {
			return &ApproverSignature{RecordedAt: "1999-01-01T00:00:00.000Z"}
		},
		"far future": func(*ApproverSignature) *ApproverSignature {
			return &ApproverSignature{RecordedAt: "9999-12-31T23:59:59.999Z"}
		},
		"not a time": func(*ApproverSignature) *ApproverSignature {
			return &ApproverSignature{RecordedAt: "not a time \x00 ‮"}
		},
	}
	// rewrite replaces the stamp on every record of run "r" that carries a lone recorded_at or is
	// named in names.
	rewrite := func(t *testing.T, src *Journal, names map[string]bool, edit func(*ApproverSignature) *ApproverSignature) *Journal {
		t.Helper()
		out := memJournal()
		recs, err := src.History(ctx, "r")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Kind == StepHeader {
				continue
			}
			if names[r.Name] || r.ApproverSignature != nil && r.Approver() == "" {
				r.ApproverSignature = edit(r.ApproverSignature)
			}
			r.salt, r.raw = nil, nil
			if _, err := out.put(ctx, "r", r.Name, r); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	type outcome struct {
		text             string
		err              string
		lookups, refunds int
	}
	cases := map[string]struct {
		setup func(t *testing.T) *Journal
		drive func(t *testing.T, j *Journal) outcome
		names []string
	}{
		"single approval": {
			setup: func(t *testing.T) *Journal {
				j := memJournal()
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, SingleApproval())).Run(ctx, "r", UserText("go")); !isPending(err) {
					t.Fatalf("setup: %v", err)
				}
				if err := Approve(ctx, j, "r", "c2", true, WithClock(fixedClock(timesA))); err != nil {
					t.Fatal(err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, SingleApproval())).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				return o
			},
			names: []string{approvalStep("c2")},
		},
		"single denial": {
			setup: func(t *testing.T) *Journal {
				j := memJournal()
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, SingleApproval())).Run(ctx, "r", UserText("go")); !isPending(err) {
					t.Fatalf("setup: %v", err)
				}
				if err := Approve(ctx, j, "r", "c2", false, WithClock(fixedClock(timesA))); err != nil {
					t.Fatal(err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, SingleApproval())).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				if o.refunds != 0 {
					t.Errorf("a denied refund ran")
				}
				return o
			},
			names: []string{approvalStep("c2")},
		},
		"resolved halt": {
			setup: func(t *testing.T) *Journal {
				s := &loseOnce{Store: NewMemStore(), lose: ToolResultStep("c2")}
				j := mustJournal(s)
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "r", UserText("go")); !errors.Is(err, errLost) {
					t.Fatalf("setup: %v", err)
				}
				if err := ResolveHalt(ctx, mustJournal(s.Store), HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c2", ToolName: "refund"}, Cause: HaltCrashed}, Outcome{Result: "refunded elsewhere"}, WithClock(fixedClock(timesA)), WithoutLiveDriverCheck()); err != nil {
					t.Fatal(err)
				}
				return mustJournal(s.Store)
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, nil)).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				return o
			},
			names: []string{ToolResultStep("c2")},
		},
		"finished run": {
			setup: func(t *testing.T) *Journal {
				j := memJournal()
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil), WithClock(fixedClock(timesA))).Run(ctx, "r", UserText("go")); err != nil {
					t.Fatal(err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, nil)).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				if done, err := IsComplete(ctx, j, "r"); err != nil || !done {
					t.Errorf("IsComplete = %v, %v", done, err)
				}
				return o
			},
			names: []string{runCompleteStep},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := c.setup(t)
			names := map[string]bool{}
			for _, n := range c.names {
				names[n] = true
			}
			var want outcome
			for _, v := range []string{"as written", "absent", "different", "far future", "not a time"} {
				j := rewrite(t, src, names, variants[v])
				got := c.drive(t, j)
				if v == "as written" {
					want = got
				} else if got != want {
					t.Errorf("recorded_at %s: %+v, want %+v", v, got, want)
				}
			}
		})
	}
}

func isPending(err error) bool {
	_, ok := errors.AsType[*ApprovalPending](err)
	return ok
}

// A record that carries recorded_at alone is written and read back as itself: the encoding holds
// one top-level member and nothing for the signature, a decoded record encodes to the same bytes,
// and one written without it decodes with none. A time-only stamp is not a signed decision.
func TestRecordTimes_Encoding(t *testing.T) {
	rec := Record{Name: "approval:c1", Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{RecordedAt: stampA}}
	b, err := EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"approval:c1","kind":"approval","tool_use_id":"c1","approved":true,"recorded_at":"` + stampA + `"}`
	if string(b) != want {
		t.Fatalf("encoding = %s, want %s", b, want)
	}
	for _, m := range []string{"approver", "signature", "decided_at"} {
		if bytes.Contains(b, []byte(m)) {
			t.Fatalf("a time-only stamp wrote %q: %s", m, b)
		}
	}
	back, err := DecodeStoredRecord("r", "approval:c1", b)
	if err != nil {
		t.Fatal(err)
	}
	if back.RecordedAt() != stampA || back.Approver() != "" || back.DecidedAt() != "" || !back.Approved {
		t.Fatalf("decoded %+v", back)
	}
	if again, _ := EncodeRecord(back); !bytes.Equal(again, b) {
		t.Fatalf("decode then encode changed the bytes:\n %s\n %s", b, again)
	}
	// A record written before the time existed has none, and gains none on a round trip.
	old, err := decodeTimes(`{"name":"approval:c1","kind":"approval","tool_use_id":"c1","approved":true}`)
	if err != nil || old.RecordedAt() != "" || old.ApproverSignature != nil {
		t.Fatalf("old approval %+v, %v: want no time", old, err)
	}
	if again, _ := EncodeRecord(old); strings.Contains(string(again), "recorded_at") {
		t.Fatalf("a round trip invented a time: %s", again)
	}
	empty, err := decodeTimes(`{"name":"run:complete","kind":"value","recorded_at":""}`)
	if err != nil || empty.ApproverSignature != nil {
		t.Fatalf("an empty recorded_at left a signature: %+v, %v", empty, err)
	}
}

func decodeTimes(js string) (Record, error) {
	var name struct{ Name string }
	_ = json.Unmarshal([]byte(js), &name)
	return DecodeStoredRecord("r", name.Name, []byte(js))
}

// An interrupt answer and a signal note when they were written: WithClock's clock, else the
// context's, else the wall clock; a redelivery keeps the first time; a nil clock or option is
// ErrConfig and records nothing.
func TestRecordTimes_AnswersAndSignals(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if err := j.AnswerInterrupt(ctx, "r", "q", "yes", WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	if err := j.AnswerInterrupt(ctx, "r", "q", "no", WithClock(fixedClock(timesB))); err != nil {
		t.Fatal(err)
	}
	if err := j.Signal(contextWithClock(ctx, fixedClock(timesB)), "r", "s", 1); err != nil {
		t.Fatal(err)
	}
	if err := j.Signal(ctx, "r", "s2", 1, WithClock(fixedClock(timesC))); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{interruptStep("q"): stampA, signalStep("s"): stampB, signalStep("s2"): stampC} {
		rec, ok := hasStep(t, j, "r", name)
		if !ok || rec.RecordedAt() != want {
			t.Errorf("%s: %+v (found %v), want recorded_at %q", name, rec, ok, want)
		}
	}
	if err := j.Signal(ctx, "r", "bad", 1, WithClock(nil)); !errors.Is(err, ErrConfig) {
		t.Errorf("Signal with a nil clock = %v", err)
	}
	if err := j.AnswerInterrupt(ctx, "r", "bad", 1, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("AnswerInterrupt with a nil option = %v", err)
	}
	if _, ok := hasStep(t, j, "r", signalStep("bad")); ok {
		t.Error("a refused Signal recorded a record")
	}
}

// stripRecordedAt zeroes the recorded_at of every record of rs (and nothing else of a signed
// decision's signature: approver, scheme, signature and decided_at stay, so a comparison still sees
// them), and reports which records carried one.
func stripRecordedAt(rs []Record) []bool {
	had := make([]bool, len(rs))
	for i := range rs {
		s := rs[i].ApproverSignature
		if s == nil {
			continue
		}
		had[i] = s.RecordedAt != ""
		c := *s
		c.RecordedAt = ""
		if c.Approver == "" && c.ApproverAlg == "" && len(c.Signature) == 0 && c.DecidedAt == "" {
			rs[i].ApproverSignature = nil
		} else {
			rs[i].ApproverSignature = &c
		}
	}
	return had
}
