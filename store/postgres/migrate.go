package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// ErrSchemaMissing is wrapped by the error Open, New or OpenReadOnly return when the store's
// schema, or a part of it (the pinned schema, a table, the next_seq function, a lease index), does
// not exist and the store was not allowed to create it: it was opened WithoutMigration, or read
// only. Run Migrate (bide migrate) as the role that owns the schema, then open the store again.
// The error also wraps agent.ErrConfig.
var ErrSchemaMissing = errors.New("postgres: the schema is missing")

// ErrSchemaVersion is wrapped by the error returned when the database's schema version is not the
// one this version of the package uses. A newer schema needs a newer version of the program. An
// older one needs a migration step; there is one schema version (1) today, so an older one means
// the version table was edited, and Migrate refuses it rather than record a version it did not
// reach. The error also wraps agent.ErrConfig.
var ErrSchemaVersion = errors.New("postgres: the schema version does not match")

// WithoutMigration makes Open and New verify the schema instead of creating it. The store runs no
// CREATE and no INSERT into the version table, so its role needs no CREATE on the schema: the
// least-privilege deployment runs Migrate (bide migrate) once as the owner role, and the running
// service uses this option. Opening still checks everything the migration would have created and
// refuses what is not there:
//
//   - a missing schema, table (steps, leases, schema version, events), next_seq function or
//     lease index fails with an error that wraps ErrSchemaMissing;
//   - a schema version other than this package's fails with an error that wraps ErrSchemaVersion;
//   - a table without the uniqueness the store depends on, and a next_seq function that is not the
//     one this version creates, fail as they do for Open (agent.ErrConfig).
//
// Both errors also wrap agent.ErrConfig. The role needs USAGE on the schema, SELECT on the version
// table (and the steps, leases and events tables), the privileges its operations use (see the
// Postgres store guide) and EXECUTE on the next_seq function. Migrate refuses this option.
func WithoutMigration() Option {
	return optionFunc(func(c *config) error {
		c.noMigrate = true
		return nil
	})
}

// WithDryRun makes Migrate and MigrateDB run the migration in a transaction and roll it back: the
// result lists the statements and the versions as if it had committed, the role is shown to hold
// the privileges the migration needs, and nothing is left behind. Open, New and OpenReadOnly
// refuse it (agent.ErrConfig).
func WithDryRun() Option {
	return optionFunc(func(c *config) error {
		c.dryRun = true
		return nil
	})
}

// MigrationResult is what a migration did, or with WithDryRun would have done.
type MigrationResult struct {
	// Schema is the schema the tables are in.
	Schema string
	// Before is the schema version found before the migration; 0 when the schema had none yet (a
	// new database).
	Before int
	// After is the schema version once the migration is done: the version of this package.
	After int
	// Created names what the migration created (with WithDryRun, would create): tables, the
	// next_seq function and lease indexes, schema-qualified. It is empty on a current schema.
	Created []string
	// Statements are the SQL statements the migration ran, in order, as sent to the server. A
	// CREATE TABLE IF NOT EXISTS (and the version row's INSERT ... ON CONFLICT DO NOTHING) is
	// listed even when the table exists: it is sent, and does nothing. The function and each
	// index are listed only when they were missing.
	Statements []string
	// DryRun is set when the migration was rolled back (WithDryRun).
	DryRun bool
}

// Migrate creates or checks the store's schema at dsn (a pgx DSN, as Open takes) as the role in the
// DSN, which needs the privileges of the schema's owner (CREATE on the schema), and reports the
// schema version before and after. It is the migration Open runs, on its own, so that the running
// service can open the store WithoutMigration under a role that has no DDL rights.
//
// It is idempotent: on a current schema it creates nothing and changes nothing. It never alters or
// drops an existing table or replaces a function, and refuses (ErrSchemaVersion) a schema newer
// than this version's. opts are Open's (WithSchema, WithTablePrefix) and WithDryRun; WithoutMigration
// is an agent.ErrConfig error. After the migration it checks the tables and the function as Open
// does. Run it with the owner role, pin the schema with WithSchema, and see the Postgres store
// guide for the roles.
func Migrate(ctx context.Context, dsn string, opts ...Option) (MigrationResult, error) {
	db, err := openPool(ctx, dsn, "Migrate")
	if err != nil {
		return MigrationResult{}, err
	}
	defer db.Close()
	return MigrateDB(ctx, db, opts...)
}

