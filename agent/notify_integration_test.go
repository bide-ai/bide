package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixedNotifyClock(c *notifyClock) NotifyOption {
	return notifyOption(func(cfg *notifyConfig) error {
		cfg.now, cfg.sleep, cfg.jitter = c.now, c.sleep, func(d time.Duration) time.Duration { return d }
		return nil
	})
}

// A drive that ends with *OutcomeUnknown tells the notifier once, however often the run is driven
// into the same halt again, and again for nothing once the halt is resolved.
func TestNotifier_HaltIsNotifiedOnceAcrossDrives(t *testing.T) {
	ctx := context.Background()
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	j := mustJournal(markerAt(t, time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).UnixMilli()))
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	a := mustNew(NewScriptedModel(), j, WithTools(charge), WithNotifier(sink, trail))

	for range 3 {
		_, err := a.Run(ctx, "r", UserText("go"))
		var halt *OutcomeUnknown
		if !errors.As(err, &halt) {
			t.Fatalf("Run = %v, want *OutcomeUnknown", err)
		}
		if err := a.FlushNotifications(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if sink.count() != 1 {
		t.Fatalf("the halt was notified %d times, want 1", sink.count())
	}
	n := sink.calls[0]
	if n.Kind != NotificationOutcomeUnknown || n.RunID != "r" || n.Halt == nil || n.Halt.Op.ID != "c1" || n.Halt.Op.Kind != OpTool || n.Halt.Cause != HaltCrashed || n.Halt.AttemptedAt.IsZero() {
		t.Fatalf("notification = %+v", n)
	}
	var oe *OutcomeUnknown
	if !errors.As(n.Err, &oe) {
		t.Fatalf("Err = %v, want the typed halt for an in-process notifier", n.Err)
	}
	recs, _ := trail.Load(ctx, "notify:r")
	if got := recKinds(recs); !eqRecKinds(got, []NotifyRecordKind{NotifyObserved, NotifyClaimed, NotifyAcked}) {
		t.Fatalf("trail = %v", got)
	}
	// The event's EventAt is the attempt marker's time, so detection latency is real.
	if !recs[0].EventAt.Equal(n.Halt.AttemptedAt) {
		t.Fatalf("event_at = %v, want the marker time %v", recs[0].EventAt, n.Halt.AttemptedAt)
	}
}

func TestNotifier_ApprovalPendingIsNotifiedOnceWithTheGate(t *testing.T) {
	ctx := context.Background()
	ran := 0
	tool := MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { ran++; return "ok", nil },
		WithApproval(&ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}))
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	newAgent := func() *Agent {
		m := NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("done"))
		return mustNew(m, j, WithTools(tool), WithApproverVerifiers(func(string) (ApproverVerifier, bool) { return nil, false }),
			WithNotifier(sink, trail), WithIdentity(Identity{OnBehalfOf: "desk-7", AuthorityRef: "grant-1"}))
	}
	for range 2 {
		a := newAgent()
		if _, err := a.Run(ctx, "r", UserText("go")); !errors.As(err, new(*ApprovalPending)) {
			t.Fatalf("Run = %v, want *ApprovalPending", err)
		}
		if err := a.FlushNotifications(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if sink.count() != 1 {
		t.Fatalf("the gate was notified %d times, want 1", sink.count())
	}
	n := sink.calls[0]
	ap := n.Approval
	if n.Kind != NotificationApprovalPending || ap == nil || ap.ToolName != "refund" || ap.ToolUseID != "c1" || ap.Need != 2 || ap.Of != 3 || ap.Approved != 0 || len(ap.Approvers) != 3 {
		t.Fatalf("notification = %+v approval=%+v", n, ap)
	}
	if n.Principal == nil || n.Principal.OnBehalfOf != "desk-7" || n.Principal.AuthorityRef != "grant-1" {
		t.Fatalf("principal = %+v, want the run's", n.Principal)
	}
	if ran != 0 {
		t.Fatal("the tool ran without approval")
	}
}

// A sink that is down while the run is decided: the event is cleared as stale, never delivered
// late, and the clearing is itself on the trail.
func TestNotifier_AnEventThatStoppedBeingTrueIsClearedNotDelivered(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	put := func(t *testing.T, j *Journal, name string, rec Record) {
		t.Helper()
		if _, _, err := j.putEntry(ctx, "r", name, rec); err != nil {
			t.Fatal(err)
		}
	}
	gate := notifyEvent{key: ApprovalKey("r", "c1", 0), kind: NotificationApprovalPending, runID: "r", approval: &NotifyApproval{ToolUseID: "c1", ToolName: "refund"}}
	halt := haltEvent()
	for _, tc := range []struct {
		name   string
		ev     notifyEvent
		reason string
		do     func(t *testing.T, j *Journal)
	}{
		{"decided", gate, "decided", func(t *testing.T, j *Journal) {
			put(t, j, ToolResultStep("c1"), Record{Kind: StepToolResult, ToolUseID: "c1"})
		}},
		{"resolved", halt, "resolved", func(t *testing.T, j *Journal) {
			put(t, j, ToolResultStep("c1"), Record{Kind: StepToolResult, ToolUseID: "c1"})
		}},
		{"ended", gate, "ended", func(t *testing.T, j *Journal) {
			put(t, j, runStartStep, Record{Kind: StepValue, Result: []byte("{}")})
			put(t, j, runCompleteStep, Record{Kind: StepValue})
		}},
	} {
		j := memJournal()
		trail, sink := NewMemNotifyTrail(), &fakeSink{}
		e := testEngine(t, j, sink, trail, clk, "p1")
		tc.do(t, j)
		if res, err := e.round(ctx, tc.ev); err != nil || !res.done {
			t.Fatalf("%s: round = %+v, %v", tc.name, res, err)
		}
		if sink.count() != 0 {
			t.Fatalf("%s: a stale event was delivered", tc.name)
		}
		recs, _ := trail.Load(ctx, "notify:r")
		if last := recs[len(recs)-1]; last.Kind != NotifyCleared || last.Reason != tc.reason {
			t.Fatalf("%s: last record = %+v", tc.name, last)
		}
	}
	// And a live event is not cleared.
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	if res, err := testEngine(t, j, sink, trail, clk, "p1").round(ctx, gate); err != nil || !res.done || sink.count() != 1 {
		t.Fatalf("a live gate: %+v %v calls=%d", res, err, sink.count())
	}
}

func TestNotifier_RecoverNotifiesAHaltOnceAcrossPasses(t *testing.T) {
	ctx := context.Background()
	tool := MustFunc("refund", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
	j := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("done"))
	if _, err := mustNew(m, j, WithTools(tool)).Run(ctx, "r", UserText("go")); !errors.As(err, new(*ApprovalPending)) {
		t.Fatalf("setup Run = %v", err)
	}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	resume := func(context.Context, string, RunStart) error {
		return &ApprovalPending{RunRef: RunRef{RunID: "r", RootRunID: "r"}, ToolUseID: "c1", ToolName: "refund"}
	}
	for range 3 {
		if _, err := Recover(ctx, j, resume, WithNotifier(sink, trail)); err != nil && !errors.As(err, new(*ApprovalPending)) {
			t.Fatal(err)
		}
	}
	if sink.count() != 1 {
		t.Fatalf("Recover notified %d times over 3 passes, want 1", sink.count())
	}
	// A run waiting for a person is not a run whose recovery is failing.
	recs, _ := trail.Load(ctx, "notify:r")
	for _, r := range recs {
		if r.Kind == NotifyFailingSince {
			t.Fatal("a gate opened a recovery failure episode")
		}
	}
}

func TestNotifier_BackoffDoublesAndIsCapped(t *testing.T) {
	e := testEngine(t, memJournal(), &fakeSink{}, NewMemNotifyTrail(), &notifyClock{t: time.Now()}, "p", WithNotifyRetry(time.Second, 10*time.Second))
	for fails, want := range map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 5: 10 * time.Second, 50: 10 * time.Second} {
		if got := e.backoff(fails); got != want {
			t.Errorf("backoff(%d) = %v, want %v", fails, got, want)
		}
	}
}

