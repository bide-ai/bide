package postgres

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/blackwell-systems/bide"
)

func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, ctx
}

// Set PG_DSN (e.g. postgres://postgres:postgres@localhost:5432/agents?sslmode=disable)
// to run the integration test; it skips otherwise so the suite stays green offline.
func TestPostgres_DoMemoizesAndHistory(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID := "pg-test-" + t.Name()
	var runs int
	mk := func(context.Context) (agent.Record, error) {
		runs++
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
	}
	if _, err := store.Do(ctx, runID, "step", mk); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Do(ctx, runID, "step", mk); err != nil { // memoized
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("fn ran %d times, want 1 (not memoized)", runs)
	}
	h, err := store.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h[0].Name != "step" {
		t.Fatalf("history = %+v", h)
	}
}

// TestPostgres_ListerRuns checks Runs enumerates the runs the store holds (agent.Lister).
func TestPostgres_ListerRuns(t *testing.T) {
	store, ctx := openTestStore(t)
	r1, r2 := "pg-list-1-"+t.Name(), "pg-list-2-"+t.Name()
	mk := func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
	}
	if _, err := store.Do(ctx, r1, "s", mk); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Do(ctx, r2, "s", mk); err != nil {
		t.Fatal(err)
	}
	runs, err := store.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, r := range runs {
		set[r] = true
	}
	if !set[r1] || !set[r2] {
		t.Fatalf("Runs did not include the seeded runs %q, %q: %v", r1, r2, runs)
	}
}

// TestPostgres_LeaseExclusiveRenewRelease exercises the lease lifecycle (agent.Leaser): a live
// lease is exclusive, the holder can renew, a non-holder cannot renew, and release frees it.
func TestPostgres_LeaseExclusiveRenewRelease(t *testing.T) {
	store, ctx := openTestStore(t)
	run := "pg-lease-" + t.Name()
	_ = store.ReleaseLease(ctx, run, "A")
	_ = store.ReleaseLease(ctx, run, "B")

	if ok, err := store.AcquireLease(ctx, run, "A", time.Minute); err != nil || !ok {
		t.Fatalf("A should acquire a free lease (ok=%v err=%v)", ok, err)
	}
	if ok, _ := store.AcquireLease(ctx, run, "B", time.Minute); ok {
		t.Fatal("B must not acquire a lease A holds")
	}
	if ok, _ := store.RenewLease(ctx, run, "A", time.Minute); !ok {
		t.Fatal("A should renew its own lease")
	}
	if ok, _ := store.RenewLease(ctx, run, "B", time.Minute); ok {
		t.Fatal("B must not renew a lease it does not hold")
	}
	if err := store.ReleaseLease(ctx, run, "A"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.AcquireLease(ctx, run, "B", time.Minute); !ok {
		t.Fatal("B should acquire after A released")
	}
	_ = store.ReleaseLease(ctx, run, "B")
}

// TestPostgres_LeaseExpiry checks a lease is takeable once it expires by the database clock.
func TestPostgres_LeaseExpiry(t *testing.T) {
	store, ctx := openTestStore(t)
	run := "pg-lease-expiry-" + t.Name()
	_ = store.ReleaseLease(ctx, run, "A")
	_ = store.ReleaseLease(ctx, run, "B")

	if ok, _ := store.AcquireLease(ctx, run, "A", 300*time.Millisecond); !ok {
		t.Fatal("A should acquire")
	}
	if ok, _ := store.AcquireLease(ctx, run, "B", time.Minute); ok {
		t.Fatal("B must not take A's live lease")
	}
	time.Sleep(500 * time.Millisecond) // let A's lease expire against the DB clock
	if ok, _ := store.AcquireLease(ctx, run, "B", time.Minute); !ok {
		t.Fatal("B should take A's expired lease")
	}
	_ = store.ReleaseLease(ctx, run, "B")
}

// TestPostgres_HAAtMostOnceAcrossInstances is the cross-process HA e2e: two Store instances (two
// "processes" with separate in-process single-flight) contend to drive the same run with a
// non-idempotent charge. The lease must serialize them so the charge fires exactly once; without it
// both would run the charge fn before either records it and double-charge. Skips without PG_DSN.
func TestPostgres_HAAtMostOnceAcrossInstances(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres HA e2e")
	}
	ctx := context.Background()
	s1, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	run := "pg-ha-" + t.Name()
	_ = s1.ReleaseLease(ctx, run, "n1")
	_ = s1.ReleaseLease(ctx, run, "n2")

	var charges int32
	driveVia := func(s *Store) func(context.Context) error {
		return func(ctx context.Context) error {
			if _, err := s.Do(ctx, run, "charge", func(context.Context) (agent.Record, error) {
				atomic.AddInt32(&charges, 1) // non-idempotent side effect
				return agent.Record{Kind: agent.StepValue}, nil
			}); err != nil {
				return err
			}
			_, err := s.Do(ctx, run, "run:complete", func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue}, nil // the completion marker (agent.IsComplete)
			})
			return err
		}
	}
	worker := func(s *Store, holder string) {
		for iter := 0; iter < 2000; iter++ {
			if done, _ := agent.IsComplete(ctx, s, run); done {
				return
			}
			_, _ = agent.Lease(ctx, s, run, driveVia(s), agent.WithLeaseHolder(holder), agent.WithLeaseTTL(time.Minute))
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); worker(s1, "n1") }()
	go func() { defer wg.Done(); worker(s2, "n2") }()
	wg.Wait()

	if c := atomic.LoadInt32(&charges); c != 1 {
		t.Fatalf("charge fired %d times across two instances, want exactly 1 (the lease must serialize cross-process driving)", c)
	}
	if done, _ := agent.IsComplete(ctx, s1, run); !done {
		t.Fatal("run never completed")
	}
}
