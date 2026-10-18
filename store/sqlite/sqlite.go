// Package sqlite is the default on-disk agent.Store: a single-file, zero-cgo SQLite journal (via
// modernc.org/sqlite). An agent run journaled here survives a process crash and resumes without
// re-running completed steps. No cluster, no daemon: one binary and one file.
//
// Store implements agent.Store, agent.Lister (so agent.Recover finds the runs to re-drive after a
// restart) and agent.Leaser (so processes sharing the file drive each run one at a time). Open
// opens three connection pools on the file: a writer of one connection for every Insert and Get,
// readers for Load and Runs, and a lease connection of its own, whose statements use a short busy
// timeout so a writer holding the file's lock never delays a lease renewal past its cutoff.
//
// The tables are named with a prefix, "bide_" by default (see WithTablePrefix): bide_steps holds
// the journal, bide_leases the leases, bide_events the durable event trail (see EventStore), and
// bide_schema_version the schema version. Open refuses a
// file whose schema version is newer than this version knows. The file must be on a local disk:
// SQLite's locking does not work over NFS. A forward jump of the system clock expires leases
// early, which the journal's attempt claims keep safe.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/dbpath"
	"github.com/bide-ai/bide/internal/filegate"
	_ "modernc.org/sqlite"
)

// schemaVersion is the version of the tables this package creates and reads.
const schemaVersion = 1

// pageSize is how many entries a Load reads per query, and runsPage how many run IDs a Runs call
// reads per query. No connection is held between pages.
const (
	pageSize = 256
	runsPage = 500
)

// writeBusyTimeout is how long an Insert waits for another process's write lock. SQLite's busy
// handler is not fair, so under a burst of writers on one file (or a slow disk) a single writer can
// wait well past a few seconds; failing then would lose a write whose side effect already ran.
const writeBusyTimeout = 30 * time.Second

// Store is a SQLite-backed agent.Store.
type Store struct {
	w, r, l *sql.DB        // writer (one connection), readers, leases (one connection)
	own     bool           // Close closes the pools: Open opened them
	gate    *filegate.Gate // queues this process's writers on the file
	t       tables
}

var (
	_ agent.Store     = (*Store)(nil)
	_ agent.Redactor  = (*Store)(nil)
	_ agent.Compactor = (*Store)(nil)
	_ agent.Holder    = (*Store)(nil)
	_ agent.Lister    = (*Store)(nil)
	_ agent.Leaser    = (*Store)(nil)
)

// tables holds the table names, prefixed, and the statements built from them.
type tables struct {
	steps, leases, version, events    string
	holds                             string
	insert, get, load, redact         string
	held                              string
	parentLive                        string
	appendEvent, getEvent, loadEvents string
}

// Option configures Open and New.
type Option interface{ apply(*config) error }

type config struct{ prefix string }

type optionFunc func(*config) error

func (f optionFunc) apply(c *config) error { return f(c) }

var prefixPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// WithTablePrefix names the tables with prefix instead of "bide_": prefix+"steps",
// prefix+"leases", prefix+"events" and prefix+"schema_version". It must be an SQL identifier (letters, digits and
// underscores, not starting with a digit), or Open and New return an ErrConfig error.
func WithTablePrefix(prefix string) Option {
	return optionFunc(func(c *config) error {
		if !prefixPattern.MatchString(prefix) {
			return fmt.Errorf("sqlite: table prefix %q is not an SQL identifier: %w", prefix, agent.ErrConfig)
		}
		c.prefix = prefix
		return nil
	})
}

func newConfig(opts []Option) (config, error) {
	c := config{prefix: "bide_"}
	for _, o := range opts {
		if o == nil {
			return c, fmt.Errorf("sqlite: nil option: %w", agent.ErrConfig)
		}
		if err := o.apply(&c); err != nil {
			return c, err
		}
	}
	return c, nil
}

