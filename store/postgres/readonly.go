package postgres

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
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
// bide-view, never for driving a run.
func OpenReadOnly(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: OpenReadOnly: %w (%w)", err, agent.ErrConfig)
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	db := stdlib.OpenDB(*cfg)
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
