package agent

import (
	"context"
	"testing"
	"time"
)

// blockingRenewLeaser is a MemStore whose RenewLease hangs until its context is done, like a SQL
// store's renewal stuck on a slow or partitioned database. It records whether a renewal was still
// in flight when the lease was released.
type blockingRenewLeaser struct {
	*MemStore
	inRenew          chan struct{} // holds a token while a RenewLease call is in flight
	renewAtRelease   bool
	releasedOnReturn chan struct{}
}

func (l *blockingRenewLeaser) RenewLease(ctx context.Context, _, _ string, _ time.Duration) (bool, error) {
	l.inRenew <- struct{}{}
	defer func() { <-l.inRenew }()
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond) // a store call takes a moment to return after its cancellation
	return false, ctx.Err()
}

func (l *blockingRenewLeaser) ReleaseLease(ctx context.Context, runID, holder string) error {
	l.renewAtRelease = len(l.inRenew) > 0
	return l.MemStore.ReleaseLease(ctx, runID, holder)
}

// The renewal goroutine is stopped, with any renewal it has in flight abandoned, before Lease
// releases the lease and returns. Otherwise a renewal stuck on the store outlives the drive (a
// leaked goroutine per Lease call for as long as the store hangs) and can land after the release,
// which a store whose renewal is not conditional on the holder would turn back into a lease.
func TestLease_RenewerStopsBeforeRelease(t *testing.T) {
	s := &blockingRenewLeaser{MemStore: NewMemStore(), inRenew: make(chan struct{}, 1)}
	type result struct {
		driven bool
		err    error
	}
	// The renewal starts at ttl/2 (200ms) and hangs; the drive ends at 220ms, before the renewal
	// cutoff (3/4 of the TTL, 300ms) would abandon it, so only stopping the renewer ends it.
	const ttl = 400 * time.Millisecond
	res := make(chan result, 1)
	start := time.Now()
	go func() {
		driven, err := Lease(context.Background(), s, "r", func(context.Context) error {
			time.Sleep(ttl/2 + 20*time.Millisecond)
			return nil
		}, WithLeaseHolder("a"), WithLeaseTTL(ttl))
		res <- result{driven, err}
	}()
	var driven bool
	var err error
	select {
	case r := <-res:
		driven, err = r.driven, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Lease never returned after its drive did: it is waiting on a renewal that nothing cancels")
	}
	elapsed := time.Since(start)
	if err != nil || !driven {
		t.Fatalf("Lease = (%v, %v), want (true, nil)", driven, err)
	}
	if elapsed >= ttl*3/4 {
		t.Fatalf("Lease returned %v after it started, once the renewal cutoff abandoned the hung renewal, not when the drive ended", elapsed)
	}
	if s.renewAtRelease {
		t.Fatal("the lease was released while a renewal was still in flight")
	}
	if len(s.inRenew) > 0 {
		t.Fatal("a renewal is still in flight after Lease returned: the renewal goroutine outlived the drive")
	}
}
