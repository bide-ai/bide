package postgreslog

// The next_seq function's lookup, body and owner against schema names, overloads and other
// roles. (From the reviews of #113.)

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// rvPassword is the test roles' password: servers that authenticate TCP connections by password
// (the CI service, say) refuse a role that has none.
const rvPassword = "rv113b-test"

func rvAdmin(t *testing.T) (*sql.DB, string) {
	t.Helper()
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	return admin, base
}

func rvDSN(t *testing.T, base, user, searchPath string) string {
	t.Helper()
	i, j := strings.Index(base, "://"), strings.Index(base, "@")
	if i < 0 || j < 0 {
		t.Skip("PG_DSN has no user@ part")
	}
	if user != "" {
		base = base[:i+3] + user + ":" + rvPassword + base[j:]
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "search_path=" + searchPath
}

// A fresh log in a schema whose name has an upper-case letter: nextSeqPresent casts
// current_schema() to regnamespace, which folds the name to lower case, so Open fails.
func TestOpen_InMixedCaseSchema(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx := context.Background()
	schema := fmt.Sprintf("Rv113bLog_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`) })
	l, err := Open(ctx, rvDSN(t, base, "", "%22"+schema+"%22"))
	if err != nil {
		t.Fatalf("Open in schema %q: %v", schema, err)
	}
	defer l.Close()
	if _, err := l.Append(ctx, "e", "id1", "ev"); err != nil {
		t.Fatal(err)
	}
}

// hashtextextended(e, 0) in governed_events_next_seq_v1 resolves to a hashtextextended(text,
// integer) that another role created in any schema on the log's search path.
func TestAppend_NextSeqResistsOverloadHijack(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx := context.Background()
	role, attacker := "rv113b_logrole", "rv113b_logattacker"
	for _, r := range []string{role, attacker} {
		admin.ExecContext(ctx, `DROP OWNED BY `+r)
		admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+r)
		if _, err := admin.ExecContext(ctx, `CREATE ROLE `+r+` LOGIN PASSWORD '`+rvPassword+`'`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			admin.ExecContext(context.Background(), `DROP OWNED BY `+r+` CASCADE`)
			admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+r)
		})
	}
	sfx := time.Now().UnixNano()
	app, shared := fmt.Sprintf("rv_logapp_%d", sfx), fmt.Sprintf("rv_logshared_%d", sfx)
	for _, q := range []string{
		`CREATE SCHEMA ` + app + ` AUTHORIZATION ` + role,
		`CREATE SCHEMA ` + shared,
		`GRANT USAGE, CREATE ON SCHEMA ` + shared + ` TO PUBLIC`,
	} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP SCHEMA `+app+` CASCADE`)
		admin.ExecContext(context.Background(), `DROP SCHEMA `+shared+` CASCADE`)
	})
	att, err := sql.Open("pgx", rvDSN(t, base, attacker, shared))
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()
	for _, q := range []string{
		`CREATE TABLE ` + shared + `.pwned (who text, entity text)`,
		`GRANT INSERT ON ` + shared + `.pwned TO PUBLIC`,
		`CREATE FUNCTION ` + shared + `.hashtextextended(t text, s integer) RETURNS bigint LANGUAGE plpgsql AS $$
			BEGIN INSERT INTO ` + shared + `.pwned VALUES (current_user, t); RETURN 1; END $$`,
	} {
		if _, err := att.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	l, err := Open(ctx, rvDSN(t, base, role, app+","+shared))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append(ctx, "e", "id1", "ev"); err != nil {
		t.Fatal(err)
	}
	var who sql.NullString
	if err := admin.QueryRowContext(ctx, `SELECT max(who) FROM `+shared+`.pwned`).Scan(&who); err != nil {
		t.Fatal(err)
	}
	if who.Valid {
		t.Fatalf("Append ran %s's hashtextextended(text, integer) as role %q", attacker, who.String)
	}
}

// rvRole creates a login role for the test and drops it afterwards.
func rvRole(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	ctx := context.Background()
	admin.ExecContext(ctx, `DROP OWNED BY `+name)
	admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+name)
	if _, err := admin.ExecContext(ctx, `CREATE ROLE `+name+` LOGIN PASSWORD '`+rvPassword+`'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP OWNED BY `+name+` CASCADE`)
		admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+name)
	})
}

// logDefinition returns the CREATE FUNCTION statement of the next_seq function this version
// creates in schema: Open creates it in a scratch schema, and pg_get_functiondef prints it there,
// with the scratch schema's name replaced by schema's.
func logDefinition(t *testing.T, admin *sql.DB, base, schema string) string {
	t.Helper()
	ctx := context.Background()
	scratch := fmt.Sprintf("rv_logdef_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+scratch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+scratch+` CASCADE`) })
	l, err := Open(ctx, rvDSN(t, base, "", scratch))
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	var def string
	if err := admin.QueryRowContext(ctx, `SELECT pg_get_functiondef($1::regprocedure)`, scratch+"."+nextSeqFunction+"(text)").Scan(&def); err != nil {
		t.Fatal(err)
	}
	def = strings.Replace(def, "CREATE OR REPLACE FUNCTION", "CREATE FUNCTION", 1)
	return strings.ReplaceAll(def, scratch, schema)
}

// Open refuses a next_seq function that another role owns, even with the exact definition: its
// owner could replace it after Open checked it, and Append would run the owner's code. The
// function must be owned by the role that owns governed_events. Skips without PG_DSN.
func TestOpen_RefusesNextSeqOwnedByAnotherRole(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx := context.Background()
	role, other := "rv113b_logowner", "rv113b_logother"
	rvRole(t, admin, role)
	rvRole(t, admin, other)
	app := fmt.Sprintf("rv_logapp2_%d", time.Now().UnixNano())
	for _, q := range []string{
		`CREATE SCHEMA ` + app + ` AUTHORIZATION ` + role,
		`GRANT USAGE, CREATE ON SCHEMA ` + app + ` TO ` + other,
	} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+app+` CASCADE`) })
	def := logDefinition(t, admin, base, app)
	o, err := sql.Open("pgx", rvDSN(t, base, other, app))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if _, err := o.ExecContext(ctx, def); err != nil {
		t.Fatal(err)
	}
	l, err := Open(ctx, rvDSN(t, base, role, app))
	if err == nil {
		l.Close()
		t.Fatalf("Open accepted %s owned by %s", nextSeqFunction, other)
	}
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("Open = %v, want an ErrConfig about the function's owner", err)
	}
}

// The next_seq lookup in migrate must see a function another process created while this one
// waited for the migration lock, on a connection whose catalog cache looked it up before and found
// it missing: taking an advisory lock does not refresh the cache, so the lookup reads pg_proc with
// the statement's snapshot. Kills the mutant that asks to_regprocedure. Skips without PG_DSN.
func TestOpen_MigrateSeesNextSeqCreatedWhileWaiting(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx := context.Background()
	schema := fmt.Sprintf("rv_logcc_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })
	def := logDefinition(t, admin, base, schema)
	dsn := rvDSN(t, base, "", schema)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	var missing bool
	if err := db.QueryRowContext(ctx, `SELECT to_regprocedure($1) IS NULL`, nextSeqFunction+"(text)").Scan(&missing); err != nil || !missing {
		t.Fatalf("warm-up: missing=%v err=%v", missing, err)
	}
	l, err := newLog(ctx, db) // records the schema, before the other process creates the function
	if err != nil {
		t.Fatal(err)
	}
	other, err := admin.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback()
	if _, err := other.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockKey); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(ctx, def); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := l.migrate(ctx)
		if err == nil {
			err = l.checkSchema(ctx)
		}
		done <- err
	}()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for deadline := time.Now().Add(20 * time.Second); ; <-tick.C {
		var n int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
			AND ((classid::bigint << 32) | objid::bigint) = $1`, migrateLockKey).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the migration never waited for the lock")
		}
	}
	if err := other.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("migrate after another process created the function while it waited: %v", err)
	}
}

