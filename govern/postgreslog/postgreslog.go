// Package postgreslog is a Postgres-backed govern.EventLog for HIGH-AVAILABILITY governed state:
// multiple processes read the same governed event stream and replay it to the same convergent
// state. gsm guarantees order-independent convergence (WFC + CC), so shared-log replay is consistent
// across nodes regardless of the exact order each observes. It parallels store/postgres for the
// durable journal. It satisfies govern.EventLog structurally, so this package does not import
// govern, keeping the coupling one-directional.
//
// An append is one INSERT statement sent on its own, which Postgres runs as a transaction of its
// own and commits before it replies. Its position comes from the function
// governed_events_next_seq_v1, which takes the entity's transaction-level advisory lock and then
// reads MAX(seq)+1, so appends to one entity queue on the lock instead of racing for a position
// (see Append). No transaction spans two round trips, so a process that stops
// between two of its round trips (a SIGSTOP, a suspended VM, a long GC pause, a partition) holds no
// lock another process's append waits on. Reads are single SELECT statements. Each statement
// behaves the same whatever the deployment's default_transaction_isolation: one that fails with a
// serialization failure (40001), which repeatable read or serializable reports where read
// committed would act on the latest row, changed nothing and is run again. The schema migration is
// the one transaction of several statements; it sets read committed itself (see txOptions).
//
// A retry does not always follow another transaction's commit: at serializable, Postgres may fail
// a statement for a conflict with a transaction that has not committed yet, and many processes
// appending to one entity may each lose their position several times. So the log spaces the
// attempts with a capped, jittered exponential backoff (from 1ms up to 100ms, a random fraction of
// it each time) and keeps retrying until the statement succeeds, fails for another reason, or ctx
// is done: a bound on the attempts would turn contention into errors. A caller that needs a bound
// on the time an append may take sets a deadline on ctx; when ctx ends, the call returns an error
// that wraps both ctx's error and the last attempt's.
package postgreslog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/jackc/pgx/v5/pgconn"
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
	if err := l.checkSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

// nextSeqFunction is the function Append calls for its position. Its name carries the version of
// its definition: the migration creates it when missing and never replaces it, since processes
// of another version may be calling it, so a new definition takes a new name.
const nextSeqFunction = "governed_events_next_seq_v1"

// nextSeqPresent reports whether a function named $1 taking one text argument exists in the schema
// CREATE FUNCTION creates it in, reading pg_proc with the statement's snapshot.
const nextSeqPresent = `SELECT EXISTS (SELECT 1 FROM pg_proc
	WHERE proname = $1 AND pronamespace = current_schema()::regnamespace AND proargtypes = '25'::oidvector)`

// nextSeqBody is the source of nextSeqFunction. It takes the entity's transaction-level advisory
// lock, which the append that calls it holds until it commits, and then reads the entity's last
// position. A VOLATILE function takes a new snapshot for each query it runs, so at read committed
// the MAX is read after the lock is granted: appends to one entity queue on the lock and each
// takes the next position in its first attempt. At repeatable read or serializable the query uses
// the transaction's snapshot, so a queued append may collide on (entity, seq) and is run again.
// The key is the one the log has always used for the entity's lock, so processes of earlier
// versions queue on the same lock.
const nextSeqBody = `
BEGIN
	PERFORM pg_advisory_xact_lock(hashtextextended(e, 0));
	RETURN (SELECT COALESCE(MAX(seq), -1) + 1 FROM governed_events WHERE entity = e);
END
`