// Open opens (creating if needed) a SQLite journal at path, in WAL mode, with its three connection
// pools. ":memory:" opens an ephemeral in-memory store for tests, on one connection shared by all
// three roles (an in-memory database lives on one connection).
//
// Open reaches the file by its canonical path (absolute, symlinks resolved), so that every handle on
// it, whatever path it was given, makes SQLite derive the same -wal and -shm names; two names for one
// WAL-mode database corrupt it (https://sqlite.org/howtocorrupt.html). A hard link has no canonical
// name, so a file with more than one hard link is refused with an error wrapping agent.ErrConfig.
func Open(path string, opts ...Option) (*Store, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	busy := fmt.Sprintf("_pragma=busy_timeout(%d)", writeBusyTimeout.Milliseconds())
	if path == ":memory:" || strings.Contains(path, "mode=memory") {
		db, err := sql.Open("sqlite", withQuery(path, busy))
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		db.SetConnMaxIdleTime(0)
		s := &Store{w: db, r: db, l: db, own: true, t: cfg.tables(), gate: filegate.For(":memory:")}
		if err := s.migrate(ctx); err != nil {
			db.Close()
			return nil, err
		}
		return s, nil
	}
	// Every handle on a file must reach it by one name, or SQLite's -wal and -shm names differ
	// and two handles corrupt the WAL (sqlite.org/howtocorrupt.html).
	path, err = dbpath.Canonical(path, true)
	if err != nil {
		return nil, fmt.Errorf("sqlite: Open: %w (%w)", err, agent.ErrConfig)
	}
	w, err := sql.Open("sqlite", withQuery(path, busy+"&_pragma=journal_mode(WAL)"))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1) // one writer: an Insert's MAX(seq)+1 never races another in this process
	s := &Store{w: w, own: true, t: cfg.tables(), gate: filegate.For(path)}
	if err := s.migrate(ctx); err != nil { // creates the file in WAL mode before the other pools open it
		w.Close()
		s.gate.Close()
		return nil, fmt.Errorf("sqlite: Open %s: %w%s", path, err, openHint(err))
	}
	if s.r, err = sql.Open("sqlite", withQuery(path, busy+"&_pragma=query_only(1)")); err != nil {
		w.Close()
		s.gate.Close()
		return nil, err
	}
	s.r.SetMaxOpenConns(8)
	if s.l, err = sql.Open("sqlite", withQuery(path, busy)); err != nil {
		w.Close()
		s.gate.Close()
		s.r.Close()
		return nil, err
	}
	s.l.SetMaxOpenConns(1)
	return s, nil
}

// openHint says what to check when the file at a path will not open as a journal: the two ways a
// newcomer meets it are a directory that does not exist (or is not writable) and a file that is
// not a SQLite database at all. It is empty for any other error.
func openHint(err error) string {
	switch msg := err.Error(); {
	case strings.Contains(msg, "unable to open database file"):
		return "; check that the directory exists and is writable, and that the path is a file, not a directory"
	case strings.Contains(msg, "file is not a database"):
		return "; the file is not a SQLite database, so give a path to a journal bide created, or a new file name"
	}
	return ""
}

// withQuery appends the DSN query parameters q to path.
func withQuery(path, q string) string {
	if strings.Contains(path, "?") {
		return path + "&" + q
	}
	return path + "?" + q
}

// New returns a Store over db, which the caller opened and closes. db must be in WAL mode and
// allow at least two open connections (a Load's page and a write inside its loop may each need
// one); New returns an ErrConfig error otherwise. Its connections should set a busy timeout of
// several seconds (modernc.org/sqlite: _pragma=busy_timeout(30000) in the DSN), since a write that
// gives up on another process's lock fails a step whose side effect may have run. All three roles
// share db, so a lease statement can wait behind the caller's other work; Open avoids that.
func New(ctx context.Context, db *sql.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("sqlite: New: nil db: %w", agent.ErrConfig)
	}
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	if n := db.Stats().MaxOpenConnections; n == 1 {
		return nil, fmt.Errorf("sqlite: New: db allows %d open connection, want at least 2: %w", n, agent.ErrConfig)
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return nil, fmt.Errorf("sqlite: New: read journal mode: %w (%w)", err, agent.ErrStorage)
	}
	if !strings.EqualFold(mode, "wal") {
		return nil, fmt.Errorf("sqlite: New: db is in journal mode %q, want WAL: %w", mode, agent.ErrConfig)
	}
	file := mainFile(ctx, db)
	if file != "" {
		if err := dbpath.CheckLinks(file); err != nil {
			return nil, fmt.Errorf("sqlite: New: %w (%w)", err, agent.ErrConfig)
		}
	}
	s := &Store{w: db, r: db, l: db, t: cfg.tables(), gate: filegate.For(file)}
	if err := s.migrate(ctx); err != nil {
		s.gate.Close()
		return nil, err
	}
	return s, nil
}

