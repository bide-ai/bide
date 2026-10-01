package postgres

import (
	"context"
	"strings"
	"testing"
)

// Open creates the lapsed listing's two indexes on the leases table, and creates them on a leases
// table that lacks them (one a v0.9.0 store created), so a lapsed page reads the lapsed leases by
// expiry, or many of them in the listing's order, rather than scanning and sorting the table.
// Skips without PG_DSN.
func TestPostgres_OpenCreatesTheLapsedListingIndexes(t *testing.T) {
	ctx := context.Background()
	dsn, schema, admin := freshSchema(t)
	want := map[string]string{
		"bide_leases_expiry": "(expiry)",
		"bide_leases_run_c":  `(run_id COLLATE "C")`,
	}
	check := func() {
		t.Helper()
		for name, cols := range want {
			var def string
			if err := admin.QueryRowContext(ctx, `SELECT pg_get_indexdef(c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relname = $1 AND n.nspname = $2 AND c.relkind = 'i'`, name, schema).Scan(&def); err != nil {
				t.Fatalf("index %s.%s: %v", schema, name, err)
			}
			if !strings.Contains(def, "ON "+schema+".bide_leases") || !strings.HasSuffix(def, cols) {
				t.Fatalf("index %s is %q, want one on %s.bide_leases %s", name, def, schema, cols)
			}
		}
	}
	s, err := Open(ctx, dsn, WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	check()
	for name := range want {
		if _, err := admin.ExecContext(ctx, `DROP INDEX `+schema+`.`+name); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(ctx, dsn, WithSchema(schema))
	if err != nil {
		t.Fatalf("Open over a leases table without the indexes: %v", err)
	}
	s.Close()
	check()
}
