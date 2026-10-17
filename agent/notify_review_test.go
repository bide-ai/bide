package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Adversarial review of the notifier core and NotifyPending (PR #243). Every test here states a
// guarantee of docs/design/notifier.md and fails on the branch it was written against; each is a
// reproduction, not a fix. Run them with: go test -run TestReview ./agent/

// hangSink blocks every call whose key is hang (or every call, with all set) until release is
// closed or the call's context ends. It records the keys it saw and whether each call's context
// carried a deadline.
type hangSink struct {
	mu          sync.Mutex
	hang        string
	all         bool
	release     chan struct{}
	keys        []string
	hadDeadline []bool
	inflight    atomic.Int32
	maxInflight atomic.Int32
}

func newHangSink(hang string) *hangSink { return &hangSink{hang: hang, release: make(chan struct{})} }

func (h *hangSink) Notify(ctx context.Context, n Notification) error {
	_, dl := ctx.Deadline()
	h.mu.Lock()
	h.keys = append(h.keys, n.Key)
	h.hadDeadline = append(h.hadDeadline, dl)
	block := h.all || n.Key == h.hang
	h.mu.Unlock()
	if !block {
		return nil
	}
	if v := h.inflight.Add(1); v > h.maxInflight.Load() {
		h.maxInflight.Store(v)
	}
	defer h.inflight.Add(-1)
	select {
	case <-h.release:
		return errors.New("released")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hangSink) saw(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range h.keys {
		if k == key {
			return true
		}
	}
	return false
}

func (h *hangSink) count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.keys) }

// pollUntil polls cond for up to d and reports whether it became true.
func pollUntil(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func countKind(recs []NotifyRecord, kind NotifyRecordKind, key string) int {
	n := 0
	for _, r := range recs {
		if r.Kind == kind && (key == "" || r.Key == key) {
			n++
		}
	}
	return n
}

// gateRun journals run r paused at a single-approver gate on call c1 of tool refund.
func gateRun(t *testing.T) *Journal {
	t.Helper()
	ctx := context.Background()
	j := memJournal()
	tool := MustFunc("refund", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
	m := NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("done"))
	if _, err := mustNew(m, j, WithTools(tool)).Run(ctx, "r", UserText("go")); !errors.As(err, new(*ApprovalPending)) {
		t.Fatalf("setup Run = %v", err)
	}
	return j
}

// F1. "A late or failing notification is itself an event, not a silence" (Goals 3), and the
// overdue notice goes to WithOverdueNotifier "if the first is the thing that is down". In an app
// that relies on NotifyPending, each sweep is a fresh engine, and the overdue notice is raised only
// by the round that appends the overdue record. If that one delivery fails, no later sweep ever
// tries the notice again: its event is open in the trail and nothing re-derives it.
func TestReview_SweepNeverRetriesAFailedOverdueNotice(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "r", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	trail := NewMemNotifyTrail()
	primary := &fakeSink{fail: 1 << 30}                          // the main sink is down
	overdue := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent} // the overdue sink is down for one sweep
	opt := WithOverdueNotifier(overdue)

	_ = sweep(t, j, primary, trail, clk, opt)
	clk.advance(6 * time.Minute)
	_ = sweep(t, j, primary, trail, clk, opt) // past the deadline: overdue raised, its one send fails
	if overdue.count() == 0 {
		t.Fatal("setup: the overdue notice was not sent")
	}
	overdue.mu.Lock()
	overdue.fail = 0 // the overdue sink is back
	overdue.mu.Unlock()
	for range 6 {
		clk.advance(20 * time.Minute)
		_ = sweep(t, j, primary, trail, clk, opt)
	}
	recs, _ := trail.Load(ctx, NotifyTrailName("r"))
	notice := OverdueKey(ApprovalKey("r", "c1", 0))
	if countKind(recs, NotifyAcked, notice) == 0 {
		t.Fatalf("2 hours and 6 sweeps later the overdue notice was tried %d time(s) and never acknowledged, while the original is still open: trail %v",
			overdue.count(), recKinds(recs))
	}
}

// F1b. The same loss without a sweep: a process appends the overdue record and dies before the
// notice's own observed record. Every later round of the original sees Overdue set and never
// raises the notice again.
func TestReview_OverdueNoticeLostIfTheProcessDiesAfterTheOverdueRecord(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	clk := &notifyClock{t: t0.Add(7 * time.Minute)}
	ev := haltEvent()
	trail := NewMemNotifyTrail()
	name := NotifyTrailName("r")
	for i, rec := range []NotifyRecord{
		{Kind: NotifyObserved, Key: ev.key, RunID: "r", At: t0, EventKind: string(ev.kind), EventAt: t0, DeadlineAt: t0.Add(5 * time.Minute)},
		{Kind: NotifyOverdue, Key: ev.key, RunID: "r", At: t0.Add(6 * time.Minute), Latency: 6 * time.Minute},
		// the process died here, before it recorded the overdue notice as an event
	} {
		if err := trail.Append(ctx, name, i, rec); err != nil {
			t.Fatal(err)
		}
	}
	primary, overdue := &fakeSink{fail: 1 << 30}, &fakeSink{}
	e := testEngine(t, memJournal(), primary, trail, clk, "p2", WithOverdueNotifier(overdue))
	for range 4 {
		_, _ = e.round(ctx, ev)
		clk.advance(20 * time.Minute)
	}
	_ = e.flush(ctx)
	if overdue.count() == 0 {
		t.Fatalf("the original is %v overdue and still failing, and no overdue notice was ever sent", clk.now().Sub(t0))
	}
}

