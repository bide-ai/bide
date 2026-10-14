package agent

import (
	"context"
	"testing"
	"time"
)

// The all-clear (NotificationResolved): an event a receiver has, or may have, stopped being
// current, and the receiver is told so. See docs/design/notifier.md, "The all-clear".

func resolvedClock() *notifyClock {
	return &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
}

func putResult(t *testing.T, j *Journal, run, id string) {
	t.Helper()
	if _, _, err := j.putEntry(context.Background(), run, ToolResultStep(id), Record{Kind: StepToolResult, ToolUseID: id}); err != nil {
		t.Fatal(err)
	}
}

// scanAndFlush runs the catch-up scan of run's trail and waits for what it queued.
func scanAndFlush(t *testing.T, e *notifyEngine, run string) {
	t.Helper()
	ctx := context.Background()
	if res, err := e.round(ctx, notifyEvent{key: scanKey(run), scan: true, runID: run}); err != nil || !res.done {
		t.Fatalf("scan = %+v, %v", res, err)
	}
	if err := e.flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func resolvedCalls(s *fakeSink) []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Notification
	for _, c := range s.calls {
		if c.Kind == NotificationResolved {
			out = append(out, c)
		}
	}
	return out
}

func trailOf(t *testing.T, tr NotifyTrail, run, key string) []NotifyRecord {
	t.Helper()
	recs, err := tr.Load(context.Background(), NotifyTrailName(run))
	if err != nil {
		t.Fatal(err)
	}
	var out []NotifyRecord
	for _, r := range recs {
		if r.Key == key {
			out = append(out, r)
		}
	}
	return out
}

// A delivered halt that is later resolved gets its all-clear, carrying the event's key and ID.
func TestResolved_ADeliveredHaltThatIsResolvedGetsItsAllClear(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, j, sink, trail, clk, "p1")
	ev := haltEvent()
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatal(res, err)
	}

	scanAndFlush(t, e, "r") // still current: nothing
	if got := resolvedCalls(sink); len(got) != 0 {
		t.Fatalf("an all-clear for an event that is still current: %+v", got)
	}

	putResult(t, j, "r", "c1")
	clk.advance(time.Minute)
	scanAndFlush(t, e, "r")
	got := resolvedCalls(sink)
	if len(got) != 1 {
		t.Fatalf("%d all-clears, want 1 (calls %d)", len(got), sink.count())
	}
	n := got[0]
	rk := ResolvedKey(ev.key)
	if n.Key != rk || n.ID != NotifyID(rk) || n.RunID != "r" || n.Reason != "resolved" || n.Attempt != 1 ||
		n.Clears == nil || n.Clears.Key != ev.key || n.Clears.ID != NotifyID(ev.key) || n.Clears.Kind != NotificationOutcomeUnknown ||
		!n.EventAt.Equal(clk.now()) || !n.Deadline.IsZero() {
		t.Fatalf("all-clear = %+v clears=%+v", n, n.Clears)
	}
	if n.Clears.ID != sink.calls[0].ID {
		t.Fatalf("the all-clear names ID %s, the event was delivered as %s", n.Clears.ID, sink.calls[0].ID)
	}
	recs := trailOf(t, trail, "r", rk)
	if got := recKinds(recs); !eqRecKinds(got, []NotifyRecordKind{NotifyObserved, NotifyClaimed, NotifyAcked}) {
		t.Fatalf("all-clear trail = %v", got)
	}
	if recs[0].Reason != "resolved" || recs[0].EventKind != string(NotificationResolved) || !recs[0].DeadlineAt.IsZero() {
		t.Fatalf("observed = %+v", recs[0])
	}

	// Scanning again, in this process and in another, sends nothing more.
	scanAndFlush(t, e, "r")
	scanAndFlush(t, testEngine(t, j, sink, trail, clk, "p2"), "r")
	if got := resolvedCalls(sink); len(got) != 1 {
		t.Fatalf("%d all-clears after more scans, want 1", len(got))
	}
}

