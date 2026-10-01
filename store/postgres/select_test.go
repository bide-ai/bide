package postgres

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// names parses the dotted names the statement tests allow as relations or call as next_seq.
func names(t *testing.T, ns ...string) [][]sqlTok {
	t.Helper()
	var out [][]sqlTok
	for _, n := range ns {
		name, err := parseName(n)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

// newSelect accepts only one SELECT statement that passes the statement check (see sqlcheck.go),
// so the static check can trust a selectSQL on the pool.
func TestNewSelect(t *testing.T) {
	rels := names(t, `"app".bide_steps`, `"My ""Schema""".t`)
	for _, q := range []string{"SELECT 1", "  select 1", "SELECT\n1",
		`SELECT pg_catalog.max(seq), pg_catalog.now() FROM "app".bide_steps WHERE x OPERATOR(pg_catalog.=) ANY ($1) AND c OPERATOR(pg_catalog.=) 'f(x)'`,
		`SELECT run_id COLLATE pg_catalog."C" FROM "My ""Schema""".t WHERE seq OPERATOR(pg_catalog.>) $2 AND a OPERATOR(pg_catalog.<>) b`,
		`SELECT EXISTS (SELECT 1 FROM "app".bide_steps AS s WHERE s.x OPERATOR(pg_catalog.=) 'r' OR s.y OPERATOR(pg_catalog.=) ANY ($1::pg_catalog.text[]))`,
		`SELECT n.nspname FROM pg_catalog.pg_class AS c JOIN pg_catalog.pg_namespace AS n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace ORDER BY n.nspname LIMIT 1`,
		`SELECT k FROM pg_catalog.unnest($1::pg_catalog.int2[]) AS k`,
		`SELECT DISTINCT run_id FROM "app".bide_steps AS s WHERE NOT EXISTS (SELECT 1 FROM "app".bide_steps AS x WHERE x.run_id OPERATOR(pg_catalog.=) s.run_id)`,
	} {
		if got, err := newSelect(q, rels); err != nil || string(got) != q {
			t.Errorf("newSelect(%q) = %q, %v", q, got, err)
		}
	}
	for _, q := range []string{"UPDATE t SET x = 1", "SELECTED", "", "WITH x AS (DELETE FROM t) SELECT 1", "SELECT", "SELECT 1; BEGIN", "SELECT pg_advisory_lock(1)", "SELECT pg_try_advisory_lock_shared(1)", "SELECT dblink_exec('x', 'BEGIN')", "SELECT bide_next_seq_v1('r')", "SELECT evil (1)", "SELECT pg_advisory_xact_lock(1)",
		`SELECT max(seq) FROM "app".bide_steps`, "SELECT now()", // built-ins must be qualified with pg_catalog
		`SELECT U&"pg_advisory_\006Cock"(42)`,
		`SELECT "set_config"('application_name', 'x', false)`,
		`SELECT pg_catalog."now"()`,
		`SELECT set_config/**/('application_name', 'x', false)`,
		"SELECT 1 -- comment",
		`SELECT $$'$$, set_config('application_name', 'x', false), '$$'`,
		`SELECT $q$x$q$`,
		"SELECT u&'\\0041'",
		`SELECT U&"pg_advisory_!006Cock" UESCAPE '!'(42)`, // a Unicode escape with no backslash
		`SELECT run_id COLLATE "C" FROM "app".bide_steps`, // a collation must be pg_catalog."C"
		// casts only to pg_catalog built-ins: a domain's CHECK can call anything
		`SELECT x FROM "app".bide_steps WHERE y OPERATOR(pg_catalog.=) $1::text`,
		`SELECT x FROM "app".bide_steps WHERE y OPERATOR(pg_catalog.=) $1::app.dom`,
		`SELECT x FROM "app".bide_steps WHERE y OPERATOR(pg_catalog.=) $1::pg_catalog.bytea`,
		"SELECT CAST($1 AS pg_catalog.text)", "SELECT interval '1 second'", "SELECT E'x'", "SELECT pg_catalog.text 'a'",
		// operators only as OPERATOR(pg_catalog.<op>), with an allowed op
		`SELECT a || b FROM "app".bide_steps`, `SELECT x FROM "app".bide_steps WHERE y ### $1`,
		`SELECT x FROM "app".bide_steps WHERE y OPERATOR(pg_catalog.~) $1`, `SELECT x FROM "app".bide_steps WHERE y OPERATOR(app.=) $1`,
		`SELECT x FROM "app".bide_steps WHERE y OPERATOR(=) $1`, `SELECT x FROM "app".bide_steps WHERE y = $1`,
		// no non-ASCII identifier, no keyword-named function
		"SELECT hijacké($1)", "SELECT conflict($1)", "SELECT 1.5", "SELECT 1e3",
		`SELECT x FROM "app".bide_steps AS q(a)`,                              // a column list only after INSERT INTO t [AS alias]
		`SELECT x FROM "app".bide_steps WHERE y OPERATOR(pg_catalog.=) 'a\\'`, // a backslash in a literal
		// the second review of #120: keyword operators, which resolve through the search path
		"SELECT c.relname FROM pg_catalog.pg_class AS c WHERE c.relname LIKE $1",
		"SELECT c.relname FROM pg_catalog.pg_class AS c WHERE c.relname ILIKE $1",
		"SELECT c.relname FROM pg_catalog.pg_class AS c WHERE c.relname SIMILAR TO $1",
		"SELECT c.relname FROM pg_catalog.pg_class AS c WHERE c.relname IS DISTINCT FROM $1",
		"SELECT c.relname FROM pg_catalog.pg_class AS c WHERE c.relname IS NOT DISTINCT FROM $1",
		"SELECT c.relname FROM pg_catalog.pg_class AS c WHERE (c.relname, c.relname) OVERLAPS ($1, $2)",
		"SELECT c.relname FROM pg_catalog.pg_class AS c WHERE c.relname BETWEEN $1 AND $2",
		"SELECT c.relname FROM pg_catalog.pg_class AS c WHERE c.relkind IN ('r', 'p')",
		"SELECT CASE c.relkind WHEN 'r' THEN 1 END FROM pg_catalog.pg_class AS c",
		"SELECT c.relname FROM pg_catalog.pg_class AS c ORDER BY c.relname USING <",
		"SELECT c.relname FROM pg_catalog.pg_class AS c JOIN pg_catalog.pg_namespace AS n USING (oid)",
		// array constructors, and comparisons whose operator the check cannot vouch for
		"SELECT ARRAY[c.relname] FROM pg_catalog.pg_class AS c",
		"SELECT c.relname FROM pg_catalog.pg_class AS c WHERE c.relname = $1",
		// relations other than the store's own tables and pg_catalog's
		"SELECT x FROM t", "SELECT x FROM public.other", `SELECT x FROM "other".bide_steps`, `SELECT x FROM app.bide_steps`,
		"SELECT x FROM pg_catalog.pg_class AS c, public.other AS o",
		`SELECT x FROM "app".bide_steps AS s JOIN public.other AS o ON s.x OPERATOR(pg_catalog.=) o.x`,
		"SELECT x FROM (SELECT y FROM public.other) AS o",
	} {
		if _, err := newSelect(q, rels); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("newSelect(%q) = %v, want ErrConfig", q, err)
		}
	}
}

// newWrite accepts only one INSERT, UPDATE or DELETE statement that passes the statement check, so
// the static check can trust a writeSQL on the pool to be a single statement, which Postgres runs
// as a transaction of its own; and next_seq only under its exact schema-qualified name, once, in an
// INSERT with no SELECT.
func TestNewWrite(t *testing.T) {
	ns := names(t, `"app".bide_next_seq_v1`)[0]
	rels := names(t, `"app".bide_steps`, `"app".bide_leases`)
	for _, c := range []struct {
		q       string
		nextSeq []sqlTok
	}{
		{`INSERT INTO "app".bide_steps VALUES (1)`, nil},
		{`UPDATE "app".bide_leases SET x = 1, y = ';'`, nil}, // assignments; a semicolon in a literal is not a second statement
		{"DELETE\nFROM \"app\".bide_leases", nil},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1::pg_catalog.text, "app".bide_next_seq_v1($1::pg_catalog.text))`, ns},
		{`UPDATE "app".bide_leases SET e = pg_catalog.now() OPERATOR(pg_catalog.+) ($1::pg_catalog.float8 OPERATOR(pg_catalog.*) '1 second'::pg_catalog.interval) WHERE s OPERATOR(pg_catalog.=) 'f(x)'`, nil},
		{`INSERT INTO "app".bide_leases AS l (a) VALUES ($1) ON CONFLICT (a) DO UPDATE SET a = EXCLUDED.a, b = EXCLUDED.b WHERE l.a OPERATOR(pg_catalog.=) EXCLUDED.a OR l.b OPERATOR(pg_catalog.<) pg_catalog.now()`, nil},
	} {
		if got, err := newWrite(c.q, c.nextSeq, rels); err != nil || string(got) != c.q {
			t.Errorf("newWrite(%q) = %q, %v", c.q, got, err)
		}
	}
	for _, c := range []struct {
		q       string
		nextSeq []sqlTok
	}{
		{"SELECT 1", nil}, {"BEGIN", nil}, {"INSERTED", nil}, {"", nil}, {"UPDATE", nil}, {`DELETE FROM "app".bide_leases; COMMIT`, nil},
		{`UPDATE "app".bide_leases SET x = pg_advisory_lock(1)`, nil}, {`INSERT INTO "app".bide_steps (a) VALUES (evil($1))`, nil},
		{`UPDATE "app".bide_leases SET x = dblink_exec('x', 'BEGIN')`, nil},
		{`UPDATE "app".bide_leases SET e = now()`, nil}, // built-ins must be qualified with pg_catalog
		// a bare operator, outside an assignment
		{`UPDATE "app".bide_leases SET x = 1 WHERE y = 2`, nil}, {`UPDATE "app".bide_leases SET x = y = 2`, nil},
		{`DELETE FROM "app".bide_leases WHERE run_id = $1`, nil},
		// next_seq only under its exact schema-qualified name, once, in an INSERT with no SELECT
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, "app".bide_next_seq_v1($1))`, nil},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, bide_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, "other".bide_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, app.bide_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq) VALUES ($1, "app".evil_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq, name) VALUES ($1, "app".bide_next_seq_v1($1), "app".bide_next_seq_v1($1))`, ns},
		{`INSERT INTO "app".bide_steps (run_id, seq) SELECT run_id, "app".bide_next_seq_v1(run_id) FROM "app".bide_steps`, ns},
		{`UPDATE "app".bide_steps SET seq = "app".bide_next_seq_v1(run_id)`, ns},
		{`DELETE FROM "app".bide_leases WHERE "app".bide_next_seq_v1($1) OPERATOR(pg_catalog.>) 0`, ns},
		{`INSERT INTO "app".bide_steps (a) VALUES (1) -- x`, nil},
		{`INSERT INTO "app".bide_steps (a) VALUES ($$x$$)`, nil},
		// relations other than the store's own
		{`INSERT INTO "other".bide_steps (a) VALUES (1)`, nil}, {`UPDATE public.t SET a = 1`, nil}, {`DELETE FROM bide_leases`, nil},
		// the review of #120: a keyword-named function, a non-ASCII name, a domain, an operator
		{`INSERT INTO "app".bide_leases (run_id, holder, expiry) VALUES (conflict($1), $2, pg_catalog.now())`, nil},
		{`INSERT INTO "app".bide_leases (run_id, holder, expiry) VALUES (hijacké($1), $2, pg_catalog.now())`, nil},
		{`UPDATE "app".bide_leases SET x = $1::rv120hide.evil_domain`, nil},
		{`DELETE FROM "app".bide_leases WHERE run_id ### $1`, nil},
	} {
		if _, err := newWrite(c.q, c.nextSeq, rels); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("newWrite(%q) = %v, want ErrConfig", c.q, err)
		}
	}
}

