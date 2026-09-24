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

func (l *Log) Close() error { return l.db.Close() }

// Append durably records an event for an entity (monotonic seq).
func (l *Log) Append(ctx context.Context, entity, event string) error {
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO events (entity, seq, event)
		VALUES (?, (SELECT COALESCE(MAX(seq), -1) + 1 FROM events WHERE entity = ?), ?)`,
		entity, entity, event)
	return err
}

// Events returns an entity's events in append order.
func (l *Log) Events(ctx context.Context, entity string) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT event FROM events WHERE entity = ? ORDER BY seq`, entity)
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
