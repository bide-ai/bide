package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// OpenReadOnly connects to the Postgres journal at dsn (a pgx DSN, as Open takes) for reading
// only. Every connection is opened with default_transaction_read_only on, so the server refuses
// any write on it, and the store neither migrates nor creates anything: it reads the schema version
// and checks the tables as Open does after migrating. Get, Load and Runs (agent.Lister) read as
// they do on a Store from Open; Insert and the agent.Leaser methods fail with the server's
// read-only error (wrapping agent.ErrStorage for Insert). opts are Open's (WithSchema,
// WithTablePrefix).
//
// It refuses a database with no journal tables, and one whose schema version is newer than this
// version's (ErrConfig). Close closes the pool. It is for tools that inspect a journal, such as
// bide view, never for driving a run.
func OpenReadOnly(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	ro, err := readOnlyDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: OpenReadOnly: %w (%w)", err, agent.ErrConfig)
	}
	db, err := sql.Open("pgx", ro)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s, err := newStore(ctx, db, true, opts)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.own = true
	return s, nil
}

// readOnlyDSN is dsn with the run-time parameter default_transaction_read_only set to on, which
// pgx sends when it connects: as a query parameter of a URL DSN (replacing any value the DSN
// gives), or appended to a keyword/value DSN, where the last value of a keyword wins.
func readOnlyDSN(dsn string) (string, error) {
	if !strings.Contains(dsn, "://") {
		return dsn + " default_transaction_read_only=on", nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("default_transaction_read_only", "on")
	u.RawQuery = q.Encode()
	return u.String(), nil
}
