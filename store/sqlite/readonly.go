package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// OpenReadOnly opens the SQLite journal at path for reading only: the file is opened read-only
// (mode=ro, query_only), nothing is created or migrated, and every write (Insert, and a lease
// statement) fails with an error wrapping agent.ErrStorage. A missing file, or one with no journal
// tables or a schema version newer than this version's, is an error. It is for tools that inspect
// a journal (bide-view), never for driving a run.
func OpenReadOnly(path string, opts ...Option) (*Store, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" || strings.Contains(path, "mode=memory") {
		return nil, fmt.Errorf("sqlite: OpenReadOnly: an in-memory database has nothing to read: %w", agent.ErrConfig)
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
		return nil, fmt.Errorf("sqlite: OpenReadOnly %s: read schema version (is it a bide journal?): %w (%w)", path, err, agent.ErrStorage)
	}
	if v > schemaVersion {
		db.Close()
		return nil, fmt.Errorf("sqlite: the database's schema version is %d, newer than this version's %d: %w", v, schemaVersion, agent.ErrConfig)
	}
	return s, nil
}