// The queries Open and the migration send outside newSelect and newWrite (the catalog reads, and
// the migration transaction's next_seq lookup and version read, which the static check does not
// govern because a transaction runs them) pass the same statement check: every operator written
// OPERATOR(pg_catalog.<op>), every relation the store's own or pg_catalog's.
func TestOpenQueriesPassTheCheck(t *testing.T) {
	tb, err := newTables("bide_", `My "Schema`)
	if err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"firstRelation": firstRelation, "relationIsTable": relationIsTable, "requiredUnique": requiredUnique,
		"nextSeqPresent": nextSeqPresent, "expectedFunction": expectedFunction, "versionQuery": versionQuery(tb.qVersion),
	} {
		if err := checkSQL(q, nil, tb.rels); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// WithSchema refuses the system schemas, information_schema and every name starting with pg_
// (pg_catalog, pg_toast, pg_temp and a session's pg_temp_N among them), with ErrConfig.
func TestWithSchemaRefusesSystemSchemas(t *testing.T) {
	for _, name := range []string{"information_schema", "pg_catalog", "pg_toast", "pg_temp", "pg_temp_3", "pg_toast_temp_1", "pg_anything", "pg_"} {
		var c config
		if err := WithSchema(name).apply(&c); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("WithSchema(%q) = %v, want ErrConfig", name, err)
		}
	}
	for _, name := range []string{"app", "Pg_upper", "information_schema2", "my pg_x"} {
		var c config
		if err := WithSchema(name).apply(&c); err != nil || c.schema != name {
			t.Errorf("WithSchema(%q) = %v, schema %q", name, err, c.schema)
		}
	}
}
