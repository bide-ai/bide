// Package postgres is a Postgres-backed agent.Store for high-availability durable agents: shared
// Postgres storage lets any node resume any run, so a dead worker's in-flight agents pick up
// elsewhere. It implements agent.Store, agent.Lister and agent.Leaser.
//
// The tables are named with a prefix, "bide_" by default (see WithTablePrefix): bide_steps holds
// the journal (run_id, seq, name, data), where data is bytea, each entry's bytes verbatim;
// bide_leases holds the leases; bide_schema_version the schema version. Open and New create the
// tables if they do not exist and refuse a database whose schema version is newer than this
// version knows. Run IDs are compared and ordered by their bytes (collation "C"), so a Lister's
// order does not depend on the database's locale.
//
// # Isolation
//
// Every write the store makes (an insert, the lease calls, creating the schema) runs in a
// transaction that sets its own isolation level, read committed, whatever the deployment's
// default_transaction_isolation (set on the server, database, role or DSN) says. The journal's
// correctness depends on it: an insert takes a per-run advisory lock and then reads the run's last
// position, which at repeatable read or serializable would come from a snapshot taken before the
// lock was granted. Reads are single SELECT statements, which see one snapshot at any level.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// schemaVersion is the version of the tables this package creates and reads.
const schemaVersion = 1

// pageSize is how many entries a Load reads per query, and runsPage how many run IDs a Runs call
// reads per query. No connection is held between pages.
const (
	pageSize = 256
	runsPage = 500
)

// Store is a Postgres-backed agent.Store.
type Store struct {
	db  *sql.DB
	own bool // Close closes db: Open opened it
	t   tables

	jOnce sync.Once
	j     *agent.Journal
}

var (
	_ agent.Store   = (*Store)(nil)
	_ agent.Lister  = (*Store)(nil)
	_ agent.Leaser  = (*Store)(nil)
	_ agent.Durable = (*Store)(nil) // transitional: Do and History go through its Journal
)

// tables holds the table names, prefixed, and the statements built from them.
type tables struct {
	steps, leases, version string
	insert                 string
	get, load              selectSQL
}

func newTables(prefix string) (tables, error) {
	t := tables{steps: prefix + "steps", leases: prefix + "leases", version: prefix + "schema_version"}
	t.insert = `INSERT INTO ` + t.steps + ` (run_id, seq, name, data)
		VALUES ($1, (SELECT COALESCE(MAX(seq), -1) + 1 FROM ` + t.steps + ` WHERE run_id = $1), $2, $3)
		ON CONFLICT (run_id, name) DO NOTHING
		RETURNING seq`
	var err error
	if t.get, err = newSelect(`SELECT seq, data FROM ` + t.steps + ` WHERE run_id = $1 AND name = $2`); err != nil {
		return tables{}, err
	}
	if t.load, err = newSelect(`SELECT seq, name, data FROM ` + t.steps + ` WHERE run_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`); err != nil {
		return tables{}, err
	}
	return t, nil
}

// selectSQL is a query that starts with the keyword SELECT, the only kind the store runs on the
// pool outside a transaction begun with txOptions (the isolation check in txoptions_test.go allows
// a value of this type there, as it allows a constant SELECT). Only newSelect makes one.
type selectSQL string

// newSelect returns q as a selectSQL, or an error if q does not start with the SELECT keyword.
func newSelect(q string) (selectSQL, error) {
	t := strings.TrimLeftFunc(q, unicode.IsSpace)
	if len(t) <= len("SELECT") || !strings.EqualFold(t[:len("SELECT")], "SELECT") || !unicode.IsSpace(rune(t[len("SELECT")])) {
		return "", fmt.Errorf("postgres: a read on the pool must be a SELECT, got %.40q: %w", q, agent.ErrConfig)
	}
	return selectSQL(q), nil
}

// txOptions are the options of every transaction the store begins. The isolation level is set
// explicitly so the store does not inherit the deployment's default_transaction_isolation: each
// write takes an advisory lock and then reads rows committed while it waited for the lock, which
// only read committed shows it (at repeatable read or serializable the transaction's snapshot is
// taken by its first statement, the lock), and each lease upsert or update must proceed on a row
// another transaction changed after it began, where the stricter levels fail with 40001.
var txOptions = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// Option configures Open and New.
type Option interface{ apply(*config) error }

