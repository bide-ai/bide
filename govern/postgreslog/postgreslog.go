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
// Every name in every statement the log sends is qualified: governed_events and next_seq with the
// log's schema, and every function, type and operator with pg_catalog (operators written
// OPERATOR(pg_catalog.<op>)), so no statement, Open's and the migration's included, looks a name up
// through the search path (the statement check in sqlcheck.go holds them to it). Which schema is
// the log's is the one thing the search path can still decide, and only when the schema is not
// pinned. With WithSchema, the recommended deployment, the search path plays no part. Without it,
// every Open discovers the schema: the first schema on the search path holding a relation named
// governed_events, or the first schema on the path when none does yet (see logSchema); it refuses
// a first relation of that name that is not an ordinary or partitioned table, migrates a legacy
// table in a later schema in place, and logs a warning. Discovery runs again at every Open, so a
// role that can create a schema earlier on the path (any role with CREATE on the database can
// create the "$user" schema the default search path puts first) can redirect a restarting
// process: discovery is safe only when every schema on the search path is trusted. Within one
// process, after Open, no role can redirect the log, pinned or not.
//
// The log trusts the owner of its schema and every role that can create objects in it, as it
// trusts the table's owner: such a role could replace the table. Within that boundary it refuses
// what it can check: the next_seq function must be owned by the table's owner, run with
// search_path = pg_catalog, pg_temp and have exactly this version's body, whose names are all
// qualified. Unpinned, the log also trusts every role that can create a schema earlier on its
// search path (see above). Any role that can connect can hold an
// entity's advisory key (pg_advisory_lock(hashtextextended(entity, 0))) and stall its appends, as
// in earlier versions; it corrupts nothing. After ALTER SCHEMA ... RENAME, drop the function
// (DROP FUNCTION <schema>.governed_events_next_seq_v1(text)) and Open again.
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
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Log is a Postgres-backed append-only event log per entity.
type Log struct {
	db     *sql.DB
	schema string // the schema governed_events and next_seq are in, recorded at Open (see logSchema)
	// The statements, each naming the schema and qualifying every other name, so no statement looks
	// a name up through the search path: a schema that appears earlier on the path after Open (a
	// role's "$user" schema, which any role with CREATE on the database can create) cannot take
	// over the table or next_seq in this process. Which schema an unpinned Open uses is another
	// matter (see logSchema and WithSchema).
	append, byID, since logSQL
}

// logSQL is one statement that passed the statement check (see sqlcheck.go), naming the log's
// schema. The static check in statements_test.go allows a value of this type on the pool. Only
// newLogSQL makes one.
type logSQL string

// newLogSQL returns q as a logSQL, or an error unless q starts with SELECT or INSERT and passes the
// statement check. table is the log's governed_events, schema-qualified: the one relation besides
// pg_catalog's that q may name. nextSeq, when not nil, is the schema-qualified next_seq function,
// which q may call once if it is an INSERT into table with no SELECT.
func newLogSQL(q string, nextSeq, table []sqlTok) (logSQL, error) {
	toks, err := sqlTokens(q)
	if err != nil || len(toks) == 0 || !isTok(toks, 0, 'i', "select") && !isTok(toks, 0, 'i', "insert") {
		return "", fmt.Errorf("postgreslog: a statement on the pool must be one SELECT or INSERT, got %.40q: %w", q, agent.ErrConfig)
	}
	if nextSeq != nil {
		if !isTok(toks, 1, 'i', "into") || len(toks) < 2+len(table)*2-1 {
			return "", fmt.Errorf("postgreslog: next_seq outside an INSERT into governed_events, in %.40q: %w", q, agent.ErrConfig)
		}
		if name, _ := sqlName(toks, 2); !sameName(name, table) {
			return "", fmt.Errorf("postgreslog: next_seq outside an INSERT into governed_events, in %.40q: %w", q, agent.ErrConfig)
		}
	}
	var rels [][]sqlTok
	if table != nil {
		rels = [][]sqlTok{table}
	}
	if err := checkSQL(q, nextSeq, rels); err != nil {
		return "", fmt.Errorf("postgreslog: a statement on the pool holds %v, in %.40q: %w", err, q, agent.ErrConfig)
	}
	return logSQL(q), nil
}

