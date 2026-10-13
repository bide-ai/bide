package agent

import (
	"context"
	"errors"
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
	pass(fine) // recovers: the episode closes
	clk.advance(time.Hour)
	pass(broken) // a new episode opens: not due yet
	clk.advance(16 * time.Minute)
	pass(broken)
	waitFor(t, func() bool { return sink.count() == 2 })
	if sink.calls[1].Key != RecoveryKey("r", 1) {
		t.Fatalf("second episode key = %q", sink.calls[1].Key)
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
