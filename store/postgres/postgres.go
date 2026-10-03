// Package postgres is a Postgres-backed agent.Store for high-availability durable agents: shared
// Postgres storage lets any node resume any run, so a dead worker's in-flight agents pick up
// elsewhere. It implements agent.Store, agent.Lister and agent.Leaser.
//
// The tables are named with a prefix, "bide_" by default (see WithTablePrefix): bide_steps holds
// the journal (run_id, seq, name, data), where data is bytea, each entry's bytes verbatim;
// bide_leases holds the leases; bide_schema_version the schema version; and the function
// bide_next_seq_v1 gives an insert its position. Open and New create the tables and the function
// if they do not exist and refuse a database whose schema version is newer than this version
// knows. They never alter an existing table or replace a function, so they refuse a table that
// lacks a uniqueness the store's statements depend on (unique (run_id, seq) and (run_id, name) on
// bide_steps, and run_id on bide_leases) and a next_seq function with another definition. Run IDs
// are compared and ordered by their bytes (collation "C"), so a Lister's order does not depend on
// the database's locale.
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
// lease being live); and Insert is an INSERT whose position comes from next_seq.
//
// # Inserts into one run
//
// next_seq takes the run's transaction-level advisory lock and then reads MAX(seq)+1. The lock is
// held by the insert's own transaction, the one statement, and released when it commits, so a
// stalled client holds it no longer than any statement's row locks. Inserts into one run queue on
// it; the function is VOLATILE, so its read takes a snapshot after the lock is granted and, at read
// committed, sees every insert that held the lock before. Each insert therefore takes its position
// in its first attempt, and positions follow commit order. Should two inserts still read the same
// MAX (at repeatable read or serializable, where the function reads the transaction's snapshot,
// or beside a writer that does not take the lock), they collide on the (run_id, seq) key: the
// second waits for the first to commit, fails, and is run again. The lock's key is the one earlier
// versions took in a statement of their own, so nodes of both versions queue on the same lock
// during a rolling upgrade.
//
// The key is hashtextextended(run_id, 0), which names neither the table prefix nor the schema, and
// govern/postgreslog takes the same key for an entity. So stores with different prefixes, stores
// in different schemas of one database, and a governed entity named like a run share one lock.
// That costs throughput, not correctness: the unique (run_id, seq) index keeps each store's
// positions distinct whatever the lock does. The sharing stays, since the upgrade depends on the
// key.
//
// At repeatable read or serializable, a queued insert's snapshot was taken before it was granted
// the lock, so under sustained contention on one run most inserts collide once or more and retry
// with a backoff (see Retries and deadlines): positions stay correct, but throughput on that run
// falls well below read committed's, and a caller without a deadline may wait long. A deployment
// that writes one run from many goroutines at once is better served at read committed.
//
// # Schema and trust
//
// Every name in every statement the store sends is qualified: its tables and next_seq function
// with the store's schema, and every function, type, collation and operator with pg_catalog
// (operators written OPERATOR(pg_catalog.<op>)). No statement, Open's and the migration's
// included, looks a name up through the search path. Postgres still resolves some things without
// a name, none of them through the search path: ORDER BY, DISTINCT and the ON CONFLICT arbiter use
// the type's default operator class and the table's indexes, and casts use pg_cast. The statement
// check (sqlcheck.go) holds every statement to this.
//
// Which schema is the store's is the one thing the search path can still decide, and only when the
// schema is not pinned. With WithSchema, the recommended deployment, the store uses the schema
// given and the search path plays no part. Without it, every Open discovers the schema: the first
// schema on the search path holding a relation named like the steps table, or the first schema on
// the path (current_schema()), where the migration creates the tables, when none does; it refuses
// a first relation of that name that is not an ordinary or partitioned table, and logs a warning.
// Discovery runs again at every Open, so a role that can create a schema earlier on the path can
// redirect a restarting node to a store of its own: any role with CREATE on the database can
// create the "$user" schema the default search path puts first. Discovery is safe only when every
// schema on the search path is trusted. Within one process, after Open, no role can redirect the
// store, pinned or not, as it could in every earlier version.
//
// The store trusts the owner of its schema and every role that can create objects in it, as it
// trusts the owner of its tables: such a role could replace a table, and so it could replace the
// next_seq function. Within that boundary, the store refuses what it can check. The next_seq
// function must be owned by the owner of the steps table, run with search_path = pg_catalog,
// pg_temp and have exactly this version's body, whose names are all qualified; Open refuses it
// otherwise. Unpinned, the store also trusts every role that can create a schema earlier on its
// search path (see above).
//
// Any role that can connect can take a run's advisory lock key itself
// (pg_advisory_lock(hashtextextended(run_id, 0))) and hold it, and every insert into that run then
// waits until it lets go. That was so before next_seq too, since earlier versions took the same
// key; it stalls a run's writes, and corrupts nothing.
//
// The next_seq function's body names its schema. After ALTER SCHEMA ... RENAME, every insert fails
// and Open refuses the function: drop it (DROP FUNCTION <schema>.bide_next_seq_v1(text), with the
// prefix) and Open again, and the migration creates it under the new name.
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
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
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
	db     *sql.DB
	own    bool // Close closes db: Open opened it
	t      tables
	schema string // the schema the tables and next_seq are in, recorded at Open (see storeSchema)

}

