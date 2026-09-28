package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHA_MutualExclusionUnderConcurrency is the core distributed-HA property: many workers contend
// over a shared store to drive the same set of runs, and the lease must ensure no run is ever driven
// by two workers at once. The drive detects concurrent entry (a per-run in-flight flag), so any
// failure of mutual exclusion is caught. All runs must complete.
func TestHA_MutualExclusionUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	runIDs := []string{"r0", "r1", "r2", "r3", "r4"}
	for _, r := range runIDs {
		seedRun(t, s, r)
	}

	var inflight sync.Map // runID -> *int32 (1 = a worker is inside drive)
	var violations int32
	drive := func(runID string) func(context.Context) error {
		return func(ctx context.Context) error {
			flag, _ := inflight.LoadOrStore(runID, new(int32))
			f := flag.(*int32)
			if !atomic.CompareAndSwapInt32(f, 0, 1) {
				atomic.AddInt32(&violations, 1) // two drivers in the same run at once
				return nil
			}
			time.Sleep(time.Millisecond) // widen the window so a lease failure would overlap
			atomic.StoreInt32(f, 0)
			_, _ = s.Do(ctx, runID, runCompleteStep, func(context.Context) (Record, error) {
				return Record{Kind: StepValue}, nil
			})
			return nil
		}
	}

	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			holder := fmt.Sprintf("w%d", id)
			// Drive to completion, bounded by wall-clock rather than a fixed
			// iteration count: losing workers spin through failed lease
			// acquisitions far faster than winners drive the runs, and on a
			// slow (or few-core) runner a fixed cap can expire before the runs
			// finish. The deadline still fails a genuine livelock instead of
			// hanging.
			deadline := time.Now().Add(30 * time.Second)
			for {
				allDone := true
				for _, r := range runIDs {
					done, _ := IsComplete(ctx, s, r)
					if done {
						continue
					}
					allDone = false
					_, _ = Lease(ctx, s, r, drive(r), WithLeaseHolder(holder), WithLeaseTTL(time.Second))
				}
				if allDone {
					return
				}
				if time.Now().After(deadline) {
					t.Errorf("worker %d did not converge within deadline", id)
					return
				}
				time.Sleep(time.Millisecond) // yield so a lease holder can make progress
			}
		}(w)
	}
	wg.Wait()

	if v := atomic.LoadInt32(&violations); v != 0 {
		t.Fatalf("%d concurrent drives of the same run: the lease failed to exclude", v)
	}
	for _, r := range runIDs {
		if done, _ := IsComplete(ctx, s, r); !done {
			t.Fatalf("run %s never completed", r)
		}
	}
}

// TestHA_CrashTakeover is the availability property: a worker that leases a run and dies (never
// renews or releases) must not block the run forever. Once its lease expires, another worker takes
// over and completes it. A controlled clock makes the expiry deterministic.
func TestHA_CrashTakeover(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	clk := time.Unix(1000, 0)
	s.setNow(func() time.Time { return clk })
	ttl := time.Minute

	if ok, _ := s.AcquireLease(ctx, "x", "A", ttl); !ok {
		t.Fatal("A should acquire the lease")
	}
	if ok, _ := s.AcquireLease(ctx, "x", "B", ttl); ok {
		t.Fatal("B must not take A's live lease")
	}
	clk = clk.Add(2 * ttl) // A crashed: it never renewed, so its lease expires

	driven := false
	took, err := Lease(ctx, s, "x", func(context.Context) error { driven = true; return nil },
		WithLeaseHolder("B"), WithLeaseTTL(ttl))
	if err != nil || !took || !driven {
		t.Fatalf("B should take over A's expired lease (took=%v driven=%v err=%v)", took, driven, err)
	}
}

// TestHA_AtMostOnceUnderConcurrentDriving drives one run with a non-idempotent journaled step from
// many workers at once. The step must fire at most once (memoization) and the run completes: the
// integrated Lease + Do path preserves at-most-once under contention.
func TestHA_AtMostOnceUnderConcurrentDriving(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "run")

	var charges int32
	drive := func(ctx context.Context) error {
		_, _ = s.Do(ctx, "run", "charge", func(context.Context) (Record, error) {
			atomic.AddInt32(&charges, 1) // the non-idempotent side effect
			return Record{Kind: StepValue}, nil
		})
		_, _ = s.Do(ctx, "run", runCompleteStep, func(context.Context) (Record, error) {
			return Record{Kind: StepValue}, nil
		})
		return nil
	}

	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			holder := fmt.Sprintf("w%d", id)
			for iter := 0; iter < 1000; iter++ {
				if done, _ := IsComplete(ctx, s, "run"); done {
					return
				}
				_, _ = Lease(ctx, s, "run", drive, WithLeaseHolder(holder), WithLeaseTTL(time.Second))
			}
		}(w)
	}
	wg.Wait()

	if c := atomic.LoadInt32(&charges); c != 1 {
		t.Fatalf("charge fired %d times under concurrent HA driving, want exactly 1", c)
	}
	if done, _ := IsComplete(ctx, s, "run"); !done {
		t.Fatal("run never completed")
	}
}