// newLog returns a Log over db in schema, or, when schema is empty, in the schema logSchema finds
// (with a warning), with its statements.
func newLog(ctx context.Context, db *sql.DB, schema string) (*Log, error) {
	if schema == "" {
		var first, current sql.NullString
		var isTable sql.NullBool
		if err := retry(ctx, func() error {
			return db.QueryRowContext(ctx, logSchema).Scan(&first, &isTable, &current)
		}); err != nil {
			return nil, fmt.Errorf("postgreslog: find the schema of governed_events: %w", err)
		}
		schema = current.String
		switch {
		case first.Valid && !isTable.Bool:
			return nil, fmt.Errorf("postgreslog: %s.governed_events, the first relation of that name on the search path, is not a table: %w",
				quoteIdent(first.String), agent.ErrConfig)
		case first.Valid:
			schema = first.String
		case !current.Valid:
			return nil, fmt.Errorf("postgreslog: no schema on the search path to create governed_events in: %w", agent.ErrConfig)
		}
		warnf("govern/postgreslog: the log's schema was found through the search path; pin it with WithSchema",
			"schema", schema, "why", "discovery runs again at every Open, so a role that can create a schema earlier on the search path (any role with CREATE on the database can create the \"$user\" schema) can redirect a restarting process; discovery is safe only when every schema on the search path is trusted")
	}
	table := quoteIdent(schema) + ".governed_events"
	into, err := parseName(table)
	if err != nil {
		return nil, fmt.Errorf("postgreslog: schema %q: %w (%w)", schema, err, agent.ErrConfig)
	}
	nextSeq, err := parseName(quoteIdent(schema) + "." + nextSeqFunction)
	if err != nil {
		return nil, fmt.Errorf("postgreslog: schema %q: %w (%w)", schema, err, agent.ErrConfig)
	}
	l := &Log{db: db, schema: schema}
	if l.append, err = newLogSQL(`INSERT INTO `+table+` (entity, seq, event, append_id)
			VALUES ($1::pg_catalog.text, `+quoteIdent(schema)+`.`+nextSeqFunction+`($1::pg_catalog.text), $2, $3)
			ON CONFLICT (entity, append_id) DO NOTHING
			RETURNING seq`, nextSeq, into); err != nil {
		return nil, err
	}
	if l.byID, err = newLogSQL(`SELECT seq, event FROM `+table+` WHERE entity OPERATOR(pg_catalog.=) $1 AND append_id OPERATOR(pg_catalog.=) $2`, nil, into); err != nil {
		return nil, err
	}
	if l.since, err = newLogSQL(`SELECT seq, event FROM `+table+` WHERE entity OPERATOR(pg_catalog.=) $1 AND seq OPERATOR(pg_catalog.>=) $2 ORDER BY seq`, nil, into); err != nil {
		return nil, err
	}
	return l, nil
}

// Option configures Open.
type Option interface{ apply(*config) error }

type config struct {
	schema string // set by WithSchema; empty: discover it (see logSchema)
}

type optionFunc func(*config) error

func (f optionFunc) apply(c *config) error { return f(c) }

// WithSchema pins the schema governed_events and its next_seq function are in, instead of
// discovering it through the search path at every Open. It is the recommended deployment: with it,
// the search path plays no part in which schema the log uses, and no role can redirect a
// restarting process by creating a schema earlier on the path. The name is used as given (quoted),
// so "App" and "app" are different schemas; the schema must exist. An empty name is an ErrConfig
// error.
func WithSchema(name string) Option {
	return optionFunc(func(c *config) error {
		if name == "" || strings.IndexByte(name, 0) >= 0 {
			return fmt.Errorf("postgreslog: schema name %q is empty or holds a NUL: %w", name, agent.ErrConfig)
		}
		c.schema = name
		return nil
	})
}

// warnf reports the log discovering its schema through the search path. It logs through the
// default slog logger, which an application sets with slog.SetDefault; a test replaces it.
var warnf = func(msg string, args ...any) { slog.Warn(msg, args...) }