var (
	_ agent.Store  = (*Store)(nil)
	_ agent.Lister = (*Store)(nil)
	_ agent.Leaser = (*Store)(nil)
)

// tables holds the table names, prefixed, their schema-qualified forms, and the statements built
// from them. Every statement names the schema Open recorded and qualifies every other name, so no
// statement looks a name up through the search path: a schema that appears earlier on the path
// after Open (a role's "$user" schema, which any role with CREATE on the database can create)
// cannot take over a table or the next_seq function in this process. Which schema an unpinned Open
// uses is another matter (see storeSchema and WithSchema).
type tables struct {
	steps, leases, version              string     // bare names, as in the catalog
	nextSeq                             string     // the next_seq function's bare name
	qSteps, qLeases, qVersion, qNextSeq string     // schema-qualified, as SQL
	rels                                [][]sqlTok // the qualified tables, as the statement check names them
	insert, acquire, renew, release     writeSQL
	reap                                writeSQL
	get, load                           selectSQL
}

// nextSeqVersion names the next_seq function's definition. The migration creates the function
// when it is missing and never replaces one, since nodes of another version may be calling it; a
// change to the definition takes a new version, so a new function beside the old one.
const nextSeqVersion = "v1"

// nextSeqTemplate is the body of the next_seq function, over the steps table %s (qualified with its
// schema). It takes the run's transaction-level advisory lock, which the insert that calls it
// holds until it commits, and then reads the run's last position. A VOLATILE function takes a new
// snapshot for each query it runs, so at read committed the MAX is read after the lock is granted
// and sees every insert into the run that held the lock before: inserts into one run queue on the
// lock and each takes the next position in its first attempt. At repeatable read or serializable
// the query uses the transaction's snapshot, taken before the lock was granted, so a queued insert
// may collide on (run_id, seq) and is run again, as without the function.
//
// Every name in the body is qualified, every operator written OPERATOR(pg_catalog.<op>) (the
// unary minus of -1 included), and the function also runs with search_path = pg_catalog, pg_temp
// (nextSeqConfig), a second layer: a role that can create objects in a schema on the store's
// search path must not be able to take over a call with an overload that matches better (a
// hashtextextended(text, integer), say, for an unqualified hashtextextended(r, 0)).
//
// The key, hashtextextended(run_id, 0), is the one the store has always used for the run's lock,
// so nodes of earlier versions queue on the same lock during a rolling upgrade. It does not name
// the table prefix or schema, and govern/postgreslog uses the same key for an entity, so stores
// with different prefixes, stores in different schemas of one database, and a governed entity
// with the same name as a run all queue on one lock. That costs throughput, not correctness: the
// unique (run_id, seq) index keeps each store's positions distinct whatever the lock does.
const nextSeqTemplate = `
BEGIN
	PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(r, 0::pg_catalog.int8));
	RETURN (SELECT COALESCE(pg_catalog.max(seq), OPERATOR(pg_catalog.-) 1) OPERATOR(pg_catalog.+) 1
		FROM %s WHERE run_id OPERATOR(pg_catalog.=) r);
END
`

// nextSeqConfig is the setting the next_seq function runs with, as pg_proc.proconfig stores it.
const nextSeqConfig = "search_path=pg_catalog, pg_temp"

// quoteIdent quotes name as a SQL identifier.
func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// nextSeqBody returns the next_seq function's source for the steps table in schema.
func (t tables) nextSeqBody(schema string) string {
	return fmt.Sprintf(nextSeqTemplate, quoteIdent(schema)+"."+t.steps)
}

