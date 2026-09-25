// Package postgres is a Postgres-backed agent.Durable for HIGH-AVAILABILITY durable
// agents. Unlike a single-file SQLite journal (or agenticenv's single-writer flock,
// which forbids it outright), shared Postgres storage lets ANY node resume ANY run —
// so a dead worker's in-flight agents pick up elsewhere. Same named-step memoization
// semantics as the SQLite backend; the side-effect-safety layer sits above it.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	agent "github.com/dayna/go-agents"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/sync/singleflight"
)

// Store is a Postgres-backed agent.Durable.
type Store struct {
	db *sql.DB
	sf singleflight.Group // collapse concurrent Do on the same (runID,name) — at-most-once fn
}

var (
	_ agent.Durable = (*Store)(nil) // port/adapter contract
	_ agent.Lister  = (*Store)(nil) // enumerates runs for crash recovery
	_ agent.Leaser  = (*Store)(nil) // leases runs so competing recoverers do not double-drive
)

// Open connects to Postgres via a pgx DSN (e.g. "postgres://user:pass@host:5432/db")
// and ensures the schema exists.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS steps (
			run_id text   NOT NULL,
			seq    bigint NOT NULL,
			name   text   NOT NULL,
			data   jsonb  NOT NULL,
			PRIMARY KEY (run_id, name)
		);
		CREATE INDEX IF NOT EXISTS idx_steps_run_seq ON steps (run_id, seq);
		CREATE TABLE IF NOT EXISTS leases (
			run_id text        PRIMARY KEY,
			holder text        NOT NULL,
			expiry timestamptz NOT NULL
		);`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Do implements agent.Durable with named-step memoization. Concurrency- and HA-safe:
// the INSERT ... ON CONFLICT DO NOTHING against PRIMARY KEY(run_id,name) means two
// nodes racing the same step converge on one recorded result.
func (s *Store) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	// Single-flight per (runID,name): concurrent in-process callers run fn ONCE. The
	// INSERT ... ON CONFLICT DO NOTHING additionally dedupes across nodes (HA).
	v, err, _ := s.sf.Do(runID+"\x00"+name, func() (any, error) {
		if rec, ok, e := s.load(ctx, runID, name); e != nil {
			return nil, e
		} else if ok {
			return rec, nil
		}

		rec, e := fn(ctx) // run outside any transaction (may do slow model/tool I/O)
		if e != nil {
			return nil, e // not recorded — re-runs next attempt
		}
		rec.Name = name

		data, e := json.Marshal(rec)
		if e != nil {
			return nil, fmt.Errorf("marshal step %q: %w (%w)", name, e, agent.ErrStorage)
		}
		tag, e := s.db.ExecContext(ctx, `
			INSERT INTO steps (run_id, seq, name, data)
			VALUES ($1, (SELECT COALESCE(MAX(seq), -1) + 1 FROM steps WHERE run_id = $1), $2, $3)
			ON CONFLICT (run_id, name) DO NOTHING`,
			runID, name, data)
		if e != nil {
			return nil, fmt.Errorf("insert step %q: %w (%w)", name, e, agent.ErrStorage)
		}
		if n, _ := tag.RowsAffected(); n == 0 {
			if existing, ok, e := s.load(ctx, runID, name); e == nil && ok {
				return existing, nil // another node won the race
			}
		}
		return rec, nil
	})
	if err != nil {
		return agent.Record{}, err
	}
	return v.(agent.Record), nil
}

// History implements agent.Durable.
func (s *Store) History(ctx context.Context, runID string) ([]agent.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM steps WHERE run_id = $1 ORDER BY seq`, runID)
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
	err := s.db.QueryRowContext(ctx, `SELECT data FROM steps WHERE run_id = $1 AND name = $2`, runID, name).Scan(&data)
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

// Runs implements agent.Lister: the distinct run IDs the store holds, so a crash-recovery
// supervisor can enumerate in-flight runs (see agent.Recover).
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

// AcquireLease implements agent.Leaser: claim runID for holder until now()+ttl. The upsert grants
// the lease when the run is unleased, already held by holder (renewal), or the current lease has
// expired, and grants nothing when another holder's lease is still live. Expiry uses the database
// clock (now()) so all nodes compare against one clock, not their own.
func (s *Store) AcquireLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	tag, err := s.db.ExecContext(ctx, `
		INSERT INTO leases (run_id, holder, expiry)
		VALUES ($1, $2, now() + ($3 * interval '1 second'))
		ON CONFLICT (run_id) DO UPDATE
			SET holder = EXCLUDED.holder, expiry = EXCLUDED.expiry
			WHERE leases.holder = EXCLUDED.holder OR leases.expiry < now()`,
		runID, holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("acquire lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	n, _ := tag.RowsAffected()
	return n > 0, nil
}

// RenewLease implements agent.Leaser: extend holder's still-live lease on runID. Returns false if
// holder no longer holds it (expired or taken over).
func (s *Store) RenewLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	tag, err := s.db.ExecContext(ctx, `
		UPDATE leases SET expiry = now() + ($3 * interval '1 second')
		WHERE run_id = $1 AND holder = $2 AND expiry >= now()`,
		runID, holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("renew lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	n, _ := tag.RowsAffected()
	return n > 0, nil
}

// ReleaseLease implements agent.Leaser: relinquish runID if held by holder (a no-op otherwise).
func (s *Store) ReleaseLease(ctx context.Context, runID, holder string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE run_id = $1 AND holder = $2`, runID, holder); err != nil {
		return fmt.Errorf("release lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return nil
}
