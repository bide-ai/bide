package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
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

// The drive that follows ResolveHalt tells the notifier, and the receiver gets the all-clear
// without any sweep.
func TestResolved_TheDriveAfterResolveHaltSendsTheAllClear(t *testing.T) {
	ctx := context.Background()
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	store := markerAt(t, time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).UnixMilli())
	j := mustJournal(store)
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	a := mustNew(NewScriptedModel(TextTurn("done")), j, WithTools(charge), WithNotifier(sink, trail))

	if _, err := a.Run(ctx, "r", UserText("go")); !errorsAsHalt(err) {
		t.Fatalf("Run = %v, want *OutcomeUnknown", err)
	}
	if err := a.FlushNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 {
		t.Fatalf("calls = %d, want the halt", sink.count())
	}
	if err := ResolveHalt(ctx, j, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "r", UserText("go")); err != nil {
		t.Fatalf("Run after the resolution = %v", err)
	}
	if err := a.FlushNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	got := resolvedCalls(sink)
	if len(got) != 1 || got[0].Reason != "ended" && got[0].Reason != "resolved" || got[0].Clears.ID != sink.calls[0].ID {
		t.Fatalf("calls = %+v", sink.calls)
	}
}

func errorsAsHalt(err error) bool {
	var h *OutcomeUnknown
	return errors.As(err, &h)
}

// A recovery pass that ends a run's story queues the scan too, and a pass of a sub-run does not.
func TestResolved_ARecoveryPassScansTheRunsTrail(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	loads := &loadCounter{NotifyTrail: NewMemNotifyTrail(), n: map[string]int{}}
	trail, sink := loads, &fakeSink{}
	e := testEngine(t, j, sink, trail, resolvedClock(), "p1")
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatal(res, err)
	}
	putResult(t, j, "r", "c1")
	e.afterRecover(ctx, "r>sub", true, true, nil)
	if err := e.flush(ctx); err != nil || len(resolvedCalls(sink)) != 0 {
		t.Fatalf("a sub-run's pass scanned: %v %d", err, len(resolvedCalls(sink)))
	}
	// The one read is the recovery episode's (recoveryOK); a scan would be a second.
	if n := loads.count("notify:r>sub"); n != 1 {
		t.Fatalf("a sub-run's pass read its own trail %d times, want 1 (the episode's): the root run's trail holds the events", n)
	}
	e.afterRecover(ctx, "r", true, true, nil)
	if err := e.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := resolvedCalls(sink); len(got) != 1 {
		t.Fatalf("all-clears = %+v", got)
	}
}

// loadCounter counts the loads of each trail.
type loadCounter struct {
	NotifyTrail
	mu sync.Mutex
	n  map[string]int
}

func (l *loadCounter) Load(ctx context.Context, trail string) ([]NotifyRecord, error) {
	l.mu.Lock()
	l.n[trail]++
	l.mu.Unlock()
	return l.NotifyTrail.Load(ctx, trail)
}

func (l *loadCounter) count(trail string) int { l.mu.Lock(); defer l.mu.Unlock(); return l.n[trail] }

// A process that cleared an event after a delivery was attempted and died before it queued the
// all-clear: the next scan finds the cleared event with its claim and owes the receiver.
func TestResolved_AClearedEventWhoseAllClearWasNeverQueuedIsFoundByTheScan(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	ev := haltEvent()
	at := clk.now()
	for i, r := range []NotifyRecord{
		{Kind: NotifyObserved, Key: ev.key, RunID: "r", At: at, EventKind: string(ev.kind), EventAt: at, DeadlineAt: at.Add(time.Minute)},
		{Kind: NotifyClaimed, Key: ev.key, RunID: "r", At: at, Holder: "gone", Until: at.Add(time.Minute)},
		{Kind: NotifyCleared, Key: ev.key, RunID: "r", At: at.Add(time.Second), Reason: "resolved"},
	} {
		if err := trail.Append(ctx, "notify:r", i, r); err != nil {
			t.Fatal(err)
		}
	}
	scanAndFlush(t, testEngine(t, memJournal(), sink, trail, clk, "p1"), "r")
	got := resolvedCalls(sink)
	if len(got) != 1 || got[0].Reason != "resolved" || !got[0].EventAt.Equal(at.Add(time.Second)) {
		t.Fatalf("all-clears = %+v", got)
	}
}

