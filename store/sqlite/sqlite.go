// Package sqlite is the default on-disk agent.Durable: a single-file, zero-cgo SQLite
// journal (via modernc.org/sqlite). It implements named-step memoization — the same
// primitive as DBOS RunAsStep / ADK RunNode — so an agent run survives a process crash
// and resumes without re-running completed steps. No cluster, no daemon: one binary +
// one file. The side-effect-safety layer (agent.Safety / ResumeHalt) sits above this.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	agent "github.com/dayna/go-agents"
	"golang.org/x/sync/singleflight"
	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed agent.Durable.
type Store struct {
	db *sql.DB
	sf singleflight.Group // collapse concurrent Do on the same (runID,name) — at-most-once fn
}

var _ agent.Durable = (*Store)(nil) // port/adapter contract

// Open opens (creating if needed) a SQLite journal at path. Use ":memory:" for an
// ephemeral store in tests. WAL mode is enabled for concurrent readers alongside the
// single writer.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One writer at a time keeps the memoization insert race-free without extra locking.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS steps (
			run_id TEXT NOT NULL,
			seq    INTEGER NOT NULL,
			name   TEXT NOT NULL,
			data   BLOB NOT NULL,
			PRIMARY KEY (run_id, name)
		);
		CREATE INDEX IF NOT EXISTS idx_steps_run_seq ON steps(run_id, seq);
	`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Do implements agent.Durable: memoized, named-step execution.
func (s *Store) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	// Single-flight per (runID,name): concurrent in-process callers run fn ONCE. The
	// INSERT OR IGNORE + PK(run_id,name) additionally dedupes across processes.
	v, err, _ := s.sf.Do(runID+"\x00"+name, func() (any, error) {
		if rec, ok, e := s.load(ctx, runID, name); e != nil {
			return nil, e
		} else if ok {
			return rec, nil // already recorded → don't run fn
		}

		rec, e := fn(ctx) // run outside any transaction (may do slow model/tool I/O)
		if e != nil {
			return nil, e // not recorded — re-runs on the next attempt
		}
		rec.Name = name

		data, e := json.Marshal(rec)
		if e != nil {
			return nil, fmt.Errorf("marshal step %q: %w (%w)", name, e, agent.ErrStorage)
		}
		res, e := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO steps (run_id, seq, name, data)
			VALUES (?, (SELECT COALESCE(MAX(seq), -1) + 1 FROM steps WHERE run_id = ?), ?, ?)`,
			runID, runID, name, data)
		if e != nil {
			return nil, fmt.Errorf("insert step %q: %w (%w)", name, e, agent.ErrStorage)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if existing, ok, e := s.load(ctx, runID, name); e == nil && ok {
				return existing, nil // another process won the race
			}
		}
		return rec, nil
	})
	if err != nil {
		return agent.Record{}, err
	}
	return v.(agent.Record), nil
}

// History implements agent.Durable: all recorded steps for a run, in order.
func (s *Store) History(ctx context.Context, runID string) ([]agent.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM steps WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("query history %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	defer rows.Close()

	var out []agent.Record
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scan step: %w (%w)", err, agent.ErrStorage)
		}
		var rec agent.Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("unmarshal step: %w (%w)", err, agent.ErrStorage)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) load(ctx context.Context, runID, name string) (agent.Record, bool, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM steps WHERE run_id = ? AND name = ?`, runID, name).Scan(&data)
	if err == sql.ErrNoRows {
		return agent.Record{}, false, nil
	}
	if err != nil {
		return agent.Record{}, false, fmt.Errorf("load step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	var rec agent.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return agent.Record{}, false, fmt.Errorf("unmarshal step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	return rec, true, nil
}
