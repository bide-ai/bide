package runview

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/approveline"
)

// The help text of the flags that name a store, shared by every command that reads one (bide view,
// bide edit), so the two cannot drift.
const (
	DBFlagHelp          = "SQLite journal file to read, opened read-only (sqlite.OpenReadOnly)"
	PostgresFlagHelp    = "Postgres journal to read, a pgx DSN such as postgres://user:pass@host:5432/db, read in sessions the server holds read-only (postgres.OpenReadOnly)"
	PostgresEnvFlagHelp = "name of the environment variable that holds the Postgres DSN, instead of -postgres: keeps the password out of the argument list, and is the name the Needs attention list's bide approve commands use"
	JournalFlagHelp     = "exported journal (audit.ExportJournal as JSON) to read; repeatable"
)

// StoreFlags are the flags that name the journal a command reads: -db, -postgres, -postgres-env and
// -journal. At most one kind is given.
type StoreFlags struct {
	DB          string
	Postgres    string
	PostgresEnv string
	Journals    []string
}

// ErrDuplicateRun is what Open wraps when two -journal files export the same run: a usage error,
// since the later file would replace the earlier with no sign.
var ErrDuplicateRun = errors.New("two exported journals hold the same run")

// Register adds the four flags to fs.
func (f *StoreFlags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.DB, "db", "", DBFlagHelp)
	fs.StringVar(&f.Postgres, "postgres", "", PostgresFlagHelp)
	fs.StringVar(&f.PostgresEnv, "postgres-env", "", PostgresEnvFlagHelp)
	fs.Var((*multi)(&f.Journals), "journal", JournalFlagHelp)
}

// Kinds is how many kinds of store the flags name: a SQLite file, a Postgres DSN (either flag), an
// exported journal.
func (f StoreFlags) Kinds() int {
	n := 0
	for _, set := range []bool{f.DB != "", f.Postgres != "" || f.PostgresEnv != "", len(f.Journals) > 0} {
		if set {
			n++
		}
	}
	return n
}

// Validate returns the usage message for flags that do not go together, or "". Naming no store is
// valid here: a command that needs one checks Kinds. The message never echoes a flag's value, which
// may be a DSN with a password.
func (f StoreFlags) Validate() string {
	switch {
	case f.Kinds() > 1:
		return "give exactly one of -db, -postgres (or -postgres-env) and -journal"
	case f.PostgresEnv != "" && !approveline.ValidEnvName(f.PostgresEnv):
		return approveline.NotAnEnvName
	case f.Postgres != "" && f.PostgresEnv != "":
		return "give -postgres or -postgres-env, not both"
	}
	return ""
}

// OpenedStore is what StoreFlags.Open yields. Journal and Store are nil for exported journals, which
// have no store behind them. Closer is nil when there is nothing to close.
type OpenedStore struct {
	Source  Source
	Journal *agent.Journal
	Store   agent.Store
	Closer  io.Closer
}

// Open opens what the flags name, read-only, through stores. It opens nothing when no flag is given
// (the result is zero). An exported journal is read whole.
func (f StoreFlags) Open(stores Stores) (OpenedStore, error) {
	var o OpenedStore
	open, what := stores.SQLite, f.DB
	dsn := f.Postgres
	if f.PostgresEnv != "" {
		if dsn = os.Getenv(f.PostgresEnv); dsn == "" {
			return o, fmt.Errorf("-postgres-env %s: the variable is empty or not set", f.PostgresEnv)
		}
	}
	if dsn != "" {
		open, what = stores.Postgres, "the Postgres journal"
	}
	if what != "" {
		if open == nil {
			return o, errors.New("this build cannot read that store; use -journal")
		}
		s, c, err := open(cmp.Or(f.DB, dsn))
		if err != nil {
			return o, fmt.Errorf("open %s: %w", what, err)
		}
		j, err := agent.NewJournal(s)
		if err != nil {
			c.Close()
			return o, err
		}
		return OpenedStore{Source: JournalSource{J: j}, Journal: j, Store: s, Closer: c}, nil
	}
	if len(f.Journals) == 0 {
		return o, nil
	}
	x := Exports{}
	from := map[string]string{}
	for _, path := range f.Journals {
		var exp audit.JournalExport
		if err := readStrict("", path, &exp); err != nil {
			return o, err
		}
		recs, err := exp.Journal()
		if err != nil {
			return o, fmt.Errorf("%s: %w", path, err)
		}
		if first, dup := from[exp.RunID]; dup {
			return o, fmt.Errorf("-journal %s and -journal %s both export run %q: %w", first, path, exp.RunID, ErrDuplicateRun)
		}
		from[exp.RunID] = path
		x[exp.RunID] = recs
	}
	o.Source = x
	return o, nil
}
