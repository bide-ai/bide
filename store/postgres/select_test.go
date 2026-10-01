package postgres

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// newSelect accepts only one SELECT statement that passes the statement check (see sqlcheck.go),
// so the static check can trust a selectSQL on the pool.
func TestNewSelect(t *testing.T) {
	for _, q := range []string{"SELECT 1", "  select x FROM t", "SELECT\n1",
		"SELECT pg_catalog.max(seq), pg_catalog.now() FROM t WHERE x = ANY($1) AND c = 'f(x)'",
		`SELECT run_id COLLATE pg_catalog."C" FROM "My ""Schema""".t WHERE seq > $2 AND a <> b`,
		`SELECT EXISTS (SELECT 1 FROM t WHERE x IN ('r', 'p') AND y = ANY($1::pg_catalog.text[]))`,
	} {
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
		`SELECT U&"pg_advisory_!006Cock" UESCAPE '!'(42)`, // a Unicode escape with no backslash
		`SELECT run_id COLLATE "C" FROM t`,                // a collation must be pg_catalog."C"
		// casts only to pg_catalog built-ins: a domain's CHECK can call anything
		"SELECT x FROM t WHERE y = $1::text", "SELECT x FROM t WHERE y = $1::app.dom", "SELECT x FROM t WHERE y = $1::pg_catalog.bytea",
		"SELECT CAST($1 AS pg_catalog.text)", "SELECT interval '1 second'", "SELECT E'x'", "SELECT x FROM t WHERE y = pg_catalog.text 'a'",
		// operators only from the allowlist: an operator is a call
		"SELECT a || b FROM t", "SELECT x FROM t WHERE y ### $1", "SELECT x FROM t WHERE y ~ $1", "SELECT x FROM t WHERE y OPERATOR(pg_catalog.=) $1",
		// no non-ASCII identifier, no keyword-named function
		"SELECT hijacké($1)", "SELECT conflict($1)", "SELECT 1.5", "SELECT x FROM t WHERE y = 1e3",
		"SELECT x FROM t AS q(a)",                // a column list only after INSERT INTO t [AS alias]
		`SELECT x FROM t WHERE y = 'a\\' OR z = 'b'`, // a backslash in a literal
	} {
		if _, err := newSelect(q); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("newSelect(%q) = %v, want ErrConfig", q, err)
		}
	}
}

// newWrite accepts only one INSERT, UPDATE or DELETE statement that passes the statement check, so
// the static check can trust a writeSQL on the pool to be a single statement, which Postgres runs
// as a transaction of its own; and next_seq only under its exact schema-qualified name, once, in an
// INSERT with no SELECT.
func TestNewWrite(t *testing.T) {
	ns, err := parseName(`"app".bide_next_seq_v1`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		q       string
		nextSeq []sqlTok
	}{
		{"INSERT INTO t VALUES (1)", nil},
		{"UPDATE t SET x = ';'", nil}, // a semicolon in a literal is not a second statement
		{"  update t SET x = 1", nil},
		{"DELETE\nFROM t", nil},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1::pg_catalog.text, "app".bide_next_seq_v1($1::pg_catalog.text))`, ns},
		{`UPDATE "app".t SET e = pg_catalog.now() + ($1::pg_catalog.float8 * '1 second'::pg_catalog.interval) WHERE s = 'f(x)'`, nil},
		{`INSERT INTO "app".l AS l (a) VALUES ($1) ON CONFLICT (a) DO UPDATE SET a = EXCLUDED.a WHERE l.a = EXCLUDED.a OR l.b < pg_catalog.now()`, nil},
	} {
		if got, err := newWrite(c.q, c.nextSeq); err != nil || string(got) != c.q {
			t.Errorf("newWrite(%q) = %q, %v", c.q, got, err)
		}
	}
	for _, c := range []struct {
		q       string
		nextSeq []sqlTok
	}{
		{"SELECT 1", nil}, {"BEGIN", nil}, {"INSERTED", nil}, {"", nil}, {"UPDATE", nil}, {"DELETE FROM t; COMMIT", nil},
		{"UPDATE t SET x = pg_advisory_lock(1)", nil}, {"INSERT INTO t (a) VALUES (evil($1))", nil},
		{"UPDATE t SET x = dblink_exec('x', 'BEGIN')", nil},
		{"UPDATE t SET e = now()", nil}, // built-ins must be qualified with pg_catalog
		// next_seq only under its exact schema-qualified name, once, in an INSERT with no SELECT
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, "app".bide_next_seq_v1($1))`, nil},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, bide_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, "other".bide_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, app.bide_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, "app".evil_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq, name) VALUES ($1, "app".bide_next_seq_v1($1), "app".bide_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq) SELECT run_id, "app".bide_next_seq_v1(run_id) FROM "app".bide_steps`, ns},
		{`UPDATE "app".bide_steps SET seq = "app".bide_next_seq_v1(run_id) WHERE run_id = $1`, ns},
		{`DELETE FROM "app".bide_leases WHERE "app".bide_next_seq_v1($1) > 0`, ns},
		{`INSERT INTO t (a) VALUES (1) -- x`, nil},
		{`INSERT INTO t (a) VALUES ($$x$$)`, nil},
		// the review of #120: a keyword-named function, a non-ASCII name, a domain, an operator
		{`INSERT INTO bide_leases (run_id, holder, expiry) VALUES (conflict($1), $2, pg_catalog.now())`, nil},
		{`INSERT INTO bide_leases (run_id, holder, expiry) VALUES (hijacké($1), $2, pg_catalog.now())`, nil},
		{`UPDATE t SET x = $1::rv120hide.evil_domain`, nil},
		{`DELETE FROM t WHERE run_id ### $1`, nil},
	} {
		if _, err := newWrite(c.q, c.nextSeq); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("newWrite(%q) = %v, want ErrConfig", c.q, err)
		}
	}
}
