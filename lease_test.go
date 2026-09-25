package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// seedRun records one step so the run exists (Lister.Runs returns it) and is incomplete
// (no completion marker), i.e. something Recover would try to re-drive.
func seedRun(t *testing.T, s *MemStore, id string) {
	t.Helper()
	if _, err := s.Do(context.Background(), id, "seed", func(context.Context) (Record, error) {
		return Record{Kind: StepValue}, nil
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestLease_ExclusiveAndExpiry checks the lease semantics with a controlled clock: a live lease is
// exclusive, an expired one is available, renewal fails once expired, and release frees it.
func TestLease_ExclusiveAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	clk := time.Unix(1000, 0)
	s.now = func() time.Time { return clk }
	ttl := time.Minute

	if ok, _ := s.AcquireLease(ctx, "r", "A", ttl); !ok {
		t.Fatal("A should acquire a free lease")
	}
	if ok, _ := s.AcquireLease(ctx, "r", "B", ttl); ok {
		t.Fatal("B must not acquire a lease A holds")
	}
	if ok, _ := s.AcquireLease(ctx, "r", "A", ttl); !ok {
		t.Fatal("A reacquiring its own live lease should renew")
	}

	clk = clk.Add(2 * ttl) // expire A's lease
	if ok, _ := s.RenewLease(ctx, "r", "A", ttl); ok {
		t.Fatal("A must not renew an expired lease")
	}
	if ok, _ := s.AcquireLease(ctx, "r", "B", ttl); !ok {
		t.Fatal("B should acquire an expired lease")
	}
	if err := s.ReleaseLease(ctx, "r", "A"); err != nil { // non-holder release is a no-op
		t.Fatalf("release: %v", err)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "A", ttl); ok {
		t.Fatal("A must not acquire while B holds")
	}
	if err := s.ReleaseLease(ctx, "r", "B"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "A", ttl); !ok {
		t.Fatal("A should acquire after B released")
	}
}

// TestRecover_SkipsLeasedByOther confirms Recover skips a run another holder currently leases (so
// competing recoverers do not both drive it) and drives and then releases a free run.
func TestRecover_SkipsLeasedByOther(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "r1")
	seedRun(t, s, "r2")

	if ok, _ := s.AcquireLease(ctx, "r1", "other", time.Hour); !ok {
		t.Fatal("setup: other should lease r1")
	}

	var mu sync.Mutex
	driven := map[string]bool{}
	resume := func(_ context.Context, id string) error {
		mu.Lock()
		driven[id] = true
		mu.Unlock()
		return nil
	}

	n, err := Recover(ctx, s, resume, WithLeaseHolder("me"), WithLeaseTTL(time.Hour))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered %d, want 1 (r1 is leased by another holder)", n)
	}
	if driven["r1"] {
		t.Fatal("r1 is leased by another holder and must not be driven")
	}
	if !driven["r2"] {
		t.Fatal("r2 is free and should be driven")
	}
	if ok, _ := s.AcquireLease(ctx, "r2", "someone", time.Hour); !ok {
		t.Fatal("r2's lease should have been released after Recover drove it")
	}
}
