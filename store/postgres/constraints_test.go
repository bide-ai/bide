package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// Open refuses tables that lack a uniqueness the store's single statements depend on, since the
// migration never alters an existing table. Without UNIQUE (run_id, seq), two inserts that read
// the same MAX(seq) would both commit at one position; without uniqueness on (run_id, name), a
// step could be recorded twice (and ON CONFLICT (run_id, name) has no arbiter); without a primary
// key on the leases' run_id, AcquireLease has no arbiter either. Each must be an immediate,
// non-partial unique index on exactly those columns. Skips without PG_DSN.
func TestPostgres_OpenRefusesTablesWithoutTheirUniqueness(t *testing.T) {
	const leases = `CREATE TABLE %s.bide_leases (run_id text PRIMARY KEY, holder text NOT NULL, expiry timestamptz NOT NULL);`
	steps := func(constraints string) string {
		return `CREATE TABLE %s.bide_steps (run_id text COLLATE "C" NOT NULL, seq bigint NOT NULL, name text NOT NULL, data bytea NOT NULL` + constraints + `);`
	}
	for _, tc := range []struct {
		name, ddl, want string // want is empty when Open must succeed
	}{
		{"complete", steps(`, PRIMARY KEY (run_id, name), UNIQUE (run_id, seq)`) + leases, ""},
		{"columns_in_another_order", steps(`, UNIQUE (name, run_id), UNIQUE (seq, run_id)`) + leases, ""},
		{"no_seq_uniqueness", steps(`, PRIMARY KEY (run_id, name)`) + leases, "(run_id, seq)"},
		{"no_name_uniqueness", steps(`, PRIMARY KEY (run_id, seq)`) + leases, "(run_id, name)"},
		{"seq_uniqueness_too_wide", steps(`, PRIMARY KEY (run_id, name), UNIQUE (run_id, seq, name)`) + leases, "(run_id, seq)"},
		{"seq_uniqueness_deferrable", steps(`, PRIMARY KEY (run_id, name), UNIQUE (run_id, seq) DEFERRABLE INITIALLY DEFERRED`) + leases, "(run_id, seq)"},
		{"seq_uniqueness_partial", steps(`, PRIMARY KEY (run_id, name)`) + leases + `CREATE UNIQUE INDEX ON %s.bide_steps (run_id, seq) WHERE seq > 0;`, "(run_id, seq)"},
		{"no_lease_key", steps(`, PRIMARY KEY (run_id, name), UNIQUE (run_id, seq)`) + `CREATE TABLE %s.bide_leases (run_id text NOT NULL, holder text NOT NULL, expiry timestamptz NOT NULL);`, "(run_id)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dsn, schema, admin := freshSchema(t)
			if _, err := admin.ExecContext(ctx, strings.ReplaceAll(tc.ddl, "%s", schema)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, dsn)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Open = %v, want it to accept the tables", err)
				}
				s.Close()
				return
			}
			if err == nil {
				s.Close()
				t.Fatalf("Open accepted tables without a unique %s", tc.want)
			}
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open = %v, want an ErrConfig naming %s", err, tc.want)
			}
		})
	}
}