// mainFile is the path of db's main database file, or "" for an in-memory or temporary database or
// when it cannot be read; the writers of a Store over such a db do not queue.
func mainFile(ctx context.Context, db *sql.DB) string {
	var seq int
	var name, file string
	if err := db.QueryRowContext(ctx, `SELECT seq, name, file FROM pragma_database_list WHERE name = 'main'`).Scan(&seq, &name, &file); err != nil {
		return ""
	}
	return file
}

func (c config) tables() tables {
	t := tables{steps: c.prefix + "steps", leases: c.prefix + "leases", version: c.prefix + "schema_version", events: c.prefix + "events", holds: c.prefix + "holds"}
	t.insert = `INSERT INTO ` + t.steps + ` (run_id, name, data) VALUES (?, ?, ?) ON CONFLICT (run_id, name) DO NOTHING`
	// The replacement is one statement, so it is atomic: the row's bytes are the caller's, the run
	// holds an end marker, and no lease on it is live, all read in the same write transaction.
	t.redact = `UPDATE ` + t.steps + ` SET data = ?3 WHERE run_id = ?1 AND name = ?2 AND data = ?4
		AND EXISTS (SELECT 1 FROM ` + t.steps + ` AS e WHERE e.run_id = ?1 AND e.name IN (SELECT value FROM json_each(?5)))
		AND NOT EXISTS (SELECT 1 FROM ` + t.leases + ` AS l WHERE l.run_id = ?1 AND l.expiry >= unixepoch('subsec'))
		AND NOT EXISTS (` + parentLiveSQL(t.steps) + `)
		AND NOT EXISTS (` + heldSQL(t.holds) + `)`
	// A run above a sub-agent's run (?6, a JSON array of run IDs) that holds records and no end
	// marker (?5) is still being driven, and replays the sub-run's records.
	t.parentLive = parentLiveSQL(t.steps)
	t.held = heldSQL(t.holds)
	t.get = `SELECT seq, data FROM ` + t.steps + ` WHERE run_id = ? AND name = ?`
	t.load = `SELECT seq, name, data FROM ` + t.steps + ` WHERE run_id = ? AND seq > ? ORDER BY seq LIMIT ?`
	t.appendEvent = `INSERT INTO ` + t.events + ` (run_id, seq, leaf)
		SELECT ?1, ?2, ?3 WHERE ?2 = (SELECT COALESCE(MAX(seq) + 1, 0) FROM ` + t.events + ` WHERE run_id = ?1)
		ON CONFLICT (run_id, seq) DO NOTHING`
	t.getEvent = `SELECT leaf FROM ` + t.events + ` WHERE run_id = ? AND seq = ?`
	t.loadEvents = `SELECT seq, leaf FROM ` + t.events + ` WHERE run_id = ? AND seq >= ? ORDER BY seq LIMIT ?`
	return t
}

// heldSQL selects a hold that covers the run ?1: one that names it, or, with tree set, one that names a
// run above it (?6, the JSON array of the run's ancestors).
func heldSQL(holds string) string {
	return `SELECT 1 FROM ` + holds + ` AS h WHERE h.run_id = ?1 OR (h.tree = 1 AND h.run_id IN (SELECT value FROM json_each(?6)))`
}

// parentLiveSQL selects a run among the JSON array ?6 that holds an entry and no end marker (the
// JSON array ?5).
func parentLiveSQL(steps string) string {
	return `SELECT 1 FROM ` + steps + ` AS p WHERE p.run_id IN (SELECT value FROM json_each(?6))
			AND NOT EXISTS (SELECT 1 FROM ` + steps + ` AS m WHERE m.run_id = p.run_id AND m.name IN (SELECT value FROM json_each(?5)))`
}

// legacyTable is the journal table of v0.8.0 and earlier, whose journals this version does not
// read (they have no journal format header).
const legacyTable = "steps"

