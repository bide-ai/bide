package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// notHeldLeaser is a MemStore whose RenewLease reports the lease no longer held, as a store does
// once the lease lapsed and another process took it.
type notHeldLeaser struct{ *MemStore }

func (notHeldLeaser) RenewLease(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}

// waitForCancel is a drive that runs until its context is cancelled and records the cause.
func waitForCancel(cause *error) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			*cause = context.Cause(ctx)
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return errors.New("the drive was never cancelled")
		}
	}
}

// A drive whose lease is lost, whether taken by another process or not renewable before the
// cutoff, is cancelled with ErrLeaseLost as the cause, and Lease returns the drive's error wrapped
// with it, so a caller can tell a lost lease from a shutdown or a genuine failure.
func TestLease_LostLeaseEndsWithErrLeaseLost(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Durable
	}{
		{"taken", notHeldLeaser{NewMemStore()}},
		{"not renewable in time", &failingRenewLeaser{MemStore: NewMemStore(), failures: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cause error
			driven, err := Lease(context.Background(), tc.s, "r", waitForCancel(&cause), WithLeaseTTL(40*time.Millisecond))
			if !driven {
				t.Fatal("the drive did not run")
			}
			if !errors.Is(cause, ErrLeaseLost) {
				t.Fatalf("the drive's context was cancelled with cause %v, want ErrLeaseLost", cause)
			}
			if !errors.Is(err, ErrLeaseLost) || !errors.Is(err, context.Canceled) {
				t.Fatalf("Lease returned %v, want the drive's error (context.Canceled) wrapped with ErrLeaseLost", err)
			}
		})
	}
}

// A drive cancelled by its caller (a shutdown) did not lose its lease, and a drive that finished
// despite losing its lease succeeded: neither reports ErrLeaseLost.
func TestLease_ErrLeaseLostOnlyForALostLease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var cause error
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, err := Lease(ctx, NewMemStore(), "r", waitForCancel(&cause), WithLeaseTTL(time.Hour)); errors.Is(err, ErrLeaseLost) || errors.Is(cause, ErrLeaseLost) {
		t.Fatalf("a caller's cancellation was reported as a lost lease (err %v, cause %v)", err, cause)
	}
	driven, err := Lease(context.Background(), notHeldLeaser{NewMemStore()}, "r", func(ctx context.Context) error {
		<-ctx.Done()
		return nil // the work finished anyway
	}, WithLeaseTTL(40*time.Millisecond))
	if !driven || err != nil {
		t.Fatalf("Lease = (%v, %v), want (true, nil) for a drive that finished", driven, err)
	}
}

// Recover counts a run whose lease was lost mid-drive as recovered, not failed: another process
// holds it now and carries it on.
func TestRecover_LostLeaseIsNotAFailure(t *testing.T) {
	s := notHeldLeaser{NewMemStore()}
	seedRun(t, s.MemStore, "r")
	var cause error
	n, err := Recover(context.Background(), s, func(ctx context.Context, _ string) error { return waitForCancel(&cause)(ctx) },
		WithLeaseTTL(40*time.Millisecond))
	if err != nil {
		t.Fatalf("Recover = %v, want nil: a lost lease is not a failure", err)
	}
	if n != 1 {
		t.Fatalf("Recover recovered %d runs, want 1", n)
	}
}