// checkSchema checks that governed_events has each uniqueness Append depends on: (entity, seq),
// on which a race for a position fails and is retried, and (entity, append_id), the arbiter of its
// ON CONFLICT; and that nextSeqFunction is the function this version creates. The migration never
// alters an existing table or replaces a function, and it skips a table whose append_id index
// exists by name, so the columns and kind of each index, and the function's definition, are
// checked here.
func (l *Log) checkSchema(ctx context.Context) error {
	for _, cols := range [][]string{{"entity", "seq"}, {"entity", "append_id"}} {
		var ok bool
		if err := l.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_index i
			WHERE i.indrelid = to_regclass('governed_events') AND i.indisunique AND i.indisvalid AND i.indimmediate
				AND i.indpred IS NULL AND i.indexprs IS NULL AND i.indnkeyatts = i.indnatts
				AND (SELECT array_agg(a.attname::text ORDER BY a.attname::text)
					FROM unnest(i.indkey::int2[]) AS k JOIN pg_attribute AS a ON a.attrelid = i.indrelid AND a.attnum = k)
					= (SELECT array_agg(c ORDER BY c) FROM unnest($1::text[]) AS c))`, cols).Scan(&ok); err != nil {
			return fmt.Errorf("postgreslog: check the uniqueness of governed_events: %w", err)
		}
		if !ok {
			return fmt.Errorf("postgreslog: governed_events has no unique index on exactly (%s) that is checked at once and covers every row; Append depends on it and Open never alters an existing index: %w",
				strings.Join(cols, ", "), agent.ErrConfig)
		}
	}
	var same bool
	if err := l.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc AS p JOIN pg_language AS l ON l.oid = p.prolang
		WHERE p.oid = to_regprocedure($1) AND l.lanname = 'plpgsql' AND p.provolatile = 'v'
			AND NOT p.prosecdef AND p.proconfig IS NULL AND NOT p.proretset
			AND p.prorettype = 'bigint'::regtype AND p.prosrc = $2)`, nextSeqFunction+"(text)", nextSeqBody).Scan(&same); err != nil {
		return fmt.Errorf("postgreslog: check %s: %w", nextSeqFunction, err)
	}
	if !same {
		return fmt.Errorf("postgreslog: function %s(text) is not the one this version creates (VOLATILE plpgsql returning bigint, not SECURITY DEFINER, with no settings of its own, whose body takes the entity's advisory lock and then reads MAX(seq)); Append depends on it and Open never replaces a function: %w",
			nextSeqFunction, agent.ErrConfig)
	}
	return nil
}

// txOptions are the options of the one transaction the log begins, the schema migration. The
// isolation level is set explicitly so the migration does not inherit the deployment's
// default_transaction_isolation.
var txOptions = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// migrateLockKey is the advisory lock key that serializes schema migrations of the event log
// across processes (any 64-bit constant private to this package).
const migrateLockKey int64 = 0x62696465_6576_6c67 // "bide" "ev" "lg"

// migrateLockTimeout bounds how long a migration waits for a table lock. ALTER TABLE and CREATE
// INDEX queue for their lock behind every open transaction that has read the table, and while
// they wait, every later read and write of the table queues behind them; so a migration that
// cannot get its lock soon gives up, and Open fails, rather than stall every other process.
const migrateLockTimeout = 3 * time.Second

// migrateIdleTimeout bounds how long the migration's transaction may sit idle between two of its
// statements. A live process sends its next statement within milliseconds.
const migrateIdleTimeout = 5 * time.Second