func completeRun(t *testing.T, j *Journal, run string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := j.putEntry(ctx, run, runStartStep, Record{Kind: StepValue, Result: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.putEntry(ctx, run, runCompleteStep, Record{Kind: StepValue}); err != nil {
		t.Fatal(err)
	}
}

func openTrail(t *testing.T, tr NotifyTrail) []NotifyRecord {
	t.Helper()
	recs, err := tr.Load(context.Background(), NotifyOpenTrail)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// A run that completes in a process that dies before it scans (a crash right after completion) is
// not listed as unfinished, yet the next sweep finds its delivered halt through the open trail and
// sends the all-clear, once.
func TestResolved_ARunThatCompletedWhileNobodyScannedIsFoundByTheSweep(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, j, sink, trail, clk, "dies")
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatal(res, err)
	}
	// The process dies here: the run is resolved and completed, nothing scanned.
	putResult(t, j, "r", "c1")
	completeRun(t, j, "r")

	if err := sweep(t, j, sink, trail, clk); err != nil {
		t.Fatal(err)
	}
	got := resolvedCalls(sink)
	if len(got) != 1 || got[0].Reason != "ended" || got[0].Clears.ID != sink.calls[0].ID {
		t.Fatalf("calls = %+v", sink.calls)
	}
	// Once: the key is unwatched, and a later sweep sends nothing and reads no run trail.
	if last := openTrail(t, trail); last[len(last)-1].Kind != NotifyUnwatch {
		t.Fatalf("open trail = %v, want the key unwatched", recKinds(last))
	}
	if err := sweep(t, j, sink, trail, clk); err != nil || len(resolvedCalls(sink)) != 1 {
		t.Fatalf("a later sweep: %v, %d all-clears", err, len(resolvedCalls(sink)))
	}
}

// The same through a recovery pass, which lists only unfinished runs.
func TestResolved_ARecoveryPassFindsTheAllClearOfACompletedRun(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, j, sink, trail, clk, "dies")
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatal(res, err)
	}
	putResult(t, j, "r", "c1")
	completeRun(t, j, "r")
	resume := func(context.Context, string, RunStart) error { return nil }
	if _, err := Recover(ctx, j, resume, WithNotifier(sink, trail, fixedNotifyClock(clk), WithNotifyHolder("p"))); err != nil {
		t.Fatal(err)
	}
	if got := resolvedCalls(sink); len(got) != 1 || got[0].Reason != "ended" {
		t.Fatalf("calls = %+v", sink.calls)
	}
}

type openFails struct{ NotifyTrail }

func (o openFails) Append(ctx context.Context, trail string, seq int, rec NotifyRecord) error {
	if trail == NotifyOpenTrail {
		return errors.New("open trail down")
	}
	return o.NotifyTrail.Append(ctx, trail, seq, rec)
}

// The event is listed before it is observed: if the list cannot be written, the event is not
// observed or sent in that round, so no delivered event is ever missing from the list.
func TestResolved_AnEventIsWatchedBeforeItIsObserved(t *testing.T) {
	ctx := context.Background()
	mem := NewMemNotifyTrail()
	sink := &fakeSink{}
	e := testEngine(t, memJournal(), sink, openFails{mem}, resolvedClock(), "p1")
	if _, err := e.round(ctx, haltEvent()); err == nil {
		t.Fatal("round succeeded with the open trail down")
	}
	if recs, _ := mem.Load(ctx, "notify:r"); len(recs) != 0 || sink.count() != 0 {
		t.Fatalf("an unlisted event was observed (%v) or sent (%d)", recKinds(recs), sink.count())
	}
	// Listed once however many rounds and processes see it, and only events that can end.
	e2 := testEngine(t, memJournal(), sink, mem, resolvedClock(), "p2")
	for range 2 {
		if _, err := e2.round(ctx, haltEvent()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e2.round(ctx, notifyEvent{key: AbortedKey("r"), kind: NotificationRunAborted, runID: "r"}); err != nil {
		t.Fatal(err)
	}
	if recs := openTrail(t, mem); len(recs) != 1 || recs[0].Kind != NotifyWatch || recs[0].Key != haltEvent().key || recs[0].RunID != "r" {
		t.Fatalf("open trail = %+v", recs)
	}
}

// An event listed whose observed record never came (the process died between the two) is dropped
// from the list after an hour, and listed again if it is observed later.
func TestResolved_AnOrphanedWatchIsDropped(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	if err := trail.Append(ctx, NotifyOpenTrail, 0, NotifyRecord{Kind: NotifyWatch, Key: haltEvent().key, RunID: "r", At: clk.now()}); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, memJournal(), sink, trail, clk, "p1")
	scanOpenAndFlush(t, e)
	if n := len(openTrail(t, trail)); n != 1 {
		t.Fatalf("a young orphan was dropped: %d records", n)
	}
	clk.advance(61 * time.Minute)
	scanOpenAndFlush(t, e)
	if recs := openTrail(t, trail); len(recs) != 2 || recs[1].Kind != NotifyUnwatch || recs[1].Key != haltEvent().key {
		t.Fatalf("open trail = %v", recKinds(recs))
	}
	if sink.count() != 0 {
		t.Fatal("something was sent for an orphan")
	}
}

func scanOpenAndFlush(t *testing.T, e *notifyEngine) {
	t.Helper()
	ctx := context.Background()
	if res, err := e.round(ctx, notifyEvent{key: scanKey("@open"), scan: true}); err != nil || !res.done {
		t.Fatalf("scan = %+v, %v", res, err)
	}
	if err := e.flush(ctx); err != nil {
		t.Fatal(err)
	}
}

// An event that was cleared before any delivery was attempted is unlisted: nobody was told.
func TestResolved_AnEventNeverClaimedIsUnlisted(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, j, sink, trail, resolvedClock(), "p1")
	putResult(t, j, "r", "c1")
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatal(res, err)
	}
	scanOpenAndFlush(t, e)
	if recs := openTrail(t, trail); len(recs) != 2 || recs[1].Kind != NotifyUnwatch {
		t.Fatalf("open trail = %v", recKinds(recs))
	}
	if sink.count() != 0 {
		t.Fatal("sent for an event nobody was told of")
	}
}

