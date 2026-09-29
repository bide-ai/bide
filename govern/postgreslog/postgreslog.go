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
	"errors"
	"fmt"

	"github.com/bide-ai/bide/agent"
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

// migrate creates the governed_events table. Each entity's events carry a dense position (seq),
// unique per entity, assigned by Append under a per-entity lock, and the append id they were
// recorded under, unique per entity. A table created before appends carried ids gains the column;
// its earlier rows have no id and never match a new append.
func (l *Log) migrate(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS governed_events (
			entity    text   NOT NULL,
			seq       bigint NOT NULL,
			event     text   NOT NULL,
			append_id text,
			PRIMARY KEY (entity, seq)
		);
		ALTER TABLE governed_events ADD COLUMN IF NOT EXISTS append_id text;
		CREATE UNIQUE INDEX IF NOT EXISTS governed_events_append_id ON governed_events (entity, append_id);`)
	return err
}

// Close releases the underlying database connection pool.
func (l *Log) Close() error { return l.db.Close() }

// Append durably records an event for an entity under id and returns its position (its seq).
// Concurrent appends to the same entity, from any number of processes, are serialized, so
// positions are dense and unique and the entity's log is append-only as every reader sees it. If
// the entity already holds id, nothing is inserted and the recorded position is returned.
//
// An append whose commit reply is lost is not retried here: database/sql retries a statement only
// on driver.ErrBadConn, which the driver reports only when nothing was sent on the connection. A
// caller that retries after such an error sends the same id, which returns the recorded position.
func (l *Log) Append(ctx context.Context, entity, id, event string) (int64, error) {
	if id == "" {
		return 0, fmt.Errorf("postgreslog: empty append id: %w", agent.ErrConfig)
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	seq, err := appendIn(ctx, tx, entity, id, event)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	return seq, tx.Commit()
}

// appendIn inserts one event inside tx and returns its position, or returns the position already
// recorded for id. It first takes a transaction-scoped advisory lock keyed by the entity, held
// until tx commits, so a second append to the same entity cannot look up its id or compute its
// position until the first is visible. Without it, two overlapping appends could both compute the
// same next position, or a slow append could become visible after a fast one that followed it, and
// a reader would see an event appear before one it had already seen. Appends to different entities
// do not wait on each other (a hash collision only costs some serialization). The primary key
// rejects a duplicate position and the unique index a duplicate id regardless.
func appendIn(ctx context.Context, tx *sql.Tx, entity, id, event string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, entity); err != nil {
		return 0, err
	}
	var seq int64
	var recorded string
	err := tx.QueryRowContext(ctx, `SELECT seq, event FROM governed_events WHERE entity = $1 AND append_id = $2`, entity, id).Scan(&seq, &recorded)
	if err == nil {
		if recorded != event {
			return 0, fmt.Errorf("postgreslog: append id %q holds event %q, not %q: %w", id, recorded, event, agent.ErrConfig)
		}
		return seq, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO governed_events (entity, seq, event, append_id)
		SELECT $1, COALESCE(MAX(seq) + 1, 0), $2, $3 FROM governed_events WHERE entity = $1
		RETURNING seq`, entity, event, id).Scan(&seq)
	return seq, err
}

// Events returns an entity's events at positions from onward, in log order.
func (l *Log) Events(ctx context.Context, entity string, from int64) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT event FROM governed_events WHERE entity = $1 AND seq >= $2 ORDER BY seq`, entity, from)
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
