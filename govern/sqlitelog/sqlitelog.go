// Package sqlitelog is a durable, on-disk govern.EventLog backed by single-file SQLite
// (zero-cgo). It gives a PersistentGovernor real crash-recovery: events survive a process
// restart on disk, and (with a shared file / Postgres equivalent) multiple processes read
// the same governed event stream. It satisfies govern.EventLog structurally, so this
// package doesn't import govern — keeping the coupling one-directional.
package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bide-ai/bide/agent"
	_ "modernc.org/sqlite"
)

// Log is an on-disk append-only event log per entity.
type Log struct {
	db *sql.DB
}

// Open opens (creating if needed) an event log at path (":memory:" for ephemeral).
func Open(path string) (*Log, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// A writer waits up to 30s for another process's write lock. SQLite's busy handler is not
	// fair, so under a burst of writers on one file (or a slow disk) a single writer can wait
	// well past a few seconds; failing then would lose a write whose side effect already ran.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=30000;`); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Log{db: db}, nil
}

// migrate creates the events table, and adds the append_id column and its unique index to a table
// created before appends carried ids. Rows from before that have no id; they never match a new
// append.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS events (
			entity    text    NOT NULL,
			seq       INTEGER NOT NULL,
			event     text    NOT NULL,
			append_id text,
			PRIMARY KEY (entity, seq)
		);`); err != nil {
		return err
	}
	var has int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('events') WHERE name = 'append_id'`).Scan(&has); err != nil {
		return err
	}
	if has == 0 {
		if _, err := db.Exec(`ALTER TABLE events ADD COLUMN append_id text`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS events_append_id ON events (entity, append_id)`)
	return err
}

// Close releases the underlying database handle.
func (l *Log) Close() error { return l.db.Close() }

// Append durably records an event for an entity under id and returns its position (its seq). The
// seq is computed and inserted in one statement; SQLite serializes writers, and the (entity, seq)
// primary key rejects a duplicate, so concurrent appends from any number of processes get
// distinct, dense positions. The unique (entity, append_id) index makes the append idempotent: if
// the entity already holds id, nothing is inserted and the recorded position is returned.
func (l *Log) Append(ctx context.Context, entity, id, event string) (int64, error) {
	if id == "" {
		return 0, fmt.Errorf("sqlitelog: empty append id: %w", agent.ErrConfig)
	}
	var seq int64
	err := l.db.QueryRowContext(ctx, `
		INSERT INTO events (entity, seq, event, append_id)
		VALUES (?, (SELECT COALESCE(MAX(seq), -1) + 1 FROM events WHERE entity = ?), ?, ?)
		ON CONFLICT (entity, append_id) DO NOTHING
		RETURNING seq`,
		entity, entity, event, id).Scan(&seq)
	if err == nil {
		return seq, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	// The id is already recorded: return its position, if it holds the same event.
	var recorded string
	if err := l.db.QueryRowContext(ctx, `SELECT seq, event FROM events WHERE entity = ? AND append_id = ?`, entity, id).Scan(&seq, &recorded); err != nil {
		return 0, fmt.Errorf("sqlitelog: read the append recorded for id %q: %w", id, err)
	}
	if recorded != event {
		return 0, fmt.Errorf("sqlitelog: append id %q holds event %q, not %q: %w", id, recorded, event, agent.ErrConfig)
	}
	return seq, nil
}

// Events returns an entity's events at positions from onward, in log order.
func (l *Log) Events(ctx context.Context, entity string, from int64) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT event FROM events WHERE entity = ? AND seq >= ? ORDER BY seq`, entity, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
