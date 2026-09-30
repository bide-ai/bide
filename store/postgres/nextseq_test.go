package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// completeTables creates the store's tables in schema, with every uniqueness Open requires, and
// no next_seq function.
const completeTables = `CREATE TABLE %[1]s.bide_steps (run_id text COLLATE "C" NOT NULL, seq bigint NOT NULL, name text NOT NULL, data bytea NOT NULL, PRIMARY KEY (run_id, name), UNIQUE (run_id, seq));
	CREATE TABLE %[1]s.bide_leases (run_id text PRIMARY KEY, holder text NOT NULL, expiry timestamptz NOT NULL);`

// nextSeqSource returns the source of schema's next_seq function named fn, or "" if there is none.
func nextSeqSource(t *testing.T, admin *sql.DB, schema, fn string) string {
	t.Helper()
	var src sql.NullString
	if err := admin.QueryRowContext(context.Background(), `SELECT prosrc FROM pg_proc WHERE oid = to_regprocedure($1)`,
		schema+"."+fn+"(text)").Scan(&src); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return src.String
}

// Open creates the next_seq function the insert calls, in a new schema and beside tables an
// earlier version created, under the table prefix. Skips without PG_DSN.
func TestPostgres_OpenCreatesNextSeq(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, prefix, ddl string
	}{
		{"new_schema", "bide_", ""},
		{"existing_tables", "bide_", completeTables},
		{"prefix", "app_", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, schema, admin := freshSchema(t)
			if tc.ddl != "" {
				if _, err := admin.ExecContext(ctx, fmt.Sprintf(tc.ddl, schema)); err != nil {
					t.Fatal(err)
				}
			}
			s, err := Open(ctx, dsn, WithTablePrefix(tc.prefix))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			src := nextSeqSource(t, admin, schema, tc.prefix+"next_seq_v1")
			if !strings.Contains(src, "pg_advisory_xact_lock") || !strings.Contains(src, tc.prefix+"steps") {
				t.Fatalf("%snext_seq_v1 = %q, want a function that locks the run and reads %ssteps", tc.prefix, src, tc.prefix)
			}
			if _, _, err := s.Insert(ctx, "r", "a", []byte("a")); err != nil {
				t.Fatalf("Insert through the created function: %v", err)
			}
			// A second Open finds the function and leaves it.
			s2, err := Open(ctx, dsn, WithTablePrefix(tc.prefix))
			if err != nil {
				t.Fatalf("a second Open: %v", err)
			}
			s2.Close()
		})
	}
}

// nextSeqDefinition returns the CREATE FUNCTION statement of the next_seq function this version
// creates in schema: Open creates it in a scratch schema, and pg_get_functiondef prints it there,
// with the scratch schema's name replaced by schema's.
func nextSeqDefinition(t *testing.T, schema string) string {
	t.Helper()
	dsn, scratch, admin := freshSchema(t)
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	var def string
	if err := admin.QueryRowContext(context.Background(), `SELECT pg_get_functiondef($1::regprocedure)`, scratch+".bide_next_seq_v1(text)").Scan(&def); err != nil {
		t.Fatal(err)
	}
	def = strings.Replace(def, "CREATE OR REPLACE FUNCTION", "CREATE FUNCTION", 1)
	return strings.ReplaceAll(def, scratch, schema)
}

// Open refuses a next_seq function whose definition is not the one this version creates: the
// insert's ordering depends on its lock and its fresh snapshot, its safety on its fixed search
// path and qualified names, and the migration never replaces a function other nodes may be
// running. Each case changes one property of the exact definition. Skips without PG_DSN.
func TestPostgres_OpenRefusesAnotherNextSeq(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		old, new string // replaced once in the exact definition
	}{
		{"no_lock", "pg_advisory_xact_lock", "pg_advisory_xact_lock_shared"},
		{"stable", "LANGUAGE plpgsql", "LANGUAGE plpgsql STABLE"},
		{"security_definer", "LANGUAGE plpgsql", "LANGUAGE plpgsql SECURITY DEFINER"},
		{"another_search_path", "LANGUAGE plpgsql", "LANGUAGE plpgsql SET search_path = public"},
		{"no_search_path", " SET search_path TO 'pg_catalog', 'pg_temp'\n", ""},
		{"returns_int", "RETURNS bigint", "RETURNS integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, schema, admin := freshSchema(t)
			def := nextSeqDefinition(t, schema)
			if tc.name == "another_search_path" {
				def = strings.Replace(def, " SET search_path TO 'pg_catalog', 'pg_temp'\n", "", 1)
			}
			changed := strings.Replace(def, tc.old, tc.new, 1)
			if changed == def && tc.name != "no_search_path" {
				t.Fatalf("the definition holds no %q:\n%s", tc.old, def)
			}
			if _, err := admin.ExecContext(ctx, fmt.Sprintf(completeTables, schema)+changed); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, dsn)
			if err == nil {
				s.Close()
				t.Fatalf("Open accepted a next_seq function with another definition:\n%s", changed)
			}
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "bide_next_seq_v1") {
				t.Fatalf("Open = %v, want an ErrConfig naming bide_next_seq_v1", err)
			}
		})
	}
}
