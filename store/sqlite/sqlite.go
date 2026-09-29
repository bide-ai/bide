// Package sqlite is the default on-disk agent.Durable: a single-file, zero-cgo SQLite
// journal (via modernc.org/sqlite). It implements named-step memoization — the same
// primitive as DBOS RunAsStep / ADK RunNode — so an agent run survives a process crash
// and resumes without re-running completed steps. No cluster, no daemon: one binary +
// one file. The side-effect-safety layer (agent.Safety / ResumeHalt) sits above this.
//
// Store implements agent.Lister, so single-node crash recovery (agent.Recover) works on the
// default on-disk store: after a restart it enumerates the journal's runs and re-drives the
// in-flight ones. It does NOT implement agent.Leaser: cross-process HA lease coordination
// (one driver per run across nodes) remains Postgres-only, since SQLite is a single-writer,
// single-node journal. Under a single recoverer this is safe regardless, because memoization
// makes re-driving at-most-once.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"golang.org/x/sync/singleflight"
	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed agent.Durable.
type Store struct {
	db *sql.DB
	sf singleflight.Group // collapse concurrent Do on the same (runID,name) — at-most-once fn
}

var (
	_ agent.Durable = (*Store)(nil) // port/adapter contract
	_ agent.Lister  = (*Store)(nil) // enumerates runs for single-node crash recovery
)

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
	// A writer waits up to 30s for another process's write lock. SQLite's busy handler is not
	// fair, so under a burst of writers on one file (or a slow disk) a single writer can wait
	// well past a few seconds; failing then would lose a write whose side effect already ran.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=30000;`); err != nil {
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
	// INSERT OR IGNORE + PK(run_id,name) additionally dedupes across processes. The callers
	// share the stored bytes, and each decodes its own copy below, so the record Do returns is
	// the one History returns for this step, on the live path too.
	v, err, _ := s.sf.Do(runID+"\x00"+name, func() (any, error) {
		if data, ok, e := s.load(ctx, runID, name); e != nil {
			return nil, e
		} else if ok {
			return data, nil // already recorded → don't run fn
		}

		rec, e := fn(ctx) // run outside any transaction (may do slow model/tool I/O)
		if e != nil {
			return nil, e // not recorded — re-runs on the next attempt
		}
		data, e := agent.JournalEntry(name, rec) // names and salts the record
		if e != nil {
			return nil, fmt.Errorf("marshal step %q: %w (%w)", name, e, agent.ErrStorage)
		}
		// Record under a context that ignores cancellation: fn has run, so its side effect may
		// have happened, and a driver whose lease lapsed or whose process is shutting down must
		// still journal the outcome rather than leave it unknown (resume would halt on it).
		res, e := s.db.ExecContext(context.WithoutCancel(ctx), `
			INSERT OR IGNORE INTO steps (run_id, seq, name, data)
			VALUES (?, (SELECT COALESCE(MAX(seq), -1) + 1 FROM steps WHERE run_id = ?), ?, ?)`,
			runID, runID, name, data)
		if e != nil {
			return nil, fmt.Errorf("insert step %q: %w (%w)", name, e, agent.ErrStorage)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Another process recorded this step first: its record is the step's. Return it or an
			// error, never rec, which a caller such as ClaimAttempt would read as a win. The
			// reload ignores cancellation so a caller cancelled mid-step still learns the truth.
			existing, ok, e := s.load(context.WithoutCancel(ctx), runID, name)
			if e != nil {
				return nil, fmt.Errorf("reload step %q after a conflicting insert: %w (%w)", name, e, agent.ErrStorage)
			}
			if !ok {
				return nil, fmt.Errorf("step %q: insert conflicted but no record exists (%w)", name, agent.ErrStorage)
			}
			return existing, nil
		}
		return data, nil
	})
	if err != nil {
		return agent.Record{}, err
	}
	return agent.DecodeRecord(v.([]byte))
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
		rec, err := agent.DecodeRecord(data)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Runs implements agent.Lister: the distinct run IDs the store holds, so agent.Recover can
// enumerate in-flight runs to re-drive after a restart. This makes single-node crash recovery
// work on the default on-disk store; cross-process HA leasing (agent.Leaser) remains
// Postgres-only, since SQLite is a single-writer, single-node journal.
func (s *Store) Runs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT run_id FROM steps ORDER BY run_id`)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w (%w)", err, agent.ErrStorage)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan run id: %w (%w)", err, agent.ErrStorage)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// load returns the stored encoding of the step (runID, name), if it is recorded.
func (s *Store) load(ctx context.Context, runID, name string) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM steps WHERE run_id = ? AND name = ?`, runID, name).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	return data, true, nil
}