func TestNotifier_ASubRunsDriveRaisesNothing(t *testing.T) {
	e := testEngine(t, memJournal(), &fakeSink{}, NewMemNotifyTrail(), &notifyClock{t: time.Now()}, "p")
	e.driveEnd("root>sub", &ApprovalPending{RunRef: RunRef{RunID: "root>sub", RootRunID: "root"}, ToolUseID: "c1"})
	if !e.w.idle() {
		t.Fatal("a sub-run's drive queued an event; the root run's drive carries it")
	}
	e.driveEnd("root", &ApprovalPending{RunRef: RunRef{RunID: "root>sub", RootRunID: "root"}, ToolUseID: "c1"})
	if e.w.idle() {
		t.Fatal("the root run's drive did not queue the event")
	}
	_ = e.flush(context.Background())
}

func TestNotifier_NilPartsAreConfigErrors(t *testing.T) {
	j := memJournal()
	if _, err := New(NewScriptedModel(), j, WithNotifier(nil, NewMemNotifyTrail())); !errors.Is(err, ErrConfig) {
		t.Errorf("nil notifier: %v", err)
	}
	if _, err := New(NewScriptedModel(), j, WithNotifier(&fakeSink{}, nil)); !errors.Is(err, ErrConfig) {
		t.Errorf("nil trail: %v", err)
	}
	if _, err := New(NewScriptedModel(), j, WithNotifier(&fakeSink{}, NewMemNotifyTrail(), WithNotifyDeadline(0))); !errors.Is(err, ErrConfig) {
		t.Errorf("zero deadline: %v", err)
	}
}

