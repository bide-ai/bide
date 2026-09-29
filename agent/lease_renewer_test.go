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
	driven, err := Lease(context.Background(), s, "r", func(context.Context) error {
		time.Sleep(50 * time.Millisecond) // long enough for a renewal (every ttl/2) to start and hang
		return nil
	}, WithLeaseHolder("a"), WithLeaseTTL(20*time.Millisecond))
	if err != nil || !driven {
		t.Fatalf("Lease = (%v, %v), want (true, nil)", driven, err)
	}
	if s.renewAtRelease {
		t.Fatal("the lease was released while a renewal was still in flight")
	}
	if len(s.inRenew) > 0 {
		t.Fatal("a renewal is still in flight after Lease returned: the renewal goroutine outlived the drive")
	}
}
