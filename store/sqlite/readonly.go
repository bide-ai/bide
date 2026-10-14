package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/dbpath"
)

// OpenReadOnly opens the SQLite journal at path for reading only. The file is opened read-only
// (mode=ro and query_only) on one pool of up to four connections; nothing is created, migrated or
// checkpointed. Get, Load and Runs (agent.Lister) read as they do on a Store from Open. Every write
// fails: Insert with an error wrapping agent.ErrStorage, and the agent.Leaser methods with an
// error. opts are Open's (WithTablePrefix names the tables to read).
//
// It refuses ":memory:" (ErrConfig), a missing file (which it does not create), a file with no
// journal tables, and one whose schema version is newer than this version's (ErrConfig). A journal
// another process is writing in WAL mode can be read while it runs; each read sees the journal as
// of that read. Close closes the pool. It is for tools that inspect a journal, such as bide view,
// never for driving a run.
func OpenReadOnly(path string, opts ...Option) (*Store, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" || strings.Contains(path, "mode=memory") {
		return nil, fmt.Errorf("sqlite: OpenReadOnly: an in-memory database has nothing to read: %w", agent.ErrConfig)
	}
	if path, err = dbpath.Canonical(path, false); err != nil {
		return nil, fmt.Errorf("sqlite: OpenReadOnly: %w (%w)", err, agent.ErrConfig)
	}
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&_pragma=query_only(1)&" +
		fmt.Sprintf("_pragma=busy_timeout(%d)", writeBusyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{w: db, r: db, l: db, own: true, t: cfg.tables()}
	var v int
	if err := db.QueryRowContext(context.Background(), `SELECT version FROM `+s.t.version+` WHERE id = 1`).Scan(&v); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: OpenReadOnly %s: read schema version (is it a bide journal?): %w%s (%w)", path, err, readHint(err), agent.ErrStorage)
	}
	if v > schemaVersion {
		db.Close()
		return nil, fmt.Errorf("sqlite: the database's schema version is %d, newer than this version's %d: %w", v, schemaVersion, agent.ErrConfig)
	}
	return s, nil
}

// readHint says what to check when a path will not open read-only: it names the file a read-only
// open needs to exist, or the file that is not a journal. It is empty for any other error.
func readHint(err error) string {
	switch msg := err.Error(); {
	case strings.Contains(msg, "unable to open database file"):
		return "; check that the file exists (a read-only open never creates it) and that the path is a file, not a directory"
	case strings.Contains(msg, "file is not a database"):
		return "; the file is not a SQLite database, so point at a journal a bide run wrote"
	case strings.Contains(msg, "no such table"):
		return "; the file is a SQLite database with no bide journal in it, so point at a journal a bide run wrote"
	}
	return ""
}