// A crash loop keeps failing recovery and keeps resetting memory; the alert fires from the trail.
func TestNotifier_RecoveryFailingCountsAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	j := memJournal()
	tool := MustFunc("refund", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
	m := NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("done"))
	if _, err := mustNew(m, j, WithTools(tool)).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("setup")
	}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	broken := func(context.Context, string, RunStart) error { return errors.New("boom") }
	fine := func(context.Context, string, RunStart) error { return nil }
	pass := func(resume Resumer) {
		t.Helper()
		// Every pass is a new process: a new engine with empty memory over the same trail.
		_, _ = Recover(ctx, j, resume, WithNotifier(sink, trail, fixedNotifyClock(clk), WithNotifyHolder("p")))
	}
	pass(broken) // opens the episode at 09:00
	clk.advance(14 * time.Minute)
	pass(broken)
	if sink.count() != 0 {
		t.Fatal("alerted before the failure lasted 15 minutes")
	}
	clk.advance(2 * time.Minute) // 16 minutes
	pass(broken)
	waitFor(t, func() bool { return sink.count() == 1 })
	got := sink.calls[0]
	if got.Kind != NotificationRecoveryFailing || got.Reason != "resume_error" || got.Key != RecoveryKey("r", 0) {
		t.Fatalf("notification = %+v", got)
	}
	clk.advance(time.Minute)
	pass(broken)
	pass(broken)
	time.Sleep(20 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("notified %d times in one episode, want 1", sink.count())
	}
	pass(fine) // recovers: the episode closes, and the receiver is told
	waitFor(t, func() bool { return sink.count() == 2 })
	if c := sink.calls[1]; c.Kind != NotificationResolved || c.Reason != "recovered" || c.Clears == nil || c.Clears.Key != RecoveryKey("r", 0) {
		t.Fatalf("after the recovery: %+v", c)
	}
	clk.advance(time.Hour)
	pass(broken) // a new episode opens: not due yet
	clk.advance(16 * time.Minute)
	pass(broken)
	waitFor(t, func() bool { return sink.count() == 3 })
	if sink.calls[2].Key != RecoveryKey("r", 1) {
		t.Fatalf("second episode key = %q", sink.calls[2].Key)
	}
}

func TestNotifier_EventFromErr(t *testing.T) {
	e := &notifyEngine{}
	ref := RunRef{RunID: "sub", RootRunID: "root"}
	cases := []struct {
		name string
		err  error
		kind NotificationKind
		ok   bool
	}{
		{"halt", &OutcomeUnknown{RunRef: ref, Op: OpRef{Kind: OpTool, ID: "c1"}}, NotificationOutcomeUnknown, true},
		{"wrapped halt", errors.Join(errors.New("x"), &OutcomeUnknown{RunRef: ref, Op: OpRef{Kind: OpStep, ID: "s"}}), NotificationOutcomeUnknown, true},
		{"gate", &ApprovalPending{RunRef: ref, ToolUseID: "c1", ToolName: "t"}, NotificationApprovalPending, true},
		{"aborted saga", &SagaAborted{RunID: "root", Cause: errors.New("x")}, NotificationRunAborted, true},
		{"cancelled saga", &SagaAborted{RunID: "root", Cause: ErrRunCancelled}, "", false},
		{"saga whose rollback failed", &SagaAborted{RunID: "root", CompensateErr: errors.New("x")}, "", false},
		{"cancelled", ErrRunCancelled, "", false},
		{"plain error", errors.New("boom"), "", false},
		{"nil", nil, "", false},
	}
	for _, c := range cases {
		ev, ok := e.eventFromErr("root", c.err)
		if ok != c.ok || ev.kind != c.kind {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, ev.kind, ok, c.kind, c.ok)
		}
	}
	ev, _ := e.eventFromErr("elsewhere", &OutcomeUnknown{RunRef: ref, Op: OpRef{Kind: OpTool, ID: "c1"}})
	gate, _ := e.eventFromErr("elsewhere", &ApprovalPending{RunRef: ref, ToolUseID: "c1"})
	if gate.runID != "root" || gate.opRun != "sub" {
		t.Errorf("a sub-agent's gate must be keyed to the root run and checked in the sub-run: %+v", gate)
	}
	if ev.runID != "root" || ev.opRun != "sub" {
		t.Errorf("a sub-agent's halt must be keyed to the root run and checked in the sub-run: %+v", ev)
	}
}