// F2. A Notifier with no timeout of its own (the interface does not require one, and the engine
// sets none) hangs the worker: every other event of the process waits behind it, the hung event's
// claim lapses while it is in flight, and its overdue notice (to a healthy WithOverdueNotifier) is
// queued behind it too. The engine should bound each call, by the claim TTL at most.
func TestReview_AHungSinkStallsEveryOtherEvent(t *testing.T) {
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	evA := haltEvent()
	sink := newHangSink(evA.key)
	e := testEngine(t, memJournal(), sink, NewMemNotifyTrail(), clk, "p1")
	defer func() { close(sink.release); _ = e.flush(context.Background()) }()

	e.w.submit(evA)
	if !pollUntil(time.Second, func() bool { return sink.saw(evA.key) }) {
		t.Fatal("setup: A was not sent")
	}
	evB := notifyEvent{key: ApprovalKey("r2", "c9", 0), kind: NotificationApprovalPending, runID: "r2",
		approval: &NotifyApproval{ToolUseID: "c9", ToolName: "pay"}}
	e.w.submit(evB)
	delivered := pollUntil(500*time.Millisecond, func() bool { return sink.saw(evB.key) })
	sink.mu.Lock()
	deadline := sink.hadDeadline[0]
	sink.mu.Unlock()
	if !delivered || !deadline {
		t.Fatalf("with one sink call hung: event B delivered=%v; the hung call had a deadline=%v (claim TTL %v)",
			delivered, deadline, e.cfg.claimTTL)
	}
}

