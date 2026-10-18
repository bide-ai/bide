package postgres

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// A hold whose placement has committed (PlaceHold has returned) must stop every erasure that has not
// yet committed. The redaction function takes the holds table's SHARE lock before its UPDATE, so a hold
// placed while the UPDATE waits for a row lock waits for the erasure, and is seen by every later one.
// The check accepts either order that is consistent: held, or the erasure committed first.
func TestHold_PlacedWhileTheRedactionWaitsIsHonoured(t *testing.T) {
	r := newRedactRig(t)
	ctx := context.Background()
	const ms = 1_700_000_000_000
	run := uniqueID(t, "race-")
	r.put(run, "run:complete", agent.StepValue, `"ok"`)
	tool := r.put(run, "tool:x", agent.StepToolResult, `{"pii":1}`)

	tx, err := r.admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM rrschema.bide_steps WHERE run_id = $1 AND name = 'tool:x' FOR UPDATE`, run); err != nil {
		t.Fatal(err)
	}
	type res struct {
		code string
		at   time.Time
	}
	redacted, held := make(chan res, 1), make(chan time.Time, 1)
	go func() {
		var code string
		if err := r.raw.QueryRowContext(ctx, `SELECT rrschema.bide_redact_v3($1, $2, $3, $4)`, run, "tool:x", tool, int64(ms)).Scan(&code); err != nil {
			code = err.Error()
		}
		redacted <- res{code, time.Now()}
	}()
	time.Sleep(500 * time.Millisecond) // the redaction is inside its UPDATE, waiting for the row
	go func() {
		if _, err := r.admin.ExecContext(ctx, `INSERT INTO rrschema.bide_holds (hold_id, run_id, tree, matter, placed_at_ms) VALUES ('h1', $1, false, 'case', 1)`, run); err != nil {
			t.Error(err)
		}
		held <- time.Now()
	}()
	time.Sleep(500 * time.Millisecond)
	tx.Commit()
	rr, hh := <-redacted, <-held
	if rr.code == "replaced" && hh.Before(rr.at) {
		t.Fatalf("the hold's placement returned at %s, before the erasure committed at %s, and the erasure still replaced the record", hh.Format("15:04:05.000"), rr.at.Format("15:04:05.000"))
	}
	// Whichever order it was, a later erasure of another record sees the hold.
	other := r.put(run, "tool:y", agent.StepToolResult, `{"pii":2}`)
	if got := r.call(run, "tool:y", other, ms); got != "held" {
		t.Errorf("an erasure after the hold committed: code %q, want held", got)
	}
}

// A schema migrated before holds: the service role still opens it without migrating.
func TestHold_ServiceOpensASchemaWithoutHolds(t *testing.T) {
	r := newRedactRig(t)
	ctx := context.Background()
	if _, err := r.admin.ExecContext(ctx, `DROP TABLE rrschema.bide_holds`); err != nil {
		t.Fatal(err)
	}
	_, base := rv120Admin(t)
	s, err := Open(ctx, rv120DSN(t, base, "rr_app", ""), WithSchema("rrschema"), WithoutMigration())
	if err != nil {
		t.Fatalf("a service role opening a schema without bide_holds: %v", err)
	}
	s.Close()
}

// The redaction function runs only at READ COMMITTED: a caller whose transaction already took a
// snapshot (REPEATABLE READ, SERIALIZABLE) would not see a hold placed after it, so it is refused
// ("isolation"), and at READ COMMITTED the same call goes through, and a hold is seen.
func TestRedactFunction_RefusesAStaleSnapshot(t *testing.T) {
	r := newRedactRig(t)
	ctx := context.Background()
	const ms = 1_700_000_000_000
	run := uniqueID(t, "iso-")
	r.put(run, "run:complete", agent.StepValue, `"ok"`)
	a := r.put(run, "tool:a", agent.StepToolResult, `{"pii":1}`)
	b := r.put(run, "tool:b", agent.StepToolResult, `{"pii":2}`)
	callIn := func(level sql.IsolationLevel, name string, cur []byte) string {
		t.Helper()
		tx, err := r.raw.BeginTx(ctx, &sql.TxOptions{Isolation: level})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var code string
		if err := tx.QueryRowContext(ctx, `SELECT rrschema.bide_redact_v3($1, $2, $3, $4)`, run, name, cur, int64(ms)).Scan(&code); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return code
	}
	for _, level := range []sql.IsolationLevel{sql.LevelRepeatableRead, sql.LevelSerializable} {
		if got := callIn(level, "tool:a", a); got != "isolation" {
			t.Errorf("at %v: code %q, want isolation", level, got)
		}
	}
	if !strings.Contains(string(r.stored(run, "tool:a")), `"pii":1`) {
		t.Fatal("a refused call changed the row")
	}
	if got := callIn(sql.LevelReadCommitted, "tool:a", a); got != "replaced" {
		t.Errorf("at READ COMMITTED: code %q, want replaced", got)
	}
	if _, err := r.admin.ExecContext(ctx, `INSERT INTO rrschema.bide_holds (hold_id, run_id, tree, matter, placed_at_ms) VALUES ('h', $1, false, 'm', 1)`, run); err != nil {
		t.Fatal(err)
	}
	if got := callIn(sql.LevelReadCommitted, "tool:b", b); got != "held" {
		t.Errorf("at READ COMMITTED after a hold: code %q, want held", got)
	}
}

// The migration withdraws EXECUTE on every older version of the redaction function (v1 reads no
// holds; v2 refuses an end marker the current one erases) from PUBLIC and from every role it was
// granted to: after it, the erasing role cannot call any of them, and still can call the current one.
// The functions are revoked, not replaced or dropped; a name that is not an older version is left alone.
func TestMigration_RevokesEveryOlderRedactionFunction(t *testing.T) {
	r := newRedactRig(t)
	ctx := context.Background()
	const ms = 1_700_000_000_000
	// A schema from an earlier version: each old function exists, owned by the schema's owner,
	// executable by PUBLIC and explicitly by the erasing role, and erases whatever it is asked to.
	olds := []string{"bide_redact_v1", "bide_redact_v2"}
	for _, f := range append(olds, "bide_redact_v10", "bide_redact_v02", "bide_redact_vx") {
		rv120Exec(t, r.admin,
			`CREATE FUNCTION rrschema.`+f+`(r text, n text, cur bytea, ms bigint) RETURNS text LANGUAGE sql AS $$ SELECT 'replaced' $$`,
			`ALTER FUNCTION rrschema.`+f+`(text, text, bytea, bigint) OWNER TO rr_owner`,
			`GRANT EXECUTE ON FUNCTION rrschema.`+f+`(text, text, bytea, bigint) TO PUBLIC, rr_redactor, rr_app`,
		)
	}
	call := func(f string) error {
		var code string
		return r.raw.QueryRowContext(ctx, `SELECT rrschema.`+f+`('x', 'y', '\\x00'::bytea, 1)`).Scan(&code)
	}
	for _, f := range olds {
		if err := call(f); err != nil {
			t.Fatalf("setup: the erasing role cannot call %s before the migration: %v", f, err)
		}
	}
	_, base := rv120Admin(t)
	res, err := Migrate(ctx, rv120DSN(t, base, "rr_owner", ""), WithSchema("rrschema"))
	if err != nil {
		t.Fatal(err)
	}
	if containsPrefix(res.Created, "function") {
		t.Fatalf("the migration created %v: it must only revoke", res.Created)
	}
	grantees := func(f string) (n int) {
		t.Helper()
		if err := r.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_proc p CROSS JOIN LATERAL aclexplode(p.proacl) a WHERE p.proname = $1 AND p.pronamespace = 'rrschema'::regnamespace AND a.grantee <> p.proowner`, f).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, f := range olds {
		if err := call(f); err == nil || sqlState(err) != "42501" {
			t.Fatalf("the erasing role called %s after the migration: %v (want permission denied)", f, err)
		}
		var stillThere bool
		if err := r.admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'rrschema' AND p.proname = $1)`, f).Scan(&stillThere); err != nil || !stillThere {
			t.Fatalf("%s was dropped or replaced (%v): the migration only revokes", f, err)
		}
		// Nobody but the owner holds EXECUTE on it any more.
		if n := grantees(f); n != 0 {
			t.Fatalf("%d role(s) besides the owner still hold a privilege on %s", n, f)
		}
	}
	// Names that are not an older version of this function (a later or malformed number) are not touched.
	for _, f := range []string{"bide_redact_v10", "bide_redact_v02", "bide_redact_vx"} {
		if n := grantees(f); n == 0 {
			t.Errorf("%s lost its grants: only the older versions v1 and v2 are revoked", f)
		}
	}
	// The current function still works for the erasing role.
	run := uniqueID(t, "after-")
	r.put(run, "run:complete", agent.StepValue, `"ok"`)
	tool := r.put(run, "tool:x", agent.StepToolResult, `{"pii":1}`)
	if got := r.call(run, "tool:x", tool, ms); got != "replaced" {
		t.Errorf("the current function after the migration: code %q", got)
	}
	// A second migration is harmless.
	if _, err := Migrate(ctx, rv120DSN(t, base, "rr_owner", ""), WithSchema("rrschema")); err != nil {
		t.Fatalf("a second migration: %v", err)
	}
}