func TestNotifier_RecoveryEpisodeEventsAreClearedWhenTheEpisodeEnds(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	at := clk.now()
	for i, r := range []NotifyRecord{
		{Kind: NotifyFailingSince, RunID: "r", At: at},
		{Kind: NotifyRecovered, RunID: "r", At: at.Add(time.Minute)},
		{Kind: NotifyFailingSince, RunID: "r", At: at.Add(time.Hour)},
	} {
		if err := trail.Append(ctx, "notify:r", i, r); err != nil {
			t.Fatal(err)
		}
	}
	e := testEngine(t, memJournal(), sink, trail, clk, "p1")
	old := notifyEvent{key: RecoveryKey("r", 0), kind: NotificationRecoveryFailing, runID: "r", episode: 0}
	if res, err := e.round(ctx, old); err != nil || !res.done || sink.count() != 0 {
		t.Fatalf("the first episode ended, yet: %+v %v calls=%d", res, err, sink.count())
	}
	cur := notifyEvent{key: RecoveryKey("r", 1), kind: NotificationRecoveryFailing, runID: "r", episode: 1, reason: "resume_error"}
	if res, err := e.round(ctx, cur); err != nil || !res.done || sink.count() != 1 {
		t.Fatalf("the open episode: %+v %v calls=%d", res, err, sink.count())
	}
}

func TestNotifier_AnOverdueNoticeForAClosedEventIsCleared(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, memJournal(), sink, trail, clk, "p1")
	ev := haltEvent()
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatal(res, err)
	}
	n := sink.count()
	notice := notifyEvent{key: OverdueKey(ev.key), kind: NotificationOverdue, runID: "r", origKey: ev.key}
	if res, err := e.round(ctx, notice); err != nil || !res.done || sink.count() != n {
		t.Fatalf("a notice for an acknowledged event was sent: %+v %v", res, err)
	}
}

// clearingSink closes the event in the trail while its delivery is in flight, as a racing process
// that found the halt resolved would.
type clearingSink struct {
	trail NotifyTrail
	key   string
}

func (c *clearingSink) Notify(ctx context.Context, n Notification) error {
	recs, _ := c.trail.Load(ctx, "notify:r")
	return c.trail.Append(ctx, "notify:r", len(recs), NotifyRecord{Kind: NotifyCleared, Key: c.key, RunID: "r", At: time.Now(), Reason: "resolved"})
}

// An acknowledgement is not recorded for an event another process closed meanwhile: a verifier
// reads acked-after-cleared as a defect.
func TestNotifyRound_NoAckAfterAClear(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := NewMemNotifyTrail()
	ev := haltEvent()
	e := testEngine(t, memJournal(), &clearingSink{trail: trail, key: ev.key}, trail, clk, "p1")
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatalf("round = %+v, %v", res, err)
	}
	recs, _ := trail.Load(ctx, "notify:r")
	for _, r := range recs {
		if r.Kind == NotifyAcked {
			t.Fatalf("acked after cleared: %v", recKinds(recs))
		}
	}
}

func TestNotifier_RecoveryOKWritesNothingWithoutAnEpisode(t *testing.T) {
	ctx := context.Background()
	trail := NewMemNotifyTrail()
	e := testEngine(t, memJournal(), &fakeSink{}, trail, &notifyClock{t: time.Now()}, "p")
	e.recoveryOK(ctx, "r")
	if recs, _ := trail.Load(ctx, "notify:r"); len(recs) != 0 {
		t.Fatalf("a successful pass with no failure wrote %v", recKinds(recs))
	}
}

// A halt that Recover sees is a run waiting for a person, not a failing recovery, even though the
// Resumer returned it as an error.
func TestNotifier_RecoverHaltIsNotARecoveryFailure(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	tool := MustFunc("refund", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
	if _, err := mustNew(NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("done")), j, WithTools(tool)).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("setup")
	}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	halting := func(context.Context, string, RunStart) error {
		return &OutcomeUnknown{RunRef: RunRef{RunID: "r", RootRunID: "r"}, Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	}
	_, _ = Recover(ctx, j, halting, WithNotifier(sink, trail))
	recs, _ := trail.Load(ctx, "notify:r")
	for _, r := range recs {
		if r.Kind == NotifyFailingSince {
			t.Fatalf("a halt opened a recovery failure episode: %v", recKinds(recs))
		}
	}
	if sink.count() != 1 {
		t.Fatalf("notified %d times, want 1", sink.count())
	}
}

