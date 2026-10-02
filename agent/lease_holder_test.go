package agent

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Two Lease calls under the same holder name are two drivers, and the lease must exclude one of
// them. A worker that gives its primary driver and its recoverer the same holder (the natural
// reading of WithLeaseHolder: one stable identity per worker) must not have Recover re-drive the
// run its primary is driving, nor have Recover's release free the lease under the primary.
func TestLease_SameHolderDoesNotDriveTwice(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "r")

	entered, finish := make(chan struct{}), make(chan struct{})
	primaryDone := make(chan error, 1)
	go func() {
		_, err := Lease(ctx, s, "r", func(context.Context) error {
			close(entered)
			<-finish
			return nil
		}, WithLeaseHolder("worker-1"), WithLeaseTTL(time.Hour))
		primaryDone <- err
	}()
	<-entered

	redriven := false
	n, err := Recover(ctx, s, func(context.Context, string, RunStart) error { redriven = true; return nil },
		WithLeaseHolder("worker-1"), WithLeaseTTL(time.Hour))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if redriven || n != 0 {
		t.Fatalf("Recover under the same holder re-drove a run its primary is driving (recovered %d)", n)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "worker-2", time.Hour); ok {
		t.Fatal("another worker took the lease while the primary was still driving: a same-holder driver released it")
	}
	if ok, _ := s.RenewLease(ctx, "r", "worker-1", time.Hour); ok {
		t.Fatal("a driver that never acquired this lease could renew it by holder name alone")
	}

	close(finish)
	if err := <-primaryDone; err != nil {
		t.Fatalf("primary Lease: %v", err)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "worker-2", time.Hour); !ok {
		t.Fatal("the primary's lease was not released after its drive")
	}
}

// A drive that outlasts several renewal periods keeps its lease: the renewer renews the claim this
// Lease call made, so the drive is never cancelled while it still holds the run. It runs on a
// synctest clock, so how busy the machine is does not decide when the renewer wakes.
func TestLease_RenewsItsOwnClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		driven, err := Lease(context.Background(), NewMemStore(), "r", func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return errors.New("the drive was cancelled: its own lease was not renewed")
			case <-time.After(500 * time.Millisecond): // five TTLs
				return nil
			}
		}, WithLeaseHolder("worker-1"), WithLeaseTTL(100*time.Millisecond))
		if err != nil || !driven {
			t.Fatalf("Lease = (%v, %v), want (true, nil)", driven, err)
		}
	})
}
