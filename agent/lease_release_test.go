package agent

import (
	"context"
	"testing"
	"time"
)

// ctxLeaser is a MemStore whose ReleaseLease, like a SQL store's, fails on a cancelled context.
type ctxLeaser struct{ *MemStore }

func (l ctxLeaser) ReleaseLease(ctx context.Context, runID, holder string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.MemStore.ReleaseLease(ctx, runID, holder)
}

// A drive stopped by cancellation (a shutdown) still releases its lease, so another node can
// take the run at once instead of waiting out the TTL.
func TestLease_ReleasedWhenTheDriveIsCancelled(t *testing.T) {
	store := ctxLeaser{NewMemStore()}
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = Lease(ctx, store, "r1", func(context.Context) error {
		cancel() // shutdown arrives mid-drive
		return context.Canceled
	}, WithLeaseHolder("node-a"), WithLeaseTTL(time.Hour))
	got, err := store.AcquireLease(context.Background(), "r1", "node-b", time.Hour)
	if err != nil || !got {
		t.Fatalf("another node could not take the run after the drive stopped (got=%v, err=%v): the lease was never released", got, err)
	}
}
