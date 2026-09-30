package postgres

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// newSelect accepts only a query that starts with the SELECT keyword, so the statement check can
// trust a selectSQL on the pool.
func TestNewSelect(t *testing.T) {
	for _, q := range []string{"SELECT 1", "  select x FROM t", "SELECT\n1", "SELECT pg_catalog.max(seq), pg_catalog.now() FROM t WHERE x = ANY($1) AND c = 'f(x)'", `SELECT run_id COLLATE "C" FROM t`} {
		if got, err := newSelect(q); err != nil || string(got) != q {
			t.Errorf("newSelect(%q) = %q, %v", q, got, err)
		}
	}
	for _, q := range []string{"UPDATE t SET x = 1", "SELECTED", "", "WITH x AS (DELETE FROM t) SELECT 1", "SELECT", "SELECT 1; BEGIN", "SELECT pg_advisory_lock(1)", "SELECT pg_try_advisory_lock_shared(1)", "SELECT dblink_exec('x', 'BEGIN')", "SELECT bide_next_seq_v1('r')", "SELECT evil (1)", "SELECT pg_advisory_xact_lock(1)",
		"SELECT max(seq) FROM t", "SELECT now()", // built-ins must be qualified with pg_catalog
		`SELECT U&"pg_advisory_\006Cock"(42)`,
		`SELECT "set_config"('application_name', 'x', false)`,
		`SELECT pg_catalog."now"()`,
		`SELECT set_config/**/('application_name', 'x', false)`,
		"SELECT 1 -- comment",
		`SELECT $$'$$, set_config('application_name', 'x', false), '$$'`,
		`SELECT $q$x$q$`,
		"SELECT u&'\\0041'",
	} {
		if _, err := newSelect(q); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("newSelect(%q) = %v, want ErrConfig", q, err)
		}
	}
}

// newWrite accepts only one INSERT, UPDATE or DELETE statement, so the statement check can trust a
// writeSQL on the pool to be a single statement, which Postgres runs as a transaction of its own.
func TestNewWrite(t *testing.T) {
	for _, q := range []string{"INSERT INTO t VALUES (1)", "  update t SET x = 1", "DELETE\nFROM t", "INSERT INTO app_steps (run_id, seq) VALUES ($1, app_next_seq_v1($1::text))", "UPDATE t SET e = pg_catalog.now() + ($1::pg_catalog.float8 * interval '1 second') WHERE s = 'f(x)'"} {
		if got, err := newWrite(q); err != nil || string(got) != q {
			t.Errorf("newWrite(%q) = %q, %v", q, got, err)
		}
	}
	for _, q := range []string{"SELECT 1", "BEGIN", "INSERTED", "", "UPDATE", "DELETE FROM t; COMMIT", "UPDATE t SET x = ';'", "UPDATE t SET x = pg_advisory_lock(1)", "INSERT INTO t (a) VALUES (evil($1))", "UPDATE t SET x = dblink_exec('x', 'BEGIN')",
		"UPDATE t SET e = now()", // built-ins must be qualified with pg_catalog
		// next_seq only in an INSERT into its own prefix's steps table, under its exact name, once
		`UPDATE bide_steps SET seq = bide_next_seq_v1(run_id) WHERE run_id = $1`,
		`DELETE FROM bide_leases WHERE bide_next_seq_v1($1) > 0`,
		`INSERT INTO bide_steps (run_id, seq, name, data) VALUES ($1, evil_next_seq_v1($1), $2, $3)`,
		`INSERT INTO bide_steps (run_id, seq, name, data) VALUES ($1, app_next_seq_v1($1), $2, $3)`,
		`INSERT INTO bide_steps (run_id, seq, name, data) VALUES ($1, bide_next_seq_v1($1), bide_next_seq_v1($1), $3)`,
		`INSERT INTO bide_steps (run_id, seq, name, data) SELECT run_id, bide_next_seq_v1(run_id), name, data FROM bide_steps`,
		`INSERT INTO t (a) VALUES (1) -- x`,
		`INSERT INTO t (a) VALUES ($$x$$)`,
	} {
		if _, err := newWrite(q); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("newWrite(%q) = %v, want ErrConfig", q, err)
		}
	}
}