// MigrateDB is Migrate over db, which the caller opened (with the pgx driver) and closes, as New is
// to Open.
func MigrateDB(ctx context.Context, db *sql.DB, opts ...Option) (MigrationResult, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return MigrationResult{}, err
	}
	if cfg.noMigrate {
		return MigrationResult{}, fmt.Errorf("postgres: Migrate with WithoutMigration would do nothing: %w", agent.ErrConfig)
	}
	s, err := prepare(ctx, db, cfg)
	if err != nil {
		return MigrationResult{}, err
	}
	res, err := s.migrate(ctx, cfg.dryRun)
	if err != nil || cfg.dryRun { // a rolled-back migration left nothing to check
		return res, err
	}
	if err := s.checkSchema(ctx); err != nil {
		return res, err
	}
	if err := s.checkEvents(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// errSchemaNewer is the error for a database whose schema version v is newer than this version's.
func errSchemaNewer(v int) error {
	return fmt.Errorf("postgres: the database's schema version is %d, newer than this version's %d; use a newer version of the program: %w (%w)",
		v, schemaVersion, ErrSchemaVersion, agent.ErrConfig)
}

// errSchemaMissing is the error for a part of the schema that is not there, when the store may not
// create it.
func errSchemaMissing(what string) error {
	return fmt.Errorf("postgres: %s does not exist; the schema's owner creates it with Migrate (bide migrate), and a store that may not migrate never does: %w (%w)",
		what, ErrSchemaMissing, agent.ErrConfig)
}

// checkVersion reads the schema version and refuses a schema that has none (ErrSchemaMissing) or
// one other than this package's (ErrSchemaVersion). It is what a store that does not migrate
// (OpenReadOnly, WithoutMigration) checks first.
func (s *Store) checkVersion(ctx context.Context) error {
	present, err := s.relationPresent(ctx, s.t.version)
	if err != nil {
		return err
	}
	if !present {
		return errSchemaMissing("the schema version table " + quoteIdent(s.schema) + "." + s.t.version)
	}
	var v int
	if err := retryScan(ctx, func() error { return s.db.QueryRowContext(ctx, string(s.t.readVersion)).Scan(&v) }); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errSchemaMissing("the schema version row in " + quoteIdent(s.schema) + "." + s.t.version)
		}
		return fmt.Errorf("postgres: read schema version (is it a bide journal?): %w (%w)", err, agent.ErrStorage)
	}
	switch {
	case v > schemaVersion:
		return errSchemaNewer(v)
	case v != schemaVersion:
		return fmt.Errorf("postgres: the database's schema version is %d, not this version's %d; the schema's owner runs Migrate (bide migrate): %w (%w)",
			v, schemaVersion, ErrSchemaVersion, agent.ErrConfig)
	}
	return nil
}

// relationPresent reports whether a relation named name exists in the store's schema.
func (s *Store) relationPresent(ctx context.Context, name string) (bool, error) {
	var isTable sql.NullBool
	err := retryScan(ctx, func() error { return s.db.QueryRowContext(ctx, relationIsTable, name, s.schema).Scan(&isTable) })
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: look up %s: %w (%w)", name, err, agent.ErrStorage)
	}
	return true, nil
}

// verify is what a store opened WithoutMigration does in place of the migration: it checks that
// everything the migration creates is there, at this version, and creates nothing. The uniqueness
// and function checks (checkSchema, checkEvents) follow it, as after a migration.
func (s *Store) verify(ctx context.Context) error {
	if err := s.checkVersion(ctx); err != nil {
		return err
	}
	for _, table := range []string{s.t.steps, s.t.leases, s.t.events} {
		present, err := s.relationPresent(ctx, table)
		if err != nil {
			return err
		}
		if !present {
			return errSchemaMissing("table " + quoteIdent(s.schema) + "." + table)
		}
	}
	var haveNextSeq bool
	if err := retryScan(ctx, func() error {
		return s.db.QueryRowContext(ctx, nextSeqPresent, s.t.nextSeq, s.schema).Scan(&haveNextSeq)
	}); err != nil {
		return fmt.Errorf("postgres: look up %s: %w (%w)", s.t.nextSeq, err, agent.ErrStorage)
	}
	if !haveNextSeq {
		return errSchemaMissing("function " + quoteIdent(s.schema) + "." + s.t.nextSeq + "(text)")
	}
	for _, ix := range leaseIndexes(s.t) {
		var have bool
		if err := retryScan(ctx, func() error { return s.db.QueryRowContext(ctx, indexPresent, ix.name, s.schema).Scan(&have) }); err != nil {
			return fmt.Errorf("postgres: look up index %s: %w (%w)", ix.name, err, agent.ErrStorage)
		}
		if !have {
			return errSchemaMissing("index " + quoteIdent(s.schema) + "." + ix.name)
		}
	}
	return nil
}
