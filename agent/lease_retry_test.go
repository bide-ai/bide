package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// failingRenewLeaser is a MemStore whose RenewLease fails its first failures calls with a store
// error (forever when failures < 0), or hangs until its context is done when hang is set.
type failingRenewLeaser struct {
	*MemStore
	failures int64
	hang     bool
	calls    atomic.Int64
}

var errStoreDown = errors.New("store unreachable")

func (l *failingRenewLeaser) RenewLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	n := l.calls.Add(1)
	if l.hang {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if l.failures < 0 || n <= l.failures {
		return false, errStoreDown
	}
	return l.MemStore.RenewLease(ctx, runID, holder, ttl)
}

// driveFor returns a drive that runs for d unless its context is cancelled first, and reports
// how long after start it stopped and whether it was cancelled.
func driveFor(d time.Duration, stopped *time.Duration, cancelled *bool) func(context.Context) error {
	return func(ctx context.Context) error {
		start := time.Now()
		select {
		case <-ctx.Done():
			*stopped, *cancelled = time.Since(start), true
			return ctx.Err()
		case <-time.After(d):
			*stopped = time.Since(start)
			return nil
		}
	}
}

// A renewal that fails with a store error is retried while there is still time to renew before
// the lease could expire, so one dropped connection does not cancel a drive that still holds its
// lease.
func TestLease_RetriesAFailedRenewal(t *testing.T) {
	const ttl = 400 * time.Millisecond
	s := &failingRenewLeaser{MemStore: NewMemStore(), failures: 2}
	var stopped time.Duration
	var cancelled bool
	driven, err := Lease(context.Background(), s, "r", driveFor(2*ttl, &stopped, &cancelled),
		WithLeaseHolder("a"), WithLeaseTTL(ttl))
	if !driven || err != nil || cancelled {
		t.Fatalf("Lease = (%v, %v), cancelled after %v: two failed renewals, with time left to retry, cancelled the drive", driven, err, stopped)
	}
	if n := s.calls.Load(); n < 3 {
		t.Fatalf("RenewLease called %d times, want at least 3 (two failures, then a success)", n)
	}
}

// A renewal that keeps failing, or hangs, is given up at 3/4 of the TTL after the last renewal
// was issued: the drive is cancelled a quarter of the TTL before any other process could take the
// lease, not when the lease has already lapsed (or never, for a renewal that hangs).
func TestLease_GivesUpBeforeTheLeaseCanExpire(t *testing.T) {
	const ttl = 400 * time.Millisecond
	for _, tc := range []struct {
		name string
		s    *failingRenewLeaser
	}{
		{"failing", &failingRenewLeaser{MemStore: NewMemStore(), failures: -1}},
		{"hanging", &failingRenewLeaser{MemStore: NewMemStore(), hang: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stopped time.Duration
			var cancelled bool
			start := time.Now()
			driven, _ := Lease(context.Background(), tc.s, "r", driveFor(3*ttl, &stopped, &cancelled),
				WithLeaseHolder("a"), WithLeaseTTL(ttl))
			elapsed := time.Since(start)
			if !driven || !cancelled {
				t.Fatalf("the drive was not cancelled (driven=%v) although its lease could not be renewed; it ran %v", driven, stopped)
			}
			if elapsed >= ttl {
				t.Fatalf("the drive was cancelled %v after the lease was acquired, at or after its %v TTL, when another process may already hold it", elapsed, ttl)
			}
			if elapsed < ttl/2 {
				t.Fatalf("the drive was cancelled %v after the lease was acquired, before the first renewal was due (%v)", elapsed, ttl/2)
			}
			if !tc.s.hang && tc.s.calls.Load() < 2 {
				t.Fatalf("RenewLease called %d times, want retries before giving up", tc.s.calls.Load())
			}
		})
	}
}
