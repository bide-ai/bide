package postgreslog_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/govern/postgreslog"
)

// A governed_events table created before appends carried ids is migrated on Open: its events are
// kept, and new appends are idempotent by id. The legacy table lives in its own schema, so the
// test does not touch the shared table.
func TestOpen_MigratesALogWithoutAppendIDs(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("legacy_%d", time.Now().UnixNano())
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE SCHEMA %[1]s;
		CREATE TABLE %[1]s.governed_events (entity text NOT NULL, seq bigint NOT NULL, event text NOT NULL, PRIMARY KEY (entity, seq));
		INSERT INTO %[1]s.governed_events (entity, seq, event) VALUES ('e', 0, 'old0'), ('e', 1, 'old1');`, schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	l, err := postgreslog.Open(ctx, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("Open over a log without append ids: %v", err)
	}
	defer l.Close()
	for range 2 {
		if pos, err := l.Append(ctx, "e", "new", "new2"); err != nil || pos != 2 {
			t.Fatalf("Append after migration = %d, %v; want position 2", pos, err)
		}
	}
	if got, err := l.Events(ctx, "e", 0); err != nil || !slices.Equal(got, []string{"old0", "old1", "new2"}) {
		t.Fatalf("events after migration = %v (%v), want [old0 old1 new2]", got, err)
	}
	// The schema itself refuses a second row for an id, whatever writes it.
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s.governed_events (entity, seq, event, append_id) VALUES ('e', 3, 'dup', 'new')`, schema)); err == nil {
		t.Fatal("the table accepted a second row for append id \"new\"")
	}
}

// migrationSchema creates a fresh schema, with a governed_events table from before appends
// carried ids when legacy is set, and returns an admin handle and a DSN whose search_path is it.
func migrationSchema(t *testing.T, legacy bool) (*sql.DB, string, string) {
	t.Helper()
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := fmt.Sprintf("mig_%d", time.Now().UnixNano())
	q := "CREATE SCHEMA " + schema + ";"
	if legacy {
		q += fmt.Sprintf(`CREATE TABLE %[1]s.governed_events (entity text NOT NULL, seq bigint NOT NULL, event text NOT NULL, PRIMARY KEY (entity, seq));
			INSERT INTO %[1]s.governed_events (entity, seq, event) VALUES ('e', 0, 'old0');`, schema)
	}
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return db, schema, dsn + sep + "search_path=" + schema
}

// A table that has the append_id column but lacks its unique index gains the index on Open.
func TestOpen_AddsAMissingAppendIDIndex(t *testing.T) {
	ctx := context.Background()
	db, schema, dsn := migrationSchema(t, true)
	if _, err := db.Exec("ALTER TABLE " + schema + ".governed_events ADD COLUMN append_id text"); err != nil {
		t.Fatal(err)
	}
	l, err := postgreslog.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := db.Exec(fmt.Sprintf(`INSERT INTO %[1]s.governed_events VALUES ('e', 1, 'a', 'x'), ('e', 2, 'b', 'x')`, schema)); err == nil {
		t.Fatal("Open left the table without its unique append_id index")
	}
}

// Several processes opening the log at once all succeed, on a new database and on a table from
// before appends carried ids: the migration runs in one process at a time.
func TestOpen_ConcurrentOpens(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for trial := range 5 {
			_, _, dsn := migrationSchema(t, legacy)
			var wg sync.WaitGroup
			errs := make([]error, 8)
			for i := range errs {
				wg.Go(func() {
					l, err := postgreslog.Open(context.Background(), dsn)
					if err != nil {
						errs[i] = err
						return
					}
					defer l.Close()
					_, errs[i] = l.Append(context.Background(), "e", fmt.Sprintf("id%d", i), "new")
				})
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("legacy=%v trial %d: concurrent Open %d: %v", legacy, trial, i, err)
				}
			}
		}
	}
}

// Opening a log whose table is already migrated takes no table lock: it neither waits for a
// session reading the table nor makes a running process's appends wait behind it.
func TestOpen_MigratedTableTakesNoLock(t *testing.T) {
	ctx := context.Background()
	db, schema, dsn := migrationSchema(t, false)
	running, err := postgreslog.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	reader, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	if _, err := reader.Exec("SELECT count(*) FROM " + schema + ".governed_events"); err != nil {
		t.Fatal(err)
	}
	// A starting process opens the log while the reader is open.
	opened := make(chan error, 1)
	go func() {
		l, err := postgreslog.Open(ctx, dsn)
		if err == nil {
			l.Close()
		}
		opened <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the Open reach the table
	actx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := running.Append(actx, "e", "id1", "x"); err != nil {
		t.Fatalf("an append by a running process waited while another process opened the log: %v", err)
	}
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("Open of a migrated table beside an open reader: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open of a migrated table waited behind an open reader")
	}
}

// A migration that must alter a table another session is reading gives up after its lock timeout
// rather than queue for the table lock, which would stall every other session's reads and writes
// behind it; once the reader is done, Open migrates the table.
func TestOpen_MigrationGivesUpBehindAReader(t *testing.T) {
	ctx := context.Background()
	db, schema, dsn := migrationSchema(t, true)
	reader, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	if _, err := reader.Exec("SELECT count(*) FROM " + schema + ".governed_events"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if l, err := postgreslog.Open(octx, dsn); err == nil {
		l.Close()
		t.Fatal("Open altered a table another session holds")
	} else if waited := time.Since(start); waited > 15*time.Second {
		t.Fatalf("Open waited %v behind a reader before giving up: %v", waited, err)
	}
	_ = reader.Rollback()
	l, err := postgreslog.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open after the reader finished: %v", err)
	}
	defer l.Close()
	if pos, err := l.Append(ctx, "e", "new", "new1"); err != nil || pos != 1 {
		t.Fatalf("Append after migration = %d, %v; want position 1", pos, err)
	}
}