type config struct{ prefix string }

type optionFunc func(*config) error

func (f optionFunc) apply(c *config) error { return f(c) }

var prefixPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// WithTablePrefix names the tables with prefix instead of "bide_": prefix+"steps",
// prefix+"leases" and prefix+"schema_version". It must be a lower-case SQL identifier (letters,
// digits and underscores, not starting with a digit), or Open and New return an ErrConfig error.
func WithTablePrefix(prefix string) Option {
	return optionFunc(func(c *config) error {
		if !prefixPattern.MatchString(prefix) {
			return fmt.Errorf("postgres: table prefix %q is not a lower-case SQL identifier: %w", prefix, agent.ErrConfig)
		}
		c.prefix = prefix
		return nil
	})
}

func newConfig(opts []Option) (config, error) {
	c := config{prefix: "bide_"}
	for _, o := range opts {
		if o == nil {
			return c, fmt.Errorf("postgres: nil option: %w", agent.ErrConfig)
		}
		if err := o.apply(&c); err != nil {
			return c, err
		}
	}
	return c, nil
}

// Open connects to Postgres via a pgx DSN (e.g. "postgres://user:pass@host:5432/db") and ensures
// the schema exists. Close closes the connection pool.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s, err := New(ctx, db, opts...)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.own = true
	return s, nil
}

// New returns a Store over db, which the caller opened (with the pgx driver) and closes, and
// ensures the schema exists.
func New(ctx context.Context, db *sql.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("postgres: New: nil db: %w", agent.ErrConfig)
	}
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	t, err := newTables(cfg.prefix)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, t: t}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// migrateLock is the advisory lock key that serializes schema creation across nodes opening the
// store at once ("bide" in ASCII).
const migrateLock = 0x62696465

