// Package postgreslog is a Postgres-backed govern.EventLog for HIGH-AVAILABILITY governed state:
// multiple processes read the same governed event stream and replay it to the same convergent
// state. gsm guarantees order-independent convergence (WFC + CC), so shared-log replay is consistent
// across nodes regardless of the exact order each observes. It parallels store/postgres for the
// durable journal. It satisfies govern.EventLog structurally, so this package does not import
// govern, keeping the coupling one-directional.
package postgreslog

import (
	"context"
	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Log is a Postgres-backed append-only event log per entity.
type Log struct {
	db *sql.DB
}

// Open connects to Postgres via a pgx DSN (e.g. "postgres://user:pass@host:5432/db") and ensures
// the schema exists.
func Open(ctx context.Context, dsn string) (*Log, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	l := &Log{db: db}
	if err := l.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

// migrate creates the events table. A global bigserial id gives a total append order and, unlike a
// per-entity MAX(seq)+1, is race-free under concurrent appends (multiple nodes appending to the same
// entity never collide on a computed sequence). Per-entity order is the id order.
func (l *Log) migrate(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS events (
			id     bigserial PRIMARY KEY,
			entity text      NOT NULL,
			event  text      NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_events_entity_id ON events (entity, id);`)
	return err
}

// Close releases the underlying database connection pool.
func (l *Log) Close() error { return l.db.Close() }

// Append durably records an event for an entity. The bigserial id is assigned atomically, so
// concurrent appends never race on a sequence.
func (l *Log) Append(ctx context.Context, entity, event string) error {
	_, err := l.db.ExecContext(ctx, `INSERT INTO events (entity, event) VALUES ($1, $2)`, entity, event)
	return err
}

// Events returns an entity's events in append order.
func (l *Log) Events(ctx context.Context, entity string) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT event FROM events WHERE entity = $1 ORDER BY id`, entity)
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