func newTables(prefix, schema string) (tables, error) {
	t := tables{steps: prefix + "steps", leases: prefix + "leases", version: prefix + "schema_version",
		nextSeq: prefix + "next_seq_" + nextSeqVersion}
	q := quoteIdent(schema) + "."
	t.qSteps, t.qLeases, t.qVersion, t.qNextSeq = q+t.steps, q+t.leases, q+t.version, q+t.nextSeq
	var names [4][]sqlTok
	for i, n := range []string{t.qNextSeq, t.qSteps, t.qLeases, t.qVersion} {
		var err error
		if names[i], err = parseName(n); err != nil {
			return tables{}, fmt.Errorf("postgres: schema %q: %w (%w)", schema, err, agent.ErrConfig)
		}
	}
	nextSeq, steps, leases := names[0], [][]sqlTok{names[1]}, [][]sqlTok{names[2]}
	t.rels = [][]sqlTok{names[1], names[2], names[3]}
	var err error
	// Every name is qualified: tables with the recorded schema, built-ins, types and the collation
	// with pg_catalog, and every operator written OPERATOR(pg_catalog.<op>), so that no name in a
	// statement resolves through the search path (see sqlcheck.go).
	// The position comes from next_seq, which queues the insert on the run's lock (see
	// nextSeqTemplate). Should two inserts still read the same MAX (at repeatable read or
	// serializable, or beside a writer that does not take the lock), they collide on UNIQUE
	// (run_id, seq), which is not the conflict target, so the second fails with 23505 once the
	// first commits and insert runs it again (see insert).
	if t.insert, err = newWrite(`INSERT INTO `+t.qSteps+` (run_id, seq, name, data)
		VALUES ($1::pg_catalog.text, `+t.qNextSeq+`($1::pg_catalog.text), $2, $3)
		ON CONFLICT (run_id, name) DO NOTHING
		RETURNING seq`, nextSeq, steps); err != nil {
		return tables{}, err
	}
	// Granted when the run is unleased, when the live lease is already holder's, or when the lease
	// has expired; a row another holder holds live is left alone and nothing is returned.
	if t.acquire, err = newWrite(`INSERT INTO `+t.qLeases+` AS l (run_id, holder, expiry)
		VALUES ($1, $2, pg_catalog.now() OPERATOR(pg_catalog.+) ($3::pg_catalog.float8 OPERATOR(pg_catalog.*) '1 second'::pg_catalog.interval))
		ON CONFLICT (run_id) DO UPDATE
			SET holder = EXCLUDED.holder, expiry = EXCLUDED.expiry
			WHERE l.holder OPERATOR(pg_catalog.=) EXCLUDED.holder OR l.expiry OPERATOR(pg_catalog.<) pg_catalog.now()`, nil, leases); err != nil {
		return tables{}, err
	}
	if t.renew, err = newWrite(`UPDATE `+t.qLeases+` SET expiry = pg_catalog.now() OPERATOR(pg_catalog.+) ($3::pg_catalog.float8 OPERATOR(pg_catalog.*) '1 second'::pg_catalog.interval)
		WHERE run_id OPERATOR(pg_catalog.=) $1 AND holder OPERATOR(pg_catalog.=) $2 AND expiry OPERATOR(pg_catalog.>=) pg_catalog.now()`, nil, leases); err != nil {
		return tables{}, err
	}
	if t.release, err = newWrite(`DELETE FROM `+t.qLeases+` WHERE run_id OPERATOR(pg_catalog.=) $1 AND holder OPERATOR(pg_catalog.=) $2`, nil, leases); err != nil {
		return tables{}, err
	}
	// The lapsed leases no recovery pass takes over: a finished run's, one on a run the steps table
	// does not hold, or one on a run whose ID contains '>' (a session's or a sub-agent's run). The expiry is checked by the DELETE itself, which at read committed
	// re-evaluates it on a row a concurrent acquisition or renewal updated, so a lease taken or
	// renewed meanwhile is kept (at repeatable read or serializable the conflict fails the
	// statement and write runs it again).
	if t.reap, err = newWrite(`DELETE FROM `+t.qLeases+` AS l WHERE l.expiry OPERATOR(pg_catalog.<) pg_catalog.now()
		AND (pg_catalog.strpos(l.run_id, '>') OPERATOR(pg_catalog.>) 0
			OR NOT EXISTS (SELECT 1 FROM `+t.qSteps+` AS y WHERE y.run_id OPERATOR(pg_catalog.=) l.run_id)
			OR EXISTS (SELECT 1 FROM `+t.qSteps+` AS x WHERE x.run_id OPERATOR(pg_catalog.=) l.run_id AND x.name OPERATOR(pg_catalog.=) ANY ($1::pg_catalog.text[])))`, nil, [][]sqlTok{names[1], names[2]}); err != nil {
		return tables{}, err
	}
	if t.get, err = newSelect(`SELECT seq, data FROM `+t.qSteps+` WHERE run_id OPERATOR(pg_catalog.=) $1 AND name OPERATOR(pg_catalog.=) $2`, steps); err != nil {
		return tables{}, err
	}
	if t.load, err = newSelect(`SELECT seq, name, data FROM `+t.qSteps+` WHERE run_id OPERATOR(pg_catalog.=) $1 AND seq OPERATOR(pg_catalog.>) $2 ORDER BY seq LIMIT $3`, steps); err != nil {
		return tables{}, err
	}
	return t, nil
}

