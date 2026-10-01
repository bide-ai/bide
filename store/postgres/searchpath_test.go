package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Nothing the store sends resolves through the search path after Open: Open records the schema
// and every statement names it. The store's only connection is pointed, after Open, at a decoy
// schema holding tables of the store's names and a next_seq that raises; every operation must
// still read and write the store's own schema, and leave the decoys untouched. Skips without
// PG_DSN.
func TestPostgres_NothingResolvesThroughSearchPathAfterOpen(t *testing.T) {
	ctx := context.Background()
	dsn, schema, admin := freshSchema(t)
	decoy := fmt.Sprintf("rv_decoy_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+decoy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+decoy+` CASCADE`) })
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(completeTables, decoy)+fmt.Sprintf(`
		CREATE TABLE %[1]s.bide_schema_version (id int PRIMARY KEY, version int NOT NULL);
		INSERT INTO %[1]s.bide_schema_version VALUES (1, 1);
		CREATE FUNCTION %[1]s.bide_next_seq_v1(r text) RETURNS bigint LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'decoy next_seq ran'; END $$;`, decoy)); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.SetMaxOpenConns(1) // one connection, which the SET below points at the decoys
	if _, err := s.db.ExecContext(ctx, `SET search_path = `+decoy); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT current_setting('search_path')`).Scan(&path); err != nil || path != decoy {
		t.Fatalf("search_path = %q, %v; want %s", path, err, decoy)
	}
	if _, _, err := s.Insert(ctx, "run", "a", []byte("a")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if e, ok, err := s.Insert(ctx, "run", "b", []byte("b")); err != nil || !ok || e.Seq != 1 {
		t.Fatalf("second Insert = %+v, %v, %v; want seq 1", e, ok, err)
	}
	if _, ok, err := s.Get(ctx, "run", "a"); err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	n := 0
	for _, err := range s.Load(ctx, "run", -1) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	runs := 0
	for _, err := range s.Runs(ctx, agent.RunFilter{Prefix: "ru", ExcludeHolding: []string{"zz"}}) {
		if err != nil {
			t.Fatal(err)
		}
		runs++
	}
	if n != 2 || runs != 1 {
		t.Fatalf("Load read %d entries and Runs %d runs, want 2 and 1", n, runs)
	}
	if ok, err := s.AcquireLease(ctx, "run", "h", time.Minute); err != nil || !ok {
		t.Fatalf("AcquireLease = %v, %v", ok, err)
	}
	if ok, err := s.RenewLease(ctx, "run", "h", time.Minute); err != nil || !ok {
		t.Fatalf("RenewLease = %v, %v", ok, err)
	}
	var leases int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM `+schema+`.bide_leases`).Scan(&leases); err != nil || leases != 1 {
		t.Fatalf("the store's schema holds %d leases (%v), want 1", leases, err)
	}
	if err := s.ReleaseLease(ctx, "run", "h"); err != nil {
		t.Fatal(err)
	}
	var steps, decoySteps, decoyLeases int
	if err := admin.QueryRowContext(ctx, fmt.Sprintf(`SELECT (SELECT count(*) FROM %[1]s.bide_steps), (SELECT count(*) FROM %[2]s.bide_steps), (SELECT count(*) FROM %[2]s.bide_leases)`, schema, decoy)).Scan(&steps, &decoySteps, &decoyLeases); err != nil {
		t.Fatal(err)
	}
	if steps != 2 || decoySteps != 0 || decoyLeases != 0 {
		t.Fatalf("the store's schema holds %d steps, the decoy schema %d steps and %d leases; want 2, 0, 0", steps, decoySteps, decoyLeases)
	}
}

// Open refuses when the first relation on the search path named like the store's steps table is
// not an ordinary or partitioned table: statements would read and write through it while Open
// checked another table. Skips without PG_DSN.
func TestPostgres_OpenRefusesANonTableSteps(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, ddl string }{
		{"view", `CREATE TABLE %[1]s.shadow (run_id text, seq bigint, name text, data bytea); CREATE VIEW %[1]s.bide_steps AS SELECT * FROM %[1]s.shadow`},
		{"sequence", `CREATE SEQUENCE %[1]s.bide_steps`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, schema, admin := freshSchema(t)
			later := fmt.Sprintf("rv_later_%d", time.Now().UnixNano())
			if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+later); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+later+` CASCADE`) })
			if _, err := admin.ExecContext(ctx, fmt.Sprintf(completeTables, later)+fmt.Sprintf(tc.ddl, schema)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, dsn+","+later)
			if err == nil {
				s.Close()
				t.Fatal("Open accepted a search path whose first bide_steps is not a table")
			}
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "bide_steps") {
				t.Fatalf("Open = %v, want an ErrConfig naming bide_steps", err)
			}
		})
	}
}

// A store whose tables are in a later schema on the search path than an empty one keeps them: Open
// records the later schema, and the migration's DDL names it, so nothing is created in the empty
// first schema and the run's entries stay where they were. Skips without PG_DSN.
func TestPostgres_OpenKeepsTablesInALaterSchema(t *testing.T) {
	ctx := context.Background()
	dsn, schema, admin := freshSchema(t)
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "run", "a", []byte("a")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	first := fmt.Sprintf("rv_emptyfirst_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+first); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+first+` CASCADE`) })
	s, err = Open(ctx, strings.Replace(dsn, "search_path="+schema, "search_path="+first+","+schema, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if e, ok, err := s.Insert(ctx, "run", "b", []byte("b")); err != nil || !ok || e.Seq != 1 {
		t.Fatalf("Insert = %+v, %v, %v; want seq 1 in the existing table", e, ok, err)
	}
	var objects int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1`, first).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if objects != 0 {
		t.Fatalf("Open created %d relations in the empty schema %s", objects, first)
	}
}
