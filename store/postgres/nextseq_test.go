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

// Open refuses a next_seq function whose definition is not the one this version creates: the
// insert's ordering depends on its lock and its fresh snapshot, and the migration never replaces
// a function other nodes may be running. Skips without PG_DSN.
func TestPostgres_OpenRefusesAnotherNextSeq(t *testing.T) {
	ctx := context.Background()
	dsn, schema, admin := freshSchema(t)
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	want := nextSeqSource(t, admin, schema, "bide_next_seq_v1")
	if want == "" {
		t.Fatal("Open created no bide_next_seq_v1")
	}
	quote := func(src string) string { return "$src$" + src + "$src$" }
	noLock := `BEGIN RETURN (SELECT COALESCE(MAX(seq), -1) + 1 FROM bide_steps WHERE run_id = r); END`
	for _, tc := range []struct{ name, def string }{
		{"no_lock", `CREATE FUNCTION %[1]s.bide_next_seq_v1(r text) RETURNS bigint LANGUAGE plpgsql VOLATILE AS ` + quote(noLock)},
		{"stable", `CREATE FUNCTION %[1]s.bide_next_seq_v1(r text) RETURNS bigint LANGUAGE plpgsql STABLE AS ` + quote(want)},
		{"security_definer", `CREATE FUNCTION %[1]s.bide_next_seq_v1(r text) RETURNS bigint LANGUAGE plpgsql VOLATILE SECURITY DEFINER AS ` + quote(want)},
		{"own_search_path", `CREATE FUNCTION %[1]s.bide_next_seq_v1(r text) RETURNS bigint LANGUAGE plpgsql VOLATILE SET search_path = public AS ` + quote(want)},
		{"returns_int", `CREATE FUNCTION %[1]s.bide_next_seq_v1(r text) RETURNS int LANGUAGE plpgsql VOLATILE AS ` + quote(want)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, schema, admin := freshSchema(t)
			if _, err := admin.ExecContext(ctx, fmt.Sprintf(completeTables+tc.def, schema)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, dsn)
			if err == nil {
				s.Close()
				t.Fatal("Open accepted a next_seq function with another definition")
			}
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "bide_next_seq_v1") {
				t.Fatalf("Open = %v, want an ErrConfig naming bide_next_seq_v1", err)
			}
		})
	}
}
