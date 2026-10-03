package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
func TestLease_RetriesAFailedRenewal(t *testing.T) { synctest.Test(t, testRetriesAFailedRenewal) }

func testRetriesAFailedRenewal(t *testing.T) {
	const ttl = 400 * time.Millisecond
	s := &failingRenewLeaser{MemStore: NewMemStore(), failures: 2}
	j := mustJournal(s)
	var stopped time.Duration
	var cancelled bool
	driven, err := Lease(context.Background(), j, "r", driveFor(2*ttl, &stopped, &cancelled),
		WithLeaseHolder("a"), WithLeaseTTL(ttl))
	if !driven || err != nil || cancelled {
		t.Fatalf("Lease = (%v, %v), cancelled after %v: two failed renewals, with time left to retry, cancelled the drive", driven, err, stopped)
	}
	// Renewals are due every ttl/2 and a failed one is retried every ttl/20, so over 2*ttl the
	// renewer tries at 200ms (fails), 220ms (fails), 240ms, 440ms and 640ms; the drive ends at 800ms.
	if n := s.calls.Load(); n != 5 {
		t.Fatalf("RenewLease called %d times, want 5 (two failures, then a success every ttl/2)", n)
	}
}

// A renewal that keeps failing, or hangs, is given up at 3/4 of the TTL after the last renewal
// was issued: the drive is cancelled a quarter of the TTL before any other process could take the
// lease, not when the lease has already lapsed (or never, for a renewal that hangs). On a synctest
// clock the cancellation lands exactly at the cutoff.
func TestLease_GivesUpBeforeTheLeaseCanExpire(t *testing.T) {
	const ttl = 400 * time.Millisecond
	for _, tc := range []struct {
		name  string
		store func() *failingRenewLeaser
		calls int64 // renewals tried before the cutoff
	}{
		// Tries at 200ms and every 20ms after, the last at 280ms.
		{"failing", func() *failingRenewLeaser { return &failingRenewLeaser{MemStore: NewMemStore(), failures: -1} }, 5},
		// One try at 200ms, abandoned at the cutoff.
		{"hanging", func() *failingRenewLeaser { return &failingRenewLeaser{MemStore: NewMemStore(), hang: true} }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := tc.store()
				j := mustJournal(s)
				var stopped time.Duration
				var cancelled bool
				start := time.Now()
				driven, _ := Lease(context.Background(), j, "r", driveFor(3*ttl, &stopped, &cancelled),
					WithLeaseHolder("a"), WithLeaseTTL(ttl))
				elapsed := time.Since(start)
				if !driven || !cancelled {
					t.Fatalf("the drive was not cancelled (driven=%v) although its lease could not be renewed; it ran %v", driven, stopped)
				}
				if want := ttl * 3 / 4; elapsed != want {
					t.Fatalf("the drive was cancelled %v after the lease was acquired, want exactly the cutoff, %v (before its %v TTL)", elapsed, want, ttl)
				}
				if n := s.calls.Load(); n != tc.calls {
					t.Fatalf("RenewLease called %d times before the cutoff, want %d", n, tc.calls)
				}
			})
		})
	}
}