func TestNewRunNotifier_Config(t *testing.T) {
	good := WithNotifier(&fakeSink{}, NewMemNotifyTrail())
	if _, err := NewRunNotifier(nil, good); !errors.Is(err, ErrConfig) {
		t.Errorf("nil journal: %v", err)
	}
	if _, err := NewRunNotifier(memJournal(), nil); !errors.Is(err, ErrConfig) {
		t.Errorf("nil option: %v", err)
	}
	if _, err := NewRunNotifier(memJournal(), WithNotifier(nil, NewMemNotifyTrail())); !errors.Is(err, ErrConfig) {
		t.Errorf("nil notifier: %v", err)
	}
	rn, err := NewRunNotifier(memJournal(), good)
	if err != nil {
		t.Fatal(err)
	}
	rn.DriveEnd("r", errors.New("plain"))
	if err := rn.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// D2. A marker with no time (one an earlier version wrote) must not give every halt of the call the
// same key: the first attempt and a re-attempt after a not-started record are different events.
func TestNotifier_UntimedHaltsOfSuccessiveAttemptsHaveDifferentKeys(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	turn := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: []byte(`{}`)}}}
	if _, err := j.do(ctx, "r", modelStep(0), func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &turn}, nil
	}); err != nil {
		t.Fatal(err)
	}
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	a := mustNew(NewScriptedModel(), j, WithTools(charge))
	e := testEngine(t, j, &fakeSink{}, NewMemNotifyTrail(), &notifyClock{t: time.Now()}, "p")
	haltKey := func() string {
		t.Helper()
		_, err := a.Run(ctx, "r", UserText("go"))
		ev, ok := e.eventFromErr("r", err)
		if !ok || ev.halt == nil || !ev.halt.AttemptedAt.IsZero() || ev.keyFn == nil {
			t.Fatalf("Run = %v; want a halt on an untimed marker whose key is computed off the drive's path", err)
		}
		key, kerr := ev.keyFn(ctx)
		if kerr != nil {
			t.Fatal(kerr)
		}
		return key
	}
	won, m0, key0, err := j.claimNext(ctx, "r", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1"})
	if err != nil || !won {
		t.Fatal(won, err)
	}
	k0 := haltKey()
	if err := j.notStarted(ctx, "r", key0, m0); err != nil {
		t.Fatal(err)
	}
	if won, _, _, err := j.claimNext(ctx, "r", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1"}); err != nil || !won {
		t.Fatal(won, err)
	}
	k1 := haltKey()
	if k0 == k1 || strings.HasSuffix(k0, ":0") || strings.HasSuffix(k1, ":0") {
		t.Fatalf("keys %q and %q: successive untimed attempts must differ, and neither may be :0", k0, k1)
	}
}

// P4. A recovery_failing notification carries a closed class, and an error that is a sentinel or
// nothing: never the Resumer's own text, which can hold a person's data.
func TestNotifier_RecoveryFailingCarriesNoRawErrorText(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		err  error
		is   error
	}{
		{"generic", errors.New("charge for card 4111111111111111 failed"), nil},
		{"storage", fmt.Errorf("insert for card 4111111111111111: %w", ErrStorage), ErrStorage},
	} {
		clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
		j := memJournal()
		tool := MustFunc("refund", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
		if _, err := mustNew(NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("done")), j, WithTools(tool)).Run(ctx, "r", UserText("go")); err == nil {
			t.Fatal("setup")
		}
		trail, sink := NewMemNotifyTrail(), &fakeSink{}
		broken := func(context.Context, string, RunStart) error { return tc.err }
		for _, adv := range []time.Duration{0, 16 * time.Minute, time.Minute} {
			clk.advance(adv)
			_, _ = Recover(ctx, j, broken, WithNotifier(sink, trail, fixedNotifyClock(clk), WithNotifyHolder("p")))
		}
		waitFor(t, func() bool { return sink.count() >= 1 })
		n := sink.calls[0]
		if n.Kind != NotificationRecoveryFailing {
			t.Fatalf("%s: %+v", tc.name, n)
		}
		if n.Err != nil && strings.Contains(n.Err.Error(), "4111") {
			t.Fatalf("%s: the notification carries the Resumer's text: %v", tc.name, n.Err)
		}
		if tc.is != nil && !errors.Is(n.Err, tc.is) {
			t.Fatalf("%s: Err = %v, want the sentinel %v", tc.name, n.Err, tc.is)
		}
		recs, _ := trail.Load(ctx, "notify:r")
		for _, r := range recs {
			if strings.Contains(r.Reason, "4111") {
				t.Fatalf("%s: the trail carries the text: %+v", tc.name, r)
			}
		}
	}
}