// selectSQL is one SELECT statement that passed the statement check (see sqlcheck.go). The static
// check in statements_test.go allows a value of this type on the pool. Only newSelect makes one.
type selectSQL string

// writeSQL is one INSERT, UPDATE or DELETE statement that passed the statement check, which the
// store sends on the pool on its own, so Postgres runs it as a transaction of its own. The static
// check allows a value of this type on the pool. Only newWrite makes one.
type writeSQL string

// newWrite returns q as a writeSQL, or an error unless q starts with INSERT, UPDATE or DELETE and
// passes the statement check. nextSeq, when not nil, is the schema-qualified next_seq function,
// which q may call once if it is an INSERT with no SELECT; rels are the relations, besides
// pg_catalog's, q may name.
func newWrite(q string, nextSeq []sqlTok, rels [][]sqlTok) (writeSQL, error) {
	if !startsWith(q, "INSERT") && !startsWith(q, "UPDATE") && !startsWith(q, "DELETE") {
		return "", fmt.Errorf("postgres: a write on the pool must be one INSERT, UPDATE or DELETE statement, got %.40q: %w", q, agent.ErrConfig)
	}
	if err := checkSQL(q, nextSeq, rels); err != nil {
		return "", fmt.Errorf("postgres: a write on the pool holds %v, in %.40q: %w", err, q, agent.ErrConfig)
	}
	return writeSQL(q), nil
}

