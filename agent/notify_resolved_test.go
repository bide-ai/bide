package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	recs, err := tr.Load(context.Background(), OpenSegment(resolvedClock().now()))
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
	if strings.HasPrefix(trail, NotifyOpenTrail) {
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
	if recs := FoldWatch(openTrail(t, mem)); len(recs) != 1 || recs[0].Key != haltEvent().key || recs[0].RunID != "r" {
		t.Fatalf("open trail = %+v", recs)
	}
}

// An event listed whose observed record never came (the process died between the two) is dropped
// from the list after an hour, and listed again if it is observed later.
func TestResolved_AnOrphanedWatchIsDropped(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	if err := trail.Append(ctx, OpenSegment(clk.now()), 0, NotifyRecord{Kind: NotifyWatch, Key: haltEvent().key, RunID: "r", At: clk.now()}); err != nil {
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
	if recs := openTrail(t, trail); len(FoldWatch(recs)) != 0 || recs[len(recs)-1].Kind != NotifyUnwatch {
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

// readCounter counts the loads of a trail family and the records they return.
type readCounter struct {
	NotifyTrail
	mu      sync.Mutex
	loads   map[string]int
	records int
}

func newReadCounter() *readCounter {
	return &readCounter{NotifyTrail: NewMemNotifyTrail(), loads: map[string]int{}}
}

func (r *readCounter) Load(ctx context.Context, trail string) ([]NotifyRecord, error) {
	recs, err := r.NotifyTrail.Load(ctx, trail)
	r.mu.Lock()
	r.loads[trail]++
	r.records += len(recs)
	r.mu.Unlock()
	return recs, err
}

func (r *readCounter) reset() { r.mu.Lock(); r.loads, r.records = map[string]int{}, 0; r.mu.Unlock() }

func (r *readCounter) openLoads() (n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.loads {
		if strings.HasPrefix(k, NotifyOpenTrail) {
			n += v
		}
	}
	return n
}

// Thousands of events over many days: the list a sweep reads stays the size of what is listed now
// and of one day, not of history, and loses nothing.
func TestResolved_TheOpenTrailsReadCostDoesNotGrowWithHistory(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	tr := newReadCounter()
	e := testEngine(t, memJournal(), &fakeSink{}, tr, clk, "p1")
	cost := func() int {
		tr.reset()
		if _, recs, err := e.openHead(ctx); err != nil {
			t.Fatal(err)
		} else {
			return len(recs)
		}
		return 0
	}
	var prev []string
	var atDay30 int
	for day := range 1100 { // three years
		clk.advance(24 * time.Hour)
		var keys []string
		for i := range 6 {
			k := fmt.Sprintf("halted:d%d:tool:c%d:1", day, i)
			if err := e.watch(ctx, k, "r"); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
		}
		for _, k := range prev { // yesterday's are done
			if err := e.unwatch(ctx, k); err != nil {
				t.Fatal(err)
			}
		}
		for _, k := range keys[:4] {
			if err := e.unwatch(ctx, k); err != nil {
				t.Fatal(err)
			}
		}
		prev = keys[4:]
		if day == 400 {
			atDay30 = cost()
		}
	}
	// 6600 events listed over three years; two are open.
	n := cost()
	if n > 40 || atDay30 > 40 {
		t.Fatalf("the head segment holds %d records at day 1100 and %d at day 400; it must not grow with history", n, atDay30)
	}
	if loads := tr.openLoads(); loads > 4 {
		t.Fatalf("finding the head took %d loads", loads)
	}
	_, recs, _ := e.openHead(ctx)
	st := FoldOpen(recs)
	if len(st.Watched) != 2 || st.Watched[0].Key != prev[0] || st.Watched[1].Key != prev[1] {
		t.Fatalf("listed = %+v, want %v", st.Watched, prev)
	}
	// And the work of a scan is that bound too: it reads the head and the trails of the runs listed.
	tr.reset()
	if err := e.scanOpen(ctx); err != nil {
		t.Fatal(err)
	}
	// The head, and the index of the year: at most a year of segments, never the years before it.
	if tr.records > 450 {
		t.Fatalf("a scan read %d records", tr.records)
	}
}

// A segment is sealed, and the next begins with a checkpoint of what the sealed one lists; a roller
// that died between the two is finished by the next writer.
func TestResolved_ARolloverSealsAndCheckpoints(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	tr := NewMemNotifyTrail()
	e := testEngine(t, memJournal(), &fakeSink{}, tr, clk, "p1")
	if err := e.watch(ctx, "k1", "r"); err != nil {
		t.Fatal(err)
	}
	if err := e.watch(ctx, "k2", "r"); err != nil {
		t.Fatal(err)
	}
	if err := e.unwatch(ctx, "k2"); err != nil {
		t.Fatal(err)
	}
	day1 := OpenSegment(clk.now())
	// The roller dies after the seal.
	recs, _ := tr.Load(ctx, day1)
	clk.advance(24 * time.Hour)
	day2 := OpenSegment(clk.now())
	if err := tr.Append(ctx, day1, len(recs), NotifyRecord{Kind: NotifySeal, At: clk.now(), Next: day2}); err != nil {
		t.Fatal(err)
	}
	name, head, err := e.openHead(ctx)
	if err != nil || name != day2 {
		t.Fatalf("head = %s, %v", name, err)
	}
	if len(head) != 1 || head[0].Kind != NotifyCheckpoint || len(head[0].Watching) != 1 || head[0].Watching[0].Key != "k1" {
		t.Fatalf("head = %+v", head)
	}
	// An ordinary rollover: day 3's writer seals day 2 and checkpoints.
	clk.advance(24 * time.Hour)
	if err := e.watch(ctx, "k3", "r"); err != nil {
		t.Fatal(err)
	}
	d2, _ := tr.Load(ctx, day2)
	if d2[len(d2)-1].Kind != NotifySeal {
		t.Fatalf("day 2 = %v, want it sealed", recKinds(d2))
	}
	_, head, _ = e.openHead(ctx)
	if got := FoldOpen(head).Watched; len(got) != 2 || got[0].Key != "k1" || got[1].Key != "k3" {
		t.Fatalf("listed = %+v", got)
	}
	// Nothing is appended to a sealed segment: a writer with a stale view loses the race.
	if err := tr.Append(ctx, day2, len(d2)-1, NotifyRecord{Kind: NotifyWatch, Key: "late", RunID: "r", At: clk.now()}); !errors.Is(err, ErrTrailConflict) {
		t.Fatalf("append over the seal = %v, want a conflict", err)
	}
}

// A process whose clock is a day behind follows the seals to the newest segment and writes there.
func TestResolved_AProcessWithALaggingClockWritesToTheNewestSegment(t *testing.T) {
	ctx := context.Background()
	clkA := resolvedClock()
	tr := NewMemNotifyTrail()
	a := testEngine(t, memJournal(), &fakeSink{}, tr, clkA, "a")
	if err := a.watch(ctx, "k1", "r"); err != nil {
		t.Fatal(err)
	}
	clkB := resolvedClock()
	b := testEngine(t, memJournal(), &fakeSink{}, tr, clkB, "b")
	clkA.advance(24 * time.Hour)
	if err := a.watch(ctx, "k2", "r"); err != nil { // A rolls to day 2
		t.Fatal(err)
	}
	if err := b.watch(ctx, "k3", "r"); err != nil { // B still thinks it is day 1
		t.Fatal(err)
	}
	day1, _ := tr.Load(ctx, OpenSegment(clkB.now()))
	if last := day1[len(day1)-1]; last.Kind != NotifySeal {
		t.Fatalf("day 1 ends %v", recKinds(day1))
	}
	_, recs, _ := a.openHead(ctx)
	if got := FoldOpen(recs).Watched; len(got) != 3 {
		t.Fatalf("listed = %+v, want k1, k2, k3", got)
	}
}

// A notifier that was idle for months still finds what it listed, once it has looked back; a
// fresh deployment looks back once and then finds today's segment at once.
func TestResolved_AnIdleGapKeepsTheListAndALookupIsCheapAfterTheFirst(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	tr := newReadCounter()
	e := testEngine(t, memJournal(), &fakeSink{}, tr, clk, "p1")
	if _, recs, err := e.openHead(ctx); err != nil || len(recs) != 1 || recs[0].Kind != NotifyCheckpoint {
		t.Fatal(recs, err)
	}
	tr.reset()
	if _, _, err := e.openHead(ctx); err != nil || tr.openLoads() > 2 {
		t.Fatalf("the second lookup took %d loads: %v", tr.openLoads(), err)
	}
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done { // delivered, listed
		t.Fatal(res, err)
	}
	clk.advance(200 * 24 * time.Hour)
	if err := e.scanOpen(ctx); err != nil {
		t.Fatal(err)
	}
	tr.reset()
	_, recs, err := e.openHead(ctx)
	if err != nil || tr.openLoads() > 2 {
		t.Fatalf("after the roll the lookup took %d loads: %v", tr.openLoads(), err)
	}
	if got := FoldOpen(recs).Watched; len(got) != 1 || got[0].Key != haltEvent().key {
		t.Fatalf("the idle gap lost the list: %+v", got)
	}
}

// Events observed before the open trail existed are listed, once, by the first sweep: a delivered
// halt of a run that has since completed gets its all-clear, and a later sweep does not read the
// finished run again.
func TestResolved_TheFirstSweepListsEventsObservedBeforeTheOpenTrail(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	tr := newReadCounter()
	sink := &fakeSink{}
	at := clk.now()
	ev := haltEvent()
	for i, r := range []NotifyRecord{
		{Kind: NotifyObserved, Key: ev.key, RunID: "r", At: at, EventKind: string(ev.kind), EventAt: at, DeadlineAt: at.Add(time.Minute)},
		{Kind: NotifyClaimed, Key: ev.key, RunID: "r", At: at, Holder: "old", Until: at.Add(time.Minute)},
		{Kind: NotifyAcked, Key: ev.key, RunID: "r", At: at, Attempts: 1, Status: 200},
	} {
		if err := tr.Append(ctx, "notify:r", i, r); err != nil {
			t.Fatal(err)
		}
	}
	putResult(t, j, "r", "c1")
	completeRun(t, j, "r")

	if err := sweep(t, j, sink, tr, clk); err != nil {
		t.Fatal(err)
	}
	if got := resolvedCalls(sink); len(got) != 1 || got[0].Reason != "ended" {
		t.Fatalf("calls = %+v", sink.calls)
	}
	_, recs, _ := testEngine(t, j, sink, tr, clk, "x").openHead(ctx)
	if st := FoldOpen(recs); !st.Backfilled || len(st.Watched) != 0 {
		t.Fatalf("open state = %+v", st)
	}
	tr.reset()
	if err := sweep(t, j, sink, tr, clk); err != nil {
		t.Fatal(err)
	}
	if n := tr.loads["notify:r"]; n != 0 {
		t.Fatalf("the second sweep read the finished run's trail %d times", n)
	}
	if len(resolvedCalls(sink)) != 1 {
		t.Fatal("sent again")
	}
}

// A lookup costs the same few loads on a fresh deployment, after a 400-day gap, and across a year
// boundary.
func TestResolved_ALookupIsConstantWhateverTheGap(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	tr := newReadCounter()
	e := testEngine(t, memJournal(), &fakeSink{}, tr, clk, "p1")
	loads := func() int {
		tr.reset()
		if _, _, err := e.openHead(ctx); err != nil {
			t.Fatal(err)
		}
		return tr.openLoads()
	}
	if n := loads(); n > 12 { // one index lookup per year since the open trail began, then creates the first segment
		t.Fatalf("a fresh deployment's first lookup took %d loads", n)
	}
	if n := loads(); n > 3 {
		t.Fatalf("a lookup took %d loads", n)
	}
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatal(res, err)
	}
	clk.advance(400 * 24 * time.Hour)
	if n := loads(); n > 16 { // first of a new year, after a gap of more than a year: the index walk is by year
		t.Fatalf("the lookup after a 400-day gap took %d loads", n)
	}
	if n := loads(); n > 3 {
		t.Fatalf("the next lookup took %d loads", n)
	}
	_, recs, _ := e.openHead(ctx)
	if got := FoldOpen(recs).Watched; len(got) != 1 || got[0].Key != haltEvent().key {
		t.Fatalf("the gap lost the list: %+v", got)
	}
}

// A writer that dies after creating a segment and before indexing it, or after sealing and before
// the checkpoint: the next writer finishes the work, and nothing listed is lost.
func TestResolved_ACrashBetweenCreatingASegmentAndIndexingItIsRepaired(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	tr := NewMemNotifyTrail()
	e := testEngine(t, memJournal(), &fakeSink{}, tr, clk, "p1")
	if err := e.watch(ctx, "k1", "r"); err != nil {
		t.Fatal(err)
	}
	day1 := OpenSegment(clk.now())
	recs, _ := tr.Load(ctx, day1)
	clk.advance(24 * time.Hour)
	day2 := OpenSegment(clk.now())
	// The roller sealed day 1 and created day 2 with its checkpoint, and died before indexing it.
	if err := tr.Append(ctx, day1, len(recs), NotifyRecord{Kind: NotifySeal, At: clk.now(), Next: day2}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(ctx, day2, 0, NotifyRecord{Kind: NotifyCheckpoint, At: clk.now(), Watching: FoldWatch(recs)}); err != nil {
		t.Fatal(err)
	}
	idx, _ := tr.Load(ctx, OpenIndex(clk.now().Year()))
	if LastSegment(idx) != day1 {
		t.Fatalf("setup: index ends with %s", LastSegment(idx))
	}
	name, head, err := e.openHead(ctx)
	if err != nil || name != day2 || len(FoldWatch(head)) != 1 {
		t.Fatalf("head = %s %v %v", name, head, err)
	}
	idx, _ = tr.Load(ctx, OpenIndex(clk.now().Year()))
	if LastSegment(idx) != day2 {
		t.Fatalf("the index was not repaired: ends with %s", LastSegment(idx))
	}
	// Another writer died after indexing the first segment and before creating it.
	tr2 := NewMemNotifyTrail()
	if err := tr2.Append(ctx, OpenIndex(clk.now().Year()), 0, NotifyRecord{Kind: NotifySegment, At: clk.now(), Next: day2}); err != nil {
		t.Fatal(err)
	}
	e2 := testEngine(t, memJournal(), &fakeSink{}, tr2, clk, "p2")
	if name, head, err := e2.openHead(ctx); err != nil || name != day2 || len(head) != 1 || head[0].Kind != NotifyCheckpoint {
		t.Fatalf("head = %s %v %v", name, head, err)
	}
}

// The backfill is done once, however many processes try at the same time, and it survives the
// rollover to a new day; an event nobody was told of is not listed.
func TestResolved_ConcurrentBackfillsListEachEventOnceAndRecordOneMarker(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	tr := NewMemNotifyTrail()
	at := clk.now()
	for i := range 12 {
		run := fmt.Sprintf("run%02d", i)
		key := HaltedKey(run, OpTool, "c1", 100)
		recs := []NotifyRecord{
			{Kind: NotifyObserved, Key: key, RunID: run, At: at, EventKind: string(NotificationOutcomeUnknown), EventAt: at, DeadlineAt: at.Add(time.Minute)},
		}
		if i%3 != 0 { // delivered; the others were cleared before any claim
			recs = append(recs,
				NotifyRecord{Kind: NotifyClaimed, Key: key, RunID: run, At: at, Holder: "old", Until: at.Add(time.Minute)},
				NotifyRecord{Kind: NotifyAcked, Key: key, RunID: run, At: at, Attempts: 1, Status: 200})
		} else {
			recs = append(recs, NotifyRecord{Kind: NotifyCleared, Key: key, RunID: run, At: at, Reason: "resolved"})
		}
		for k, r := range recs {
			if err := tr.Append(ctx, NotifyTrailName(run), k, r); err != nil {
				t.Fatal(err)
			}
		}
		putMarker(t, j, run, "c1", at.UnixMilli())
	}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := testEngine(t, j, &fakeSink{}, tr, clk, fmt.Sprintf("p%d", i))
			if err := e.backfillOpen(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	e := testEngine(t, j, &fakeSink{}, tr, clk, "x")
	_, recs, err := e.openHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	marks := 0
	for _, r := range recs {
		if r.Kind == NotifyBackfilled {
			marks++
		}
	}
	st := FoldOpen(recs)
	if marks != 1 || !st.Backfilled || len(st.Watched) != 8 {
		t.Fatalf("markers = %d, backfilled = %v, listed = %d (want 1, true, 8)", marks, st.Backfilled, len(st.Watched))
	}
	clk.advance(48 * time.Hour)
	_, recs, _ = e.openHead(ctx)
	if st := FoldOpen(recs); !st.Backfilled || len(st.Watched) != 8 {
		t.Fatalf("after the rollover: backfilled = %v, listed = %d", st.Backfilled, len(st.Watched))
	}
}

// Recover and RecoverLoop run the backfill too, for a deployment that never calls NotifyPending.
func TestResolved_RecoverAndRecoverLoopBackfill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := resolvedClock()
	setup := func() (*Journal, NotifyTrail) {
		j := memJournal()
		tr := NewMemNotifyTrail()
		e := testEngine(t, j, &fakeSink{}, tr, clk, "old")
		if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
			t.Fatal(res, err)
		}
		// An older version: nothing is listed.
		clean := NewMemNotifyTrail()
		recs, _ := tr.Load(ctx, "notify:r")
		for i, r := range recs {
			if err := clean.Append(ctx, "notify:r", i, r); err != nil {
				t.Fatal(err)
			}
		}
		putMarker(t, j, "r", "c1", clk.now().UnixMilli())
		if _, _, err := j.putEntry(ctx, "r", runStartStep, Record{Kind: StepValue, Result: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
		return j, clean
	}
	resume := func(context.Context, string, RunStart) error { return nil }
	listed := func(j *Journal, tr NotifyTrail) []Watched {
		_, recs, err := testEngine(t, j, &fakeSink{}, tr, clk, "x").openHead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return FoldWatch(recs)
	}
	j, tr := setup()
	if _, err := Recover(ctx, j, resume, WithNotifier(&fakeSink{}, tr, fixedNotifyClock(clk), WithNotifyHolder("p"))); err != nil {
		t.Fatal(err)
	}
	if got := listed(j, tr); len(got) != 1 {
		t.Fatalf("Recover listed %+v", got)
	}
	j, tr = setup()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RecoverLoop(ctx, j, resume, WithNotifier(&fakeSink{}, tr, fixedNotifyClock(clk), WithNotifyHolder("loop")), WithRecoverInterval(10*time.Millisecond))
	}()
	waitFor(t, func() bool { return len(listed(j, tr)) == 1 })
	cancel()
	<-done
}

// A flow run is its own root, whatever its ID holds: a flow named "tenant>flow1" is not a
// sub-agent of "tenant". The hook path (RunNotifier.DriveEnd) and the sweep file its gate under the
// same trail, the flow run's, and deliver it once.
func TestNotifier_AFlowRunWithAMarkInItsIDHasOneRootOnBothPaths(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	const flow = "tenant>flow1"
	j := memJournal()
	start, _ := json.Marshal(RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}})
	if _, _, err := j.putEntry(ctx, flow, runStartStep, Record{Kind: StepValue, Result: start}); err != nil {
		t.Fatal(err)
	}
	putGate(t, j, flow, ApprovalWait{ToolUseID: "node:pay", ToolName: "pay", Single: true})
	tr, sink := NewMemNotifyTrail(), &fakeSink{}

	rn, err := NewRunNotifier(j, WithNotifier(sink, tr, fixedNotifyClock(clk), WithNotifyHolder("flow")))
	if err != nil {
		t.Fatal(err)
	}
	rn.DriveEnd(flow, &ApprovalPending{RunRef: RunRef{RunID: flow, RootRunID: flow}, ToolUseID: "node:pay", ToolName: "pay"})
	if err := rn.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 || sink.calls[0].RunID != flow {
		t.Fatalf("the hook path: %d deliveries, %+v", sink.count(), sink.calls)
	}
	if err := sweep(t, j, sink, tr, clk); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 {
		t.Fatalf("the sweep delivered the gate again: %d deliveries", sink.count())
	}
	for name, recs := range tr.trails {
		if len(recs) > 0 && name != NotifyTrailName(flow) && !strings.HasPrefix(name, NotifyOpenTrail) {
			t.Fatalf("a record under %s: the event belongs to %s only", name, NotifyTrailName(flow))
		}
	}
}

// Every segment is indexed, in the index of the year it is named for, whatever the writer's clock
// or how many days passed between the crash and the repair.
func TestResolved_EverySegmentIsIndexedUnderItsOwnYearInOrder(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 12, 30, 9, 0, 0, 0, time.UTC)}
	tr := NewMemNotifyTrail()
	e := testEngine(t, memJournal(), &fakeSink{}, tr, clk, "p1")
	if err := e.watch(ctx, "k1", "r"); err != nil {
		t.Fatal(err)
	}
	d1 := OpenSegment(clk.now())
	recs, _ := tr.Load(ctx, d1)
	// A roller sealed day 1 into day 2 and wrote day 2's checkpoint, then died; days later (and
	// across the new year) another writer comes, and a third unindexed segment follows.
	clk.advance(24 * time.Hour)
	d2 := OpenSegment(clk.now())
	if err := tr.Append(ctx, d1, len(recs), NotifyRecord{Kind: NotifySeal, At: clk.now(), Next: d2}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(ctx, d2, 0, NotifyRecord{Kind: NotifyCheckpoint, At: clk.now(), Watching: FoldWatch(recs)}); err != nil {
		t.Fatal(err)
	}
	clk.advance(5 * 24 * time.Hour) // 2027-01-04
	if err := e.watch(ctx, "k2", "r"); err != nil {
		t.Fatal(err)
	}
	d3 := OpenSegment(clk.now())
	got := map[string][]string{}
	for _, year := range []int{2026, 2027} {
		irecs, _ := tr.Load(ctx, OpenIndex(year))
		for _, r := range irecs {
			got[OpenIndex(year)] = append(got[OpenIndex(year)], r.Next)
		}
	}
	want := map[string][]string{OpenIndex(2026): {d1, d2}, OpenIndex(2027): {d3}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("indexes = %v, want %v", got, want)
	}
}

