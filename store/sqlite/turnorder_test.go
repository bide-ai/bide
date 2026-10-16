package sqlite_test

import (
	"database/sql"
	"fmt"
	"testing"
)

// commitOrder makes the file record the order in which its event rows commit, and returns a
// function that reads it back: the writer (run id) of each append, in commit order. The order comes
// from an autoincrement column filled by a trigger inside the writing transaction, so it is the
// order SQLite's write lock admitted the writers, and does not depend on when a goroutine gets
// scheduled after its append returns (a test that records the order itself, after Append returns,
// reads the scheduler too, and fails on a loaded machine).
func commitOrder(t *testing.T, path string) func() []string {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS turns (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL)`,
		`CREATE TRIGGER IF NOT EXISTS turns_log AFTER INSERT ON bide_events BEGIN INSERT INTO turns (run_id) VALUES (NEW.run_id); END`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(fmt.Errorf("%s: %w", q, err))
		}
	}
	return func() []string {
		rows, err := db.Query(`SELECT run_id FROM turns ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
}