// Open connects to Postgres via a pgx DSN (e.g. "postgres://user:pass@host:5432/db") and ensures
// the schema exists. Pin the schema with WithSchema; without it, Open discovers the schema through
// the search path (see logSchema) and logs a warning.
func Open(ctx context.Context, dsn string, opts ...Option) (*Log, error) {
	var cfg config
	for _, o := range opts {
		if o == nil {
			return nil, fmt.Errorf("postgreslog: nil option: %w", agent.ErrConfig)
		}
		if err := o.apply(&cfg); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	l, err := newLog(ctx, db, cfg.schema)
	if err != nil {
		db.Close()
		return nil, err
	}
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

// The catalog queries below read pg_catalog's tables with the statement's snapshot and look names
// up by schema, never through the session's catalog cache (to_regclass, to_regprocedure), which
// taking an advisory lock does not refresh. Every built-in and type is qualified with pg_catalog.

// logSchema returns, for the relation name governed_events: the schema of the first relation of
// that name on the search path, as an unqualified governed_events resolves (NULL when there is
// none); whether that relation is an ordinary or partitioned table; and current_schema(), where
// the migration creates the table when there is none. newLog records the schema, and refuses a
// first relation that is not a table (a view, a foreign table, a sequence): appends would go
// through it while Open checked another table.
const logSchema = `SELECT
	(SELECT n.nspname FROM pg_catalog.pg_class AS c JOIN pg_catalog.pg_namespace AS n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE c.relname OPERATOR(pg_catalog.=) 'governed_events' AND n.nspname OPERATOR(pg_catalog.=) ANY (pg_catalog.current_schemas(false))
		ORDER BY pg_catalog.array_position(pg_catalog.current_schemas(false), n.nspname) LIMIT 1),
	(SELECT c.relkind OPERATOR(pg_catalog.=) 'r' OR c.relkind OPERATOR(pg_catalog.=) 'p' FROM pg_catalog.pg_class AS c JOIN pg_catalog.pg_namespace AS n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE c.relname OPERATOR(pg_catalog.=) 'governed_events' AND n.nspname OPERATOR(pg_catalog.=) ANY (pg_catalog.current_schemas(false))
		ORDER BY pg_catalog.array_position(pg_catalog.current_schemas(false), n.nspname) LIMIT 1),
	pg_catalog.current_schema()`

// nextSeqPresent reports whether a function named $1 taking one text argument exists in schema $2.
const nextSeqPresent = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_proc AS p
	WHERE p.proname OPERATOR(pg_catalog.=) $1 AND proargtypes OPERATOR(pg_catalog.=) '25'::pg_catalog.oidvector
		AND pronamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))`

// nextSeqTemplate is the source of nextSeqFunction, over the governed_events table %s (qualified
// with its schema). It takes the entity's transaction-level advisory lock, which the append that
// calls it holds until it commits, and then reads the entity's last position. A VOLATILE function
// takes a new snapshot for each query it runs, so at read committed the MAX is read after the lock
// is granted: appends to one entity queue on the lock and each takes the next position in its
// first attempt. At repeatable read or serializable the query uses the transaction's snapshot, so
// a queued append may collide on (entity, seq) and is run again; under sustained contention on
// one entity most appends then retry, with a backoff, and throughput falls well below read
// committed's.
//
// Every name in the body is qualified, and the function runs with search_path = pg_catalog,
// pg_temp (nextSeqConfig), so a role that can create objects in a schema on the log's search path
// cannot take over a call with an overload that matches better (a hashtextextended(text,
// integer), say, for an unqualified hashtextextended(e, 0)).
//
// The key, hashtextextended(entity, 0), is the one the log has always used for the entity's lock,
// so processes of earlier versions queue on the same lock. store/postgres takes the same key for
// a run, and it names no schema, so an entity named like a run, or logs in two schemas of one
// database, share a lock: that costs throughput, not correctness, since the (entity, seq) primary
// key keeps positions distinct whatever the lock does.
const nextSeqTemplate = `
BEGIN
	PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(e, 0::pg_catalog.int8));
	RETURN (SELECT COALESCE(pg_catalog.max(seq), -1) + 1 FROM %s WHERE entity = e);