// migrate refuses a file that holds journals of v0.8.0 or earlier, creates the tables if they do not exist, and
// checks the schema version.
func (s *Store) migrate(ctx context.Context) error {
	if err := s.refuseLegacy(ctx); err != nil {
		return err
	}
	if _, err := s.w.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %[1]s (
			seq    INTEGER PRIMARY KEY,
			run_id TEXT    NOT NULL,
			name   TEXT    NOT NULL,
			data   BLOB    NOT NULL,
			UNIQUE (run_id, name)
		);
		CREATE INDEX IF NOT EXISTS %[1]s_run_seq ON %[1]s (run_id, seq);
		CREATE TABLE IF NOT EXISTS %[2]s (
			run_id TEXT PRIMARY KEY,
			holder TEXT NOT NULL,
			expiry REAL NOT NULL
		);
		CREATE TABLE IF NOT EXISTS %[3]s (
			id      INTEGER PRIMARY KEY CHECK (id = 1),
			version INTEGER NOT NULL
		);
		INSERT INTO %[3]s (id, version) VALUES (1, %[4]d) ON CONFLICT (id) DO NOTHING;
		CREATE TABLE IF NOT EXISTS %[5]s (
			run_id TEXT    NOT NULL,
			seq    INTEGER NOT NULL CHECK (seq >= 0),
			leaf   BLOB    NOT NULL,
			PRIMARY KEY (run_id, seq)
		) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS %[6]s (
			hold_id      TEXT    NOT NULL,
			run_id       TEXT    NOT NULL,
			tree         INTEGER NOT NULL CHECK (tree IN (0, 1)),
			matter       TEXT    NOT NULL,
			placed_at_ms INTEGER NOT NULL,
			PRIMARY KEY (hold_id, run_id)
		) WITHOUT ROWID;`,
		s.t.steps, s.t.leases, s.t.version, schemaVersion, s.t.events, s.t.holds)); err != nil {
		return fmt.Errorf("sqlite: create tables: %w (%w)", err, agent.ErrStorage)
	}
	var v int
	if err := s.w.QueryRowContext(ctx, `SELECT version FROM `+s.t.version+` WHERE id = 1`).Scan(&v); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w (%w)", err, agent.ErrStorage)
	}
	if v > schemaVersion {
		return fmt.Errorf("sqlite: the database's schema version is %d, newer than this version's %d: %w", v, schemaVersion, agent.ErrConfig)
	}
	return nil
}

// refuseLegacy refuses a file whose v0.8.0-or-earlier journal table holds rows. Opened as empty, such a file
// would lose its runs: a finished run re-invoked would run again from the start, and fire its side
// effects a second time. An empty legacy table holds nothing to lose.
func (s *Store) refuseLegacy(ctx context.Context) error {
	// A table of that name with the journal's four columns; any other table is not ours to judge.
	var cols int
	err := s.w.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name IN ('run_id', 'seq', 'name', 'data')`, legacyTable).Scan(&cols)
	if err != nil {
		return fmt.Errorf("sqlite: look for a v0.8.0-or-earlier journal table: %w (%w)", err, agent.ErrStorage)
	}
	if cols != 4 {
		return nil
	}
	var rows int
	if err := s.w.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM `+legacyTable+` LIMIT 1)`).Scan(&rows); err != nil {
		return fmt.Errorf("sqlite: read the v0.8.0-or-earlier journal table: %w (%w)", err, agent.ErrStorage)
	}
	if rows == 0 {
		return nil
	}
	return fmt.Errorf("sqlite: the file holds journals written by bide v0.8.0 or earlier (table %q), which have no journal format header and which this version does not read; finish their runs with that version, or drop the table, before opening the file with this one: %w", legacyTable, agent.ErrJournalVersion)
}

// Close closes the connection pools Open opened. A Store made with New leaves its db open.
func (s *Store) Close() error {
	s.gate.Close()
	if !s.own {
		return nil
	}
	errs := []error{s.w.Close()}
	if s.r != s.w {
		errs = append(errs, s.r.Close())
	}
	if s.l != s.w {
		errs = append(errs, s.l.Close())
	}
	return errors.Join(errs...)
}

// Insert implements agent.Store. An entry's Seq is its row id, which SQLite assigns as one more than
// the table's highest when the row is inserted, under the database's single write lock: so it
// increases in commit order, within a run and across the table. The entry it returns for a new row
// holds data itself (SQLite copied it).
func (s *Store) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	res, err := s.write(ctx, s.t.insert, runID, name, data)
	if err != nil {
		return agent.Entry{}, false, fmt.Errorf("insert step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	if n, err := res.RowsAffected(); err != nil {
		return agent.Entry{}, false, fmt.Errorf("insert step %q: %w (%w)", name, err, agent.ErrStorage)
	} else if n == 1 {
		seq, err := res.LastInsertId()
		if err != nil {
			return agent.Entry{}, false, fmt.Errorf("insert step %q: %w (%w)", name, err, agent.ErrStorage)
		}
		return agent.Entry{Seq: seq, Name: name, Data: data}, true, nil
	}
	// Another writer stored the name first: its entry is the name's. Read it on the writer, which
	// has seen that commit.
	e, ok, err := s.get(ctx, s.w, runID, name)
	if err != nil {
		return agent.Entry{}, false, err
	}
	if !ok {
		return agent.Entry{}, false, fmt.Errorf("insert step %q: the insert conflicted but no entry exists: %w", name, agent.ErrStorage)
	}
	return e, false, nil
}

// Redact implements agent.Redactor with one UPDATE that replaces the entry only if it holds current,
// the run holds an end marker and no lease on the run is live (see agent.Redactor). The entry
// keeps its row id, so its Seq and every other entry are unchanged. When the update changes
// nothing, the store reads why (the row, the end marker, the lease) to say which refusal it was; a
// row that already holds the tombstone is not one.
func (s *Store) Redact(ctx context.Context, runID, name string, current, tombstone []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := agent.CheckRedaction(runID, name, current, tombstone); err != nil {
		return false, err
	}
	markers, err := json.Marshal(agent.RedactionEndMarkers())
	if err != nil {
		return false, fmt.Errorf("redact step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	ancestors, err := json.Marshal(append([]string{}, agent.RedactionAncestors(runID)...))
	if err != nil {
		return false, fmt.Errorf("redact step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	res, err := s.writeSecure(ctx, s.t.redact, runID, name, tombstone, current, string(markers), string(ancestors))
	if err != nil {
		return false, fmt.Errorf("redact step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("redact step %q: %w (%w)", name, err, agent.ErrStorage)
	}
	if n == 1 {
		return true, nil
	}
	st := agent.RedactState{}
	if st.Row, st.Found, err = s.get(ctx, s.w, runID, name); err != nil {
		return false, err
	}
	if st.Found {
		for _, m := range agent.RedactionEndMarkers() {
			if _, ok, err := s.get(ctx, s.w, runID, m); err != nil {
				return false, err
			} else if ok {
				st.Ended = true
			}
		}
		if err := s.w.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.leases+` WHERE run_id = ? AND expiry >= unixepoch('subsec'))`, runID).Scan(&st.Leased); err != nil {
			return false, fmt.Errorf("redact step %q: read the lease: %w (%w)", name, err, agent.ErrStorage)
		}
		if err := s.w.QueryRowContext(ctx, `SELECT EXISTS (`+s.t.parentLive+`)`, runID, name, nil, nil, string(markers), string(ancestors)).Scan(&st.ParentLive); err != nil {
			return false, fmt.Errorf("redact step %q: read the parent runs: %w (%w)", name, err, agent.ErrStorage)
		}
		if err := s.w.QueryRowContext(ctx, `SELECT EXISTS (`+s.t.held+`)`, runID, nil, nil, nil, nil, string(ancestors)).Scan(&st.Held); err != nil {
			return false, fmt.Errorf("redact step %q: read the legal holds: %w (%w)", name, err, agent.ErrStorage)
		}
	}
	return false, st.Explain(runID, name, current, tombstone)
}

// writeSecure runs one write statement with secure_delete on, on the writer's connection, so the
// bytes the statement replaces are overwritten with zeros in the page rather than left in it. The
// pragma is switched back off afterwards: it makes every write slower, and only an erasure needs it.
func (s *Store) writeSecure(ctx context.Context, query string, args ...any) (sql.Result, error) {
	leave, err := s.gate.Enter(ctx)
	if err != nil {
		return nil, err
	}
	defer leave()
	conn, err := s.w.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return nil, fmt.Errorf("secure_delete: %w", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA secure_delete = OFF`)
	return conn.ExecContext(ctx, query, args...)
}