// An ID too long to encode is a digest in the key. The observed record names the operation, so the
// all-clear can still be checked. A record from before the fields falls back to the key.
func TestResolved_AnOverLongIDIsCheckedFromTheRecord(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("c", 200)
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, j, sink, trail, resolvedClock(), "p1")
	ev := notifyEvent{key: HaltedKey("r", OpTool, long, 100), kind: NotificationOutcomeUnknown, runID: "r",
		halt: &NotifyHalt{Op: OpRef{Kind: OpTool, ID: long, ToolName: "pay"}}}
	if strings.Contains(ev.key, long) {
		t.Fatal("setup: the key holds the ID")
	}
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatal(res, err)
	}
	recs := trailOf(t, trail, "r", ev.key)
	if recs[0].OpRun != "r" || recs[0].OpKind != "tool" || recs[0].OpID != long {
		t.Fatalf("observed = %+v", recs[0])
	}
	scanAndFlush(t, e, "r")
	if len(resolvedCalls(sink)) != 0 {
		t.Fatal("cleared while current")
	}
	putResult(t, j, "r", long)
	scanAndFlush(t, e, "r")
	if got := resolvedCalls(sink); len(got) != 1 || got[0].Reason != "resolved" {
		t.Fatalf("calls = %+v", sink.calls)
	}
}

func TestResolved_ARecordWithoutTheOperationFieldsIsReadFromItsKey(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	ev := haltEvent()
	at := clk.now()
	for i, r := range []NotifyRecord{
		{Kind: NotifyObserved, Key: ev.key, RunID: "r", At: at, EventKind: string(ev.kind), EventAt: at, DeadlineAt: at.Add(time.Minute)},
		{Kind: NotifyClaimed, Key: ev.key, RunID: "r", At: at, Holder: "old", Until: at.Add(time.Minute)},
		{Kind: NotifyAcked, Key: ev.key, RunID: "r", At: at, Attempts: 1, Status: 200},
	} {
		if err := trail.Append(ctx, "notify:r", i, r); err != nil {
			t.Fatal(err)
		}
	}
	putResult(t, j, "r", "c1")
	scanAndFlush(t, testEngine(t, j, sink, trail, clk, "p1"), "r")
	if got := resolvedCalls(sink); len(got) != 1 {
		t.Fatalf("calls = %+v", sink.calls)
	}
}

// RecoverLoop reaches the all-clear of a completed run too, on its first pass.
func TestResolved_RecoverLoopFindsTheAllClearOfACompletedRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := resolvedClock()
	j := memJournal()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	e := testEngine(t, j, sink, trail, clk, "dies")
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatal(res, err)
	}
	putResult(t, j, "r", "c1")
	completeRun(t, j, "r")
	resume := func(context.Context, string, RunStart) error { return nil }
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RecoverLoop(ctx, j, resume, WithNotifier(sink, trail, fixedNotifyClock(clk), WithNotifyHolder("loop")), WithRecoverInterval(10*time.Millisecond))
	}()
	waitFor(t, func() bool { return len(resolvedCalls(sink)) == 1 })
	cancel()
	<-done
}