// F3. RecoverLoop flushes its notifier on the way out with context.WithoutCancel, and the worker's
// rounds run under context.Background, so a sink that does not return keeps RecoverLoop from
// returning after its context is cancelled: a shutdown hang.
func TestReview_RecoverLoopReturnsOnCancelWithAHungSink(t *testing.T) {
	j := gateRun(t)
	sink := newHangSink("")
	sink.all = true
	resume := func(context.Context, string, RunStart) error {
		return &ApprovalPending{RunRef: RunRef{RunID: "r", RootRunID: "r"}, ToolUseID: "c1", ToolName: "refund"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RecoverLoop(ctx, j, resume, WithNotifier(sink, NewMemNotifyTrail()), WithRecoverInterval(10*time.Millisecond))
	}()
	if !pollUntil(2*time.Second, func() bool { return sink.count() > 0 }) {
		cancel()
		t.Fatal("setup: the gate was not sent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(sink.release)
		<-done
		t.Fatal("RecoverLoop did not return within 2s of its context being cancelled: its notifier flush waits on a hung sink")
	}
	close(sink.release)
}

// F4. The worker loop defers t.Stop() inside its for loop, so every wait adds a deferred call (and
// keeps its timer reachable) until the goroutine exits, which with pending events is never. Each
// RecoverLoop pass re-submits a still-pending event and so wakes the loop: memory grows with
// uptime.
func TestReview_WorkerMemoryDoesNotGrowWithWakeups(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	sink := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
	e := testEngine(t, memJournal(), sink, NewMemNotifyTrail(), clk, "p", WithNotifyRetry(time.Hour, time.Hour))
	defer func() { _ = e.flush(ctx) }()
	ev := haltEvent()
	e.w.submit(ev)
	if !pollUntil(2*time.Second, func() bool { return sink.count() > 0 && e.w.settled() }) {
		t.Fatal("setup: the first round did not run")
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapInuse
	}
	before := heap()
	const wakes = 50000
	for range wakes {
		e.w.submit(ev) // already pending: only wakes the worker, as a RecoverLoop pass does
		for len(e.w.wake) > 0 {
			runtime.Gosched()
		}
	}
	after := heap()
	if after > before && after-before > 2<<20 {
		t.Fatalf("heap grew %d KiB over %d wakeups of an idle worker (one deferred t.Stop and live timer per wait)", (after-before)>>10, wakes)
	}
}

// F5. The engine's per-key counters (attempts, fails, firstAt) are dropped only when this process
// records the ack. An event that is cleared, or acked by another process, after a failed attempt
// stays in the maps for the life of the process.
func TestReview_ClosedEventsAreForgotten(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	j := memJournal()
	sink := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
	e := testEngine(t, j, sink, NewMemNotifyTrail(), clk, "p")
	ev := haltEvent()
	_, _ = e.round(ctx, ev)
	if _, _, err := j.putEntry(ctx, "r", ToolResultStep("c1"), Record{Kind: StepToolResult, ToolUseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatalf("round after the resolution = %+v, %v", res, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.attempts)+len(e.fails)+len(e.firstAt) != 0 {
		t.Fatalf("a cleared event is still tracked: attempts=%v fails=%v firstAt=%d", e.attempts, e.fails, len(e.firstAt))
	}
}

// F6. "After the 8th total failure append failing once" (Protocol step 5). The count is held in
// the engine's memory, so an app that uses NotifyPending (a fresh engine each sweep, 3 attempts a
// round) never reaches it, and neither does a process that restarts: the failing record and the
// WithNotifyErrors report are never produced, however long the sink is down.
func TestReview_FailingIsRecordedAcrossSweeps(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "r", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	trail, sink := NewMemNotifyTrail(), &fakeSink{fail: 1 << 30}
	var reported atomic.Int32
	for range 10 {
		_ = sweep(t, j, sink, trail, clk, WithNotifyErrors(func(err error) {
			if strings.Contains(err.Error(), "is failing") {
				reported.Add(1)
			}
		}))
		clk.advance(20 * time.Minute)
	}
	recs, _ := trail.Load(ctx, NotifyTrailName("r"))
	if countKind(recs, NotifyFailing, "") == 0 || reported.Load() == 0 {
		t.Fatalf("%d failed attempts over 10 sweeps: failing records=%d, failing reports=%d",
			sink.count(), countKind(recs, NotifyFailing, ""), reported.Load())
	}
}

// F7. "At most about four claimed records an hour for an undelivered event" at the backoff cap.
// A sweep is a fresh engine with no backoff state, so the trail grows with the sweep rate: a sweep
// every minute writes a claim every other minute, forever.
func TestReview_SweepClaimsAreBoundedByTheBackoffCap(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, "r", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	trail, sink := NewMemNotifyTrail(), &fakeSink{fail: 1 << 30}
	before := 0
	for minute := range 180 {
		if minute == 120 {
			recs, _ := trail.Load(ctx, NotifyTrailName("r"))
			before = countKind(recs, NotifyClaimed, "")
		}
		_ = NotifyPending(ctx, j, sink, trail, fixedNotifyClock(clk)) // a fresh holder per sweep, as by default
		clk.advance(time.Minute)
	}
	recs, _ := trail.Load(ctx, NotifyTrailName("r"))
	if got := countKind(recs, NotifyClaimed, "") - before; got > 6 {
		t.Fatalf("the third hour of a sink outage wrote %d claimed records for one event (sweep every minute); the design bounds it at about 4", got)
	}
}

// F8. A pass that ends in a pause (a gate, a halt) is not a failure, and the design counts it as a
// success ("both are pauses, which a pass counts as success"). afterRecover returns early for a
// halt or a gate, so it never closes an open failure episode. The episode's failing_since then
// stays old, and the next transient failure, possibly days later, alerts at once.
func TestReview_APauseClosesTheRecoveryEpisode(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	j := gateRun(t)
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	broken := func(context.Context, string, RunStart) error { return errors.New("store blip") }
	paused := func(context.Context, string, RunStart) error {
		return &ApprovalPending{RunRef: RunRef{RunID: "r", RootRunID: "r"}, ToolUseID: "c1", ToolName: "refund"}
	}
	pass := func(resume Resumer) {
		t.Helper()
		_, _ = Recover(ctx, j, resume, WithNotifier(sink, trail, fixedNotifyClock(clk), WithNotifyHolder("p")))
	}
	pass(broken) // one failed pass opens an episode
	clk.advance(time.Minute)
	pass(paused) // recovery works: the run is waiting for its approver
	for range 24 {
		clk.advance(time.Hour)
		pass(paused)
	}
	pass(broken) // a day later, one transient failure
	time.Sleep(20 * time.Millisecond)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, n := range sink.calls {
		if n.Kind == NotificationRecoveryFailing {
			t.Fatalf("one failed pass after a day of successful (paused) passes raised %s: the episode opened a day ago was never closed", n.Key)
		}
	}
}

// F9. The events table lists "not resumable" as a recovery_failing cause, and causeClass has a
// "not_resumable" class, but recoverRun turns a Resumer's ErrNotResumable into a nil error with
// resumed=false, so afterRecover never sees it: a run no Resumer can resume never alerts.
func TestReview_NotResumableRaisesRecoveryFailing(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	j := gateRun(t)
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	stuck := func(context.Context, string, RunStart) error {
		return fmt.Errorf("no agent registered for this run: %w", ErrNotResumable)
	}
	for range 4 {
		_, _ = Recover(ctx, j, stuck, WithNotifier(sink, trail, fixedNotifyClock(clk), WithNotifyHolder("p")))
		clk.advance(10 * time.Minute)
	}
	time.Sleep(20 * time.Millisecond)
	recs, _ := trail.Load(ctx, NotifyTrailName("r"))
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, n := range sink.calls {
		if n.Kind == NotificationRecoveryFailing {
			return
		}
	}
	t.Fatalf("a run that is not resumable for 40 minutes raised no recovery_failing (trail %v)", recKinds(recs))
}

// F10. Protocol step 3: the event is current while "no decision" is recorded. current() and the
// sweep test only for the tool's result, which is written when the run is next driven, not when
// the approver decides. A gate decided while the sink is down is delivered as pending afterwards
// (and goes overdue), for as long as nothing re-drives the run; the sweep also reports a decided
// gate it has never seen.
func TestReview_ADecidedGateIsNotReported(t *testing.T) {
	ctx := context.Background()
	decide := func(t *testing.T, j *Journal) {
		t.Helper()
		if err := Approve(ctx, j, "r", "c1", true); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("sweep", func(t *testing.T) {
		j := gateRun(t)
		decide(t, j)
		sink := &fakeSink{}
		_ = sweep(t, j, sink, NewMemNotifyTrail(), sweepClock())
		if sink.count() != 0 {
			t.Fatalf("the sweep reported a gate the approver had already decided (%d notification)", sink.count())
		}
	})
	t.Run("stale round", func(t *testing.T) {
		clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
		j := gateRun(t)
		sink := &fakeSink{fail: 3}
		e := testEngine(t, j, sink, NewMemNotifyTrail(), clk, "p")
		ev := notifyEvent{key: ApprovalKey("r", "c1", 0), kind: NotificationApprovalPending, runID: "r", opRun: "r",
			approval: &NotifyApproval{ToolUseID: "c1", ToolName: "refund"}}
		_, _ = e.round(ctx, ev) // the sink is down
		decide(t, j)
		clk.advance(10 * time.Minute)
		_, _ = e.round(ctx, ev)
		_ = e.flush(ctx)
		pending := 0
		for _, c := range sink.calls {
			if c.Kind == NotificationApprovalPending {
				pending++
			}
		}
		if pending > 3 {
			t.Fatalf("a gate decided %v earlier was delivered as pending after the decision", 10*time.Minute)
		}
		// The three failed attempts may have landed, so the receiver is told it is over.
		if last := sink.calls[len(sink.calls)-1]; last.Kind != NotificationResolved || last.Reason != "decided" {
			t.Fatalf("last call = %+v, want the all-clear", last)
		}
	})
}

// F11. "The at-least-once guarantee needs one of two catch-up paths": RecoverLoop or NotifyPending.
// For run_aborted there is neither: both enumerate runs with recoverFilter, which excludes a run
// holding run:aborted. A process that dies between the abort and the observed record loses the
// event for good.
func TestReview_AnAbortedRunHasACatchUpPath(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	clk := sweepClock()
	// An aborted run has a run:start, which records when it started: that is the time the sweep
	// bounds a run_aborted catch-up by (the abort itself records none).
	started := RunStart{StartedAt: clk.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}
	startJSON, _ := json.Marshal(started)
	if _, _, err := j.putEntry(ctx, "r", runStartStep, Record{Kind: StepValue, Result: startJSON}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: mustJSON("refund failed; rolled back")}); err != nil {
		t.Fatal(err)
	}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	_, _ = Recover(ctx, j, func(context.Context, string, RunStart) error { return nil }, WithNotifier(sink, trail, fixedNotifyClock(clk)))
	_ = sweep(t, j, sink, trail, clk)
	if sink.count() == 0 {
		t.Fatal("an aborted run whose drive died before recording the event is found by neither Recover nor NotifyPending")
	}
}

// loadBlipTrail fails the next Load after failNext is set.
type loadBlipTrail struct {
	*MemNotifyTrail
	failNext atomic.Bool
}

func (l *loadBlipTrail) Load(ctx context.Context, trail string) ([]NotifyRecord, error) {
	if l.failNext.CompareAndSwap(true, false) {
		return nil, errors.New("store blip")
	}
	return l.MemNotifyTrail.Load(ctx, trail)
}

type hookSink struct {
	fakeSink
	onCall func(n int)
}

func (h *hookSink) Notify(ctx context.Context, n Notification) error {
	err := h.fakeSink.Notify(ctx, n)
	h.onCall(h.fakeSink.count())
	return err
}

// F12. The failing record is attempted only when the failure count equals failingAfter exactly.
// One store error at that moment and it is never written: every later failure is past the count.
func TestReview_FailingRecordSurvivesABlipAtTheThreshold(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := &loadBlipTrail{MemNotifyTrail: NewMemNotifyTrail()}
	sink := &hookSink{fakeSink: fakeSink{fail: 1 << 30}}
	sink.onCall = func(n int) {
		if n == 8 {
			trail.failNext.Store(true)
		}
	}
	// A deadline far off, so no overdue notice shares the sink and the trail with the event.
	e := testEngine(t, memJournal(), sink, trail, clk, "p", WithNotifyDeadline(1000*time.Hour))
	ev := haltEvent()
	for range 10 {
		_, _ = e.round(ctx, ev)
		clk.advance(time.Hour)
	}
	recs, _ := trail.Load(ctx, NotifyTrailName("r"))
	if countKind(recs, NotifyFailing, "") == 0 {
		t.Fatalf("%d failed attempts and no failing record, after one store error at the 8th", sink.count())
	}
}

// F13 (design gap). "No two processes are in flight for one key while one claimed.until is in the
// future." Until is written by the holder's clock and read by the other's; a peer whose clock runs
// ahead by more than the claim's remaining time takes over a live claim and sends concurrently.
// Nothing bounds or detects skew.
func TestReview_ClockSkewBreaksTheLiveClaim(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	trail := NewMemNotifyTrail()
	ev := haltEvent()
	sink := newHangSink(ev.key)
	p1 := testEngine(t, memJournal(), sink, trail, &notifyClock{t: t0}, "p1")
	p2 := testEngine(t, memJournal(), sink, trail, &notifyClock{t: t0.Add(3 * time.Minute)}, "p2") // 3 minutes ahead
	done := make(chan struct{})
	go func() { _, _ = p1.round(ctx, ev); close(done) }()
	if !pollUntil(time.Second, func() bool { return sink.inflight.Load() == 1 }) {
		t.Fatal("setup: p1 is not in flight")
	}
	go func() { _, _ = p2.round(ctx, ev) }()
	pollUntil(300*time.Millisecond, func() bool { return sink.inflight.Load() == 2 })
	got := sink.maxInflight.Load()
	close(sink.release)
	<-done
	if got > 1 {
		t.Fatalf("%d processes in flight for one key while p1's claim was live by p1's clock (p2 runs 3 minutes ahead)", got)
	}
}

// F14. An Agent's notifier worker has no Close and outlives the work: after its only event is
// acknowledged, the goroutine waits forever on its wake channel unless FlushNotifications is
// called. An app that builds an Agent per request leaks one goroutine per Agent that saw an event.
func TestReview_AnIdleNotifierWorkerExits(t *testing.T) {
	ctx := context.Background()
	workers := func() int {
		buf := make([]byte, 1<<22)
		return strings.Count(string(buf[:runtime.Stack(buf, true)]), "(*notifyWorker).loop")
	}
	base := workers()
	j := gateRun(t)
	sink := &fakeSink{}
	tool := MustFunc("refund", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithApproval(SingleApproval()))
	for range 5 {
		a := mustNew(NewScriptedModel(), j, WithTools(tool), WithNotifier(sink, NewMemNotifyTrail()))
		_, _ = a.Run(ctx, "r", UserText("go"))
		if !pollUntil(time.Second, func() bool { return a.notify.w.idle() }) {
			t.Fatal("setup: the event was not delivered")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := workers() - base; n > 0 {
		t.Fatalf("%d notifier worker goroutines are still running after 5 Agents delivered their only event and went idle", n)
	}
}

// M2 (Model 14 finding 2). Two deliverers in one process given one holder name (WithNotifyHolder
// passed to both an Agent's WithNotifier and NotifyPending, say) each read the other's claim as
// their own and deliver concurrently. The docs forbid only two processes sharing a name.
func TestReview_SharedHolderNameDeliversConcurrently(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := NewMemNotifyTrail()
	ev := haltEvent()
	sink := newHangSink(ev.key)
	drive := testEngine(t, memJournal(), sink, trail, clk, "app")
	sweeper := testEngine(t, memJournal(), sink, trail, clk, "app")
	done := make(chan struct{})
	go func() { _, _ = drive.round(ctx, ev); close(done) }()
	if !pollUntil(time.Second, func() bool { return sink.inflight.Load() == 1 }) {
		t.Fatal("setup: the drive's deliverer is not in flight")
	}
	go func() { _, _ = sweeper.round(ctx, ev) }()
	pollUntil(300*time.Millisecond, func() bool { return sink.inflight.Load() == 2 })
	got := sink.maxInflight.Load()
	close(sink.release)
	<-done
	if got > 1 {
		t.Fatalf("%d deliverers in flight for one key under one live claim", got)
	}
}

// ackFlakyTrail fails the first n appends of an acked record.
type ackFlakyTrail struct {
	*MemNotifyTrail
	n atomic.Int32
}

func (a *ackFlakyTrail) Append(ctx context.Context, trail string, seq int, rec NotifyRecord) error {
	if rec.Kind == NotifyAcked && a.n.Add(-1) >= 0 {
		return errors.New("store blip")
	}
	return a.MemNotifyTrail.Append(ctx, trail, seq, rec)
}

// M3 (Model 14 finding 3). A store blip longer than ackRetries back-to-back appends (no delay
// between them) abandons the ack, and the next round sends the accepted notification again.
func TestReview_AckAppendErrorIsRetriedNotRedelivered(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := &ackFlakyTrail{MemNotifyTrail: NewMemNotifyTrail()}
	trail.n.Store(5)
	sink := &fakeSink{}
	e := testEngine(t, memJournal(), sink, trail, clk, "p")
	ev := haltEvent()
	for range 3 {
		if res, _ := e.round(ctx, ev); res.done {
			break
		}
		clk.advance(2 * time.Second)
	}
	if sink.count() != 1 {
		t.Fatalf("the sink accepted once and was sent the same event %d times: the ack was abandoned after a 5-append blip", sink.count())
	}
}

// Second round of the adversarial review, against the fixed stack (681e5f8e). Tests named
// TestReview2_ fail there and reproduce a finding.

// realEngine builds an engine on the real clock with scaled-down timings, for races that depend
// on the per-call timeout, which runs on real time.
func realEngine(t *testing.T, j *Journal, n Notifier, trail NotifyTrail, holder string, tune func(*notifyConfig), opts ...NotifyOption) *notifyEngine {
	t.Helper()
	all := append([]NotifyOption{
		WithNotifyHolder(holder),
		notifyOption(func(c *notifyConfig) error {
			c.jitter = func(d time.Duration) time.Duration { return d }
			if tune != nil {
				tune(c)
			}
			return nil
		}),
	}, opts...)
	e, err := newNotifyEngine(j, n, trail, all, "test")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// deafSink blocks every call for a key in hang, ignoring its context, until release is closed. The
// design allows for it ("a Notifier may ignore its context"). Other keys succeed.
type deafSink struct {
	mu      sync.Mutex
	hang    map[string]bool
	all     bool
	release chan struct{}
	ok      []string
}

func (d *deafSink) Notify(_ context.Context, n Notification) error {
	d.mu.Lock()
	block := d.all || d.hang[n.Key]
	d.mu.Unlock()
	if block {
		<-d.release
		return errors.New("released")
	}
	d.mu.Lock()
	d.ok = append(d.ok, n.Key)
	d.mu.Unlock()
	return nil
}

func (d *deafSink) delivered(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range d.ok {
		if k == key {
			return true
		}
	}
	return false
}

// R2-1. The per-call timeout cancels the call's context, but the engine still waits for Notify to
// return. A primary Notifier that ignores its context holds the round, and with it the event's key
// lock and the round slot, forever: the event never gets another round in this process, so its
// overdue record is never written and the notice never reaches a healthy WithOverdueNotifier, the
// escape hatch for exactly "the first sink is the thing that is down".
func TestReview2_ADeafPrimarySinkStillRaisesTheOverdueNotice(t *testing.T) {
	ev := haltEvent()
	primary := &deafSink{hang: map[string]bool{ev.key: true}, release: make(chan struct{})}
	overdue := &fakeSink{}
	e := realEngine(t, memJournal(), primary, NewMemNotifyTrail(), "p1", func(c *notifyConfig) {
		c.claimTTL, c.attemptBackoff = 400*time.Millisecond, time.Millisecond
	}, WithNotifyDeadline(150*time.Millisecond), WithNotifyRetry(10*time.Millisecond, 10*time.Millisecond), WithOverdueNotifier(overdue))
	defer func() { close(primary.release); _ = e.flush(context.Background()) }()

	e.w.submit(ev)
	if !pollUntil(2*time.Second, func() bool { return overdue.count() > 0 }) {
		t.Fatal("the primary sink hung (ignoring its context) and the overdue notice never reached the healthy overdue sink, 2s after a 150ms deadline")
	}
}

// R2-2. Rounds are bounded at maxInflight (8). Eight events whose sink ignores its context take
// every slot for good, and no later event of the engine is ever delivered.
func TestReview2_DeafSinksDoNotStarveOtherEvents(t *testing.T) {
	sink := &deafSink{hang: map[string]bool{}, release: make(chan struct{})}
	var evs []notifyEvent
	for i := range 9 {
		run := string(rune('a' + i))
		ev := notifyEvent{key: ApprovalKey(run, "c1", 0), kind: NotificationApprovalPending, runID: run,
			approval: &NotifyApproval{ToolUseID: "c1", ToolName: "pay"}}
		if i < 8 {
			sink.hang[ev.key] = true
		}
		evs = append(evs, ev)
	}
	e := realEngine(t, memJournal(), sink, NewMemNotifyTrail(), "p1", func(c *notifyConfig) {
		c.claimTTL, c.attemptBackoff = 400*time.Millisecond, time.Millisecond
	})
	defer func() { close(sink.release); _ = e.flush(context.Background()) }()
	for _, ev := range evs[:8] {
		e.w.submit(ev)
	}
	time.Sleep(50 * time.Millisecond)
	e.w.submit(evs[8])
	if !pollUntil(time.Second, func() bool { return sink.delivered(evs[8].key) }) {
		t.Fatal("8 events with a context-ignoring sink hold every round slot; a 9th, healthy event is never delivered")
	}
}

// R2-3. A round ends inside the claim it holds, for every allowed claim. The round is run on a fake
// clock, with a sink that takes the longest a call may (the call limit) every time and a backoff in
// the fake clock too, so the worst case is exercised at once and the test does not depend on the
// speed of the machine. The round owes an overdue notice (four calls), and starts at each of several
// points of its own earlier claim, so it reuses the claim or takes a new one; after it, the clock
// must not be past the end of the claim it held (the skew margin is extra, not part of the budget).
func TestReview2_ARoundEndsInsideItsClaim(t *testing.T) {
	ctx := context.Background()
	for _, ttl := range []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour} {
		for _, frac := range []float64{0, 0.1, 0.2, 0.26, 0.3, 0.5, 0.74} {
			clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
			trail, ev := NewMemNotifyTrail(), haltEvent()
			var e *notifyEngine
			slow := notifierFunc(func(context.Context, Notification) error {
				clk.advance(e.callTimeout()) // the worst a call may take
				return errors.New("slow sink")
			})
			e = testEngine(t, memJournal(), slow, trail, clk, "p1", WithNotifyClaimTTL(ttl), WithNotifyDeadline(time.Millisecond))
			_, _ = e.round(ctx, ev) // claims for ttl
			// Start the second round part way into its own claim.
			clk.advance(time.Duration(float64(ttl) * frac))
			_, _ = e.round(ctx, ev)
			recs, _ := trail.Load(ctx, NotifyTrailName("r"))
			var until time.Time
			for _, r := range recs {
				if r.Kind == NotifyClaimed && r.Until.After(until) {
					until = r.Until
				}
			}
			if clk.now().After(until) {
				t.Fatalf("ttl %v, round started %.0f%% into the claim: it ended %v past the end of its claim", ttl, frac*100, clk.now().Sub(until))
			}
		}
	}
}

// R2-4. "FlushNotifications returns what was not delivered." An event whose round found another
// deliverer's live claim records no error, so a flush returns nil for it although nothing has
// delivered it: two deliverers in one process (an Agent's notifier and RecoverLoop's) whose other
// one failed, or died, report a clean flush.
func TestReview2_FlushReportsAnEventAPeerHoldsUndelivered(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, j, ev := NewMemNotifyTrail(), memJournal(), haltEvent()
	down := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
	peer := testEngine(t, j, down, trail, clk, "recover")
	if _, err := peer.round(ctx, ev); err == nil {
		t.Fatal("setup: the peer's delivery should fail")
	}
	e := testEngine(t, j, down, trail, clk, "drive")
	e.w.submit(ev)
	if err := e.flush(ctx); err == nil {
		t.Fatal("FlushNotifications returned nil for an event that no deliverer has delivered (its round deferred to a peer whose delivery failed)")
	}
}

// R2-5. One key mutex per event key the engine ever saw, never removed: a long-running Agent's
// keyMu map grows with every event.
func TestReview2_KeyLocksAreForgotten(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	e := testEngine(t, memJournal(), &fakeSink{}, NewMemNotifyTrail(), clk, "p")
	for i := range 100 {
		run := "r" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		ev := notifyEvent{key: ApprovalKey(run, "c1", 0), kind: NotificationApprovalPending, runID: run,
			approval: &NotifyApproval{ToolUseID: "c1"}}
		if res, err := e.round(ctx, ev); err != nil || !res.done {
			t.Fatalf("round %d = %+v, %v", i, res, err)
		}
	}
	e.mu.Lock()
	n := len(e.keyMu)
	e.mu.Unlock()
	if n > 0 {
		t.Fatalf("%d key locks held for 100 events that are all closed", n)
	}
}

// R2-6 (design gap). The sweep now raises run_aborted for every aborted run with no event on its
// trail, whenever it aborted. The first sweep after a notifier is enabled pages once for every run
// that ever aborted, and every later sweep loads the trail of every aborted run ever (the listing
// has no end).
func TestReview2_EnablingTheSweepDoesNotPageForEveryHistoricalAbort(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	for i := range 20 {
		run := "old-" + string(rune('a'+i))
		if _, _, err := j.putEntry(ctx, run, runAbortedStep, Record{Kind: StepValue, Result: mustJSON("rolled back last year")}); err != nil {
			t.Fatal(err)
		}
	}
	sink := &fakeSink{}
	_ = sweep(t, j, sink, NewMemNotifyTrail(), sweepClock())
	if sink.count() > 0 {
		t.Fatalf("the first sweep paged %d times for runs that aborted before the notifier existed", sink.count())
	}
}

// Third round of the adversarial review, against 67b9016b. Tests named TestReview3_ fail there and
// reproduce a finding; TestReview3Held_ pass and record what held up.

// R3-1. The aborted-run catch-up is bounded by the run's start time (WithNotifyAbortedWithin,
// default 24h), because run:aborted records no time. A long-lived run that started three days ago
// and aborted a minute ago, by a process that died before raising the event, is never caught up:
// a silent drop for exactly the long-running runs bide is for.
func TestReview3_ARecentAbortOfALongRunningRunIsCaughtUp(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	start, _ := json.Marshal(RunStart{StartedAt: clk.now().Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)})
	if _, _, err := j.putEntry(ctx, "r", runStartStep, Record{Kind: StepValue, Result: start}); err != nil {
		t.Fatal(err)
	}
	// The run was busy until a minute ago: its last effect's marker says so.
	putMarker(t, j, "r", "c1", clk.now().Add(-2*time.Minute).UnixMilli())
	if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: mustJSON("refund failed; rolled back")}); err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	_ = sweep(t, j, sink, NewMemNotifyTrail(), clk)
	if sink.count() == 0 {
		t.Fatal("a run started 3 days ago and aborted a minute ago (its drive died before raising the event) is never caught up by the sweep")
	}
}

// foreverSink never returns from Notify, whatever its context does: a sink stuck in a syscall, or
// one that ignores cancellation. It counts the calls it holds.
type foreverSink struct{ held atomic.Int32 }

func (f *foreverSink) Notify(context.Context, Notification) error {
	f.held.Add(1)
	select {}
}

// R3-2. callNotify abandons a call that outlives its timeout, and its goroutine with it. Against a
// sink that never returns, every round abandons up to four more, without limit: goroutines (and
// the requests they hold) grow for as long as the process runs. The worker slots are freed, the
// leak moved.
func TestReview3_AbandonedCallsAreBounded(t *testing.T) {
	sink := &foreverSink{}
	ev := haltEvent()
	e := realEngine(t, memJournal(), sink, NewMemNotifyTrail(), "p1", func(c *notifyConfig) {
		c.claimTTL, c.attemptBackoff, c.skew = 80*time.Millisecond, time.Millisecond, time.Millisecond
	}, WithNotifyRetry(time.Millisecond, time.Millisecond), WithNotifyDeadline(time.Hour))
	defer func() { _ = e.flush(context.Background()) }()
	e.w.submit(ev)
	time.Sleep(time.Second)
	if n := sink.held.Load(); n > 16 {
		t.Fatalf("%d abandoned Notify goroutines after 1s against one event whose sink never returns (one claim is 80ms here; at the defaults that is about 16 an hour per event, forever)", n)
	}
}

// slowSink takes d to accept, honoring its context.
type slowSink struct {
	d  time.Duration
	ok atomic.Int32
}

func (s *slowSink) Notify(ctx context.Context, _ Notification) error {
	select {
	case <-time.After(s.d):
		s.ok.Add(1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// R3-3. A call is bounded by claimTTL/8. WithNotifyClaimTTL says the claim "must exceed one round's
// work", so an operator with a sink that answers in 200ms and a 1s claim (five times the call)
// configures what the doc allows and gets a delivery that can never succeed: every call times out
// at 125ms. Nothing refuses the setting or says why.
func TestReview3_AClaimTheDocAllowsCanDeliver(t *testing.T) {
	// A claim too short for a call to ever succeed is refused, naming the floor...
	_, err := newNotifyEngine(memJournal(), &slowSink{}, NewMemNotifyTrail(), []NotifyOption{WithNotifyClaimTTL(time.Second)}, "test")
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "30s") {
		t.Fatalf("WithNotifyClaimTTL(1s) = %v; want an ErrConfig that names the 30s floor", err)
	}
	// ... and one the option accepts delivers a sink that takes 200ms.
	sink := &slowSink{d: 200 * time.Millisecond}
	e := realEngine(t, memJournal(), sink, NewMemNotifyTrail(), "p1", func(c *notifyConfig) {
		c.attemptBackoff = time.Millisecond
	}, WithNotifyClaimTTL(30*time.Second), WithNotifyRetry(10*time.Millisecond, 10*time.Millisecond))
	defer func() { _ = e.flush(context.Background()) }()
	e.w.submit(haltEvent())
	if !pollUntil(3*time.Second, func() bool { return sink.ok.Load() > 0 }) {
		t.Fatal("a 200ms sink under WithNotifyClaimTTL(30s) was never delivered in 3s")
	}
}

// A call that outlives its limit says what the limit is and how to raise it.
func TestNotifyRound_ATimeoutNamesTheLimit(t *testing.T) {
	ctx := context.Background()
	sink := &slowSink{d: time.Hour}
	e := realEngine(t, memJournal(), sink, NewMemNotifyTrail(), "p", func(c *notifyConfig) {
		c.claimTTL, c.attemptBackoff = 80*time.Millisecond, time.Millisecond
	}, WithNotifyDeadline(time.Hour))
	_, err := e.round(ctx, haltEvent())
	if err == nil || !strings.Contains(err.Error(), "WithNotifyClaimTTL") || !strings.Contains(err.Error(), "an eighth of the claim") {
		t.Fatalf("err = %v; want the limit named", err)
	}
}

// gateSink blocks its first call until open is closed, then accepts it; later calls accept.
type gateSink struct {
	open  chan struct{}
	first atomic.Bool
	in    atomic.Bool
}

func (g *gateSink) Notify(ctx context.Context, _ Notification) error {
	if g.first.CompareAndSwap(false, true) {
		g.in.Store(true)
		<-g.open
	}
	return nil
}

// R3-4. Flush reports a deferral as undelivered. An event this engine deferred to a peer's live
// claim, which the peer then delivered and acknowledged, is still reported by flush as not
// delivered: a false positive that tells an operator, at shutdown, that a notification was lost.
func TestReview3_FlushDoesNotReportAnEventAPeerAcked(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, j, ev := NewMemNotifyTrail(), memJournal(), haltEvent()
	peerSink := &gateSink{open: make(chan struct{})}
	peer := testEngine(t, j, peerSink, trail, clk, "recover")
	done := make(chan struct{})
	go func() { _, _ = peer.round(ctx, ev); close(done) }()
	if !pollUntil(time.Second, func() bool { return peerSink.in.Load() }) {
		t.Fatal("setup: the peer is not delivering")
	}
	e := testEngine(t, j, &fakeSink{}, trail, clk, "drive")
	e.w.submit(ev)
	if !pollUntil(time.Second, func() bool { return e.w.settled() }) {
		t.Fatal("setup: the round did not defer")
	}
	close(peerSink.open)
	<-done
	recs, _ := trail.Load(ctx, NotifyTrailName("r"))
	if countKind(recs, NotifyAcked, ev.key) != 1 {
		t.Fatalf("setup: the peer did not ack: %v", recKinds(recs))
	}
	if err := e.flush(ctx); err != nil {
		t.Fatalf("flush reported an event the peer has acknowledged as undelivered: %v", err)
	}
}

// countSink counts calls in flight per key and the most at once.
type countSink struct {
	mu    sync.Mutex
	in    map[string]int
	max   int
	calls map[string]int
}

func (c *countSink) Notify(_ context.Context, n Notification) error {
	c.mu.Lock()
	c.in[n.Key]++
	c.calls[n.Key]++
	c.max = max(c.max, c.in[n.Key])
	c.mu.Unlock()
	time.Sleep(time.Millisecond)
	c.mu.Lock()
	c.in[n.Key]--
	c.mu.Unlock()
	return nil
}

// Held: the refcounted key lock. 64 concurrent rounds over two keys on one engine never overlap on
// a key, deliver each key once, and leave the lock map empty.
func TestReview3Held_KeyLockUnderContention(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	sink := &countSink{in: map[string]int{}, calls: map[string]int{}}
	e := testEngine(t, memJournal(), sink, NewMemNotifyTrail(), clk, "p")
	gate := notifyEvent{key: ApprovalKey("r", "c2", 0), kind: NotificationApprovalPending, runID: "r", approval: &NotifyApproval{ToolUseID: "c2"}}
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := haltEvent()
			if i%2 == 1 {
				ev = gate
			}
			_, _ = e.round(ctx, ev)
		}()
	}
	wg.Wait()
	e.mu.Lock()
	n := len(e.keyMu)
	e.mu.Unlock()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if n != 0 || sink.max > 1 || sink.calls[haltEvent().key] != 1 || sink.calls[gate.key] != 1 {
		t.Fatalf("key locks left %d, max in flight per key %d, calls %v", n, sink.max, sink.calls)
	}
}

// Fourth round of the adversarial review, against d3ddd1eb. Tests named TestReview4_ fail there and
// reproduce a finding.

// blockingLoadTrail blocks every Load after block is set, until the Load's context ends.
type blockingLoadTrail struct {
	*MemNotifyTrail
	block atomic.Bool
}

func (b *blockingLoadTrail) Load(ctx context.Context, trail string) ([]NotifyRecord, error) {
	if b.block.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.MemNotifyTrail.Load(ctx, trail)
}

// R4-1. flush rechecks deferred events in the trail under context.WithoutCancel(ctx), so with the
// trail store hung, a flush whose context has ended does not return. That undoes the bound on
// RecoverLoop's exit flush (500ms) and any caller's shutdown deadline: the trail is exactly what is
// likely to be down at shutdown.
func TestReview4_FlushReturnsAtItsDeadlineWithTheTrailHung(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := &blockingLoadTrail{MemNotifyTrail: NewMemNotifyTrail()}
	j, ev := memJournal(), haltEvent()
	down := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
	peer := testEngine(t, j, down, trail, clk, "recover")
	_, _ = peer.round(ctx, ev) // the peer holds a live claim
	e := testEngine(t, j, down, trail, clk, "drive")
	e.w.submit(ev)
	if !pollUntil(time.Second, func() bool { return e.w.settled() }) {
		t.Fatal("setup: the round did not defer")
	}
	trail.block.Store(true)
	fctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.flush(fctx) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		trail.block.Store(false) // the goroutine stays blocked; the test reports
		t.Fatal("flush with a 100ms context did not return in 2s: its recheck of a deferred event loads the hung trail without a deadline")
	}
}

// R4-2 (low). "A round ends inside the claim it holds" (the round budget). At the 30s floor it does
// not once the acknowledgement is retried: a round that reuses its claim with just over three
// quarters left, makes four calls that each take the limit, and then meets a store that refuses the
// acked append, retries the append for an eighth of the claim and sleeps once more past that limit
// (the check is before the sleep). The skew margin covers the overrun, so no peer sends at once;
// the stated bound is what fails.
func TestReview4_ARoundWithAnAckBlipEndsInsideItsClaim(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := &ackFlakyTrail{MemNotifyTrail: NewMemNotifyTrail()}
	trail.n.Store(1 << 30) // the acked append keeps failing
	var e *notifyEngine
	var slow atomic.Bool
	var calls atomic.Int32
	sink := notifierFunc(func(context.Context, Notification) error {
		if !slow.Load() {
			return ErrNotifyPermanent // round 1: fail at once
		}
		clk.advance(e.callTimeout())
		if calls.Add(1) < 4 {
			return errors.New("slow sink")
		}
		return nil // the fourth call of round 2 is accepted
	})
	e = testEngine(t, memJournal(), sink, trail, clk, "p1", WithNotifyClaimTTL(30*time.Second), WithNotifyDeadline(time.Second))
	ev := haltEvent()
	_, _ = e.round(ctx, ev) // claims for 30s, fails fast
	recs, _ := trail.Load(ctx, NotifyTrailName("r"))
	var claimAt time.Time
	for _, r := range recs {
		if r.Kind == NotifyClaimed {
			claimAt = r.At
		}
	}
	clk.t = claimAt.Add(7 * time.Second) // 23s of the claim left: more than three quarters, so it is reused
	slow.Store(true)
	_, _ = e.round(ctx, ev)
	recs, _ = trail.Load(ctx, NotifyTrailName("r"))
	var until time.Time
	for _, r := range recs {
		if r.Kind == NotifyClaimed && r.Until.After(until) {
			until = r.Until
		}
	}
	if clk.now().After(until) {
		t.Fatalf("the round ended %v past the end of its claim (30s claim, reused with 23s left, four slow calls, then the ack retried)", clk.now().Sub(until))
	}
}

// R4-3 (design gap). The overdue notice is sent only inside the round of the deliverer that holds
// the event's claim. If that deliverer dies, a peer past the deadline writes the overdue record but
// defers the notice until the dead claim lapses plus the skew margin: with a long claim the notice
// is late by about the whole claim. At the defaults the delay is at most about 4 minutes past the
// deadline; with WithNotifyClaimTTL(time.Hour) it is an hour.
func TestReview4_APeerSendsTheOverdueNoticeWhenTheHolderIsGone(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, j, ev := NewMemNotifyTrail(), memJournal(), haltEvent()
	down := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
	overdue := &fakeSink{}
	opts := []NotifyOption{WithNotifyClaimTTL(time.Hour), WithOverdueNotifier(overdue)}
	dead := testEngine(t, j, down, trail, clk, "dead", opts...)
	_, _ = dead.round(ctx, ev) // claims for an hour, then its process dies
	clk.advance(6 * time.Minute)
	peer := testEngine(t, j, down, trail, clk, "peer", opts...)
	_, _ = peer.round(ctx, ev)
	recs, _ := trail.Load(ctx, NotifyTrailName("r"))
	if countKind(recs, NotifyOverdue, ev.key) != 1 {
		t.Fatalf("setup: no overdue record: %v", recKinds(recs))
	}
	if overdue.count() == 0 {
		t.Fatal("6 minutes after the event (deadline 5) the overdue record is written but its notice waits for the dead holder's 1h claim to lapse")
	}
}

// Held: an old run:aborted (a JSON string) and a new one (an object) read the same reason through
// Status, and only the new one carries a time.
func TestReview4Held_MixedAbortFormats(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	start := mustJSONValue(RunStart{StartedAt: "2026-10-13T09:00:00Z"})
	for run, val := range map[string][]byte{
		"old": mustJSON("refund failed"),
		"new": mustJSONValue(abortValue{Reason: "refund failed", AbortedAt: "2026-10-13T09:05:00.000Z"}),
	} {
		if _, _, err := j.putEntry(ctx, run, runStartStep, Record{Kind: StepValue, Result: start}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := j.putEntry(ctx, run, runAbortedStep, Record{Kind: StepValue, Result: val}); err != nil {
			t.Fatal(err)
		}
		st, err := Status(ctx, j, run)
		if err != nil || st.State != RunAborted || st.Terminal != "refund failed" {
			t.Errorf("%s: Status = %+v, %v", run, st, err)
		}
		_, has := abortedAt(Record{Result: val})
		if has != (run == "new") {
			t.Errorf("%s: abortedAt has = %v", run, has)
		}
	}
}