// compactBusyTimeout is how long Compact waits for a reader before it reports the checkpoint busy.
const compactBusyTimeout = 2 * time.Second

// Compact implements agent.Compactor: it checkpoints the write-ahead log and truncates it
// (PRAGMA wal_checkpoint(TRUNCATE)), which copies the pages an erasure rewrote (their old bytes
// zeroed, see Redact) into the database file and empties the log that held the old pages. A reader
// that holds the database open can keep the checkpoint from finishing; the engine then reports it
// busy, and so does the result (Complete false), and the old bytes may remain in the log or the
// file until the readers are closed and Compact runs again. It does not VACUUM: free pages from
// earlier deletions are not its concern, and a VACUUM rewrites the whole file.
func (s *Store) Compact(ctx context.Context) (agent.CompactionReport, error) {
	leave, err := s.gate.Enter(ctx)
	if err != nil {
		return agent.CompactionReport{}, err
	}
	defer leave()
	conn, err := s.w.Conn(ctx)
	if err != nil {
		return agent.CompactionReport{}, err
	}
	defer conn.Close()
	// A reader keeps the checkpoint waiting; do not wait the writers' long timeout for it.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, compactBusyTimeout.Milliseconds())); err != nil {
		return agent.CompactionReport{}, fmt.Errorf("busy_timeout: %w (%w)", err, agent.ErrStorage)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA busy_timeout = %d`, writeBusyTimeout.Milliseconds()))
	var busy, logFrames, checkpointed int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return agent.CompactionReport{}, fmt.Errorf("wal checkpoint: %w (%w)", err, agent.ErrStorage)
	}
	detail := fmt.Sprintf("wal_checkpoint(TRUNCATE): busy=%d, log frames=%d, checkpointed=%d", busy, logFrames, checkpointed)
	if busy != 0 {
		return agent.CompactionReport{Detail: detail + ": a reader holds the database open, so the log was not truncated and the old bytes may remain; close the readers and run the compaction again"}, nil
	}
	return agent.CompactionReport{Complete: true, Detail: detail}, nil
}

// write runs one write statement on the writer connection when it is this process's turn at the
// file: SQLite's busy handler is not a queue, so without the turns a burst of writers (or a slow
// disk) can starve one of them past the busy timeout.
func (s *Store) write(ctx context.Context, query string, args ...any) (sql.Result, error) {
	leave, err := s.gate.Enter(ctx)
	if err != nil {
		return nil, err
	}
	defer leave()
	return s.w.ExecContext(ctx, query, args...)
}

// Get implements agent.Store. It reads on the writer's connection: the journal reads a step just
// before it writes one, and a point read on another connection of the same file costs about as
// much again as the write, which the reader pool is for bulk reads (Load, Runs) to avoid.
func (s *Store) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	return s.get(ctx, s.w, runID, name)
}

func (s *Store) get(ctx context.Context, db *sql.DB, runID, name string) (agent.Entry, bool, error) {
	e := agent.Entry{Name: name}
	err := db.QueryRowContext(ctx, s.t.get, runID, name).Scan(&e.Seq, &e.Data)
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
	rows, err := s.r.QueryContext(ctx, s.t.load, runID, after, pageSize)
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
	q := `SELECT DISTINCT run_id FROM ` + s.t.steps + ` AS s WHERE run_id > ?`
	if f.LeaseLapsed {
		// The runs with a lapsed lease, read from the leases table, which holds a row only for a
		// lease taken and not released: the same comparison as AcquireLease's, and only runs the
		// steps table holds.
		q = `SELECT run_id FROM ` + s.t.leases + ` AS s WHERE expiry < unixepoch('subsec')
			AND EXISTS (SELECT 1 FROM ` + s.t.steps + ` AS y WHERE y.run_id = s.run_id) AND run_id > ?`
	}
	args := []any{after}
	if f.Prefix != "" {
		// The range lets the primary key's index serve the scan; substr checks the bytes.
		q += ` AND run_id >= ? AND substr(CAST(run_id AS BLOB), 1, ?) = CAST(? AS BLOB)`
		args = append(args, f.Prefix, len(f.Prefix), f.Prefix)
		if end, ok := prefixEnd(f.Prefix); ok {
			q += ` AND run_id < ?`
			args = append(args, end)
		}
	}
	if len(f.ExcludeHolding) > 0 {
		q += ` AND NOT EXISTS (SELECT 1 FROM ` + s.t.steps + ` AS x WHERE x.run_id = s.run_id AND x.name IN (?` + strings.Repeat(`, ?`, len(f.ExcludeHolding)-1) + `))`
		for _, n := range f.ExcludeHolding {
			args = append(args, n)
		}
	}
	q += ` ORDER BY run_id LIMIT ?`
	args = append(args, runsPage)
	rows, err := s.r.QueryContext(ctx, q, args...)
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

// prefixEnd returns the least string greater than every string that starts with p, and false when
// there is none (p is all 0xff bytes).
func prefixEnd(p string) (string, bool) {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

// leaseBusyTimeout is how long a lease statement waits for the file's write lock: an eighth of the
// lease TTL, at most 2s. A renewal starts at half the TTL and is abandoned at three quarters, so a
// statement that waits this long leaves room for retries before the cutoff (see agent.Lease), and
// a writer that holds the lock for seconds (a long step, another process) does not cost the lease.
func leaseBusyTimeout(ttl time.Duration) time.Duration {
	return max(min(ttl/8, 2*time.Second), time.Millisecond)
}

// leaseExec runs one lease statement on the lease connection under the short busy timeout, and
// reports the rows it changed. Expiries are computed in SQL from the database's clock
// (unixepoch('subsec')), so every process sharing the file compares against one clock.
func (s *Store) leaseExec(ctx context.Context, busy time.Duration, query string, args ...any) (int64, error) {
	// A lease statement has a deadline the journal writes do not: it goes ahead of the writers
	// queued on the file, behind only the one inside.
	leave, err := s.gate.EnterFirst(ctx)
	if err != nil {
		return 0, err
	}
	defer leave()
	conn, err := s.l.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, busy.Milliseconds())); err != nil {
		return 0, err
	}
	// The connection may be shared with writers (New, or an in-memory store): give it back with
	// the writers' busy timeout.
	defer conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA busy_timeout = %d`, writeBusyTimeout.Milliseconds()))
	res, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AcquireLease implements agent.Leaser with one upsert: it grants the lease when the run is
