package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// Leases on one file coordinate the handles (processes) that share it: a live lease excludes
// other holders, its holder renews it, an expired one is anyone's, and only its holder releases
// it. Expiry is computed from the database's clock.
func TestLease_Semantics(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lease.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	const ttl = 300 * time.Millisecond
	if ok, err := a.AcquireLease(ctx, "r", "A", ttl); err != nil || !ok {
		t.Fatalf("A acquires a free run: %v, %v", ok, err)
	}
	if ok, err := b.AcquireLease(ctx, "r", "B", ttl); err != nil || ok {
		t.Fatalf("B acquires A's live lease: %v, %v; want refused", ok, err)
	}
	if ok, err := a.AcquireLease(ctx, "r", "A", ttl); err != nil || !ok {
		t.Fatalf("A acquires its own lease again (a renewal): %v, %v", ok, err)
	}
	if ok, err := a.RenewLease(ctx, "r", "A", ttl); err != nil || !ok {
		t.Fatalf("A renews its live lease: %v, %v", ok, err)
	}
	if ok, err := b.RenewLease(ctx, "r", "B", ttl); err != nil || ok {
		t.Fatalf("B renews a lease it does not hold: %v, %v", ok, err)
	}
	if err := b.ReleaseLease(ctx, "r", "B"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.AcquireLease(ctx, "r", "B", ttl); ok {
		t.Fatal("a release by a non-holder freed A's lease")
	}
	time.Sleep(ttl + 100*time.Millisecond)
	if ok, err := a.RenewLease(ctx, "r", "A", ttl); err != nil || ok {
		t.Fatalf("A renews its expired lease: %v, %v; want refused", ok, err)
	}
	if ok, err := b.AcquireLease(ctx, "r", "B", ttl); err != nil || !ok {
		t.Fatalf("B acquires the expired lease: %v, %v", ok, err)
	}
	if err := b.ReleaseLease(ctx, "r", "B"); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.AcquireLease(ctx, "r", "A", ttl); err != nil || !ok {
		t.Fatalf("A acquires the released run: %v, %v", ok, err)
	}
}

// Recover and Lease find the SQLite store's Leaser, so two recoverers on one file do not both
// drive a run.
func TestLease_RecoverUsesTheLeaser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(s)
	defer s.Close()
	if _, ok := agent.Capability[agent.Leaser](s); !ok {
		t.Fatal("the SQLite store does not report a Leaser")
	}
	ctx := context.Background()
	if ok, _ := s.AcquireLease(ctx, "busy", "someone-else", time.Minute); !ok {
		t.Fatal("could not lease the run")
	}
	if _, err := journaltest.Do(ctx, j, "busy", "x", func(context.Context) (agent.Record, error) { return agent.Record{Kind: agent.StepValue}, nil }); err != nil {
		t.Fatal(err)
	}
	driven := false
	if n, err := agent.Recover(ctx, j, func(context.Context, string, agent.RunStart) error { driven = true; return nil }); err != nil || n != 0 || driven {
		t.Fatalf("Recover = %d, %v (drove: %v); want the leased run skipped", n, err, driven)
	}
}

// A writer that holds the file's write lock for 5s (a long transaction in another process) does
// not cost a lease: the lease statements run on their own connection with a short busy timeout, so
// a renewal that meets the lock fails fast and is retried before the renewal cutoff, and the drive
// keeps its lease.
func TestLease_SurvivesAFiveSecondWriterHold(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 17s")
	}
	path := filepath.Join(t.TempDir(), "lease.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(s)
	defer s.Close()
	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)

	const ttl = 24 * time.Second // renewal from 12s, cutoff at 18s
	hold := func() error {
		conn, err := other.Conn(context.Background())
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
			return err
		}
		time.Sleep(5 * time.Second)
		_, err = conn.ExecContext(context.Background(), `COMMIT`)
		return err
	}
	held := make(chan error, 1)
	driven, err := agent.Lease(context.Background(), j, "run", func(ctx context.Context) error {
		// The hold starts just before the first renewal and ends after it.
		time.Sleep(ttl/2 - 500*time.Millisecond)
		go func() { held <- hold() }()
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(5*time.Second + time.Second):
		}
		return ctx.Err()
	}, agent.WithLeaseTTL(ttl), agent.WithLeaseHolder("w"))
	if err := <-held; err != nil {
		t.Fatalf("the other writer's hold: %v", err)
	}
	if !driven || err != nil {
		t.Fatalf("Lease = %v, %v; want the drive to keep its lease through the writer's hold", driven, err)
	}
	if errors.Is(err, agent.ErrLeaseLost) {
		t.Fatal("the lease was lost")
	}
}

// A lease statement that meets another writer's lock gives up after the short lease busy timeout
// (an eighth of the TTL, at most 2s) instead of waiting out the writer, so no renewal blocks past
// its cutoff whatever the caller's context allows; the renewer then retries.
func TestLease_StatementsDoNotWaitOutAWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if ok, err := s.AcquireLease(ctx, "run", "A", time.Minute); err != nil || !ok {
		t.Fatalf("acquire: %v, %v", ok, err)
	}
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `ROLLBACK`)

	const ttl = 8 * time.Second // busy timeout 1s
	start := time.Now()
	ok, err := s.RenewLease(ctx, "run", "A", ttl)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("RenewLease waited %v for the writer's lock; want it to give up after about 1s", took)
	}
	if err == nil || ok {
		t.Fatalf("RenewLease under another writer's lock = %v, %v; want a busy error", ok, err)
	}
}
