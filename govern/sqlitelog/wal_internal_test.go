package sqlitelog

import (
	"context"
	"database/sql"
	"database/sql/driver"
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

// Every connection the pool opens has the busy timeout, not only the first: a connection the pool
// replaces (after a driver error, say) would otherwise fail with SQLITE_BUSY the moment another
// writer held the lock.
func TestOpen_BusyTimeoutOnEveryConnection(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "bt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	old, err := l.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Report the connection as bad, so the pool discards it and opens a new one.
	_ = old.Raw(func(any) error { return driver.ErrBadConn })
	old.Close()
	var ms int
	if err := l.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	if want := int(busyWait.Milliseconds()); ms != want {
		t.Fatalf("busy_timeout on a replacement connection = %dms, want %dms", ms, want)
	}
}