// newSelect returns q as a selectSQL, or an error unless q starts with SELECT and passes the
// statement check; rels are the relations, besides pg_catalog's, q may name.
func newSelect(q string, rels [][]sqlTok) (selectSQL, error) {
	if !startsWith(q, "SELECT") {
		return "", fmt.Errorf("postgres: a read on the pool must be one SELECT statement, got %.40q: %w", q, agent.ErrConfig)
	}
	if err := checkSQL(q, nil, rels); err != nil {
		return "", fmt.Errorf("postgres: a read on the pool holds %v, in %.40q: %w", err, q, agent.ErrConfig)
	}
	return selectSQL(q), nil
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

type config struct {
	prefix string
	schema string // set by WithSchema; empty: discover it (see storeSchema)
}

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

// WithSchema pins the schema the store's tables and next_seq function are in, instead of
// discovering it through the search path at every Open. It is the recommended deployment: with it,
// the search path plays no part in which schema the store uses, and no role can redirect a
// restarting node by creating a schema earlier on the path. The name is used as given (quoted),
// so "App" and "app" are different schemas, and Postgres truncates it, like any identifier, to 63
// bytes. The schema must exist: Open fails with ErrConfig otherwise, and never creates it. An empty
// name, and a system schema (information_schema, or a name starting with pg_), is an ErrConfig
// error.
func WithSchema(name string) Option {
	return optionFunc(func(c *config) error {
		if name == "" || strings.IndexByte(name, 0) >= 0 {
			return fmt.Errorf("postgres: schema name %q is empty or holds a NUL: %w", name, agent.ErrConfig)
		}
		if systemSchema(name) {
			return fmt.Errorf("postgres: schema %q is a system schema (information_schema, or a name starting with pg_, pg_temp among them): %w", name, agent.ErrConfig)
		}
		c.schema = name
		return nil
	})
}

// systemSchema reports whether name is information_schema or starts with pg_, the prefix Postgres
// reserves for its own schemas (pg_catalog, pg_toast, and pg_temp, which names each session's own
// temporary schema, so a store there would lose its journal with each pooled connection).
func systemSchema(name string) bool {
	return name == "information_schema" || strings.HasPrefix(name, "pg_")
}

// schemaExists reports whether the schema $1 exists. A pinned schema must: Open never creates one.
const schemaExists = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $1)`

// warnf reports the store discovering its schema through the search path. It logs through the
// default slog logger, which an application sets with slog.SetDefault; a test replaces it.
var warnf = func(msg string, args ...any) { slog.Warn(msg, args...) }

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
	schema := cfg.schema
	if schema != "" {
		var exists bool
		if err := retryScan(ctx, func() error { return db.QueryRowContext(ctx, schemaExists, schema).Scan(&exists) }); err != nil {
			return nil, fmt.Errorf("postgres: look up schema %q: %w (%w)", schema, err, agent.ErrStorage)
		}
		if !exists {
			return nil, fmt.Errorf("postgres: the pinned schema %q does not exist; create it, Open does not: %w", schema, agent.ErrConfig)
		}
	}
	if schema == "" {
		if schema, err = storeSchema(ctx, db, cfg.prefix+"steps"); err != nil {
			return nil, err
		}
		warnf("store/postgres: the store's schema was found through the search path; pin it with WithSchema",
			"schema", schema, "why", "discovery runs again at every Open, so a role that can create a schema earlier on the search path (any role with CREATE on the database can create the \"$user\" schema) can redirect a restarting node; discovery is safe only when every schema on the search path is trusted")
	}
	t, err := newTables(cfg.prefix, schema)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, t: t, schema: schema}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	if err := s.checkSchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// storeSchema returns the schema the store's tables are in, which Open records and every statement
// names from then on: the first schema on the search path holding a relation named steps (the
// steps table, as an unqualified name would resolve), or the first schema on the path, where the
// migration creates the tables, when none does. It refuses, with ErrConfig, a first relation of
// that name that is not an ordinary or partitioned table (a view, a foreign table, a sequence):
// the store would read and write through it while checking another table.
func storeSchema(ctx context.Context, db *sql.DB, steps string) (string, error) {
	var first, current sql.NullString
	var isTable sql.NullBool
	err := retryScan(ctx, func() error {
		return db.QueryRowContext(ctx, firstRelation, steps).Scan(&first, &isTable, &current)
	})
	if err != nil {
		return "", fmt.Errorf("postgres: find the schema of %s: %w (%w)", steps, err, agent.ErrStorage)
	}
	if first.Valid {
		if !isTable.Bool {
			return "", fmt.Errorf("postgres: %s.%s, the first relation of that name on the search path, is not a table: %w",
				quoteIdent(first.String), steps, agent.ErrConfig)
		}
		return first.String, nil
	}
	if !current.Valid {
		return "", fmt.Errorf("postgres: no schema on the search path to create the tables in: %w", agent.ErrConfig)
	}
	return current.String, nil
}

// retryScan runs fn, one statement, again after a backoff while it fails with an error retryable
// accepts (see retry).
func retryScan(ctx context.Context, fn func() error) error {
	_, err := retry(ctx, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

// firstRelation returns, for the relation name $1: the schema of the first relation of that name
// on the search path (NULL when there is none); whether that relation is an ordinary or
// partitioned table; and current_schema(), where the migration creates the tables when there is
// none.
const firstRelation = `SELECT
	(SELECT n.nspname FROM pg_catalog.pg_class AS c JOIN pg_catalog.pg_namespace AS n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE c.relname OPERATOR(pg_catalog.=) $1 AND n.nspname OPERATOR(pg_catalog.=) ANY (pg_catalog.current_schemas(false))
		ORDER BY pg_catalog.array_position(pg_catalog.current_schemas(false), n.nspname) LIMIT 1),
	(SELECT c.relkind OPERATOR(pg_catalog.=) 'r' OR c.relkind OPERATOR(pg_catalog.=) 'p'
		FROM pg_catalog.pg_class AS c JOIN pg_catalog.pg_namespace AS n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE c.relname OPERATOR(pg_catalog.=) $1 AND n.nspname OPERATOR(pg_catalog.=) ANY (pg_catalog.current_schemas(false))
		ORDER BY pg_catalog.array_position(pg_catalog.current_schemas(false), n.nspname) LIMIT 1),
	pg_catalog.current_schema()`

// migrateLock is the advisory lock key that serializes schema creation across nodes opening the
// store at once ("bide" in ASCII).
const migrateLock = 0x62696465

// migrate creates the tables if they do not exist and checks the schema version. It never alters
// an existing table: altering a table other nodes are running on would take its exclusive lock and
// change what they write. So Open then checks that an existing table has each uniqueness the
// store's single statements depend on, and the next_seq function its definition (see
// checkSchema), and refuses one that does not.
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
	if _, err := tx.ExecContext(ctx, `SELECT pg_catalog.pg_advisory_xact_lock($1::pg_catalog.int8)`, migrateLock); err != nil {
		return fmt.Errorf("postgres: migrate: %w (%w)", err, agent.ErrStorage)
	}
	// The next_seq function, created if missing and never replaced (see nextSeqVersion); the first
	// statement after the lock, so no statement between could refresh the session's catalog cache
	// and hide a stale lookup from the tests. The migration lock makes
	// the check and the creation one step across nodes. The lookup reads pg_proc with the
	// statement's snapshot, not the session's catalog cache, which taking the advisory lock does not
	// refresh: it sees a function another node created while this one waited for the lock.
	var haveNextSeq bool
	if err := tx.QueryRowContext(ctx, nextSeqPresent, s.t.nextSeq, s.schema).Scan(&haveNextSeq); err != nil {
		return fmt.Errorf("postgres: look up %s: %w (%w)", s.t.nextSeq, err, agent.ErrStorage)
	}
	if !haveNextSeq {
		body := s.t.nextSeqBody(s.schema)
		if strings.Contains(body, "$bide$") {
			return fmt.Errorf("postgres: schema name %q holds $bide$: %w", s.schema, agent.ErrConfig)
		}
		if _, err := tx.ExecContext(ctx, `CREATE FUNCTION `+s.t.qNextSeq+
			`(r pg_catalog.text) RETURNS pg_catalog.int8 LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp AS $bide$`+body+`$bide$`); err != nil {
			return fmt.Errorf("postgres: create %s: %w (%w)", s.t.nextSeq, err, agent.ErrStorage)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %[1]s (
			run_id pg_catalog.text COLLATE pg_catalog."C" NOT NULL,
			seq    pg_catalog.int8  NOT NULL,
			name   pg_catalog.text  NOT NULL,
			data   pg_catalog.bytea NOT NULL,
			PRIMARY KEY (run_id, name),
			UNIQUE (run_id, seq)
		);
		CREATE TABLE IF NOT EXISTS %[2]s (
			run_id pg_catalog.text        PRIMARY KEY,
			holder pg_catalog.text        NOT NULL,
			expiry pg_catalog.timestamptz NOT NULL
		);
		CREATE TABLE IF NOT EXISTS %[3]s (
			id      pg_catalog.int4 PRIMARY KEY CHECK (id OPERATOR(pg_catalog.=) 1),
			version pg_catalog.int4 NOT NULL
		);
		INSERT INTO %[3]s (id, version) VALUES (1, %[4]d) ON CONFLICT (id) DO NOTHING;`,
		s.t.qSteps, s.t.qLeases, s.t.qVersion, schemaVersion)); err != nil {
		return fmt.Errorf("postgres: create tables: %w (%w)", err, agent.ErrStorage)
	}
	// The lapsed listing's indexes (see runsPage): expiry finds the few lapsed leases among many
	// live ones, and run_id under "C", the listing's order, pages through many lapsed leases
	// without sorting them. They are created when missing, on a table of v0.9.0 too: building one
	// holds the leases table's SHARE lock (lease writes wait) for as long as it takes to index a
	// table that holds only live and lapsed leases. Creating an index takes the table's owner, so
	// a store role that does not own the tables needs the owner to open the store once.
	for _, ix := range []struct{ name, cols string }{
		{s.t.leases + "_expiry", `(expiry)`},
		{s.t.leases + "_run_c", `((run_id COLLATE pg_catalog."C"))`},
	} {
		var have bool
		if err := tx.QueryRowContext(ctx, indexPresent, ix.name, s.schema).Scan(&have); err != nil {
			return fmt.Errorf("postgres: look up index %s: %w (%w)", ix.name, err, agent.ErrStorage)
		}
		if have {
			continue
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS `+quoteIdent(ix.name)+` ON `+s.t.qLeases+` `+ix.cols); err != nil {
			return fmt.Errorf("postgres: create index %s on %s (the tables' owner must open the store once to create it): %w (%w)", ix.name, s.t.leases, err, agent.ErrStorage)
		}
	}
	var v int
	if err := tx.QueryRowContext(ctx, versionQuery(s.t.qVersion)).Scan(&v); err != nil {
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

// checkSchema checks, after the migration committed, that the tables have each uniqueness the
// store's statements depend on and that next_seq is the function this version creates; the
// migration never alters a table or replaces a function, so it cannot repair either. It runs in
// transactions of its own on the pool, which read the catalog as of their start, so it sees what
// another node's migration created while this one waited for the migration lock.
func (s *Store) checkSchema(ctx context.Context) error {
	for _, table := range []string{s.t.steps, s.t.leases, s.t.version} {
		var isTable sql.NullBool
		if err := s.db.QueryRowContext(ctx, relationIsTable, table, s.schema).Scan(&isTable); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("postgres: check %s: %w (%w)", table, err, agent.ErrStorage)
		}
		if !isTable.Bool {
			return fmt.Errorf("postgres: %s.%s is not an ordinary or partitioned table: %w", quoteIdent(s.schema), table, agent.ErrConfig)
		}
	}
	for _, u := range []struct {
		table string
		cols  []string
	}{
		{s.t.steps, []string{"run_id", "seq"}},
		{s.t.steps, []string{"run_id", "name"}},
		{s.t.leases, []string{"run_id"}},
	} {
		var ok bool
		if err := s.db.QueryRowContext(ctx, requiredUnique, u.table, s.schema, u.cols).Scan(&ok); err != nil {
			return fmt.Errorf("postgres: check the uniqueness of %s: %w (%w)", u.table, err, agent.ErrStorage)
		}
		if !ok {
			return fmt.Errorf("postgres: table %s has no unique index on exactly (%s) that is checked at once and covers every row; the store depends on it (it turns a race for a position into a retry, or arbitrates ON CONFLICT) and never alters an existing table: %w",
				u.table, strings.Join(u.cols, ", "), agent.ErrConfig)
		}
	}
	var sameNextSeq, sameOwner sql.NullBool
	if err := s.db.QueryRowContext(ctx, expectedFunction, s.t.nextSeq, s.schema, []string{nextSeqConfig}, s.t.nextSeqBody(s.schema), s.t.steps).Scan(&sameNextSeq, &sameOwner); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres: check %s: %w (%w)", s.t.nextSeq, err, agent.ErrStorage)
	}
	if !sameNextSeq.Bool {
		return fmt.Errorf("postgres: function %s.%s(text) is not the one this version creates (VOLATILE plpgsql returning bigint, not SECURITY DEFINER, with SET %s and no other setting, whose body takes the run's advisory lock and then reads MAX(seq), every name qualified); the store's inserts depend on it and the migration never replaces a function: %w",
			quoteIdent(s.schema), s.t.nextSeq, nextSeqConfig, agent.ErrConfig)
	}
	if !sameOwner.Bool {
		return fmt.Errorf("postgres: function %s.%s(text) has another owner than table %s: its owner could replace it after Open checked it, and every insert would run that role's code; drop it, or have the tables' owner create it: %w",
			quoteIdent(s.schema), s.t.nextSeq, s.t.steps, agent.ErrConfig)
	}
	return nil
}

// The catalog queries below read pg_catalog's tables with the statement's snapshot and look names
// up by schema, never through the session's catalog cache (to_regclass, to_regprocedure), which
// taking an advisory lock does not refresh. Every built-in and type is qualified with pg_catalog.

// requiredUnique reports whether the table $1 in schema $2 has a valid unique index whose key is
// exactly the column set $3, with no included columns, no expressions and no predicate, checked
// when each statement runs rather than deferred to commit. The store's inserts and lease upsert
// rely on each such index: a race for a position fails on it and is retried, and ON CONFLICT needs
// it as its arbiter.
const requiredUnique = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_index AS i
	WHERE i.indrelid OPERATOR(pg_catalog.=) (SELECT c.oid FROM pg_catalog.pg_class AS c
			WHERE c.relname OPERATOR(pg_catalog.=) $1
				AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))
		AND i.indisunique AND i.indisvalid AND i.indimmediate
		AND i.indpred IS NULL AND i.indexprs IS NULL AND i.indnkeyatts OPERATOR(pg_catalog.=) i.indnatts
		AND (SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY a.attname::pg_catalog.text)
			FROM pg_catalog.unnest(i.indkey::pg_catalog.int2[]) AS k
				JOIN pg_catalog.pg_attribute AS a ON a.attrelid OPERATOR(pg_catalog.=) i.indrelid AND a.attnum OPERATOR(pg_catalog.=) k)
			OPERATOR(pg_catalog.=) (SELECT pg_catalog.array_agg(c ORDER BY c) FROM pg_catalog.unnest($3::pg_catalog.text[]) AS c))`

// relationIsTable returns whether the relation $1 in schema $2 is an ordinary or partitioned
// table, and no row when there is no such relation.
const relationIsTable = `SELECT c.relkind OPERATOR(pg_catalog.=) 'r' OR c.relkind OPERATOR(pg_catalog.=) 'p'
	FROM pg_catalog.pg_class AS c
	WHERE c.relname OPERATOR(pg_catalog.=) $1
		AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2)`

// indexPresent reports whether a relation named $1 (an index, or anything else holding the name)
// exists in schema $2.
const indexPresent = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class AS c
	WHERE c.relname OPERATOR(pg_catalog.=) $1
		AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))`

// nextSeqPresent reports whether a function named $1 taking one text argument exists in schema $2.
const nextSeqPresent = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_proc AS p
	WHERE p.proname OPERATOR(pg_catalog.=) $1 AND p.proargtypes OPERATOR(pg_catalog.=) '25'::pg_catalog.oidvector
		AND p.pronamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))`

