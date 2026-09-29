package sqlitelog_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bide-ai/bide/govern/sqlitelog"
)

// A log file created before appends carried ids is migrated on Open: its events are kept, and
// new appends are idempotent by id.
func TestOpen_MigratesALogWithoutAppendIDs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE events (entity text NOT NULL, seq INTEGER NOT NULL, event text NOT NULL, PRIMARY KEY (entity, seq));
		INSERT INTO events (entity, seq, event) VALUES ('e', 0, 'old0'), ('e', 1, 'old1');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	l, err := sqlitelog.Open(path)
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
}