// P2. A shutdown's cancellation, returned by the Resumer, is not a recovery failure.
func TestNotifier_ACancelledPassIsNotARecoveryFailure(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	tool := MustFunc("refund", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
	if _, err := mustNew(NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("done")), j, WithTools(tool)).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("setup")
	}
	for name, resume := range map[string]Resumer{
		"error value": func(context.Context, string, RunStart) error { return fmt.Errorf("model call: %w", context.Canceled) },
	} {
		trail := NewMemNotifyTrail()
		_, _ = Recover(ctx, j, resume, WithNotifier(&fakeSink{}, trail))
		if recs, _ := trail.Load(ctx, "notify:r"); len(recs) != 0 {
			t.Fatalf("%s: a cancelled pass wrote %v", name, recKinds(recs))
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	trail := NewMemNotifyTrail()
	_, _ = Recover(cctx, j, func(c context.Context, _ string, _ RunStart) error {
		cancel()
		return errors.New("aborted by shutdown")
	}, WithNotifier(&fakeSink{}, trail))
	if recs, _ := trail.Load(ctx, "notify:r"); len(recs) != 0 {
		t.Fatalf("a pass that ended with the context cancelled wrote %v", recKinds(recs))
	}
}

// P1. An event queued while the worker is being stopped by a flush is not left without a worker.
func TestNotifier_ASubmitDuringAFlushStopIsDelivered(t *testing.T) {
	ctx := context.Background()
	for range 100 {
		clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
		sink := &fakeSink{}
		e := testEngine(t, memJournal(), sink, NewMemNotifyTrail(), clk, "p")
		e.w.submit(haltEvent())
		waitFor(t, func() bool { return e.w.idle() })
		// The flush has asked the dispatcher to stop; an event arrives before it has gone.
		e.w.mu.Lock()
		if e.w.running {
			e.w.stopping = true
			close(e.w.stop)
		}
		e.w.mu.Unlock()
		other := notifyEvent{key: ApprovalKey("r2", "c9", 0), kind: NotificationApprovalPending, runID: "r2", approval: &NotifyApproval{ToolUseID: "c9", ToolName: "pay"}}
		e.w.submit(other)
		waitFor(t, func() bool { return e.w.idle() })
		if sink.count() != 2 {
			t.Fatalf("%d notifications, want 2: the event queued during the stop was not delivered", sink.count())
		}
		_ = e.flush(ctx)
	}
}

// P1, deterministic: an event queued while the dispatcher is leaving marks a restart, and the exit
// then starts another dispatcher, which delivers it.
func TestNotifierWorker_AnEventQueuedWhileLeavingGetsANewDispatcher(t *testing.T) {
	sink := &fakeSink{}
	e := testEngine(t, memJournal(), sink, NewMemNotifyTrail(), &notifyClock{t: time.Now()}, "p")
	w := e.w
	w.mu.Lock()
	w.running, w.stopping = true, true // a dispatcher that has been told to stop and has not gone yet
	w.stop, w.done = make(chan struct{}), make(chan struct{})
	w.mu.Unlock()
	w.submit(haltEvent())
	w.mu.Lock()
	restart := w.restart
	w.mu.Unlock()
	if !restart {
		t.Fatal("an event queued while the dispatcher was leaving did not mark a restart")
	}
	w.exited() // the old dispatcher goes
	waitFor(t, func() bool { return sink.count() == 1 })
	_ = e.flush(context.Background())
}

// A pause that is not a halt or a gate (a timer, a signal) is a success too, and closes an open
// recovery failure episode.
func TestNotifier_AnyPauseClosesTheRecoveryEpisode(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	j := memJournal()
	tool := MustFunc("refund", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
	if _, err := mustNew(NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("done")), j, WithTools(tool)).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("setup")
	}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	pass := func(err error) {
		_, _ = Recover(ctx, j, func(context.Context, string, RunStart) error { return err }, WithNotifier(sink, trail, fixedNotifyClock(clk), WithNotifyHolder("p")))
	}
	pass(errors.New("blip"))
	clk.advance(time.Minute)
	pass(&TimerPending{RunRef: RunRef{RunID: "r", RootRunID: "r"}, Name: "tick", FireAt: clk.now().Add(time.Hour)})
	clk.advance(time.Hour)
	pass(errors.New("blip"))
	time.Sleep(20 * time.Millisecond)
	if sink.count() != 0 {
		t.Fatalf("a failure an hour after a paused pass raised %+v", sink.calls)
	}
}

