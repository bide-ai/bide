package agent

// Adversarial review of the notifier core and NotifyPending (PR #243). Every test here states a
// guarantee of docs/design/notifier.md and fails on the branch it was written against; each is a
// reproduction, not a fix. Run them with: go test -run TestReview ./agent/

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
	primary := &fakeSink{fail: 1 << 30}                    // the main sink is down
	overdue := &fakeSink{fail: 1, err: ErrNotifyPermanent} // the overdue sink has one blip
	opt := WithOverdueNotifier(overdue)

	_ = sweep(t, j, primary, trail, clk, opt)
	clk.advance(6 * time.Minute)
	_ = sweep(t, j, primary, trail, clk, opt) // past the deadline: overdue raised, its one send fails
	if overdue.count() != 1 {
		t.Fatalf("setup: the overdue sink was called %d times, want 1", overdue.count())
	}
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
		if sink.count() > 3 {
			t.Fatalf("a gate decided %v earlier was delivered as pending after the decision", 10*time.Minute)
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
	if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: mustJSON("refund failed; rolled back")}); err != nil {
		t.Fatal(err)
	}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	clk := sweepClock()
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
