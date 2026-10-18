package postgres

// The redaction function, called as the role that holds nothing but EXECUTE on it: raw SQL cannot
// make it write anything a redaction would not, whatever the arguments.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

type redactRig struct {
	app   *Store // the service role: appends
	call  func(run, name string, cur []byte, ms int64) string
	raw   *sql.DB // the redaction role's connection
	seen  map[string]bool
	admin *sql.DB
	t     *testing.T
}

func newRedactRig(t *testing.T) *redactRig {
	t.Helper()
	admin, base := rv120Admin(t)
	ctx := context.Background()
	const schema, owner, app, redactor = "rrschema", "rr_owner", "rr_app", "rr_redactor"
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
		`GRANT SELECT, INSERT ON `+schema+`.bide_steps, `+schema+`.bide_events TO `+app,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON `+schema+`.bide_leases TO `+app,
		`GRANT SELECT ON `+schema+`.bide_schema_version TO `+app,
		`GRANT EXECUTE ON FUNCTION `+schema+`.bide_next_seq_v1(text) TO `+app,
		`GRANT EXECUTE ON FUNCTION `+schema+`.bide_redact_v2(text, text, bytea, bigint) TO `+redactor,
	)
	s, err := Open(ctx, rv120DSN(t, base, app, ""), WithSchema(schema), WithoutMigration())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	raw, err := sql.Open("pgx", rv120DSN(t, base, redactor, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	r := &redactRig{app: s, raw: raw, t: t, seen: map[string]bool{}, admin: admin}
	r.call = func(run, name string, cur []byte, ms int64) string {
		t.Helper()
		var code string
		if err := raw.QueryRowContext(ctx, `SELECT `+schema+`.bide_redact_v2($1, $2, $3, $4)`, run, name, cur, ms).Scan(&code); err != nil {
			t.Fatalf("call: %v", err)
		}
		return code
	}
	return r
}

func (r *redactRig) put(run, name string, kind agent.StepKind, result string) []byte {
	r.t.Helper()
	if !r.seen[run] { // a journal starts with its header, which the journal writes
		r.seen[run] = true
		if _, err := agentJournal(r.t, r.app).Step(context.Background(), run, "seed", func(context.Context) (int, error) { return 1, nil }); err != nil {
			r.t.Fatal(err)
		}
	}
	b, err := agent.JournalEntry(name, agent.Record{Kind: kind, Result: []byte(result)})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, _, err := r.app.Insert(context.Background(), run, name, b); err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *redactRig) stored(run, name string) []byte {
	e, ok, err := r.app.Get(context.Background(), run, name)
	if err != nil || !ok {
		r.t.Fatalf("Get %s: %v %v", name, ok, err)
	}
	return e.Data
}

func TestRedactFunction_RawCallsAreRefused(t *testing.T) {
	r := newRedactRig(t)
	ctx := context.Background()
	const ms = 1_700_000_000_000

	// A live run: nothing is replaced, whatever the record.
	live := uniqueID(t, "live-")
	for name, kind := range map[string]agent.StepKind{"attempt:tool:x": agent.StepAttempt, "run:start": agent.StepValue, "tool:x": agent.StepToolResult} {
		b := r.put(live, name, kind, `1`)
		if got := r.call(live, name, b, ms); got != "not_terminal" && got != "protected" {
			t.Errorf("live %s: code %q, want a refusal", name, got)
		}
		if !bytes.Equal(r.stored(live, name), b) {
			t.Errorf("live %s: the row changed", name)
		}
	}

	// A finished run: protected records, a stale copy, and malformed arguments are refused.
	run := uniqueID(t, "over-")
	end := r.put(run, "run:complete", agent.StepValue, `"ok"`)
	tool := r.put(run, "tool:x", agent.StepToolResult, `{"pii":1}`)
	expect := func(name string, cur []byte, at int64, want string) {
		t.Helper()
		before := r.stored(run, name)
		if got := r.call(run, name, cur, at); got != want {
			t.Errorf("%s: code %q, want %q", name, got, want)
		}
		if !bytes.Equal(r.stored(run, name), before) {
			t.Errorf("%s: a refused call changed the row", name)
		}
	}
	expect("run:complete", end, ms, "protected")
	expect("tool:x", tool, -1, "mismatch")
	expect("tool:x", tool, 253402300800000, "mismatch")
	expect("tool:x", []byte(`garbage`), ms, "protected")
	expect("tool:x", []byte{0xff, 0xfe}, ms, "protected")
	other := r.put(run, "tool:y", agent.StepToolResult, `{"pii":2}`)
	expect("tool:x", other, ms, "mismatch") // another record's bytes: its name is not this row's
	stale, _ := agent.JournalEntry("tool:x", agent.Record{Kind: agent.StepToolResult, Result: []byte(`{"pii":"other"}`)})
	expect("tool:x", stale, ms, "changed")
	ghost, _ := agent.JournalEntry("tool:ghost", agent.Record{Kind: agent.StepToolResult, Result: []byte(`1`)})
	if got := r.call(run, "tool:ghost", ghost, ms); got != "no_entry" {
		t.Errorf("a missing entry: code %q", got)
	}

	// A lease, a live parent, a session.
	if ok, err := r.app.AcquireLease(ctx, run, "driver", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	expect("tool:x", tool, ms, "leased")
	if err := r.app.ReleaseLease(ctx, run, "driver"); err != nil {
		t.Fatal(err)
	}
	parent := uniqueID(t, "parent-")
	r.put(parent, "tool:p", agent.StepToolResult, `1`)
	child := agent.SubRunID(parent, "c1")
	r.put(child, "run:complete", agent.StepValue, `"ok"`)
	ct := r.put(child, "tool:x", agent.StepToolResult, `{"pii":1}`)
	if got := r.call(child, "tool:x", ct, ms); got != "parent_live" {
		t.Errorf("a child of a live parent: code %q", got)
	}
	turn := uniqueID(t, "s-") + ">@turn/1"
	r.put(turn, "run:complete", agent.StepValue, `"ok"`)
	tt := r.put(turn, "tool:x", agent.StepToolResult, `{"pii":1}`)
	if got := r.call(turn, "tool:x", tt, ms); got != "session" {
		t.Errorf("a session turn: code %q", got)
	}

	// The call that is allowed replaces the entry with a tombstone the Go readers read, once.
	if got := r.call(run, "tool:x", tool, ms); got != "replaced" {
		t.Fatalf("a valid call: code %q", got)
	}
	got := r.stored(run, "tool:x")
	if h, ok := agent.TombstoneLeafHash(got); !ok || !bytes.Equal(h, agent.LeafHash(tool)) {
		t.Fatalf("the stored bytes %s are not a tombstone of the record", got)
	}
	if n, _ := agent.TombstoneName(got); n != "tool:x" {
		t.Errorf("the tombstone names %q", n)
	}
	if code := r.call(run, "tool:x", tool, ms); code != "already" {
		t.Errorf("a repeated call: code %q, want already", code)
	}
	if code := r.call(run, "tool:x", r.stored(run, "tool:x"), ms); code != "protected" {
		t.Errorf("a call that passes a tombstone as the bytes read: code %q, want protected", code)
	}
	rec, ok, err := agentJournal(t, r.app).Get(ctx, run, "tool:x")
	if err != nil || !ok || !rec.Redacted {
		t.Fatalf("the journal reads %+v, %v, %v", rec, ok, err)
	}
}

func agentJournal(t *testing.T, s *Store) *agent.Journal {
	t.Helper()
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// Names with characters JSON escapes differently in Go and in Postgres still produce a tombstone
// the Go readers accept, and read back under the row's name.
func TestRedactFunction_NamesWithAwkwardCharacters(t *testing.T) {
	r := newRedactRig(t)
	ctx := context.Background()
	run := uniqueID(t, "names-")
	r.put(run, "run:complete", agent.StepValue, `"ok"`)
	for i, name := range []string{"step <a&b>", "step \"quoted\"", "step\\back", "step é世\U0001f600", "step   sep", "step\ttab"} {
		name := fmt.Sprintf("%d %s", i, name)
		b := r.put(run, name, agent.StepValue, `"x"`)
		if code := r.call(run, name, b, 5); code != "replaced" {
			t.Errorf("%q: code %q", name, code)
			continue
		}
		rec, ok, err := agentJournal(t, r.app).Get(ctx, run, name)
		if err != nil || !ok || !rec.Redacted || rec.Name != name {
			t.Errorf("%q reads back as %+v, %v, %v", name, rec, ok, err)
		}
		if n, ok := agent.TombstoneName(r.stored(run, name)); !ok || n != name {
			t.Errorf("%q: the tombstone names %q", name, n)
		}
	}
}

// The function classifies record names as agent.RedactionProtected does.
func TestRedactFunctionMatchesGo(t *testing.T) {
	r := newRedactRig(t)
	run := uniqueID(t, "diff-")
	r.put(run, "run:complete", agent.StepValue, `"ok"`)
	names := []string{
		"@journal", "@saga/compensate/c", "@saga/args/c", "@subrun/a/b", "@spend/x", "@spend-late/x", "@llm/0", "@retrieval/0",
		"run:start", "run:startx", "run:start:2", "run:aborted", "run:cancelled", "run:cancel-requested", "run:limits:1", "run:principal",
		"tool:x", "attempt:tool:x", "attempt:not-started:c:m", "approval:c", "approval-tally:c", "approval-identity:c", "approval-status:h",
		"approval-exec-hold:c", "approval-deadline:c", "approval-pending:c", "approval-pause:c:1", "approval-request:c",
		"signal:s", "await-timeout:s", "await-resolved:s", "timer:t", "interrupt:i", "chan:c", "chanack:c",
		"turn/1", "start/1", "from/1", "audit:policy:D1", "node:n", "switch:s", "flow:digest", "user step", "runs", "audit", "attempts",
	}
	kinds := []agent.StepKind{agent.StepValue, agent.StepToolResult, agent.StepModel, agent.StepApproval, agent.StepAttempt, agent.StepNotStarted, agent.StepSagaFail, agent.StepSignal}
	for _, name := range names {
		for _, kind := range kinds {
			rn := fmt.Sprintf("%s-%s-%s", run, kind, name)
			r.put(rn, "run:complete", agent.StepValue, `"ok"`)
			b := r.put(rn, name, kind, `1`)
			want := "replaced"
			if agent.RedactionProtected(name, kind) != "" {
				want = "protected"
			}
			if name == "@journal" || name == "run:complete" {
				continue
			}
			if got := r.call(rn, name, b, 1); got != want {
				t.Errorf("%q as %s: the function says %q, agent.RedactionProtected says %q", name, kind, got, want)
			}
		}
	}
}

// Only the roles granted EXECUTE can erase: not the service role, not PUBLIC.
func TestRedactFunction_NeedsAGrant(t *testing.T) {
	r := newRedactRig(t)
	ctx := context.Background()
	run := uniqueID(t, "grant-")
	r.put(run, "run:complete", agent.StepValue, `"ok"`)
	b := r.put(run, "tool:x", agent.StepToolResult, `1`)
	app, err := sql.Open("pgx", rv120DSNFor(t, "rr_app"))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	var code string
	err = app.QueryRowContext(ctx, `SELECT rrschema.bide_redact_v2($1, $2, $3, $4)`, run, "tool:x", b, int64(1)).Scan(&code)
	if err == nil || sqlState(err) != "42501" {
		t.Fatalf("the service role called the redaction function: %q, %v; want permission denied", code, err)
	}
	if !bytes.Equal(r.stored(run, "tool:x"), b) {
		t.Fatal("the row changed")
	}
	// The Store the service role opened says what to do rather than failing opaquely.
	tomb, _ := agent.NewTombstone("tool:x", b, time.UnixMilli(1))
	if done, err := r.app.Redact(ctx, run, "tool:x", b, tomb); done || err == nil {
		t.Fatalf("Redact as the service role = %v, %v", done, err)
	}
}

// Open checks the function's definition: one that is not the one this version creates is refused,
// and a schema migrated before it existed opens, and gets it from the next migration.
func TestRedactFunction_DefinitionIsChecked(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	const schema, owner = "rdschema", "rd_owner"
	rv120Role(t, admin, owner)
	admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	rv120Exec(t, admin, `CREATE SCHEMA `+schema+` AUTHORIZATION `+owner)
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`) })
	dsn := rv120DSN(t, base, owner, "")
	res, err := Migrate(ctx, dsn, WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	if !containsPrefix(res.Created, "function \""+schema+"\".bide_redact_v2") {
		t.Fatalf("the migration created %v, want the redaction function", res.Created)
	}
	if res, err = Migrate(ctx, dsn, WithSchema(schema)); err != nil || containsPrefix(res.Created, "function") {
		t.Fatalf("a second migration = %+v, %v; want nothing created", res, err)
	}
	// A schema from before the function existed opens, and is brought up by the migration.
	rv120Exec(t, admin, `DROP FUNCTION `+schema+`.bide_redact_v2(text, text, bytea, bigint)`)
	s, err := Open(ctx, dsn, WithSchema(schema), WithoutMigration())
	if err != nil {
		t.Fatalf("a schema without the redaction function does not open: %v", err)
	}
	s.Close()
	if res, err = Migrate(ctx, dsn, WithSchema(schema)); err != nil || !containsPrefix(res.Created, "function") {
		t.Fatalf("migration of a schema without the function = %+v, %v", res, err)
	}
	// A function of that name with another body is refused.
	rv120Exec(t, admin, `CREATE OR REPLACE FUNCTION `+schema+`.bide_redact_v2(r text, n text, cur bytea, ms bigint) RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN 'replaced'; END $$`)
	if s, err := Open(ctx, dsn, WithSchema(schema), WithoutMigration()); err == nil {
		s.Close()
		t.Fatal("Open accepted a redaction function with another body")
	} else if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Open = %v, want ErrConfig", err)
	}
}