// F2: each call of a Notifier is bounded by a fraction of the claim, so a round ends inside the
// claim it holds.
func TestNotifyRound_ACallIsBoundedByTheClaim(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	var left time.Duration
	sink := notifierFunc(func(c context.Context, _ Notification) error {
		dl, ok := c.Deadline()
		if !ok {
			t.Error("a Notify call has no deadline")
		}
		left = time.Until(dl)
		return nil
	})
	e := testEngine(t, memJournal(), sink, NewMemNotifyTrail(), clk, "p", WithNotifyClaimTTL(40*time.Second))
	if _, err := e.round(ctx, haltEvent()); err != nil {
		t.Fatal(err)
	}
	if left <= 0 || left > 10*time.Second {
		t.Fatalf("the call had %v left; want at most a quarter of the 40 second claim", left)
	}
}

// An overdue notice delivered whose acknowledgement could not be recorded is recorded by the next
// round and not sent again.
func TestNotifyRound_AnUnrecordedOverdueNoticeIsNotSentAgain(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	ft := &failNTrail{NotifyTrail: NewMemNotifyTrail()}
	primary, overdue := &fakeSink{fail: 1000}, &fakeSink{}
	e := testEngine(t, memJournal(), primary, ft, clk, "p", WithOverdueNotifier(overdue))
	ev := haltEvent()
	_, _ = e.round(ctx, ev)
	clk.advance(10 * time.Minute) // past the deadline
	ft.set(1000)                  // the store refuses acknowledgements
	_, _ = e.round(ctx, ev)
	if overdue.count() != 1 {
		t.Fatalf("setup: %d notices", overdue.count())
	}
	ft.set(0)
	clk.advance(20 * time.Minute)
	_, _ = e.round(ctx, ev)
	recs, _ := ft.Load(ctx, "notify:r")
	if overdue.count() != 1 || countKind(recs, NotifyAcked, OverdueKey(ev.key)) != 1 {
		t.Fatalf("notices sent %d, acknowledged %d; want 1 and 1", overdue.count(), countKind(recs, NotifyAcked, OverdueKey(ev.key)))
	}
}

// A call abandoned at its timeout that has not returned is not repeated: the next attempts of the
// round make no new call, so a Notifier that never returns cannot gather goroutines.
func TestNotifyRound_AnAbandonedCallIsNotRepeatedWhileItIsStuck(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	release := make(chan struct{})
	sink := notifierFunc(func(context.Context, Notification) error {
		calls.Add(1)
		<-release
		return errors.New("released")
	})
	e := realEngine(t, memJournal(), sink, NewMemNotifyTrail(), "p", func(c *notifyConfig) {
		c.claimTTL, c.attemptBackoff = 400*time.Millisecond, time.Millisecond
	}, WithNotifyRetry(time.Millisecond, time.Millisecond), WithNotifyDeadline(time.Hour))
	if _, err := e.round(ctx, haltEvent()); err == nil {
		t.Fatal("a call that never returned was not an error")
	}
	if calls.Load() != 1 {
		t.Fatalf("%d calls made in a round whose first call was abandoned and still running, want 1", calls.Load())
	}
	close(release)
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.stuckAll == 0
	})
}

// RecoverLoop reports to WithRecoverErrors the events its notifier could not deliver by the time
// it stops.
func TestNotifier_RecoverLoopReportsUndeliveredEventsAtExit(t *testing.T) {
	j := gateRun(t)
	sink := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
	resume := func(context.Context, string, RunStart) error {
		return &ApprovalPending{RunRef: RunRef{RunID: "r", RootRunID: "r"}, ToolUseID: "c1", ToolName: "refund"}
	}
	var mu sync.Mutex
	var errs []error
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RecoverLoop(ctx, j, resume, WithNotifier(sink, NewMemNotifyTrail(), WithNotifyRetry(time.Hour, time.Hour)), WithRecoverInterval(10*time.Millisecond),
			WithRecoverErrors(func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }))
	}()
	waitFor(t, func() bool { return sink.count() > 0 })
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	for _, err := range errs {
		if strings.Contains(err.Error(), "sink") || errors.Is(err, ErrNotifyPermanent) {
			return
		}
	}
	t.Fatalf("RecoverLoop dropped its notifier's undelivered event at exit; errors seen: %v", errs)
}