// expectedFunction returns, for the function $1(text) in schema $2, whether it is a VOLATILE
// plpgsql function returning one bigint, not SECURITY DEFINER, whose only setting is $3 and whose
// source is $4; and whether its owner is the owner of table $5 in the same schema. Each is NULL
// when there is no such function.
const expectedFunction = `SELECT
		l.lanname OPERATOR(pg_catalog.=) 'plpgsql' AND p.provolatile OPERATOR(pg_catalog.=) 'v' AND NOT p.prosecdef AND NOT p.proretset
			AND p.proconfig OPERATOR(pg_catalog.=) $3::pg_catalog.text[]
			AND p.prorettype OPERATOR(pg_catalog.=) 'pg_catalog.int8'::pg_catalog.regtype::pg_catalog.oid
			AND p.prosrc OPERATOR(pg_catalog.=) $4,
		p.proowner OPERATOR(pg_catalog.=) (SELECT c.relowner FROM pg_catalog.pg_class AS c
			WHERE c.relname OPERATOR(pg_catalog.=) $5 AND c.relnamespace OPERATOR(pg_catalog.=) p.pronamespace)
	FROM pg_catalog.pg_proc AS p JOIN pg_catalog.pg_language AS l ON l.oid OPERATOR(pg_catalog.=) p.prolang
	WHERE p.proname OPERATOR(pg_catalog.=) $1 AND p.proargtypes OPERATOR(pg_catalog.=) '25'::pg_catalog.oidvector
		AND p.pronamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2)`

