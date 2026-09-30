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
// # One statement per write
//
// Every write the store makes after Open (an insert, AcquireLease, RenewLease, ReleaseLease) is a
// single statement sent on its own, which Postgres runs as a transaction of its own and commits
// before it replies. No transaction spans two round trips, so a client that stops between two of
// its round trips (a SIGSTOP, a suspended VM, a long GC pause, a partition) holds no row or
// advisory lock while it is stopped. Another node therefore takes an expired lease one TTL after
// the last renewal that committed, or records the run's next step, however long the stall lasts.
// Reads are single SELECT statements too, and nothing holds a connection between two pages.
//
// Each statement is atomic by itself: AcquireLease is an INSERT ... ON CONFLICT DO UPDATE whose
// WHERE grants the lease only when it is free, expired or already the holder's; RenewLease and
// ReleaseLease are an UPDATE and a DELETE conditioned on the holder (and, for a renewal, on the
// lease being live); and Insert computes the entry's position as MAX(seq)+1 in the INSERT itself.
// Two inserts into one run that read the same MAX collide on the (run_id, seq) key: the second
// waits for the first to commit, fails, and is run again, so every position follows commit order.
//
// # Isolation
//
// The statements behave the same whatever the deployment's default_transaction_isolation (set on
// the server, database, role or DSN). At read committed, a statement that meets a row another
// transaction changed after the statement began waits for that transaction and then acts on the
// row's latest version. At repeatable read or serializable, Postgres fails the statement with a
// serialization failure (40001) instead, and at serializable it may fail a read that conflicts
// with concurrent serializable writes the same way. A statement that fails that way has changed
// nothing, so the store runs it again, with a new snapshot; once the conflicting transaction has
// committed, that is the outcome read committed reaches in one attempt. The schema migration in
// Open is the one transaction of several statements; it sets read committed itself (see
// txOptions).
//
// # Retries and deadlines
//
// A statement is run again after a serialization failure or a deadlock, and an insert also after
// losing its position to another insert. A retry does not always follow another transaction's
// commit: at serializable, Postgres may fail a statement for a conflict with a transaction that
// has not committed yet, and that transaction may itself abort. Under heavy contention on one run
// (many writers inserting into it at once) an insert may lose its position several times. So the
// store spaces the attempts with a capped, jittered exponential backoff (from 1ms up to 100ms, a
// random fraction of it each time, so contending writers do not retry in lockstep), and keeps
// retrying until the statement succeeds, fails for another reason, or ctx is done: a bound on the
// attempts would turn contention into errors. A caller that needs a bound on the time a call may
// take sets a deadline on ctx; when ctx ends, the call returns an error that wraps both ctx's error
// and the last attempt's.
//
// The migration also sets idle_in_transaction_session_timeout for its own transaction, so a node
// stopped inside it cannot hold the migration lock. The store sets no session-wide timeout: no
// other statement it sends is ever inside a transaction, so the setting would guard nothing of
// the store's, and on a pool passed to New it would change the caller's own transactions. A
// deployment that wants the bound for its own code sets it on the role or the database.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
	"github.com/jackc/pgx/v5/pgconn"
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
	steps, leases, version          string
	insert, acquire, renew, release writeSQL
	get, load                       selectSQL
}