// A halt whose key needs a read of the journal is queued at once, and its key is computed in the
// worker, which tries again when the read fails.
func TestNotifier_ALazyKeyIsRetriedByTheWorker(t *testing.T) {
	st := &failGetStore{Store: markerAt(t, 0)}
	j := mustJournal(st)
	sink := &fakeSink{}
	e := realEngine(t, j, sink, NewMemNotifyTrail(), "p", nil, WithNotifyRetry(5*time.Millisecond, 5*time.Millisecond))
	halt := &OutcomeUnknown{RunRef: RunRef{RunID: "r", RootRunID: "r"}, Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	ev, ok := e.eventFromErr("r", halt)
	if !ok {
		t.Fatal("not queued")
	}
	st.armed.Store(true)
	e.w.submit(ev)
	time.Sleep(30 * time.Millisecond)
	if sink.count() != 0 {
		t.Fatal("delivered under a key that could not be computed")
	}
	st.armed.Store(false)
	waitFor(t, func() bool { return sink.count() == 1 })
	_ = e.flush(context.Background())
}

// Two peers finding an overdue event held by a dead holder send the notice once between them, under
// the notice's own claim; and a holder that claimed after the event was overdue is left to send it.
func TestNotifyRound_PeersSendTheOverdueNoticeOnceWhenTheHolderIsGone(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, j, ev := NewMemNotifyTrail(), memJournal(), haltEvent()
	down, overdue := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}, &fakeSink{fail: 1, err: ErrNotifyPermanent}
	opts := []NotifyOption{WithOverdueNotifier(overdue), WithNotifyClaimTTL(time.Hour)}
	_, _ = testEngine(t, j, down, trail, clk, "dead", opts...).round(ctx, ev) // claims for an hour, then its process dies
	clk.advance(6 * time.Minute)
	for _, name := range []string{"peer1", "peer2", "peer3"} {
		_, _ = testEngine(t, j, down, trail, clk, name, opts...).round(ctx, ev)
	}
	if overdue.count() != 1 {
		t.Fatalf("%d notice attempts from three peers inside one notice claim, want 1", overdue.count())
	}
	clk.advance(20 * time.Minute) // the notice's claim and pace have passed; its sink is back
	_, _ = testEngine(t, j, down, trail, clk, "peer4", opts...).round(ctx, ev)
	if overdue.count() != 2 {
		t.Fatalf("%d notice attempts, want the retry by a later peer", overdue.count())
	}
	recs, _ := trail.Load(ctx, "notify:r")
	if countKind(recs, NotifyAcked, OverdueKey(ev.key)) != 1 {
		t.Fatalf("the notice is not acknowledged once: %v", recKinds(recs))
	}
	// An event whose holder claimed after the deadline sends its notice itself: a peer adds no claim.
	trail2, ev2 := NewMemNotifyTrail(), haltEvent()
	late := testEngine(t, j, down, trail2, clk, "late", opts...)
	_, _ = late.round(ctx, ev2) // observed now
	clk.advance(6 * time.Minute)
	_, _ = late.round(ctx, ev2) // overdue, then a claim after it
	before, _ := trail2.Load(ctx, "notify:r")
	_, _ = testEngine(t, j, down, trail2, clk, "peer", opts...).round(ctx, ev2)
	after, _ := trail2.Load(ctx, "notify:r")
	if len(after) != len(before) {
		t.Fatalf("a peer wrote %v beside a holder that claimed after the deadline", recKinds(after[len(before):]))
	}
}

// Peers racing for the same overdue notice: the append of the notice's claim is the arbiter.
func TestNotifyRound_RacingPeersSendTheOverdueNoticeOnce(t *testing.T) {
	ctx := context.Background()
	for iter := range 30 {
		clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
		trail, j, ev := NewMemNotifyTrail(), memJournal(), haltEvent()
		down, overdue := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}, &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
		opts := []NotifyOption{WithOverdueNotifier(overdue), WithNotifyClaimTTL(time.Hour)}
		_, _ = testEngine(t, j, down, trail, clk, "dead", opts...).round(ctx, ev)
		clk.advance(6 * time.Minute)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for p := range 6 {
			e := testEngine(t, j, down, trail, clk, "peer"+string(rune('a'+p)), opts...)
			wg.Add(1)
			go func() { defer wg.Done(); <-start; _, _ = e.round(ctx, ev) }()
		}
		close(start)
		wg.Wait()
		if overdue.count() != 1 {
			t.Fatalf("iteration %d: %d peers sent the notice, want 1", iter, overdue.count())
		}
	}
}
