package sqlitelog

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// useWAL gives up once its wait is over while the file stays locked, rather than retry forever.
func TestUseWAL_GivesUpAfterItsWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	if _, err := holder.Exec(`CREATE TABLE t (x); BEGIN EXCLUSIVE; INSERT INTO t VALUES (1);`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	done := make(chan error, 1)
	go func() { done <- useWAL(db, 50*time.Millisecond) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("useWAL switched a file another connection holds exclusively")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("useWAL kept retrying past its wait")
	}
}