func newTables(prefix string) (tables, error) {
	t := tables{steps: prefix + "steps", leases: prefix + "leases", version: prefix + "schema_version"}
	var err error
	// The position is MAX(seq)+1 over the statement's snapshot. Two inserts that read the same MAX
	// collide on UNIQUE (run_id, seq), which is not the conflict target, so the second fails with
	// 23505 once the first commits and insert runs it again (see insert).
	if t.insert, err = newWrite(`INSERT INTO ` + t.steps + ` (run_id, seq, name, data)
		VALUES ($1, (SELECT COALESCE(MAX(seq), -1) + 1 FROM ` + t.steps + ` WHERE run_id = $1), $2, $3)
		ON CONFLICT (run_id, name) DO NOTHING
		RETURNING seq`); err != nil {
		return tables{}, err
	}
	// Granted when the run is unleased, when the live lease is already holder's, or when the lease
	// has expired; a row another holder holds live is left alone and nothing is returned.
	if t.acquire, err = newWrite(`INSERT INTO ` + t.leases + ` (run_id, holder, expiry)
		VALUES ($1, $2, now() + ($3 * interval '1 second'))
		ON CONFLICT (run_id) DO UPDATE
			SET holder = EXCLUDED.holder, expiry = EXCLUDED.expiry
			WHERE ` + t.leases + `.holder = EXCLUDED.holder OR ` + t.leases + `.expiry < now()`); err != nil {
		return tables{}, err
	}
	if t.renew, err = newWrite(`UPDATE ` + t.leases + ` SET expiry = now() + ($3 * interval '1 second')
		WHERE run_id = $1 AND holder = $2 AND expiry >= now()`); err != nil {
		return tables{}, err
	}
	if t.release, err = newWrite(`DELETE FROM ` + t.leases + ` WHERE run_id = $1 AND holder = $2`); err != nil {
		return tables{}, err
	}
	if t.get, err = newSelect(`SELECT seq, data FROM ` + t.steps + ` WHERE run_id = $1 AND name = $2`); err != nil {
		return tables{}, err
	}
	if t.load, err = newSelect(`SELECT seq, name, data FROM ` + t.steps + ` WHERE run_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`); err != nil {
		return tables{}, err
	}
	return t, nil
}

// selectSQL is a query that starts with the keyword SELECT. The statement check in
// txoptions_test.go allows a value of this type on the pool, as it allows a constant SELECT. Only
// newSelect makes one.
type selectSQL string

// writeSQL is one INSERT, UPDATE or DELETE statement, which the store sends on the pool on its
// own, so Postgres runs it as a transaction of its own. The statement check in txoptions_test.go
// allows a value of this type on the pool. Only newWrite makes one.
type writeSQL string

// newWrite returns q as a writeSQL, or an error unless q is one statement the pool may run on its
// own (see oneStatement) that starts with INSERT, UPDATE or DELETE.
func newWrite(q string) (writeSQL, error) {
	for _, kw := range []string{"INSERT", "UPDATE", "DELETE"} {
		if startsWith(q, kw) && oneStatement(q) {
			return writeSQL(q), nil
		}
	}
	return "", fmt.Errorf("postgres: a write on the pool must be one INSERT, UPDATE or DELETE statement, got %.40q: %w", q, agent.ErrConfig)
}

// newSelect returns q as a selectSQL, or an error unless q is one statement the pool may run on
// its own (see oneStatement) that starts with SELECT.
func newSelect(q string) (selectSQL, error) {
	if !startsWith(q, "SELECT") || !oneStatement(q) {
		return "", fmt.Errorf("postgres: a read on the pool must be one SELECT statement, got %.40q: %w", q, agent.ErrConfig)
	}
	return selectSQL(q), nil
}

// sessionLock matches the session-level advisory lock functions (pg_advisory_lock,
// pg_advisory_lock_shared, pg_try_advisory_lock and pg_try_advisory_lock_shared), whose lock
// outlives the statement's transaction and so would be held across round trips.
var sessionLock = regexp.MustCompile(`(?i)pg_(try_)?advisory_lock`)

// oneStatement reports whether q can only be one statement that holds nothing after it ends: it
// holds no semicolon (under the simple protocol, which a DSN can select, a semicolon separates
// statements, one of which could be BEGIN) and names no session-level advisory lock.
func oneStatement(q string) bool {
	return !strings.Contains(q, ";") && !sessionLock.MatchString(q)
}