func TestResolved_ReasonsAreResolvedDecidedAndEnded(t *testing.T) {
	gate := notifyEvent{key: ApprovalKey("r", "c1", 0), kind: NotificationApprovalPending, runID: "r", opRun: "r",
		approval: &NotifyApproval{ToolUseID: "c1", ToolName: "refund"}}
	for _, tc := range []struct {
		name, reason string
		ev           notifyEvent
		do           func(t *testing.T, j *Journal)
	}{
		{"decided", "decided", gate, func(t *testing.T, j *Journal) { putResult(t, j, "r", "c1") }},
		{"resolved", "resolved", haltEvent(), func(t *testing.T, j *Journal) { putResult(t, j, "r", "c1") }},
		{"ended", "ended", gate, func(t *testing.T, j *Journal) {
			if _, _, err := j.putEntry(context.Background(), "r", runStartStep, Record{Kind: StepValue, Result: []byte("{}")}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := j.putEntry(context.Background(), "r", runCompleteStep, Record{Kind: StepValue}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			j := memJournal()
			trail, sink := NewMemNotifyTrail(), &fakeSink{}
			e := testEngine(t, j, sink, trail, resolvedClock(), "p1")
			if res, err := e.round(ctx, tc.ev); err != nil || !res.done {
				t.Fatal(res, err)
			}
			tc.do(t, j)
			scanAndFlush(t, e, "r")
			got := resolvedCalls(sink)
			if len(got) != 1 || got[0].Reason != tc.reason || got[0].Clears.Kind != tc.ev.kind {
				t.Fatalf("all-clears = %+v", got)
			}
		})
	}
}

// The case the ordering rule is for: the event's delivery has not succeeded (its calls failed, one
// may even have landed) when it stops being current. The all-clear is delivered, and names the ID
// the event's own deliveries carried, so a receiver can match them in either order.
func TestResolved_AClearForAnEventThatNeverGotItsAckNamesTheEventsID(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{fail: 3}
	e := testEngine(t, j, sink, trail, clk, "p1")
	ev := haltEvent()
	if res, err := e.round(ctx, ev); err == nil || res.done {
		t.Fatalf("round = %+v, %v: want a failed round", res, err)
	}
	putResult(t, j, "r", "c1")
	clk.advance(time.Hour)
	scanAndFlush(t, e, "r")

	got := resolvedCalls(sink)
	if len(got) != 1 {
		t.Fatalf("%d all-clears, want 1", len(got))
	}
	if got[0].Clears.ID != sink.calls[0].ID || got[0].Clears.ID != NotifyID(ev.key) || got[0].Reason != "resolved" {
		t.Fatalf("all-clear = %+v clears=%+v, event delivered as %s", got[0], got[0].Clears, sink.calls[0].ID)
	}
	if kinds := recKinds(trailOf(t, trail, "r", ev.key)); !eqRecKinds(kinds, []NotifyRecordKind{NotifyObserved, NotifyClaimed, NotifyCleared}) {
		t.Fatalf("event trail = %v: want it cleared, not acked", kinds)
	}
	// The event itself is not sent again once it is cleared.
	if n := sink.count(); n != 4 {
		t.Fatalf("%d calls, want 3 failed attempts of the event and the all-clear", n)
	}
}

// A clear is owed only to a receiver that may have been told: an event that stopped being current
// before any delivery was attempted sends nothing, as before.
func TestResolved_NothingIsSentForAnEventThatWasNeverClaimed(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, j, sink, trail, resolvedClock(), "p1")
	putResult(t, j, "r", "c1")
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatal(res, err)
	}
	scanAndFlush(t, e, "r")
	if sink.count() != 0 {
		t.Fatalf("%d notifications for an event nobody was told of", sink.count())
	}
	if recs, _ := trail.Load(ctx, "notify:r"); len(recs) != 2 {
		t.Fatalf("trail = %v, want observed and cleared only", recKinds(recs))
	}
}

// A clear is never overdue: it has no deadline, no overdue record, and no overdue notice, however
// long the sink is down; it is retried for as long as the process runs, and fails like any
// delivery.
func TestResolved_ANeverDeliveredClearIsRetriedAndNeverOverdue(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	j := memJournal()
	trail := NewMemNotifyTrail()
	sink := &fakeSink{}
	e := testEngine(t, j, sink, trail, clk, "p1")
	ev := haltEvent()
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatal(res, err)
	}
	putResult(t, j, "r", "c1")
	rev := notifyEvent{key: ResolvedKey(ev.key), kind: NotificationResolved, runID: "r", origKey: ev.key, origKind: NotificationOutcomeUnknown, reason: "resolved", eventAt: clk.now()}

	sink.mu.Lock()
	sink.fail = len(sink.calls) + 100
	sink.mu.Unlock()
	for range 12 {
		_, _ = e.round(ctx, rev)
		clk.advance(30 * time.Minute) // past the deadline of any event, and the backoff cap
	}
	for _, r := range trailOf(t, trail, "r", rev.key) {
		if r.Kind == NotifyOverdue {
			t.Fatalf("an overdue record for a clear: %+v", r)
		}
	}
	if recs := trailOf(t, trail, "r", OverdueKey(rev.key)); len(recs) != 0 {
		t.Fatalf("an overdue notice for a clear: %v", recKinds(recs))
	}
	for _, c := range sink.calls {
		if c.Kind == NotificationOverdue {
			t.Fatalf("an overdue notice was sent: %+v", c)
		}
	}
	if kinds := recKinds(trailOf(t, trail, "r", rev.key)); len(kinds) < 4 || kinds[len(kinds)-1] == NotifyAcked {
		t.Fatalf("clear trail = %v: want several rounds and no ack", kinds)
	}
	// The sink comes back: it is delivered, still with the original ID.
	sink.mu.Lock()
	sink.fail = 0
	sink.mu.Unlock()
	clk.advance(time.Hour)
	if res, err := e.round(ctx, rev); err != nil || !res.done {
		t.Fatalf("round = %+v, %v", res, err)
	}
	last := sink.calls[len(sink.calls)-1]
	if last.Kind != NotificationResolved || last.ID != NotifyID(rev.key) || last.Clears.ID != NotifyID(ev.key) {
		t.Fatalf("last call = %+v", last)
	}
}

