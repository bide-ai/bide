package sqlitelog

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Another process holds the write lock for longer than a few seconds (a slow disk, a burst of
// writers on one file). An append waits its turn instead of failing with "database is locked".
func TestAppend_WaitsOutABusyWriter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.db")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	other, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO events (entity, seq, event) VALUES ('other', 0, 'x')`); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(6 * time.Second); tx.Commit() }()
	if _, err := l.Append(context.Background(), "e", "id", "ev"); err != nil {
		t.Fatalf("append while another writer held the lock for 6s: %v", err)
	}
}