// A process whose clock runs days ahead seals a segment for a day no other clock has reached. The
// other processes follow the chain to it and write there, and every record in a segment is dated no
// earlier than its day, so the open trail stays one a verifier accepts.
func TestResolved_AClockAheadSealsADayNoOneElseHasReached(t *testing.T) {
	ctx := context.Background()
	clkA, clkB := resolvedClock(), resolvedClock()
	tr := NewMemNotifyTrail()
	a := testEngine(t, memJournal(), &fakeSink{}, tr, clkA, "a")
	b := testEngine(t, memJournal(), &fakeSink{}, tr, clkB, "b")
	if err := b.watch(ctx, "k1", "r"); err != nil {
		t.Fatal(err)
	}
	clkA.advance(3 * 24 * time.Hour) // A is three days ahead
	if err := a.watch(ctx, "k2", "r"); err != nil {
		t.Fatal(err)
	}
	if err := b.watch(ctx, "k3", "r"); err != nil { // B, on its own day, follows the chain
		t.Fatal(err)
	}
	if err := b.unwatch(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	name, recs, err := b.openHead(ctx)
	if err != nil || name != OpenSegment(clkA.now()) {
		t.Fatalf("head = %s, %v: B did not follow the chain to A's segment", name, err)
	}
	got := FoldOpen(recs).Watched
	if len(got) != 2 || got[0].Key != "k2" || got[1].Key != "k3" {
		t.Fatalf("listed = %+v, want k2 and k3 (k1 unwatched)", got)
	}
	day := segmentDayOf(name)
	clamped := 0
	for i, r := range recs {
		if !r.ClockAt.IsZero() {
			clamped++
			if !r.ClockAt.Before(r.At) {
				t.Errorf("record %d: clock_at %s is not before at %s", i, r.ClockAt, r.At)
			}
		}
		if r.At.Before(day) {
			t.Errorf("record %d (%s) is dated %s, before its segment's day %s", i, r.Kind, r.At, day)
		}
		if i > 0 && r.At.Before(recs[i-1].At) {
			t.Errorf("record %d (%s) is dated %s, before record %d (%s)", i, r.Kind, r.At, i-1, recs[i-1].At)
		}
	}
	// B's watch of k3 was written with its clock three days behind the segment: dated forward, and
	// the clock reading it was moved from is kept.
	if clamped == 0 {
		t.Error("no record keeps the writer's own clock reading although B's was moved forward")
	}
	for _, r := range recs {
		if r.Key == "k3" && (r.ClockAt.IsZero() || !r.ClockAt.Equal(clkB.now())) {
			t.Errorf("k3's record: clock_at = %s, want B's clock %s", r.ClockAt, clkB.now())
		}
		if r.Key == "k2" && !r.ClockAt.IsZero() {
			t.Errorf("A's record was not moved, yet clock_at = %s", r.ClockAt)
		}
	}
	for _, y := range []int{clkA.now().Year()} {
		idx, _ := tr.Load(ctx, OpenIndex(y))
		for i, r := range idx {
			if r.At.Before(segmentDayOf(r.Next)) {
				t.Errorf("index record %d is dated before the segment it names", i)
			}
		}
	}
}

// The roller with the clock ahead dies after sealing, before the checkpoint: the process behind
// writes the checkpoint into the future segment, dated no earlier than the segment's day.
func TestResolved_ACheckpointWrittenFromBehindIsDatedInItsSegmentsDay(t *testing.T) {
	ctx := context.Background()
	clkA, clkB := resolvedClock(), resolvedClock()
	tr := NewMemNotifyTrail()
	b := testEngine(t, memJournal(), &fakeSink{}, tr, clkB, "b")
	if err := b.watch(ctx, "k1", "r"); err != nil {
		t.Fatal(err)
	}
	day0 := OpenSegment(clkB.now())
	recs, _ := tr.Load(ctx, day0)
	clkA.advance(3 * 24 * time.Hour)
	day3 := OpenSegment(clkA.now())
	if err := tr.Append(ctx, day0, len(recs), NotifyRecord{Kind: NotifySeal, At: clkA.now(), Next: day3}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(ctx, OpenIndex(clkA.now().Year()), 1, NotifyRecord{Kind: NotifySegment, At: clkA.now(), Next: day3}); err != nil {
		t.Fatal(err)
	}
	name, head, err := b.openHead(ctx)
	if err != nil || name != day3 || len(head) != 1 || head[0].Kind != NotifyCheckpoint {
		t.Fatalf("head = %s %v %v", name, head, err)
	}
	if head[0].At.Before(segmentDayOf(day3)) {
		t.Fatalf("the checkpoint is dated %s, before its segment's day", head[0].At)
	}
}