// startsWith reports whether q starts with the keyword kw, after leading white space.
func startsWith(q, kw string) bool {
	t := strings.TrimLeftFunc(q, unicode.IsSpace)
	return len(t) > len(kw) && strings.EqualFold(t[:len(kw)], kw) && unicode.IsSpace(rune(t[len(kw)]))
}

// txOptions are the options of the one transaction the store begins, the schema migration. The
// isolation level is set explicitly so the migration does not inherit the deployment's
// default_transaction_isolation: it takes an advisory lock and then reads the schema version,
// which at repeatable read or serializable would come from a snapshot taken by the lock statement,
// before another node's migration committed.
var txOptions = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// migrateIdleTimeout bounds how long the migration's transaction may sit idle between two of its
// statements. A node stopped inside it (a SIGSTOP, a suspended VM) would otherwise hold the
// migration lock, and every node opening the store would wait for as long as the stop lasts;
// Postgres ends the stopped node's session instead, which rolls the migration back. A live node
// sends its next statement within milliseconds.
const migrateIdleTimeout = 5 * time.Second

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
	// Set before the lock is taken, so the migration never holds it idle without the bound.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL idle_in_transaction_session_timeout = %d`, migrateIdleTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("postgres: migrate: %w (%w)", err, agent.ErrStorage)
	}
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

// Insert implements agent.Store. It is one INSERT statement, committed before Postgres replies,
// that takes the next position (MAX(seq)+1) in its own snapshot. An insert computes position n+1
// only when the entry at n is committed and visible to it, so positions follow commit order: a
// reader never sees an entry before one with a lower Seq. Two inserts that read the same MAX
// collide on UNIQUE (run_id, seq); the later one waits for the first to commit, fails with 23505,
// and runs again with a snapshot that sees it.
func (s *Store) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := s.insert(ctx, runID, name, data)
	if err != nil {
		return agent.Entry{}, false, fmt.Errorf("insert step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	return e, ok, nil
}

func (s *Store) insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	var b backoff
	for {
		var seq int64
		err := s.db.QueryRowContext(ctx, string(s.t.insert), runID, name, data).Scan(&seq)
		switch {
		case err == nil:
			return agent.Entry{Seq: seq, Name: name, Data: data}, true, nil // the database copied data
		case errors.Is(err, sql.ErrNoRows):
			// ON CONFLICT DO NOTHING met the name's row, which it does only once that row's writer
			// has committed (it waits for a writer still in progress), so a new snapshot sees it.
			e, err := s.get(ctx, runID, name)
			if err != nil {
				return agent.Entry{}, false, fmt.Errorf("the insert conflicted, then reading the stored entry failed: %w", err)
			}
			return e, false, nil
		case retryable(err) || sqlState(err) == uniqueViolation:
			// Nothing was written. A unique violation can only be on (run_id, seq), since a
			// conflict on the name does nothing: another insert took the position after this
			// one's snapshot, and has committed.
			if werr := b.wait(ctx); werr != nil {
				return agent.Entry{}, false, fmt.Errorf("%w (the last attempt failed: %w)", werr, err)
			}
		default:
			return agent.Entry{}, false, err
		}
	}
}

