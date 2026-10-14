package postgreslog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern/postgreslog"
)

// The least-privilege deployment: the owner runs Migrate, and the service opens the log
// WithoutMigration under a role with no CREATE on the schema.
func TestWithoutMigration_LeastPrivilege(t *testing.T) {
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	id := time.Now().UnixNano()
	schema, owner, app := fmt.Sprintf("nm_%d", id), fmt.Sprintf("nm_owner_%d", id), fmt.Sprintf("nm_app_%d", id)
	exec := func(q string) {
		t.Helper()
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, r := range []string{owner, app} {
		exec(`CREATE ROLE ` + r + ` LOGIN PASSWORD 'nm-test'`)
	}
	exec(`CREATE SCHEMA ` + schema + ` AUTHORIZATION ` + owner)
	exec(`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + app)
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		for _, r := range []string{owner, app} {
			admin.ExecContext(context.Background(), `DROP OWNED BY `+r+` CASCADE`)
			admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+r)
		}
	})
	dsn := func(user string) string {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(user, "nm-test")
		return u.String()
	}
	opts := []postgreslog.Option{postgreslog.WithSchema(schema)}
	noMig := append([]postgreslog.Option{postgreslog.WithoutMigration()}, opts...)

	// Before the migration: the typed error, for a missing table and for a missing schema.
	for name, o := range map[string][]postgreslog.Option{"table missing": noMig, "schema missing": {postgreslog.WithoutMigration(), postgreslog.WithSchema("nm_none")}} {
		l, err := postgreslog.Open(ctx, dsn(app), o...)
		if err == nil {
			l.Close()
			t.Fatalf("%s: opened", name)
		}
		if !errors.Is(err, postgreslog.ErrSchemaMissing) || !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if l, err := postgreslog.Open(ctx, dsn(app), opts...); err == nil {
		l.Close()
		t.Fatal("a role without CREATE ran the migration")
	}
	if err := postgreslog.Migrate(ctx, dsn(owner), postgreslog.WithoutMigration()); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Migrate with WithoutMigration = %v", err)
	}
	for range 2 { // idempotent
		if err := postgreslog.Migrate(ctx, dsn(owner), opts...); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	exec(`GRANT SELECT, INSERT ON ` + schema + `.governed_events TO ` + app)
	exec(`GRANT SELECT ON ` + schema + `.governed_events_schema_version TO ` + app)
	exec(`REVOKE ALL ON FUNCTION ` + schema + `.governed_events_next_seq_v1(text) FROM PUBLIC`)
	exec(`GRANT EXECUTE ON FUNCTION ` + schema + `.governed_events_next_seq_v1(text) TO ` + app)

	l, err := postgreslog.Open(ctx, dsn(app), noMig...)
	if err != nil {
		t.Fatalf("service role cannot open the log: %v", err)
	}
	defer l.Close()
	if pos, err := l.Append(ctx, "e", "a1", "ev"); err != nil || pos != 0 {
		t.Fatalf("Append = %d, %v", pos, err)
	}
	if pos, err := l.Append(ctx, "e", "a1", "ev"); err != nil || pos != 0 {
		t.Fatalf("repeated Append = %d, %v", pos, err)
	}

	// A dropped function is refused and Migrate restores it.
	exec(`DROP FUNCTION ` + schema + `.governed_events_next_seq_v1(text)`)
	if x, err := postgreslog.Open(ctx, dsn(app), noMig...); err == nil {
		x.Close()
		t.Fatal("opened without the next_seq function")
	} else if !errors.Is(err, postgreslog.ErrSchemaMissing) {
		t.Fatal(err)
	}

	if err := postgreslog.Migrate(ctx, dsn(owner), opts...); err != nil {
		t.Fatalf("Migrate after dropping the function: %v", err)
	}
	exec(`GRANT EXECUTE ON FUNCTION ` + schema + `.governed_events_next_seq_v1(text) TO ` + app)
	version := func() (v int) {
		t.Helper()
		if err := admin.QueryRowContext(ctx, `SELECT version FROM `+schema+`.governed_events_schema_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := version(); v != 1 {
		t.Fatalf("recorded version %d, want 1", v)
	}

	// A log from before the version table is version 1: the service opens it WithoutMigration,
	// and Migrate adds the table and records 1, idempotently.
	exec(`DROP TABLE ` + schema + `.governed_events_schema_version`)
	l2, err := postgreslog.Open(ctx, dsn(app), noMig...)
	if err != nil {
		t.Fatalf("a log without the version table: %v", err)
	}
	l2.Close()
	for range 2 {
		if err := postgreslog.Migrate(ctx, dsn(owner), opts...); err != nil {
			t.Fatalf("Migrate over a log without the version table: %v", err)
		}
		if v := version(); v != 1 {
			t.Fatalf("recorded version %d, want 1", v)
		}
	}
	exec(`GRANT SELECT ON ` + schema + `.governed_events_schema_version TO ` + app)

	// Another version is refused by the service, by Migrate and by a migrating Open, and a newer
	// one says why.
	for _, v := range []string{"2", "0"} {
		exec(`UPDATE ` + schema + `.governed_events_schema_version SET version = ` + v)
		if x, err := postgreslog.Open(ctx, dsn(app), noMig...); err == nil {
			x.Close()
			t.Fatalf("opened a log at version %s", v)
		} else if !errors.Is(err, postgreslog.ErrSchemaVersion) || !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("version %s: %v", v, err)
		}
		merr := postgreslog.Migrate(ctx, dsn(owner), opts...)
		if !errors.Is(merr, postgreslog.ErrSchemaVersion) {
			t.Fatalf("Migrate at version %s: %v", v, merr)
		}
		if v == "2" && !strings.Contains(merr.Error(), "newer bide wrote it") {
			t.Fatalf("Migrate at version 2 does not say why: %v", merr)
		}
		if x, err := postgreslog.Open(ctx, dsn(owner), opts...); err == nil {
			x.Close()
			t.Fatalf("a migrating Open accepted version %s", v)
		}
	}
}
