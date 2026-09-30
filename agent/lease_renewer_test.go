package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// blockingRenewLeaser is a MemStore whose RenewLease hangs until its context is done, like a SQL
// store's renewal stuck on a slow or partitioned database, and then takes a moment to return. It
// signals entered when a renewal starts and records whether a renewal was still in flight when the
// lease was released.
type blockingRenewLeaser struct {
	*MemStore
	entered        chan struct{} // closed when the first renewal starts
	once           sync.Once
	inRenew        atomic.Int32 // renewals in flight
	renewAtRelease bool
}

func (l *blockingRenewLeaser) RenewLease(ctx context.Context, _, _ string, _ time.Duration) (bool, error) {
	l.inRenew.Add(1)
	defer l.inRenew.Add(-1)
	l.once.Do(func() { close(l.entered) })
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond) // a store call takes a moment to return after its cancellation
	return false, ctx.Err()
}

func (l *blockingRenewLeaser) ReleaseLease(ctx context.Context, runID, holder string) error {
	l.renewAtRelease = l.inRenew.Load() > 0
	return l.MemStore.ReleaseLease(ctx, runID, holder)
}

// The renewal goroutine is stopped, with any renewal it has in flight abandoned, before Lease
// releases the lease and returns. Otherwise a renewal stuck on the store outlives the drive (a
// leaked goroutine per Lease call for as long as the store hangs) and can land after the release,
// which a store whose renewal is not conditional on the holder would turn back into a lease.
//
// It runs in a synctest bubble, so the TTL, the renewal schedule and the store's delay run on the
// bubble's fake clock, and the drive ends only once the renewal is in flight: the outcome does not
// depend on how the machine schedules goroutines.
func TestLease_RenewerStopsBeforeRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &blockingRenewLeaser{MemStore: NewMemStore(), entered: make(chan struct{})}
		const ttl = 400 * time.Millisecond
		start := time.Now()
		// The renewal starts at ttl/2 and hangs; the drive ends as soon as it has started, well
		// before the renewal cutoff (3/4 of the TTL) would abandon it, so only stopping the
		// renewer ends it. A renewer that nothing stops leaves the bubble deadlocked, which
		// synctest reports as a failure.
		driven, err := Lease(context.Background(), s, "r", func(context.Context) error {
			<-s.entered
			return nil
		}, WithLeaseHolder("a"), WithLeaseTTL(ttl))
		elapsed := time.Since(start)
		if err != nil || !driven {
			t.Fatalf("Lease = (%v, %v), want (true, nil)", driven, err)
		}
		if want := ttl/2 + 20*time.Millisecond; elapsed != want {
			t.Fatalf("Lease returned %v after it started, want %v: the drive ended at ttl/2 and the stopped renewal took 20ms to return", elapsed, want)
		}
		if s.renewAtRelease {
			t.Fatal("the lease was released while a renewal was still in flight")
		}
		if s.inRenew.Load() > 0 {
			t.Fatal("a renewal is still in flight after Lease returned: the renewal goroutine outlived the drive")
		}
	})
}
