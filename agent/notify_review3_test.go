package agent

// Third round of the adversarial review, against 67b9016b. Tests named TestReview3_ fail there and
// reproduce a finding; TestReview3Held_ pass and record what held up.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
