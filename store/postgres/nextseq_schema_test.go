package postgres

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

func rvAdmin(t *testing.T) (*sql.DB, string) {
	t.Helper()
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	return admin, base
}

func rvWithParams(base, params string) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + params
}

// A schema whose name has an upper-case letter is a valid search_path target, and the parent of
// #113 opens a store in it. nextSeqPresent casts current_schema() to regnamespace, which parses
// the name as an unquoted identifier and folds it to lower case, so Open fails.
func TestPostgres_OpenInMixedCaseSchema(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx := context.Background()
	schema := fmt.Sprintf("Rv113b_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`) })
	s, err := Open(ctx, rvWithParams(base, `search_path=`+`%22`+schema+`%22`))
	if err != nil {
		t.Fatalf("Open in schema %q: %v", schema, err)
	}
	defer s.Close()
	if _, _, err := s.Insert(ctx, "r", "a", []byte("a")); err != nil {
		t.Fatal(err)
	}
}

// rvRole creates a login role for the test and drops it afterwards.
func rvRole(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	ctx := context.Background()
	admin.ExecContext(ctx, `DROP OWNED BY `+name)
	admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+name)
	if _, err := admin.ExecContext(ctx, `CREATE ROLE `+name+` LOGIN`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP OWNED BY `+name+` CASCADE`)
		admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+name)
	})
}

// rvDSN returns base with the user replaced and a search_path.
func rvDSN(t *testing.T, base, user, searchPath string) string {
	t.Helper()
	i := strings.Index(base, "://")
	j := strings.Index(base, "@")
	if i < 0 || j < 0 {
		t.Skip("PG_DSN has no user@ part")
	}
	return rvWithParams(base[:i+3]+user+base[j:], "search_path="+searchPath)
}

// The function body calls hashtextextended(r, 0), whose 0 is an integer while pg_catalog's
// hashtextextended takes a bigint. A role that may create functions in any schema on the store's
// search_path, even one after the store's own schema (public on Postgres 14 and earlier, where
// every role may create there by default), defines hashtextextended(text, integer): an exact match,
// which Postgres prefers to pg_catalog's, so every insert runs that role's code with the store's
// privileges and takes whatever lock key it returns.
func TestPostgres_NextSeqResistsOverloadHijack(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx := context.Background()
	store, attacker := "rv113b_store", "rv113b_attacker"
	rvRole(t, admin, store)
	rvRole(t, admin, attacker)
	sfx := time.Now().UnixNano()
	app, shared := fmt.Sprintf("rv_app_%d", sfx), fmt.Sprintf("rv_shared_%d", sfx)
	for _, q := range []string{
		`CREATE SCHEMA ` + app + ` AUTHORIZATION ` + store,
		`CREATE SCHEMA ` + shared,
		`GRANT USAGE, CREATE ON SCHEMA ` + shared + ` TO PUBLIC`, // as public is on Postgres 14 and earlier
	} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP SCHEMA `+app+` CASCADE`)
		admin.ExecContext(context.Background(), `DROP SCHEMA `+shared+` CASCADE`)
	})
	// The store's own schema comes first; the shared one only after it.
	s, err := Open(ctx, rvDSN(t, base, store, app+","+shared))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.Insert(ctx, "r", "before", []byte("b")); err != nil {
		t.Fatal(err)
	}
	att, err := sql.Open("pgx", rvDSN(t, base, attacker, shared))
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()
	for _, q := range []string{
		`CREATE TABLE ` + shared + `.pwned (who text, run text)`,
		`GRANT INSERT ON ` + shared + `.pwned TO PUBLIC`,
		`CREATE FUNCTION ` + shared + `.hashtextextended(t text, s integer) RETURNS bigint LANGUAGE plpgsql AS $$
			BEGIN INSERT INTO ` + shared + `.pwned VALUES (current_user, t); RETURN 1; END $$`,
	} {
		if _, err := att.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// A connection that already planned the function keeps its plan; a new node's session (or a
	// pooled connection opened later) resolves hashtextextended again.
	s2, err := Open(ctx, rvDSN(t, base, store, app+","+shared))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, _, err := s2.Insert(ctx, "r", "after", []byte("a")); err != nil {
		t.Fatal(err)
	}
	var who sql.NullString
	if err := admin.QueryRowContext(ctx, `SELECT max(who) FROM `+shared+`.pwned`).Scan(&who); err != nil {
		t.Fatal(err)
	}
	if who.Valid {
		t.Fatalf("the store's insert ran %s's hashtextextended(text, integer) as role %q: the run lock's key and the code run under the store's privileges belong to another role", attacker, who.String)
	}
}

// Open verifies the function's definition but not its owner: a function with the exact body,
// created in the store's schema by another role before the store's first Open, is accepted, and
// that role can replace it afterwards (Open checks only once), so the store's inserts run its code.
func TestPostgres_OpenRefusesNextSeqOwnedByAnotherRole(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx := context.Background()
	store, other := "rv113b_store2", "rv113b_other"
	rvRole(t, admin, store)
	rvRole(t, admin, other)
	app := fmt.Sprintf("rv_app2_%d", time.Now().UnixNano())
	for _, q := range []string{
		`CREATE SCHEMA ` + app + ` AUTHORIZATION ` + store,
		`GRANT USAGE, CREATE ON SCHEMA ` + app + ` TO ` + other,
	} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+app+` CASCADE`) })
	o, err := sql.Open("pgx", rvDSN(t, base, other, app))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if _, err := o.ExecContext(ctx, nextSeqDefinition(t, app)); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, rvDSN(t, base, store, app))
	if err != nil {
		if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "owner") {
			t.Fatalf("Open = %v, want an ErrConfig about the function's owner", err)
		}
		return
	}
	defer s.Close()
	// After Open, the owner replaces the body: every insert now takes position 0.
	if _, err := o.ExecContext(ctx, `CREATE OR REPLACE FUNCTION bide_next_seq_v1(r text) RETURNS bigint LANGUAGE plpgsql VOLATILE AS $$ BEGIN RETURN 0; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "r", "a", []byte("a")); err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _, err = s.Insert(c, "r", "b", []byte("b"))
	t.Errorf("Open accepted bide_next_seq_v1 owned by %s; after its owner replaced it, the next Insert = %v (it collides on seq 0 until ctx ends)", other, err)
}
