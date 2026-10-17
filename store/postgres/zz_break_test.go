package postgres

import (
	"context"
	"database/sql"
	"testing"
)

func TestBreak_RedactorRoleRawSQL(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	const schema, owner, app, redactor = "brkschema", "brk_owner", "brk_app", "brk_redactor"
	for _, r := range []string{owner, app, redactor} {
		rv120Role(t, admin, r)
	}
	admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	rv120Exec(t, admin, `CREATE SCHEMA `+schema+` AUTHORIZATION `+owner)
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`) })
	if _, err := Migrate(ctx, rv120DSN(t, base, owner, ""), WithSchema(schema)); err != nil {
		t.Fatal(err)
	}
	rv120Exec(t, admin,
		`GRANT USAGE ON SCHEMA `+schema+` TO `+app+`, `+redactor,
		`GRANT SELECT, INSERT ON `+schema+`.bide_steps TO `+app,
		`GRANT SELECT ON `+schema+`.bide_steps, `+schema+`.bide_leases, `+schema+`.bide_schema_version TO `+redactor,
		`GRANT UPDATE (data) ON `+schema+`.bide_steps TO `+redactor,
		`GRANT SELECT, INSERT ON `+schema+`.bide_steps, `+schema+`.bide_events TO `+app,
		`GRANT SELECT ON `+schema+`.bide_schema_version TO `+app,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON `+schema+`.bide_leases TO `+app,
		`GRANT EXECUTE ON FUNCTION `+schema+`.bide_next_seq_v1(text) TO `+app,
	)
	as, err := Open(ctx, rv120DSN(t, base, app, ""), WithSchema(schema), WithoutMigration())
	if err != nil {
		t.Fatal(err)
	}
	defer as.Close()
	// a LIVE run (no end marker): a start record only
	if _, _, err := as.Insert(ctx, "live", "run:start", []byte(`{"name":"run:start"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := as.Insert(ctx, "live", "attempt:tool:x", []byte(`{"name":"attempt:tool:x"}`)); err != nil {
		t.Fatal(err)
	}
	rdb, err := sql.Open("pgx", rv120DSN(t, base, redactor, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	for _, q := range []string{
		`UPDATE ` + schema + `.bide_steps SET data = 'forged'::bytea WHERE run_id='live' AND name='attempt:tool:x'`,
		`UPDATE ` + schema + `.bide_steps SET data = 'forged'::bytea WHERE run_id='live' AND name='run:start'`,
		`UPDATE ` + schema + `.bide_steps SET data = '{"redacted":{"leaf_hash":"00"}}'::bytea`,
	} {
		if _, err := rdb.ExecContext(ctx, q); err == nil {
			t.Errorf("redaction role (raw SQL, no Store.Redact checks) ran %q on a live run", q)
		}
	}
}
