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

// migrate creates the events table. Per-entity order is the id order. A bigserial id is drawn at
// insert but becomes visible at commit, and commits can land out of id order, so Append serializes
// appends per entity (see appendIn): an entity's events then become visible in id order, and the log
// a reader sees only ever grows at the end.
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

// Append durably records an event for an entity. Concurrent appends to the same entity, from any
// number of processes, are serialized, so the entity's log is append-only as every reader sees it.
func (l *Log) Append(ctx context.Context, entity, event string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := appendIn(ctx, tx, entity, event); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// appendIn inserts one event inside tx. It first takes a transaction-scoped advisory lock keyed by
// the entity, held until tx commits, so a second append to the same entity cannot draw its id until
// the first is visible. Without it, a slow append could draw a lower id than a fast one yet commit
// after it, and a reader would see an event appear before one it had already seen. Appends to
// different entities do not wait on each other (a hash collision only costs some serialization).
func appendIn(ctx context.Context, tx *sql.Tx, entity, event string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, entity); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO events (entity, event) VALUES ($1, $2)`, entity, event)
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
