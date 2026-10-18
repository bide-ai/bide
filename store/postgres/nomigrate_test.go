package postgres

// The least-privilege deployment: the owner role migrates (Migrate), and the service role opens
// the store WithoutMigration with no CREATE on the schema. Skips without PG_DSN.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func TestWithoutMigration_OptionConflicts(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, nil, WithDryRun()); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("New with WithDryRun = %v, want ErrConfig", err)
	}
	if _, err := MigrateDB(ctx, nil, WithoutMigration()); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Migrate with WithoutMigration = %v, want ErrConfig", err)
	}
}

func TestWithoutMigration_LeastPrivilege(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	const schema, owner, app = "nmschema", "nm_owner", "nm_app"
	rv120Role(t, admin, owner)
	rv120Role(t, admin, app)
	admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	rv120Exec(t, admin, `CREATE SCHEMA `+schema+` AUTHORIZATION `+owner, `GRANT USAGE ON SCHEMA `+schema+` TO `+app)
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`) })
	ownerDSN, appDSN := rv120DSN(t, base, owner, ""), rv120DSN(t, base, app, "")
	appOpts := []Option{WithSchema(schema), WithoutMigration()}

	var canCreate bool
	if err := admin.QueryRowContext(ctx, `SELECT has_schema_privilege($1, $2, 'CREATE')`, app, schema).Scan(&canCreate); err != nil || canCreate {
		t.Fatalf("the service role has CREATE on the schema: %v, %v", canCreate, err)
	}

	// Before the migration: the typed error, whether the schema is pinned and missing or the tables
	// are; and nothing was created.
	for name, opts := range map[string][]Option{
		"tables missing":   appOpts,
		"schema not there": {WithSchema("nmnone"), WithoutMigration()},
	} {
		s, err := Open(ctx, appDSN, opts...)
		if err == nil {
			s.Close()
			t.Fatalf("%s: opened", name)
		}
		if !errors.Is(err, ErrSchemaMissing) || !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("%s: %v, want ErrSchemaMissing and ErrConfig", name, err)
		}
	}
	if s, err := Open(ctx, appDSN, WithSchema(schema)); err == nil {
		s.Close()
		t.Fatal("a role without CREATE ran the migration")
	}
	assertTables := func(want int) {
		t.Helper()
		var n int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = $1`, schema).Scan(&n); err != nil || n != want {
			t.Fatalf("%d tables in the schema, want %d (%v)", n, want, err)
		}
	}
	assertTables(0)

	// A dry run as the owner lists the statements and leaves nothing.
	dry, err := Migrate(ctx, ownerDSN, WithSchema(schema), WithDryRun())
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.DryRun || dry.Before != 0 || dry.After != schemaVersion || len(dry.Statements) < 4 {
		t.Fatalf("dry run = %+v", dry)
	}
	assertTables(0)
	// The dry run is the owner's check; the service role's dry run fails: no CREATE.
	if _, err := Migrate(ctx, appDSN, WithSchema(schema), WithDryRun()); err == nil {
		t.Fatal("a dry run as a role without CREATE succeeded")
	}

	// The migration, as the owner.
	res, err := Migrate(ctx, ownerDSN, WithSchema(schema))
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.DryRun || res.Before != 0 || res.After != schemaVersion || res.Schema != schema || len(res.Statements) < 4 {
		t.Fatalf("Migrate = %+v", res)
	}
	assertTables(5)
	again, err := Migrate(ctx, ownerDSN, WithSchema(schema))
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if again.Before != schemaVersion || again.After != schemaVersion {
		t.Fatalf("second Migrate = %+v, want version %d before and after", again, schemaVersion)
	}
	if len(again.Created) != 0 {
		t.Fatalf("second Migrate created %v", again.Created)
	}
	if len(res.Created) != 9 { // five tables, two functions and two indexes
		t.Fatalf("first Migrate created %v", res.Created)
	}
	for _, st := range again.Statements { // idempotent: no function and no index on a current schema
		if strings.Contains(st, "CREATE FUNCTION") || strings.Contains(st, "CREATE INDEX") {
			t.Fatalf("second Migrate ran %q", st)
		}
	}

	// The service role's grants: no CREATE, and SELECT only on the version table.
	rv120Exec(t, admin,
		`GRANT SELECT, INSERT ON `+schema+`.bide_steps, `+schema+`.bide_events TO `+app,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON `+schema+`.bide_leases TO `+app,
		`GRANT SELECT ON `+schema+`.bide_schema_version TO `+app,
		`REVOKE ALL ON FUNCTION `+schema+`.bide_next_seq_v1(text) FROM PUBLIC`,
		`GRANT EXECUTE ON FUNCTION `+schema+`.bide_next_seq_v1(text) TO `+app,
	)
	s, err := Open(ctx, appDSN, appOpts...)
	if err != nil {
		t.Fatalf("service role cannot open the store: %v", err)
	}
	defer s.Close()
	run := uniqueID(t, "nm-")
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	fired := 0
	for i := 0; i < 2; i++ {
		got, err := j.Step(ctx, run, "pay", func(context.Context) (string, error) { fired++; return "ok", nil })
		if err != nil || got != "ok" {
			t.Fatalf("step %d: %q, %v", i, got, err)
		}
	}
	if fired != 1 {
		t.Fatalf("the effect fired %d times", fired)
	}
	if ok, err := s.AcquireLease(ctx, run, "node-a", time.Minute); err != nil || !ok {
		t.Fatalf("acquire lease: %v, %v", ok, err)
	}
	if ok, err := s.RenewLease(ctx, run, "node-a", time.Minute); err != nil || !ok {
		t.Fatalf("renew lease: %v, %v", ok, err)
	}
	if err := s.ReleaseLease(ctx, run, "node-a"); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	found := false
	for id, err := range s.Runs(ctx, agent.RunFilter{}) {
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		found = found || id == run
	}
	if !found {
		t.Fatal("Runs does not list the run")
	}
	if err := s.EventStore().Append(ctx, run, 0, []byte("leaf")); err != nil {
		t.Fatalf("event store append: %v", err)
	}
	if got, err := s.EventStore().Load(ctx, run); err != nil || len(got) != 1 {
		t.Fatalf("event store load: %q, %v", got, err)
	}

	// A part of the schema that goes missing is refused, and the owner's Migrate restores it.
	for _, drop := range []struct{ what, stmt string }{
		{"lease index", `DROP INDEX ` + schema + `.bide_leases_expiry`},
		{"events table", `DROP TABLE ` + schema + `.bide_events`},
		{"next_seq function", `DROP FUNCTION ` + schema + `.bide_next_seq_v1(text)`},
	} {
		rv120Exec(t, admin, drop.stmt)
		x, err := Open(ctx, appDSN, appOpts...)
		if err == nil {
			x.Close()
			t.Fatalf("opened with the %s dropped", drop.what)
		}
		if !errors.Is(err, ErrSchemaMissing) {
			t.Fatalf("%s dropped: %v, want ErrSchemaMissing", drop.what, err)
		}
		if _, err := Migrate(ctx, ownerDSN, WithSchema(schema)); err != nil {
			t.Fatalf("Migrate after dropping the %s: %v", drop.what, err)
		}
		rv120Exec(t, admin,
			`GRANT SELECT, INSERT ON `+schema+`.bide_events TO `+app,
			`GRANT EXECUTE ON FUNCTION `+schema+`.bide_next_seq_v1(text) TO `+app)
		x, err = Open(ctx, appDSN, appOpts...)
		if err != nil {
			t.Fatalf("open after Migrate restored the %s: %v", drop.what, err)
		}
		x.Close()
	}

	// A version other than this one's is refused by both, with the typed error.
	for _, v := range []string{"2", "0"} {
		rv120Exec(t, admin, `UPDATE `+schema+`.bide_schema_version SET version = `+v)
		x, err := Open(ctx, appDSN, appOpts...)
		if err == nil {
			x.Close()
			t.Fatalf("opened a schema at version %s", v)
		}
		if !errors.Is(err, ErrSchemaVersion) || !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("version %s: %v, want ErrSchemaVersion and ErrConfig", v, err)
		}
		_, merr := Migrate(ctx, ownerDSN, WithSchema(schema))
		if !errors.Is(merr, ErrSchemaVersion) {
			t.Fatalf("Migrate at version %s: %v, want ErrSchemaVersion", v, merr)
		}
		if v == "2" && !strings.Contains(merr.Error(), "newer bide wrote it") {
			t.Fatalf("Migrate at version 2 does not say why it refuses: %v", merr)
		}
		if ro, err := OpenReadOnly(ctx, appDSN, WithSchema(schema)); err == nil {
			ro.Close()
			t.Fatalf("OpenReadOnly opened a schema at version %s", v)
		} else if !errors.Is(err, ErrSchemaVersion) {
			t.Fatalf("OpenReadOnly at version %s: %v", v, err)
		}
	}
}

// A journal Open migrated opens WithoutMigration, on the default schema; under a prefix with no
// tables it fails with the typed error.
func TestWithoutMigration_OpensWhatOpenMigrated(t *testing.T) {
	admin, dsn := rv120Admin(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("nm%d_", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, tbl := range []string{"steps", "leases", "schema_version", "events"} {
			admin.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+prefix+tbl)
		}
		admin.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS `+prefix+`next_seq_v1(text)`)
	})
	if x, err := Open(ctx, dsn, WithTablePrefix(prefix), WithoutMigration()); err == nil {
		x.Close()
		t.Fatal("opened with no tables")
	} else if !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("%v, want ErrSchemaMissing", err)
	}
	w, err := Open(ctx, dsn, WithTablePrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	x, err := Open(ctx, dsn, WithTablePrefix(prefix), WithoutMigration())
	if err != nil {
		t.Fatalf("WithoutMigration on a migrated journal: %v", err)
	}
	x.Close()
}