// versionQuery reads the schema version from the version table qVersion.
func versionQuery(qVersion string) string {
	return `SELECT version FROM ` + qVersion + ` WHERE id OPERATOR(pg_catalog.=) 1`
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
	q := `SELECT DISTINCT run_id COLLATE pg_catalog."C" AS id FROM ` + s.t.qSteps + ` AS s WHERE run_id COLLATE pg_catalog."C" OPERATOR(pg_catalog.>) $1::pg_catalog.text`
	if f.LeaseLapsed {
		// The runs with a lapsed lease, read from the leases table, which holds a row only for a
		// lease taken and not released: the same comparison as AcquireLease's, and only runs the
		// steps table holds.
		q = `SELECT run_id COLLATE pg_catalog."C" AS id FROM ` + s.t.qLeases + ` AS s WHERE expiry OPERATOR(pg_catalog.<) pg_catalog.now()
			AND EXISTS (SELECT 1 FROM ` + s.t.qSteps + ` AS y WHERE y.run_id OPERATOR(pg_catalog.=) s.run_id)
			AND run_id COLLATE pg_catalog."C" OPERATOR(pg_catalog.>) $1::pg_catalog.text`
	}
	args := []any{after}
	if f.Prefix != "" {
		// The range lets the primary key's index serve the scan; starts_with checks the prefix.
		q += fmt.Sprintf(` AND run_id COLLATE pg_catalog."C" OPERATOR(pg_catalog.>=) $%d::pg_catalog.text AND pg_catalog.starts_with(run_id, $%d::pg_catalog.text)`, len(args)+1, len(args)+1)
		args = append(args, f.Prefix)
		if end, ok := prefixEnd(f.Prefix); ok {
			q += fmt.Sprintf(` AND run_id COLLATE pg_catalog."C" OPERATOR(pg_catalog.<) $%d::pg_catalog.text`, len(args)+1)
			args = append(args, end)
		}
	}
	if len(f.ExcludeHolding) > 0 {
		q += fmt.Sprintf(` AND NOT EXISTS (SELECT 1 FROM %s AS x WHERE x.run_id OPERATOR(pg_catalog.=) s.run_id AND x.name OPERATOR(pg_catalog.=) ANY ($%d::pg_catalog.text[]))`, s.t.qSteps, len(args)+1)
		args = append(args, f.ExcludeHolding)
	}
	q += fmt.Sprintf(` ORDER BY id LIMIT %d`, runsPage)
	sel, err := newSelect(q, s.t.rels)
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

// ReapLeases implements agent.Leaser: one DELETE of the lapsed leases whose run is finished (holds
// an entry named in ended), not in the steps table, or a session's or a sub-agent's (its ID
// contains '>'), which checks the expiry itself, so a lease taken or renewed meanwhile is kept.
func (s *Store) ReapLeases(ctx context.Context, ended []string) (int, error) {
	if ended == nil {
		ended = []string{}
	}
	n, err := s.write(ctx, s.t.reap, ended)
	if err != nil {
		return 0, fmt.Errorf("reap leases: %w (%w)", err, agent.ErrStorage)
	}
	return int(n), nil
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
