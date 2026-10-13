package agent

// Second round of the adversarial review, against the fixed stack (681e5f8e). Tests named
// TestReview2_ fail there and reproduce a finding.

import (
	"context"
	"errors"
	"sync"
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
