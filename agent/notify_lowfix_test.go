package agent

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The recheck of deferred events in a flush shares one 2s budget: with the trail hung and many
// events deferred to a peer, a flush with no deadline returns in about 2s, not 2s per event.
func TestFlushRecheckSharesOneBudget(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := &blockingLoadTrail{MemNotifyTrail: NewMemNotifyTrail()}
	j := memJournal()
	down := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
	peer := testEngine(t, j, down, trail, clk, "recover")
	e := testEngine(t, j, down, trail, clk, "drive")
	const n = 5
	for i := range n {
		ev := haltEvent()
		ev.runID = fmt.Sprintf("r%d", i)
		ev.key = HaltedKey(ev.runID, OpTool, "c1", 100)
		_, _ = peer.round(ctx, ev) // the peer holds a live claim
		e.w.submit(ev)
	}
	if !pollUntil(2*time.Second, func() bool { return e.w.settled() }) {
		t.Fatal("setup: the rounds did not defer")
	}
	trail.block.Store(true)
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- e.flush(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("flush reported nothing for events it could not recheck")
		}
	case <-time.After(5 * time.Second):
		trail.block.Store(false)
		t.Fatalf("flush with %d deferred events and a hung trail did not return in 5s (each recheck has its own 2s)", n)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("flush took %v, want about 2s", d)
	}
}

// A holder whose own round is slow does not send the overdue notice while a peer's notice claim on
// the notice key is live: the single duplicate of a slow holder and a peer is zero.
func TestNotifyRound_AHolderSkipsTheNoticeWhilePeersNoticeClaimIsLive(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, j, ev := NewMemNotifyTrail(), memJournal(), haltEvent()
	down, overdue := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}, &fakeSink{fail: 1, err: ErrNotifyPermanent}
	opts := []NotifyOption{WithOverdueNotifier(overdue), WithNotifyClaimTTL(time.Hour)}
	holder := testEngine(t, j, down, trail, clk, "holder", opts...)
	_, _ = holder.round(ctx, ev) // claims before the deadline, then its round is slow
	clk.advance(6 * time.Minute)
	_, _ = testEngine(t, j, down, trail, clk, "peer", opts...).round(ctx, ev) // overdue record, then the peer's notice claim; its send fails
	if overdue.count() != 1 {
		t.Fatalf("setup: %d notice attempts by the peer, want 1", overdue.count())
	}
	clk.advance(10 * time.Second) // inside the peer's notice claim; the holder's own claim is still live
	_, _ = holder.round(ctx, ev)
	if overdue.count() != 1 {
		t.Fatalf("the holder sent the notice inside a peer's live notice claim: %d attempts, want 1", overdue.count())
	}
	clk.advance(20 * time.Minute) // the notice claim has lapsed; the holder's claim lives an hour: the notice is owed again
	_, _ = holder.round(ctx, ev)
	if overdue.count() != 2 {
		t.Fatalf("%d notice attempts after the peer's claim lapsed, want the holder's retry", overdue.count())
	}
}
