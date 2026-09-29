package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A wake whose resume fails transiently (a model provider blip, a store timeout) must be retried
// on a later Fire; otherwise the run stays asleep with nothing left to wake it.
func TestMemWaker_RetriesAFailedResume(t *testing.T) {
	ctx := context.Background()
	calls := 0
	w := NewMemWaker(func(context.Context, string) error {
		calls++
		if calls == 1 {
			return errors.New("model provider unavailable")
		}
		return nil
	})
	due := time.Unix(1000, 0)
	w.Schedule("r1", "nap", due)

	if _, err := w.Fire(ctx, due); err == nil {
		t.Fatal("first Fire: want the resume error reported")
	}
	if _, err := w.Fire(ctx, due.Add(time.Second)); err != nil {
		t.Fatalf("second Fire: %v", err)
	}
	if calls != 2 {
		t.Fatalf("resume called %d times, want 2: after one failed resume the wake was dropped and the run would never wake", calls)
	}
	if _, err := w.Fire(ctx, due.Add(2*time.Second)); err != nil || calls != 2 {
		t.Fatalf("after a successful resume, another Fire resumed again (calls=%d, err=%v); want the wake consumed", calls, err)
	}
}

// A resume that ends in a durable pause (the run slept again, or awaits a human) is not a failure:
// the wake is consumed, not retried every tick.
func TestMemWaker_PauseIsNotRetried(t *testing.T) {
	ctx := context.Background()
	for name, pause := range map[string]error{
		"sleeping":         &Sleeping{RunID: "r1", Name: "next"},
		"pending approval": &PendingApproval{RunID: "r1"},
	} {
		calls := 0
		w := NewMemWaker(func(context.Context, string) error { calls++; return pause })
		due := time.Unix(1000, 0)
		w.Schedule("r1", "nap", due)
		_, _ = w.Fire(ctx, due)
		_, _ = w.Fire(ctx, due.Add(time.Second))
		if calls != 1 {
			t.Fatalf("%s: resume called %d times, want 1 (a paused run must not be re-woken every tick)", name, calls)
		}
	}
}

// If the run re-registered the same timer for a later time during the failed attempt, that newer
// wake stands; the stale due one is not put back over it.
func TestMemWaker_RetryKeepsARescheduledWake(t *testing.T) {
	ctx := context.Background()
	due := time.Unix(1000, 0)
	later := due.Add(time.Hour)
	var w *MemWaker
	calls := 0
	w = NewMemWaker(func(context.Context, string) error {
		calls++
		w.Schedule("r1", "nap", later) // the run got far enough to re-register its wake, then failed
		return errors.New("store timeout")
	})
	w.Schedule("r1", "nap", due)
	_, _ = w.Fire(ctx, due)
	if _, _ = w.Fire(ctx, due.Add(time.Minute)); calls != 1 {
		t.Fatalf("resume called %d times before the rescheduled time, want 1 (the stale wake overwrote the newer one)", calls)
	}
	_, _ = w.Fire(ctx, later)
	if calls != 2 {
		t.Fatalf("resume called %d times at the rescheduled time, want 2", calls)
	}
}
