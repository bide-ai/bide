package agent

// Fourth round of the adversarial review, against d3ddd1eb. Tests named TestReview4_ fail there and
// reproduce a finding.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

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
