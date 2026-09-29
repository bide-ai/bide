package postgreslog_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
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