// Get implements agent.Store.
func (s *Store) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	e, err := s.get(ctx, runID, name)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.Entry{}, false, nil
	}
	if err != nil {
		return agent.Entry{}, false, fmt.Errorf("load step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	return e, true, nil
}

// get reads runID's entry named name, or returns sql.ErrNoRows.
func (s *Store) get(ctx context.Context, runID, name string) (agent.Entry, error) {
	return retry(ctx, func() (agent.Entry, error) {
		e := agent.Entry{Name: name}
		err := s.db.QueryRowContext(ctx, string(s.t.get), runID, name).Scan(&e.Seq, &e.Data)
		return e, err
	})
}

// Load implements agent.Store. It reads pageSize entries per query and yields them with no
// connection held, so the caller may write to the store inside its loop.
func (s *Store) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		for {
			page, err := retry(ctx, func() ([]agent.Entry, error) { return s.loadPage(ctx, runID, after) })
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
			page, err := retry(ctx, func() ([]string, error) { return s.runsPage(ctx, f, after) })
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
//
// It is one statement, committed before Postgres replies, so a holder that stops after it has
// sent it holds no lock on the lease's row (see the package documentation).
func (s *Store) AcquireLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	n, err := s.write(ctx, s.t.acquire, runID, holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("acquire lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return n > 0, nil
}

// RenewLease implements agent.Leaser: extend holder's still-live lease on runID. Returns false if
// holder no longer holds it (expired or taken over). It is one statement, like AcquireLease.
func (s *Store) RenewLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	n, err := s.write(ctx, s.t.renew, runID, holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("renew lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return n > 0, nil
}

// ReleaseLease implements agent.Leaser: relinquish runID if held by holder (a no-op otherwise). It
// is one statement, like AcquireLease.
func (s *Store) ReleaseLease(ctx context.Context, runID, holder string) error {
	if _, err := s.write(ctx, s.t.release, runID, holder); err != nil {
		return fmt.Errorf("release lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return nil
}

// write sends one data-changing statement on the pool, where Postgres runs it as a transaction of
// its own and commits it before replying, and returns the number of rows it affected. It never
// begins a transaction: one that spanned round trips would keep the statement's row locks for as
// long as a client stalled between them. A statement that fails with a serialization failure,
// which a deployment whose default isolation is repeatable read or serializable reports where read
// committed would act on the row's latest version, changed nothing and is run again.
func (s *Store) write(ctx context.Context, query writeSQL, args ...any) (int64, error) {
	return retry(ctx, func() (int64, error) {
		res, err := s.db.ExecContext(ctx, string(query), args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	})
}

// retry runs fn, which sends one statement, again after a backoff for as long as it fails with an
// error retryable accepts, until ctx is done (see the package documentation). At serializable,
// Postgres may fail even a lone SELECT with a serialization failure when it conflicts with
// concurrent serializable writes, so reads retry too.
func retry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var b backoff
	for {
		v, err := fn()
		if err == nil || !retryable(err) {
			return v, err
		}
		if werr := b.wait(ctx); werr != nil {
			return v, fmt.Errorf("%w (the last attempt failed: %w)", werr, err)
		}
	}
}

// The backoff between attempts of one statement: before attempt n+1 it waits a random duration
// in [0, min(backoffCap, backoffBase<<(n-1))), so the first retry waits under 1ms and the wait
// doubles up to the cap.
const (
	backoffBase = time.Millisecond
	backoffCap  = 100 * time.Millisecond
)

// backoff counts the failed attempts of one statement and spaces the next.
type backoff struct{ failed int }

// ceiling returns the upper bound of the wait after the failed-th failure.
func (b *backoff) ceiling() time.Duration {
	if b.failed >= 8 { // backoffBase<<7 already exceeds the cap
		return backoffCap
	}
	return min(backoffCap, backoffBase<<(b.failed-1))
}

// wait records a failed attempt and waits before the next one, or returns ctx's error as soon as
// ctx is done.
func (b *backoff) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.failed++
	t := time.NewTimer(rand.N(b.ceiling()))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SQLSTATE codes the store acts on.
const (
	serializationFailure = "40001"
	deadlockDetected     = "40P01"
	uniqueViolation      = "23505"
)

// retryable reports whether err is a failure after which a single statement, run as its own
// transaction, has changed nothing and may be run again: a serialization failure, which a
// conflict with another transaction caused (one that has committed, or at serializable one that
// may not have yet), or a deadlock (which one-row statements do not form, but which Postgres
// resolves by failing a statement that then changed nothing).
func retryable(err error) bool {
	switch sqlState(err) {
	case serializationFailure, deadlockDetected:
		return true
	}
	return false
}

// sqlState returns the SQLSTATE of a Postgres error in err's chain, or "".
func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
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
