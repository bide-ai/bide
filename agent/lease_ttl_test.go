package agent

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"
)

// A lease TTL that is not positive is a configuration error, reported before anything is driven.
// The renewer ticks every ttl/2, and time.NewTicker panics on a non-positive interval inside the
// renewal goroutine, where no caller can recover it: the whole process dies.
func TestLease_RejectsNonPositiveTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second, time.Nanosecond} {
		ctx := context.Background()
		s := &countingLister{MemStore: NewMemStore()}
		ran := false
		driven, err := Lease(ctx, s, "r", func(context.Context) error { ran = true; return nil }, WithLeaseTTL(ttl))
		if ttl <= 0 {
			if !errors.Is(err, ErrConfig) || driven || ran {
				t.Fatalf("ttl %v: Lease = (%v, %v), ran=%v; want (false, ErrConfig) and no drive", ttl, driven, err, ran)
			}
			seedRun(t, s.MemStore, "r")
			if _, err := Recover(ctx, s, func(context.Context, string, RunStart) error { ran = true; return nil }, WithLeaseTTL(ttl)); !errors.Is(err, ErrConfig) || ran || s.listed {
				t.Fatalf("ttl %v: Recover err = %v, ran=%v, listed runs=%v; want ErrConfig before touching the store", ttl, err, ran, s.listed)
			}
			continue
		}
		// A positive TTL too short to halve still drives (and renews) without panicking.
		if err != nil || !driven || !ran {
			t.Fatalf("ttl %v: Lease = (%v, %v), ran=%v; want the drive to run", ttl, driven, err, ran)
		}
	}
}

// countingLister is a MemStore that records whether its runs were listed.
type countingLister struct {
	*MemStore
	listed bool
}

func (l *countingLister) Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error] {
	l.listed = true
	return l.MemStore.Runs(ctx, f)
}