// unleased, already held by holder (a renewal), or the current lease has expired.
//
// The TTL must exceed the disk's worst commit latency: a lease statement is a committed write, which
// runs ahead of this process's queued journal writes but cannot finish faster than the disk commits,
// so a commit that stalls for longer than the TTL lets the lease lapse (agent.ErrLeaseLost).
func (s *Store) AcquireLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	n, err := s.leaseExec(ctx, leaseBusyTimeout(ttl), `
		INSERT INTO `+s.t.leases+` (run_id, holder, expiry) VALUES (?, ?, unixepoch('subsec') + ?)
		ON CONFLICT (run_id) DO UPDATE SET holder = excluded.holder, expiry = excluded.expiry
		WHERE `+s.t.leases+`.holder = excluded.holder OR `+s.t.leases+`.expiry < unixepoch('subsec')`,
		runID, holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("acquire lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return n > 0, nil
}

// RenewLease implements agent.Leaser: it extends holder's still-live lease on runID.
func (s *Store) RenewLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	n, err := s.leaseExec(ctx, leaseBusyTimeout(ttl), `
		UPDATE `+s.t.leases+` SET expiry = unixepoch('subsec') + ?
		WHERE run_id = ? AND holder = ? AND expiry >= unixepoch('subsec')`,
		ttl.Seconds(), runID, holder)
	if err != nil {
		return false, fmt.Errorf("renew lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return n > 0, nil
}

// ReleaseLease implements agent.Leaser: it deletes runID's lease if holder holds it.
func (s *Store) ReleaseLease(ctx context.Context, runID, holder string) error {
	if _, err := s.leaseExec(ctx, 2*time.Second, `DELETE FROM `+s.t.leases+` WHERE run_id = ? AND holder = ?`, runID, holder); err != nil {
		return fmt.Errorf("release lease %q: %w (%w)", runID, err, agent.ErrStorage)
	}
	return nil
}

// ReapLeases implements agent.Leaser with one DELETE, which checks the expiry in the same
// statement, so a lease taken or renewed meanwhile is kept. The lapsed leases it deletes are those
// no recovery pass takes over: a finished run's, one on a run the steps table does not hold, and
// one on a run whose ID contains '>' (a session's or a sub-agent's run).
func (s *Store) ReapLeases(ctx context.Context, ended []string) (int, error) {
	q := `DELETE FROM ` + s.t.leases + ` WHERE expiry < unixepoch('subsec')
		AND (instr(run_id, '>') > 0 OR NOT EXISTS (SELECT 1 FROM ` + s.t.steps + ` AS y WHERE y.run_id = ` + s.t.leases + `.run_id)`
	args := make([]any, 0, len(ended))
	if len(ended) > 0 {
		q += ` OR EXISTS (SELECT 1 FROM ` + s.t.steps + ` AS x WHERE x.run_id = ` + s.t.leases + `.run_id AND x.name IN (?` + strings.Repeat(`, ?`, len(ended)-1) + `))`
		for _, n := range ended {
			args = append(args, n)
		}
	}
	n, err := s.leaseExec(ctx, 2*time.Second, q+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("reap leases: %w (%w)", err, agent.ErrStorage)
	}
	return int(n), nil
}

// PlaceHold implements agent.Holder: one statement, so atomic, that inserts a row for each run the hold
// names; a hold whose ID is active already violates the key and is refused.
func (s *Store) PlaceHold(ctx context.Context, h agent.Hold) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := agent.CheckHold(h); err != nil {
		return err
	}
	runs, err := json.Marshal(h.Runs)
	if err != nil {
		return fmt.Errorf("place hold %q: %w (%w)", h.ID, err, agent.ErrStorage)
	}
	tree := 0
	if h.Tree {
		tree = 1
	}
	res, err := s.write(ctx, `INSERT INTO `+s.t.holds+` (hold_id, run_id, tree, matter, placed_at_ms)
		SELECT DISTINCT ?1, value, ?3, ?4, CAST(unixepoch('subsec') * 1000 AS INTEGER) FROM json_each(?2)
		WHERE NOT EXISTS (SELECT 1 FROM `+s.t.holds+` WHERE hold_id = ?1)`, h.ID, string(runs), tree, h.Matter)
	if err != nil {
		return fmt.Errorf("place hold %q: %w (%w)", h.ID, err, agent.ErrStorage)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("place hold %q: %w (%w)", h.ID, err, agent.ErrStorage)
	} else if n == 0 {
		return fmt.Errorf("hold %q: %w", h.ID, agent.ErrHoldExists) // nothing was inserted: the ID is active
	}
	return nil
}

// LiftHold implements agent.Holder.
func (s *Store) LiftHold(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	res, err := s.write(ctx, `DELETE FROM `+s.t.holds+` WHERE hold_id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("lift hold %q: %w (%w)", id, err, agent.ErrStorage)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("lift hold %q: %w (%w)", id, err, agent.ErrStorage)
	}
	return n > 0, nil
}

// Holds implements agent.Holder. A file with no holds table (one written before holds, opened
// read-only) holds no hold.
func (s *Store) Holds(ctx context.Context) ([]agent.Hold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var n int
	if err := s.w.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, s.t.holds).Scan(&n); err != nil {
		return nil, fmt.Errorf("read the legal holds: %w (%w)", err, agent.ErrStorage)
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := s.w.QueryContext(ctx, `SELECT hold_id, run_id, tree, matter, placed_at_ms FROM `+s.t.holds+` ORDER BY hold_id, run_id`)
	if err != nil {
		return nil, fmt.Errorf("read the legal holds: %w (%w)", err, agent.ErrStorage)
	}
	defer rows.Close()
	var out []agent.Hold
	for rows.Next() {
		var id, run, matter string
		var tree int
		var at int64
		if err := rows.Scan(&id, &run, &tree, &matter, &at); err != nil {
			return nil, fmt.Errorf("read the legal holds: %w (%w)", err, agent.ErrStorage)
		}
		if len(out) == 0 || out[len(out)-1].ID != id {
			out = append(out, agent.Hold{ID: id, Matter: matter, Tree: tree == 1, PlacedAtMs: at})
		}
		out[len(out)-1].Runs = append(out[len(out)-1].Runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the legal holds: %w (%w)", err, agent.ErrStorage)
	}
	return out, nil
}
