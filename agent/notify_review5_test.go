package agent

// Confirmation round of the adversarial review, against 143f7439: a deterministic interleaving of
// the race the author could not construct. It passes there.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noticeRaceTrail lets another peer win the first notice claim: just before the caller's append of
// a claimed record on an overdue notice's key lands, the same record under another holder takes its
// position.
type noticeRaceTrail struct {
	*MemNotifyTrail
	raced atomic.Bool
}

func (n *noticeRaceTrail) Append(ctx context.Context, trail string, seq int, rec NotifyRecord) error {
	if rec.Kind == NotifyClaimed && strings.HasPrefix(rec.Key, "overdue:") && n.raced.CompareAndSwap(false, true) {
		other := rec
		other.Holder = "peer-b"
		if err := n.MemNotifyTrail.Append(ctx, trail, seq, other); err != nil {
			return err
		}
	}
	return n.MemNotifyTrail.Append(ctx, trail, seq, rec)
}

// Held: two peers race to claim the overdue notice of an event whose holder is gone. The loser gets
// the position conflict, reloads, finds the winner's live notice claim, and sends nothing; the trail
// holds one notice claim.
func TestReview5Held_APeerThatLosesTheNoticeClaimDoesNotSend(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, j, ev := &noticeRaceTrail{MemNotifyTrail: NewMemNotifyTrail()}, memJournal(), haltEvent()
	down := &fakeSink{fail: 1 << 30, err: ErrNotifyPermanent}
	overdue := &fakeSink{}
	opts := []NotifyOption{WithNotifyClaimTTL(time.Hour), WithOverdueNotifier(overdue)}
	_, _ = testEngine(t, j, down, trail, clk, "dead", opts...).round(ctx, ev) // claims for an hour, then dies
	clk.advance(6 * time.Minute)
	_, _ = testEngine(t, j, down, trail, clk, "peer-a", opts...).round(ctx, ev)
	if !trail.raced.Load() {
		t.Fatal("setup: the notice claim was never attempted")
	}
	if overdue.count() != 0 {
		t.Fatalf("the peer that lost the notice claim sent the notice %d time(s)", overdue.count())
	}
	recs, _ := trail.Load(ctx, NotifyTrailName("r"))
	n := 0
	for _, r := range recs {
		if r.Kind == NotifyClaimed && r.Key == OverdueKey(ev.key) {
			n++
			if !strings.HasPrefix(r.Holder, "peer-b") {
				t.Fatalf("notice claim by %q", r.Holder)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d notice claims, want the winner's one", n)
	}
}
