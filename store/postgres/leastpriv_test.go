package postgres

// The least-privilege roles docs/trust/operations.md recommends, run for real: an owner role runs
// the migration, a service role drives runs (a journal write, an attempt claim, a lease, the event
// store), and a read-only role inspects. If the table in that page is missing a privilege this
// fails; the denied-statement checks show that the service role cannot edit or remove the journal.
// The service role has no CREATE on the schema and opens the store WithoutMigration; the default
// Open runs CREATE TABLE IF NOT EXISTS even when the tables exist, which Postgres allows only with
// CREATE, so it fails for that role (the test pins that). Skips without PG_DSN.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func TestLeastPrivilegeRoles(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	const schema, owner, app, reader, redactor = "lpschema", "lp_owner", "lp_app", "lp_reader", "lp_redactor"
	rv120Role(t, admin, owner)
	rv120Role(t, admin, app)
	rv120Role(t, admin, reader)
	rv120Role(t, admin, redactor)
	admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	rv120Exec(t, admin, `CREATE SCHEMA `+schema+` AUTHORIZATION `+owner)
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`) })

	// 1. The owner runs the migration once: tables, indexes and next_seq are created and owned by it.
	if _, err := Migrate(ctx, rv120DSN(t, base, owner, ""), WithSchema(schema)); err != nil {
		t.Fatalf("owner: %v", err)
	}

	// 2. The grants of the page's table, and nothing else.
	rv120Exec(t, admin,
		`GRANT USAGE ON SCHEMA `+schema+` TO `+app+`, `+reader,
		`GRANT SELECT, INSERT ON `+schema+`.bide_steps, `+schema+`.bide_events TO `+app,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON `+schema+`.bide_leases TO `+app,
		`GRANT SELECT ON `+schema+`.bide_schema_version TO `+app,
		`GRANT SELECT ON `+schema+`.bide_steps, `+schema+`.bide_events, `+schema+`.bide_leases, `+schema+`.bide_schema_version TO `+reader,
		`REVOKE ALL ON FUNCTION `+schema+`.bide_next_seq_v1(text) FROM PUBLIC`,
		`GRANT EXECUTE ON FUNCTION `+schema+`.bide_next_seq_v1(text) TO `+app,
		// The redaction role of docs/guides/postgres.md: it reads the journal and replaces an entry's
		// data, and can do nothing else; the service role cannot erase and this role cannot append.
		`GRANT USAGE ON SCHEMA `+schema+` TO `+redactor,
		`GRANT SELECT ON `+schema+`.bide_steps, `+schema+`.bide_leases, `+schema+`.bide_schema_version TO `+redactor,
		`GRANT UPDATE (data) ON `+schema+`.bide_steps TO `+redactor,
	)

	// 3. The service role has no CREATE on the schema: the default Open, which migrates, is refused;
	// opened WithoutMigration, the role drives a run.
	if x, err := Open(ctx, rv120DSN(t, base, app, ""), WithSchema(schema)); err == nil {
		x.Close()
		t.Fatal("a role without CREATE on the schema ran the migration in Open")
	}
	s, err := Open(ctx, rv120DSN(t, base, app, ""), WithSchema(schema), WithoutMigration())
	if err != nil {
		t.Fatalf("service role cannot open the store: %v", err)
	}
	defer s.Close()
	run := uniqueID(t, "lp-")
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	fired := 0
	for i := 0; i < 2; i++ { // the second call replays the recorded result: the claim and the result were read back
		got, err := j.Step(ctx, run, "pay", func(context.Context) (string, error) { fired++; return "ok", nil })
		if err != nil || got != "ok" {
			t.Fatalf("step %d: %q, %v", i, got, err)
		}
	}
	if fired != 1 {
		t.Fatalf("the effect fired %d times", fired)
	}
	ok, err := s.AcquireLease(ctx, run, "node-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire lease: %v, %v", ok, err)
	}
	if ok, err := s.RenewLease(ctx, run, "node-a", time.Minute); err != nil || !ok {
		t.Fatalf("renew lease: %v, %v", ok, err)
	}
	if err := s.ReleaseLease(ctx, run, "node-a"); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	if n := 0; true {
		for _, err := range s.Runs(ctx, agent.RunFilter{}) {
			if err != nil {
				t.Fatalf("list runs: %v", err)
			}
			n++
		}
	}
	if err := s.EventStore().Append(ctx, run, 0, []byte("leaf")); err != nil {
		t.Fatalf("event store append: %v", err)
	}
	if got, err := s.EventStore().Load(ctx, run); err != nil || len(got) != 1 {
		t.Fatalf("event store load: %q, %v", got, err)
	}

	// 4. What the service role must not be able to do, on its own connection.
	appDB, err := sql.Open("pgx", rv120DSN(t, base, app, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer appDB.Close()
	for _, q := range []string{
		`UPDATE ` + schema + `.bide_steps SET data = ''`,
		`DELETE FROM ` + schema + `.bide_steps`,
		`UPDATE ` + schema + `.bide_events SET leaf = ''`,
		`DELETE FROM ` + schema + `.bide_events`,
		`UPDATE ` + schema + `.bide_schema_version SET version = 0`,
		`DROP TABLE ` + schema + `.bide_steps`,
	} {
		if _, err := appDB.ExecContext(ctx, q); err == nil {
			t.Errorf("service role ran %q", q)
		}
	}

	// 4b. Erasure is the redaction role's alone: the service role's Redact is refused by the
	// database, the redaction role's replaces the entry, and it cannot append or delete.
	rec, err := agent.JournalEntry("tool:pii", agent.Record{Kind: agent.StepToolResult, Result: []byte(`{"name":"Alice"}`)})
	if err != nil {
		t.Fatal(err)
	}
	end, err := agent.JournalEntry("run:complete", agent.Record{Kind: agent.StepValue, Result: []byte(`"done"`)})
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"tool:pii": rec, "run:complete": end} {
		if _, _, err := s.Insert(ctx, run, name, b); err != nil {
			t.Fatal(err)
		}
	}
	tomb, err := agent.NewTombstone("tool:pii", rec, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if done, err := s.Redact(ctx, run, "tool:pii", rec, tomb); done || !errors.Is(err, agent.ErrStorage) {
		t.Fatalf("the service role redacted an entry: %v, %v", done, err)
	}
	rs, err := Open(ctx, rv120DSN(t, base, redactor, ""), WithSchema(schema), WithoutMigration())
	if err != nil {
		t.Fatalf("redaction role cannot open the store: %v", err)
	}
	defer rs.Close()
	if done, err := rs.Redact(ctx, run, "tool:pii", rec, tomb); err != nil || !done {
		t.Fatalf("the redaction role's Redact = %v, %v", done, err)
	}
	if _, _, err := rs.Insert(ctx, run, "x", []byte("x")); err == nil {
		t.Fatal("the redaction role appended to the journal")
	}
	redDB, err := sql.Open("pgx", rv120DSN(t, base, redactor, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer redDB.Close()
	for _, q := range []string{`DELETE FROM ` + schema + `.bide_steps`, `UPDATE ` + schema + `.bide_steps SET name = 'x'`, `UPDATE ` + schema + `.bide_steps SET seq = 0`} {
		if _, err := redDB.ExecContext(ctx, q); err == nil {
			t.Errorf("redaction role ran %q", q)
		}
	}

	// 5. The read-only role reads through OpenReadOnly and cannot write.
	r, err := OpenReadOnly(ctx, rv120DSN(t, base, reader, ""), WithSchema(schema))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer r.Close()
	n := 0
	for _, err := range r.Load(ctx, run, -1) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("the reader sees no records")
	}
	if _, _, err := r.Insert(ctx, run, "x", []byte("x")); err == nil || !errors.Is(err, agent.ErrStorage) {
		t.Fatalf("reader insert: %v", err)
	}
}