func containsPrefix(ss []string, p string) bool {
	for _, s := range ss {
		if len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}

func rv120DSNFor(t *testing.T, user string) string {
	_, base := rv120Admin(t)
	return rv120DSN(t, base, user, "")
}

// A legal hold is enforced by the function, for the role that has nothing but EXECUTE: the hold is in
// the database, the role cannot read or change it, and the function says "held". Lifting it lets the
// same call through. A hold placed through the store's own methods (a role with the hold table's
// privileges) is read the same way.
func TestRedactFunction_ARowInTheHoldsTableHoldsTheRun(t *testing.T) {
	r := newRedactRig(t)
	ctx := context.Background()
	const ms = 1_700_000_000_000
	run := uniqueID(t, "held-")
	r.put(run, "run:complete", agent.StepValue, `"ok"`)
	tool := r.put(run, "tool:x", agent.StepToolResult, `{"pii":1}`)
	child := agent.SubRunID(run, "c1")
	r.put(child, "run:complete", agent.StepValue, `"ok"`)
	childTool := r.put(child, "tool:y", agent.StepToolResult, `{"pii":2}`)
	r.put(run, "tool:z", agent.StepToolResult, `{"pii":3}`)

	if _, err := r.admin.ExecContext(ctx, `INSERT INTO rrschema.bide_holds (hold_id, run_id, tree, matter, placed_at_ms) VALUES ('h1', $1, true, 'case', 1)`, run); err != nil {
		t.Fatal(err)
	}
	if got := r.call(run, "tool:x", tool, ms); got != "held" {
		t.Errorf("a held run: code %q, want held", got)
	}
	if got := r.call(child, "tool:y", childTool, ms); got != "held" {
		t.Errorf("a sub-run under a tree hold: code %q, want held", got)
	}
	// The redaction role can neither read nor change the holds.
	for _, q := range []string{`SELECT * FROM rrschema.bide_holds`, `DELETE FROM rrschema.bide_holds`, `INSERT INTO rrschema.bide_holds VALUES ('x','y',false,'m',1)`} {
		if _, err := r.raw.ExecContext(ctx, q); err == nil {
			t.Errorf("the redaction role ran %q", q)
		}
	}
	if _, err := r.admin.ExecContext(ctx, `DELETE FROM rrschema.bide_holds WHERE hold_id = 'h1'`); err != nil {
		t.Fatal(err)
	}
	if got := r.call(run, "tool:x", tool, ms); got != "replaced" {
		t.Errorf("after the lift: code %q, want replaced", got)
	}
}

// A schema whose redaction function is gone (a schema migrated before holds has only the first
// version, which does not check them) is told to migrate, not erased through.
func TestRedact_WithoutTheFunctionSaysToMigrate(t *testing.T) {
	r := newRedactRig(t)
	ctx := context.Background()
	run := uniqueID(t, "nofn-")
	r.put(run, "run:complete", agent.StepValue, `"ok"`)
	tool := r.put(run, "tool:x", agent.StepToolResult, `{"pii":1}`)
	if _, err := r.admin.ExecContext(ctx, `DROP FUNCTION rrschema.bide_redact_v2(text, text, bytea, bigint)`); err != nil {
		t.Fatal(err)
	}
	// An older function of the first version, which a schema from before holds still has, is not called.
	if _, err := r.admin.ExecContext(ctx, `CREATE FUNCTION rrschema.bide_redact_v1(r text, n text, cur bytea, ms bigint) RETURNS text LANGUAGE sql AS $$ SELECT 'replaced' $$`); err != nil {
		t.Fatal(err)
	}
	tomb, _ := agent.NewTombstone("tool:x", tool, time.UnixMilli(1))
	rd := r.app
	if done, err := rd.Redact(ctx, run, "tool:x", tool, tomb); done || !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("Redact without the function = %v, %v; want ErrConfig telling to run the migration", done, err)
	}
	if !bytes.Equal(r.stored(run, "tool:x"), tool) {
		t.Fatal("the row changed")
	}
}