// A log whose table is in a schema later on the search path keeps using it when an earlier schema
// exists and is empty (a "$user" schema created after the log, say): Open finds the table the
// search path resolves, and creates next_seq beside it, rather than a new table in the first
// schema. Skips without PG_DSN.
func TestOpen_KeepsTheTableTheSearchPathFinds(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx := context.Background()
	sfx := time.Now().UnixNano()
	first, second := fmt.Sprintf("rv_first_%d", sfx), fmt.Sprintf("rv_second_%d", sfx)
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+first+` CASCADE`)
		admin.ExecContext(context.Background(), `DROP SCHEMA `+second+` CASCADE`)
	})
	l, err := Open(ctx, rvDSN(t, base, "", second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ctx, "e", "a", "ea"); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+first); err != nil {
		t.Fatal(err)
	}
	l, err = Open(ctx, rvDSN(t, base, "", first+","+second))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	events, err := l.Events(ctx, "e", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("Events = %v, %v, want the event recorded before the empty schema was added", events, err)
	}
	var tables int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1`, first).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("Open created %d objects in the empty schema %s", tables, first)
	}
	if seq, err := l.Append(ctx, "e", "b", "eb"); err != nil || seq != 1 {
		t.Fatalf("Append = %d, %v, want position 1 in the existing table", seq, err)
	}
}
