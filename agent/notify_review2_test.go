package agent

// Second round of the adversarial review, against the fixed stack (681e5f8e). Tests named
// TestReview2_ fail there and reproduce a finding.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

// modeSink fails fast until hang is set, then blocks each call until its context ends (it honors
// its context). It tracks how many calls are in flight at once.
type modeSink struct {
	hang        atomic.Bool
	inflight    atomic.Int32
	maxInflight atomic.Int32
}

func (m *modeSink) Notify(ctx context.Context, n Notification) error {
	if !m.hang.Load() {
		return ErrNotifyPermanent
	}
	v := m.inflight.Add(1)
	defer m.inflight.Add(-1)
	for {
		cur := m.maxInflight.Load()
		if v <= cur || m.maxInflight.CompareAndSwap(cur, v) {
			break
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// R2-3. "Every Notify call is bounded by a quarter of the claim, so a round of three calls and their
// backoff ends inside the claim it holds." It does not: a round that owes an overdue notice makes
// four calls, and a round reuses its own claim while more than half of it is left, so a round can
// start with half the claim left and run a whole claim long. It overruns its claim by up to half the
// claim plus the backoff, and the skew margin (fixed at 2 minutes, not scaled) covers that only
// while the claim is under about 4 minutes. With WithNotifyClaimTTL(10*time.Minute) a peer takes
// the claim over while the holder is still sending. Here the timings are scaled down 1500x (claim
// 400ms, margin 80ms: the 10-minute claim and 2-minute margin).
func TestReview2_ARoundEndsInsideItsClaim(t *testing.T) {
	ctx := context.Background()
	trail, j, ev := NewMemNotifyTrail(), memJournal(), haltEvent()
	sink := &modeSink{}
	tune := func(c *notifyConfig) {
		c.claimTTL, c.skew, c.attemptBackoff = 400*time.Millisecond, 80*time.Millisecond, time.Millisecond
	}
	opts := []NotifyOption{WithNotifyDeadline(time.Millisecond), WithNotifyRetry(time.Millisecond, time.Millisecond)}
	p1 := realEngine(t, j, sink, trail, "p1", tune, opts...)
	p2 := realEngine(t, j, sink, trail, "p2", tune, opts...)

	t0 := time.Now()
	_, _ = p1.round(ctx, ev) // claims until t0+400ms; every call fails fast
	time.Sleep(time.Until(t0.Add(150 * time.Millisecond)))
	sink.hang.Store(true)
	done := make(chan struct{})
	go func() { _, _ = p1.round(ctx, ev); close(done) }() // reuses its claim (250ms left > half), makes 4 calls of 100ms
	time.Sleep(time.Until(t0.Add(500 * time.Millisecond))) // past until + margin by p2's (same) clock
	go func() { _, _ = p2.round(ctx, ev) }()
	<-done
	if got := sink.maxInflight.Load(); got > 1 {
		t.Fatalf("%d deliverers in flight for one key: the holder's round outran its claim plus the skew margin", got)
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
