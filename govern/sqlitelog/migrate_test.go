package sqlitelog_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
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

// Several processes opening one old-schema log at once all succeed: the migration is atomic, so
// no Open fails adding a column another Open has just added. A file still in rollback-journal
// mode is switched to WAL under the same contention.
func TestOpen_ConcurrentOpensMigrateOnce(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"wal", "delete"} {
		for trial := range 10 {
			path := filepath.Join(t.TempDir(), "old.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`PRAGMA journal_mode=` + mode + `;
				CREATE TABLE events (entity text NOT NULL, seq INTEGER NOT NULL, event text NOT NULL, PRIMARY KEY (entity, seq));
				INSERT INTO events (entity, seq, event) VALUES ('e', 0, 'old0');`); err != nil {
				t.Fatal(err)
			}
			db.Close()
			var wg sync.WaitGroup
			errs := make([]error, 8)
			for i := range errs {
				wg.Go(func() {
					l, err := sqlitelog.Open(path)
					if err != nil {
						errs[i] = err
						return
					}
					defer l.Close()
					_, errs[i] = l.Append(ctx, "e", fmt.Sprintf("id%d", i), "new")
				})
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("%s mode, trial %d: concurrent Open %d of an old-schema log: %v", mode, trial, i, err)
				}
			}
		}
	}
}