// migrate creates the tables if they do not exist and checks the schema version. It never alters
// an existing table: altering a table other nodes are running on would take its exclusive lock and
// change what they write.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, txOptions)
	if err != nil {
		return fmt.Errorf("postgres: migrate: %w (%w)", err, agent.ErrStorage)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLock); err != nil {
		return fmt.Errorf("postgres: migrate: %w (%w)", err, agent.ErrStorage)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %[1]s (
			run_id text COLLATE "C" NOT NULL,
			seq    bigint NOT NULL,
			name   text   NOT NULL,
			data   bytea  NOT NULL,
			PRIMARY KEY (run_id, name),
			UNIQUE (run_id, seq)
		);
		CREATE TABLE IF NOT EXISTS %[2]s (
			run_id text        PRIMARY KEY,
			holder text        NOT NULL,
			expiry timestamptz NOT NULL
		);
		CREATE TABLE IF NOT EXISTS %[3]s (
			id      int PRIMARY KEY CHECK (id = 1),
			version int NOT NULL
		);
		INSERT INTO %[3]s (id, version) VALUES (1, %[4]d) ON CONFLICT (id) DO NOTHING;`,
		s.t.steps, s.t.leases, s.t.version, schemaVersion)); err != nil {
		return fmt.Errorf("postgres: create tables: %w (%w)", err, agent.ErrStorage)
	}
	var v int
	if err := tx.QueryRowContext(ctx, `SELECT version FROM `+s.t.version+` WHERE id = 1`).Scan(&v); err != nil {
		return fmt.Errorf("postgres: read schema version: %w (%w)", err, agent.ErrStorage)
	}
	if v > schemaVersion {
		return fmt.Errorf("postgres: the database's schema version is %d, newer than this version's %d: %w", v, schemaVersion, agent.ErrConfig)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: migrate: %w (%w)", err, agent.ErrStorage)
	}
	return nil
}

// Close closes the connection pool Open opened. A Store made with New leaves its db open.
func (s *Store) Close() error {
	if !s.own {
		return nil
	}
	return s.db.Close()
}

// Insert implements agent.Store. Inserts into one run are serialized by a transaction-scoped
// advisory lock on the run, held until commit, so each takes the next position (MAX(seq)+1) and
// positions follow commit order: a reader never sees an entry before one with a lower Seq.
func (s *Store) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := s.insert(ctx, runID, name, data)
	if err != nil {
		return agent.Entry{}, false, fmt.Errorf("insert step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	return e, ok, nil
}

func (s *Store) insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	tx, err := s.db.BeginTx(ctx, txOptions) // read committed: see txOptions
	if err != nil {
		return agent.Entry{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, runID); err != nil {
		return agent.Entry{}, false, err
	}
	var seq int64
	err = tx.QueryRowContext(ctx, s.t.insert, runID, name, data).Scan(&seq)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return agent.Entry{}, false, err
		}
		return agent.Entry{Seq: seq, Name: name, Data: data}, true, nil // the database copied data
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return agent.Entry{}, false, err
	}
	// Another writer stored the name first and has committed (it held the run's lock until then):
	// its entry is the name's.
	e := agent.Entry{Name: name}
	if err := tx.QueryRowContext(ctx, string(s.t.get), runID, name).Scan(&e.Seq, &e.Data); err != nil {
		return agent.Entry{}, false, fmt.Errorf("the insert conflicted, then reading the stored entry failed: %w", err)
	}
	return e, false, nil
}

// Get implements agent.Store.
func (s *Store) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	e := agent.Entry{Name: name}
	err := s.db.QueryRowContext(ctx, string(s.t.get), runID, name).Scan(&e.Seq, &e.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.Entry{}, false, nil
	}
	if err != nil {
		return agent.Entry{}, false, fmt.Errorf("load step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	return e, true, nil
}

// Load implements agent.Store. It reads pageSize entries per query and yields them with no
// connection held, so the caller may write to the store inside its loop.
func (s *Store) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		for {
			page, err := s.loadPage(ctx, runID, after)
			if err != nil {
				yield(agent.Entry{}, err)
				return
			}
			for _, e := range page {
				if !yield(e, nil) {
					return
				}
			}
			if len(page) < pageSize {
				return
			}
			after = page[len(page)-1].Seq
		}
	}
}

func (s *Store) loadPage(ctx context.Context, runID string, after int64) ([]agent.Entry, error) {
	rows, err := s.db.QueryContext(ctx, string(s.t.load), runID, after, pageSize)
	if err != nil {
		return nil, fmt.Errorf("query run %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	defer rows.Close()
	var page []agent.Entry
	for rows.Next() {
		var e agent.Entry
		if err := rows.Scan(&e.Seq, &e.Name, &e.Data); err != nil {
			return nil, fmt.Errorf("scan step: %w (%w)", err, agent.ErrStorage)
		}
		page = append(page, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query run %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return page, nil
}

// Runs implements agent.Lister. The filter is evaluated in the query, runsPage run IDs at a time,
// with no connection held between pages.
func (s *Store) Runs(ctx context.Context, f agent.RunFilter) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		after := f.After
		for {
			page, err := s.runsPage(ctx, f, after)
			if err != nil {
				yield("", err)
				return
			}
			for _, id := range page {
				if !yield(id, nil) {
					return
				}
			}
			if len(page) < runsPage {
				return
			}
			after = page[len(page)-1]
		}
	}
}

func (s *Store) runsPage(ctx context.Context, f agent.RunFilter, after string) ([]string, error) {
	q := `SELECT DISTINCT run_id COLLATE "C" AS id FROM ` + s.t.steps + ` AS s WHERE run_id COLLATE "C" > $1`
	args := []any{after}
	if f.Prefix != "" {
		// The range lets the primary key's index serve the scan; starts_with checks the prefix.
		q += fmt.Sprintf(` AND run_id COLLATE "C" >= $%d AND starts_with(run_id, $%d)`, len(args)+1, len(args)+1)
		args = append(args, f.Prefix)
		if end, ok := prefixEnd(f.Prefix); ok {
			q += fmt.Sprintf(` AND run_id COLLATE "C" < $%d`, len(args)+1)
			args = append(args, end)
		}
	}
	if len(f.ExcludeHolding) > 0 {
		q += fmt.Sprintf(` AND NOT EXISTS (SELECT 1 FROM %s AS x WHERE x.run_id = s.run_id AND x.name = ANY($%d))`, s.t.steps, len(args)+1)
		args = append(args, f.ExcludeHolding)
	}
	q += fmt.Sprintf(` ORDER BY id LIMIT %d`, runsPage)
	sel, err := newSelect(q)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, string(sel), args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w (%w)", err, agent.ErrStorage)
	}
	defer rows.Close()
	var page []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan run id: %w (%w)", err, agent.ErrStorage)
		}
		page = append(page, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w (%w)", err, agent.ErrStorage)
	}
	return page, nil
}

// AcquireLease implements agent.Leaser: claim runID for holder until now()+ttl. The upsert grants
// the lease when the run is unleased, already held by holder (renewal), or the current lease has
// expired, and grants nothing when another holder's lease is still live. Expiry uses the database
// clock (now()) so all nodes compare against one clock, not their own.
func (s *Store) AcquireLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	n, err := s.write(ctx, `
		INSERT INTO `+s.t.leases+` (run_id, holder, expiry)
		VALUES ($1, $2, now() + ($3 * interval '1 second'))
		ON CONFLICT (run_id) DO UPDATE
			SET holder = EXCLUDED.holder, expiry = EXCLUDED.expiry
			WHERE `+s.t.leases+`.holder = EXCLUDED.holder OR `+s.t.leases+`.expiry < now()`,
		runID, holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("acquire lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return n > 0, nil
}

// RenewLease implements agent.Leaser: extend holder's still-live lease on runID. Returns false if
// holder no longer holds it (expired or taken over).
func (s *Store) RenewLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	n, err := s.write(ctx, `
		UPDATE `+s.t.leases+` SET expiry = now() + ($3 * interval '1 second')
		WHERE run_id = $1 AND holder = $2 AND expiry >= now()`,
		runID, holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("renew lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return n > 0, nil
}

// ReleaseLease implements agent.Leaser: relinquish runID if held by holder (a no-op otherwise).
func (s *Store) ReleaseLease(ctx context.Context, runID, holder string) error {
	if _, err := s.write(ctx, `DELETE FROM `+s.t.leases+` WHERE run_id = $1 AND holder = $2`, runID, holder); err != nil {
		return fmt.Errorf("release lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return nil
}

// write runs one data-changing statement in its own transaction at read committed (txOptions) and
// returns the number of rows it affected. Run on its own, the statement would be a transaction at
// the deployment's default isolation, where an upsert or update that meets a row changed by a
// transaction that committed after the statement began fails with 40001 instead of acting on the
// row's latest version.
func (s *Store) write(ctx context.Context, query string, args ...any) (int64, error) {
	tx, err := s.db.BeginTx(ctx, txOptions)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Journal returns the Journal over s that its Do and History shims delegate to.
//
// Deprecated: transitional; removed by the 1.0 rewrite. Use agent.NewJournal(s).
func (s *Store) Journal() *agent.Journal {
	s.jOnce.Do(func() {
		j, err := agent.NewJournal(s)
		if err != nil {
			panic(err) // s is not nil
		}
		s.j = j
	})
	return s.j
}

// Do runs a memoized step through s's Journal (see agent.Journal.Do).
//
// Deprecated: transitional; removed by the 1.0 rewrite. Use a Journal over the store.
func (s *Store) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return s.Journal().Do(ctx, runID, name, fn)
}

// History reads a run back through s's Journal (see agent.Journal.History).
//
// Deprecated: transitional; removed by the 1.0 rewrite. Use a Journal over the store.
func (s *Store) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return s.Journal().History(ctx, runID)
}

// prefixEnd returns the least string greater, in byte order, than every string that starts with
// p, and false when there is none. It increments p's last code point (UTF-8 byte order is code
// point order), so the bound is valid UTF-8, which a text parameter must be.
func prefixEnd(p string) (string, bool) {
	r := []rune(p)
	for i := len(r) - 1; i >= 0; i-- {
		next := r[i] + 1
		if next == 0xD800 {
			next = 0xE000 // skip the surrogates, which UTF-8 cannot hold
		}
		if next <= utf8.MaxRune {
			r[i] = next
			return string(r[:i+1]), true
		}
	}
	return "", false
}
