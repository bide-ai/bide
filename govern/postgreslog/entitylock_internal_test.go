package postgreslog

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"
)

// Appends to one entity queue on the entity's lock instead of racing for a position: the append's
// one statement takes the entity's transaction-level advisory lock (through the next_seq function)
// and only then reads MAX(seq), with a snapshot taken after the lock was granted. Here a
// transaction holds the entity's lock with position 0 recorded, as another append's statement does
// while it runs; Append must wait on the lock, then take position 1 with one statement. Skips
// without PG_DSN.
func TestAppend_QueuesOnTheEntityLock(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if l, err := Open(ctx, dsn); err != nil { // the schema, before the counting pool uses it
		t.Fatal(err)
	} else {
		l.Close()
	}
	st := newStaller(dsn)
	db := sql.OpenDB(st)
	defer db.Close()
	l := &Log{db: db}
	entity := fmt.Sprintf("pg-entitylock-%s-%d", t.Name(), time.Now().UnixNano())
	other, err := admin.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback()
	if _, err := other.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, entity); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(ctx, `INSERT INTO governed_events (entity, seq, event, append_id) VALUES ($1, 0, 'ea', 'a')`, entity); err != nil {
		t.Fatal(err)
	}
	before := st.sent.Load()
	type result struct {
		seq int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		seq, err := l.Append(ctx, entity, "b", "eb")
		done <- result{seq, err}
	}()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for deadline := time.Now().Add(10 * time.Second); ; <-tick.C {
		var waiting int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
				AND ((classid::bigint << 32) | objid::bigint) = hashtextextended($1, 0)`, entity).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Append did not wait for the advisory lock of %q", entity)
		}
	}
	if err := other.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.seq != 1 {
		t.Fatalf("Append after the entity's lock was released = (%d, %v), want position 1", got.seq, got.err)
	}
	if n := st.sent.Load() - before; n != 1 {
		t.Fatalf("Append sent %d statements, want 1: it collided and retried instead of queueing on the entity's lock", n)
	}
}
