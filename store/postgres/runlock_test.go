package postgres

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

// waitForAdvisoryWaiter polls pg_locks until a session waits for the transaction-level advisory
// lock keyed by hashtextextended(key, 0), so a test can order a commit after a blocked statement.
func waitForAdvisoryWaiter(t *testing.T, admin *sql.DB, key string) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := admin.QueryRowContext(context.Background(), `
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
				AND ((classid::bigint << 32) | objid::bigint) = hashtextextended($1, 0)`, key).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session waited for the advisory lock of %q", key)
		}
		<-tick.C
	}
}

// Inserts into one run queue on the run's lock instead of racing for a position. The insert's one
// statement takes the run's transaction-level advisory lock (through the next_seq function) and
// only then reads MAX(seq), with a snapshot taken after the lock was granted. So an insert that
// waited for another commits the next position in its first attempt, at read committed: no
// collision on (run_id, seq), no second statement. Here a transaction holds the run's lock with
// seq 0 recorded, as another insert's statement does while it runs; the store's Insert must wait
// on the lock, then take seq 1 with one statement. Skips without PG_DSN.
func TestPostgres_InsertQueuesOnTheRunLock(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	st := newStaller(dsn)
	db := sql.OpenDB(st)
	defer db.Close()
	s, err := New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	run := uniqueID(t, "pg-runlock-")
	other, err := admin.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback()
	if _, err := other.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(ctx, `INSERT INTO bide_steps (run_id, seq, name, data) VALUES ($1, 0, 'a', '\x00')`, run); err != nil {
		t.Fatal(err)
	}
	before := st.sent.Load()
	type result struct {
		seq      int64
		inserted bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		e, inserted, err := s.Insert(ctx, run, "b", []byte("b"))
		done <- result{e.Seq, inserted, err}
	}()
	waitForAdvisoryWaiter(t, admin, run)
	if err := other.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || !got.inserted || got.seq != 1 {
		t.Fatalf("Insert after the run's lock was released = (seq %d, inserted %v, %v), want seq 1", got.seq, got.inserted, got.err)
	}
	if n := st.sent.Load() - before; n != 1 {
		t.Fatalf("Insert sent %d statements, want 1: it collided and retried instead of queueing on the run's lock", n)
	}
}