END
`

// nextSeqConfig is the setting nextSeqFunction runs with, as pg_proc.proconfig stores it.
const nextSeqConfig = "search_path=pg_catalog, pg_temp"

// quoteIdent quotes name as a SQL identifier.
func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// nextSeqBody returns the source of nextSeqFunction for the governed_events table in schema.
func nextSeqBody(schema string) string {
	return fmt.Sprintf(nextSeqTemplate, quoteIdent(schema)+".governed_events")
}

// checkSchema checks that governed_events has each uniqueness Append depends on: (entity, seq),
// on which a race for a position fails and is retried, and (entity, append_id), the arbiter of its
// ON CONFLICT; and that nextSeqFunction is the function this version creates. The migration never
// alters an existing table or replaces a function, and it skips a table whose append_id index
// exists by name, so the columns and kind of each index, and the function's definition, are
// checked here.
func (l *Log) checkSchema(ctx context.Context) error {
	schema := l.schema
	for _, cols := range [][]string{{"entity", "seq"}, {"entity", "append_id"}} {
		var ok bool
		if err := l.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_index AS i
			WHERE i.indrelid OPERATOR(pg_catalog.=) (SELECT c.oid FROM pg_catalog.pg_class AS c
					WHERE c.relname OPERATOR(pg_catalog.=) 'governed_events' AND c.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2))
				AND i.indisunique AND i.indisvalid AND i.indimmediate
				AND i.indpred IS NULL AND i.indexprs IS NULL AND i.indnkeyatts OPERATOR(pg_catalog.=) i.indnatts
				AND (SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY a.attname::pg_catalog.text)
					FROM pg_catalog.unnest(i.indkey::pg_catalog.int2[]) AS k JOIN pg_catalog.pg_attribute AS a ON a.attrelid OPERATOR(pg_catalog.=) i.indrelid AND a.attnum OPERATOR(pg_catalog.=) k)
					OPERATOR(pg_catalog.=) (SELECT pg_catalog.array_agg(c ORDER BY c) FROM pg_catalog.unnest($1::pg_catalog.text[]) AS c))`, cols, schema).Scan(&ok); err != nil {
			return fmt.Errorf("postgreslog: check the uniqueness of governed_events: %w", err)
		}
		if !ok {
			return fmt.Errorf("postgreslog: governed_events has no unique index on exactly (%s) that is checked at once and covers every row; Append depends on it and Open never alters an existing index: %w",
				strings.Join(cols, ", "), agent.ErrConfig)
		}
	}
	var same, sameOwner sql.NullBool
	if err := l.db.QueryRowContext(ctx, `SELECT
			l.lanname OPERATOR(pg_catalog.=) 'plpgsql' AND p.provolatile OPERATOR(pg_catalog.=) 'v' AND NOT p.prosecdef AND NOT p.proretset
				AND p.proconfig OPERATOR(pg_catalog.=) $3::pg_catalog.text[] AND p.prorettype OPERATOR(pg_catalog.=) 'pg_catalog.int8'::pg_catalog.regtype::pg_catalog.oid
				AND p.prosrc OPERATOR(pg_catalog.=) $4,
			p.proowner OPERATOR(pg_catalog.=) (SELECT c.relowner FROM pg_catalog.pg_class AS c WHERE c.relname OPERATOR(pg_catalog.=) 'governed_events' AND c.relnamespace OPERATOR(pg_catalog.=) p.pronamespace)
		FROM pg_catalog.pg_proc AS p JOIN pg_catalog.pg_language AS l ON l.oid OPERATOR(pg_catalog.=) p.prolang
		WHERE p.proname OPERATOR(pg_catalog.=) $1 AND p.proargtypes OPERATOR(pg_catalog.=) '25'::pg_catalog.oidvector
			AND p.pronamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $2)`,
		nextSeqFunction, schema, []string{nextSeqConfig}, nextSeqBody(schema)).Scan(&same, &sameOwner); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgreslog: check %s: %w", nextSeqFunction, err)
	}
	if !same.Bool {
		return fmt.Errorf("postgreslog: function %s.%s(text) is not the one this version creates (VOLATILE plpgsql returning bigint, not SECURITY DEFINER, with SET %s and no other setting, whose body takes the entity's advisory lock and then reads MAX(seq), every name qualified); Append depends on it and Open never replaces a function: %w",
			quoteIdent(schema), nextSeqFunction, nextSeqConfig, agent.ErrConfig)
	}
	if !sameOwner.Bool {
		return fmt.Errorf("postgreslog: function %s.%s(text) has another owner than table governed_events: its owner could replace it after Open checked it, and every append would run that role's code; drop it, or have the table's owner create it: %w",
			quoteIdent(schema), nextSeqFunction, agent.ErrConfig)
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
	schema := l.schema
	current, haveNextSeq, err := schemaCurrent(ctx, l.db, schema)
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
	if _, err := tx.ExecContext(ctx, `SELECT pg_catalog.pg_advisory_xact_lock($1::pg_catalog.int8)`, migrateLockKey); err != nil {
		return fmt.Errorf("postgreslog: migrate: take the migration lock: %w", err)
	}
	// The function first, since it takes no lock on the table; a current table needs nothing else.
	// The lookup is the first statement after the lock, so no statement between could refresh the
	// session's catalog cache and hide a stale lookup from the tests.
	// Under the migration lock, the check and the creation are one step across processes.
	// The lookup reads pg_proc with the statement's snapshot, not the session's catalog cache,
	// which taking the advisory lock does not refresh: it sees a function another process created
	// while this one waited for the lock.
	if err := tx.QueryRowContext(ctx, nextSeqPresent, nextSeqFunction, schema).Scan(&haveNextSeq); err != nil {
		return err
	}
	if !haveNextSeq {
		body := nextSeqBody(schema)
		if strings.Contains(body, "$bide$") {
			return fmt.Errorf("postgreslog: schema name %q holds $bide$: %w", schema, agent.ErrConfig)
		}
		if _, err := tx.ExecContext(ctx, `CREATE FUNCTION `+quoteIdent(schema)+`.`+nextSeqFunction+
			`(e pg_catalog.text) RETURNS pg_catalog.int8 LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp AS $bide$`+body+`$bide$`); err != nil {
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
	table := quoteIdent(schema) + ".governed_events"
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+table+` (
			entity    pg_catalog.text NOT NULL,
			seq       pg_catalog.int8 NOT NULL,
			event     pg_catalog.text NOT NULL,
			append_id pg_catalog.text,
			PRIMARY KEY (entity, seq)
		);
		ALTER TABLE `+table+` ADD COLUMN IF NOT EXISTS append_id pg_catalog.text;
		CREATE UNIQUE INDEX IF NOT EXISTS governed_events_append_id ON `+table+` (entity, append_id);`); err != nil {
		return fmt.Errorf("postgreslog: migrate governed_events (another session may hold the table; retry Open): %w", err)
	}
	return tx.Commit()
}

// schemaCurrent reports whether governed_events in schema exists with its unique append_id index
// (the last thing migrate creates, over the append_id column), and whether nextSeqFunction exists
// beside it, reading only the catalog.
func schemaCurrent(ctx context.Context, db *sql.DB, schema string) (current, haveNextSeq bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM pg_catalog.pg_index AS i
				JOIN pg_catalog.pg_class AS t ON t.oid OPERATOR(pg_catalog.=) i.indrelid JOIN pg_catalog.pg_class AS x ON x.oid OPERATOR(pg_catalog.=) i.indexrelid
				WHERE t.relname OPERATOR(pg_catalog.=) 'governed_events' AND x.relname OPERATOR(pg_catalog.=) 'governed_events_append_id'
					AND t.relnamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $1)),
			EXISTS (SELECT 1 FROM pg_catalog.pg_proc
				WHERE proname OPERATOR(pg_catalog.=) $2 AND proargtypes OPERATOR(pg_catalog.=) '25'::pg_catalog.oidvector
					AND pronamespace OPERATOR(pg_catalog.=) (SELECT n.oid FROM pg_catalog.pg_namespace AS n WHERE n.nspname OPERATOR(pg_catalog.=) $1))`,
		schema, nextSeqFunction).Scan(&current, &haveNextSeq)
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
		err := l.db.QueryRowContext(ctx, string(l.append), entity, event, id).Scan(&seq)
		switch {
		case err == nil:
			return seq, nil
		case errors.Is(err, sql.ErrNoRows):
			// The id is recorded, by an append that has committed, so a new snapshot sees it.
			var recorded string
			if err := retry(ctx, func() error {
				return l.db.QueryRowContext(ctx, string(l.byID), entity, id).Scan(&seq, &recorded)
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
	rows, err := l.db.QueryContext(ctx, string(l.since), entity, from)
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