// migrate creates the governed_events table. Each entity's events carry a dense position (seq),
// unique per entity, assigned by Append under a per-entity lock, and the append id they were
// recorded under, unique per entity. A table created before appends carried ids gains the column;
// its earlier rows have no id and never match a new append.
//
// A process stopped inside the migration (a SIGSTOP, a suspended VM) would hold the migration lock,
// and every process opening the log would wait for as long as the stop lasts, so the transaction
// sets idle_in_transaction_session_timeout (migrateIdleTimeout): Postgres ends the stopped
// process's session instead, which rolls the migration back.
//
// A table that is already current (the common case: every Open after the first) is only read from
// the catalog, so Open takes no lock on it. Otherwise the migration runs in one transaction under
// an advisory lock, so processes opening the log at once migrate it one at a time, and with a lock
// timeout (migrateLockTimeout), so it cannot wedge the table.
func (l *Log) migrate(ctx context.Context) error {
	current, haveNextSeq, err := schemaCurrent(ctx, l.db)
	if err != nil || current && haveNextSeq {
		return err
	}
	tx, err := l.db.BeginTx(ctx, txOptions)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Set before the lock is taken, so the migration never holds it idle without the bound.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL idle_in_transaction_session_timeout = %d`, migrateIdleTimeout.Milliseconds())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("postgreslog: migrate: take the migration lock: %w", err)
	}
	// The function first, since it takes no lock on the table; a current table needs nothing else.
	// Under the migration lock, the check and the creation are one step across processes.
	// The lookup reads pg_proc with the statement's snapshot, not the session's catalog cache,
	// which taking the advisory lock does not refresh: it sees a function another process created
	// while this one waited for the lock.
	if err := tx.QueryRowContext(ctx, nextSeqPresent, nextSeqFunction).Scan(&haveNextSeq); err != nil {
		return err
	}
	if !haveNextSeq {
		if _, err := tx.ExecContext(ctx, `CREATE FUNCTION `+nextSeqFunction+`(e text) RETURNS bigint LANGUAGE plpgsql VOLATILE AS $bide$`+nextSeqBody+`$bide$`); err != nil {
			return fmt.Errorf("postgreslog: migrate: create %s: %w", nextSeqFunction, err)
		}
	}
	if current {
		return tx.Commit()
	}
	// The lock timeout is set after the advisory lock, so a process waiting for another's
	// migration waits for it to finish rather than give up.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = %d`, migrateLockTimeout.Milliseconds())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS governed_events (
			entity    text   NOT NULL,
			seq       bigint NOT NULL,
			event     text   NOT NULL,
			append_id text,
			PRIMARY KEY (entity, seq)
		);
		ALTER TABLE governed_events ADD COLUMN IF NOT EXISTS append_id text;
		CREATE UNIQUE INDEX IF NOT EXISTS governed_events_append_id ON governed_events (entity, append_id);`); err != nil {
		return fmt.Errorf("postgreslog: migrate governed_events (another session may hold the table; retry Open): %w", err)
	}
	return tx.Commit()
}

// schemaCurrent reports whether the governed_events table the search path resolves to exists
// with its unique append_id index (the last thing migrate creates, over the append_id column), and
// whether nextSeqFunction exists, reading only the catalog.
func schemaCurrent(ctx context.Context, db *sql.DB) (current, haveNextSeq bool, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_index
			WHERE indrelid = to_regclass('governed_events') AND indexrelid = to_regclass('governed_events_append_id')),
			to_regprocedure('`+nextSeqFunction+`(text)') IS NOT NULL`).Scan(&current, &haveNextSeq)
	return current, haveNextSeq, err
}

// Close releases the underlying database connection pool.
func (l *Log) Close() error { return l.db.Close() }

// Append durably records an event for an entity under id and returns its position (its seq).
// Concurrent appends to the same entity, from any number of processes, take dense and unique
// positions in commit order, so the entity's log is append-only as every reader sees it. If the
// entity already holds id, nothing is inserted and the recorded position is returned.
//
// The append is one INSERT, committed before Postgres replies, that takes the next position
// (MAX(seq)+1) from nextSeqFunction, which first takes the entity's lock and so queues appends to
// one entity. An append computes position n+1 only when the event at n is committed and visible
// to it, so no reader sees an event before one at a lower position. Should two appends still read
// the same MAX (at repeatable read or serializable, or beside a writer that does not take the
// lock), they collide on the primary key (entity, seq): the later one waits for the first to
// commit, fails with 23505, and runs again with a snapshot that sees it. An append whose id is
// already recorded meets the (entity, append_id) index and does nothing, once the recording
// append has committed.
//
// An append whose commit reply is lost is not retried here: database/sql retries a statement only
// on driver.ErrBadConn, which the driver reports only when nothing was sent on the connection. A
// caller that retries after such an error sends the same id, which returns the recorded position.
func (l *Log) Append(ctx context.Context, entity, id, event string) (int64, error) {
	if id == "" {
		return 0, fmt.Errorf("postgreslog: empty append id: %w", agent.ErrConfig)
	}
	var b backoff
	for {
		var seq int64
		err := l.db.QueryRowContext(ctx, `INSERT INTO governed_events (entity, seq, event, append_id)
			VALUES ($1, `+nextSeqFunction+`($1), $2, $3)
			ON CONFLICT (entity, append_id) DO NOTHING
			RETURNING seq`, entity, event, id).Scan(&seq)
		switch {
		case err == nil:
			return seq, nil
		case errors.Is(err, sql.ErrNoRows):
			// The id is recorded, by an append that has committed, so a new snapshot sees it.
			var recorded string
			if err := retry(ctx, func() error {
				return l.db.QueryRowContext(ctx, `SELECT seq, event FROM governed_events WHERE entity = $1 AND append_id = $2`, entity, id).Scan(&seq, &recorded)
			}); err != nil {
				return 0, err
			}
			if recorded != event {
				return 0, fmt.Errorf("postgreslog: append id %q holds event %q, not %q: %w", id, recorded, event, agent.ErrConfig)
			}
			return seq, nil
		case retryable(err) || sqlState(err) == uniqueViolation:
			// Nothing was written. A unique violation can only be on (entity, seq), since a
			// conflict on the id does nothing: another append took the position after this one's
			// snapshot, and has committed.
			if werr := b.wait(ctx); werr != nil {
				return 0, fmt.Errorf("%w (the last attempt failed: %w)", werr, err)
			}
		default:
			return 0, err
		}
	}
}