// A process that died after it wrote the clear's observed record, before it sent it: the next scan
// finds the open clear in the trail and delivers it with the reason the record holds.
func TestResolved_ARecordedUndeliveredClearIsResumedFromTheTrail(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, j, sink, trail, clk, "p1")
	ev := haltEvent()
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatal(res, err)
	}
	recs, _ := trail.Load(ctx, "notify:r")
	rk := ResolvedKey(ev.key)
	if err := trail.Append(ctx, "notify:r", len(recs), NotifyRecord{Kind: NotifyObserved, Key: rk, RunID: "r", At: clk.now(),
		EventKind: string(NotificationResolved), EventAt: clk.now(), Reason: "decided"}); err != nil {
		t.Fatal(err)
	}
	// Even though the event is still current in the journal: the record is the decision.
	scanAndFlush(t, testEngine(t, j, sink, trail, clk, "p2"), "r")
	got := resolvedCalls(sink)
	if len(got) != 1 || got[0].Reason != "decided" || got[0].Clears.Key != ev.key || got[0].Clears.Kind != NotificationOutcomeUnknown {
		t.Fatalf("all-clears = %+v", got)
	}
}

// run_aborted is a fact, not a state: it never stops being current, so it is never cleared.
func TestResolved_ARunAbortedEventIsNeverCleared(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, j, sink, trail, resolvedClock(), "p1")
	if res, err := e.round(ctx, notifyEvent{key: AbortedKey("r"), kind: NotificationRunAborted, runID: "r", reason: "saga_aborted"}); err != nil || !res.done {
		t.Fatal(res, err)
	}
	putResult(t, j, "r", "c1")
	scanAndFlush(t, e, "r")
	if got := resolvedCalls(sink); len(got) != 0 {
		t.Fatalf("all-clears for run_aborted: %+v", got)
	}
}

// A recovery_failing event is cleared when recovery recovers.
func TestResolved_RecoveryFailingIsClearedByRecovery(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, memJournal(), sink, trail, clk, "p1")
	at := clk.now()
	for i, r := range []NotifyRecord{{Kind: NotifyFailingSince, RunID: "r", At: at, Reason: "resume_error"}} {
		if err := trail.Append(ctx, "notify:r", i, r); err != nil {
			t.Fatal(err)
		}
	}
	rec := notifyEvent{key: RecoveryKey("r", 0), kind: NotificationRecoveryFailing, runID: "r", episode: 0, reason: "resume_error"}
	if res, err := e.round(ctx, rec); err != nil || !res.done {
		t.Fatal(res, err)
	}
	scanAndFlush(t, e, "r")
	if got := resolvedCalls(sink); len(got) != 0 {
		t.Fatalf("an all-clear while recovery still fails: %+v", got)
	}
	recs, _ := trail.Load(ctx, "notify:r")
	if err := trail.Append(ctx, "notify:r", len(recs), NotifyRecord{Kind: NotifyRecovered, RunID: "r", At: clk.now()}); err != nil {
		t.Fatal(err)
	}
	scanAndFlush(t, e, "r")
	got := resolvedCalls(sink)
	if len(got) != 1 || got[0].Reason != "recovered" || got[0].Clears.Kind != NotificationRecoveryFailing || got[0].Clears.Key != rec.key {
		t.Fatalf("all-clears = %+v", got)
	}
}

// The sweep finds a delivered halt that was resolved while the run is still unfinished.
func TestResolved_TheSweepSendsTheAllClear(t *testing.T) {
	clk := sweepClock()
	now := clk.now()
	j := memJournal()
	putMarker(t, j, "r", "c1", now.Add(-10*time.Minute).UnixMilli())
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 1 {
		t.Fatalf("first sweep: %v calls=%d", err, sink.count())
	}
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 1 {
		t.Fatalf("a sweep of an unchanged halt: %v calls=%d", err, sink.count())
	}
	putResult(t, j, "r", "c1")
	if err := sweep(t, j, sink, trail, clk); err != nil {
		t.Fatal(err)
	}
	got := resolvedCalls(sink)
	if len(got) != 1 || got[0].Reason != "resolved" || got[0].Clears.ID != sink.calls[0].ID {
		t.Fatalf("calls = %+v", sink.calls)
	}
	if err := sweep(t, j, sink, trail, clk); err != nil || len(resolvedCalls(sink)) != 1 {
		t.Fatalf("a later sweep sent it again: %v", err)
	}
}
