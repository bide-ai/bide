// Package sqlitelog is a durable, on-disk govern.EventLog backed by single-file SQLite
// (zero-cgo). It gives a PersistentGovernor real crash-recovery: events survive a process
// restart on disk, and (with a shared file / Postgres equivalent) multiple processes read
// the same governed event stream. It satisfies govern.EventLog structurally, so this
// package doesn't import govern — keeping the coupling one-directional.
package sqlitelog

import (
	"context"
	"database/sql"

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
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS events (
			entity text    NOT NULL,
			seq    INTEGER NOT NULL,
			event  text    NOT NULL,
			PRIMARY KEY (entity, seq)
		);`); err != nil {
		db.Close()
		return nil, err
	}
	return &Log{db: db}, nil
}

// Close releases the underlying database handle.
func (l *Log) Close() error { return l.db.Close() }

// Append durably records an event for an entity and returns its position (its seq). The seq is
// computed and inserted in one statement; SQLite serializes writers, and the (entity, seq) primary
// key rejects a duplicate, so concurrent appends from any number of processes get distinct, dense
// positions.
func (l *Log) Append(ctx context.Context, entity, event string) (int64, error) {
	var seq int64
	err := l.db.QueryRowContext(ctx, `
		INSERT INTO events (entity, seq, event)
		VALUES (?, (SELECT COALESCE(MAX(seq), -1) + 1 FROM events WHERE entity = ?), ?)
		RETURNING seq`,
		entity, entity, event).Scan(&seq)
	return seq, err
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