// SQLSTATE codes the log acts on.
const (
	serializationFailure = "40001"
	deadlockDetected     = "40P01"
	uniqueViolation      = "23505"
)

// retryable reports whether err is a failure after which a single statement, run as its own
// transaction, has changed nothing and may be run again: a serialization failure, which a
// conflict with another transaction caused (one that has committed, or at serializable one that
// may not have yet), or a deadlock, which Postgres resolves by failing a statement.
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

// retry runs fn, which sends one statement, again after a backoff for as long as it fails with an
// error retryable accepts, until ctx is done (see the package documentation). At serializable,
// Postgres may fail even a lone SELECT with a serialization failure when it conflicts with
// concurrent serializable writes.
func retry(ctx context.Context, fn func() error) error {
	var b backoff
	for {
		err := fn()
		if err == nil || !retryable(err) {
			return err
		}
		if werr := b.wait(ctx); werr != nil {
			return fmt.Errorf("%w (the last attempt failed: %w)", werr, err)
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
	if b.failed > 20 { // well past the cap; a larger shift could overflow
		return backoffCap
	}
	return min(backoffCap, backoffBase<<(b.failed-1))
}

// jitter draws the wait below a ceiling: a uniformly random duration in [0, ceiling), so writers
// contending on one run spread their retries. A test replaces it to make the wait predictable.
var jitter = func(ceiling time.Duration) time.Duration { return rand.N(ceiling) }

// wait records a failed attempt and waits before the next one, or returns ctx's error as soon as
// ctx is done.
func (b *backoff) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.failed++
	t := time.NewTimer(jitter(b.ceiling()))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Events returns an entity's events at positions from onward, in log order. Positions are dense,
// so the i-th event read is the one at position from+i; a row whose seq breaks that (a row deleted
// or renumbered outside this adapter) is an error wrapping agent.ErrProtocol, since the caller
// would otherwise take a later event for the one at the missing position.
func (l *Log) Events(ctx context.Context, entity string, from int64) ([]string, error) {
	from = max(from, 0)
	var out []string
	err := retry(ctx, func() error {
		var err error
		out, err = l.events(ctx, entity, from)
		return err
	})
	return out, err
}

func (l *Log) events(ctx context.Context, entity string, from int64) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT seq, event FROM governed_events WHERE entity = $1 AND seq >= $2 ORDER BY seq`, entity, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var seq int64
		var e string
		if err := rows.Scan(&seq, &e); err != nil {
			return nil, err
		}
		if want := from + int64(len(out)); seq != want {
			return nil, fmt.Errorf("postgreslog: entity %q has an event at position %d where position %d was expected: %w", entity, seq, want, agent.ErrProtocol)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
