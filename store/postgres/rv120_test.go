package postgres

// From the review of #120 (store side). Skips without PG_DSN.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

const rv120Password = "rv120-test"

func rv120Admin(t *testing.T) (*sql.DB, string) {
	t.Helper()
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, base
}

// rv120DSN returns base with user (when not empty) and search_path (when not empty) replaced.
func rv120DSN(t *testing.T, base, user, searchPath string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		u.User = url.UserPassword(user, rv120Password)
	}
	q := u.Query()
	if searchPath != "" {
		q.Set("search_path", searchPath)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func rv120Exec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func rv120Role(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	ctx := context.Background()
	admin.ExecContext(ctx, `DROP OWNED BY `+name+` CASCADE`)
	admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+name)
	rv120Exec(t, admin, `CREATE ROLE `+name+` LOGIN PASSWORD '`+rv120Password+`'`)
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP OWNED BY `+name+` CASCADE`)
		admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+name)
	})
}

func rv120Schema(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+name+` CASCADE`)
	rv120Exec(t, admin, `CREATE SCHEMA `+name)
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+name+` CASCADE`) })
}

// ---------------------------------------------------------------------------------------------
// 1. Static check: spellings that call a function the check does not see.
// ---------------------------------------------------------------------------------------------

// rv120Hidden are statements newWrite/newSelect (run time) and unknownCall (constant queries)
// accept, each of which makes Postgres run a function a deployment can define.
var rv120Hidden = []struct{ name, q string }{
	// CONFLICT is an unreserved keyword, so it can name a function; the check treats "conflict("
	// as the keyword that takes ON CONFLICT's list.
	{"keyword-named function", `INSERT INTO bide_leases (run_id, holder, expiry) VALUES (conflict($1), $2, pg_catalog.now())`},
	// A non-ASCII letter ends the name, so the call regexp never matches it.
	{"non-ASCII function name", `INSERT INTO bide_leases (run_id, holder, expiry) VALUES (hijacké($1), $2, pg_catalog.now())`},
	// A cast to a domain runs its CHECK, which can call any function.
	{"cast to a domain", `SELECT seq FROM bide_steps WHERE run_id = $1::rv120hide.evil_domain`},
	// A user-defined operator is a call to its function.
	{"user-defined operator", `SELECT seq FROM bide_steps WHERE run_id ### $1`},
}

func TestRV120_StaticCheckRefusesHiddenCalls(t *testing.T) {
	for _, c := range rv120Hidden {
		t.Run(c.name, func(t *testing.T) {
			var err error
			if strings.HasPrefix(c.q, "SELECT") {
				_, err = newSelect(c.q, nil)
			} else {
				_, err = newWrite(c.q, nil, nil)
			}
			if err == nil {
				t.Errorf("run-time check accepted %q", c.q)
			}
			if why, bad := unknownCall(c.q); !bad {
				t.Errorf("constant-query check accepted %q (%s)", c.q, why)
			}
		})
	}
}

// The spellings above do run a function a deployment defines: Postgres calls it.
func TestRV120_HiddenCallsRunUserCode(t *testing.T) {
	admin, _ := rv120Admin(t)
	ctx := context.Background()
	rv120Schema(t, admin, "rv120hide")
	rv120Exec(t, admin,
		`CREATE FUNCTION rv120hide.boom(text) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'user code ran: %', $1; END $$`,
		`CREATE FUNCTION rv120hide.conflict(text) RETURNS text LANGUAGE sql AS $$ SELECT rv120hide.boom('conflict')::text $$`,
		`CREATE FUNCTION rv120hide."hijacké"(text) RETURNS text LANGUAGE sql AS $$ SELECT rv120hide.boom('unicode')::text $$`,
		`CREATE DOMAIN rv120hide.evil_domain AS text CHECK (rv120hide.boom('domain'))`,
		`CREATE FUNCTION rv120hide.op(text, text) RETURNS boolean LANGUAGE sql AS $$ SELECT rv120hide.boom('operator') $$`,
		`CREATE OPERATOR rv120hide.### (LEFTARG = text, RIGHTARG = text, FUNCTION = rv120hide.op)`,
		`CREATE TABLE rv120hide.bide_steps (run_id text, seq bigint)`,
		`CREATE TABLE rv120hide.bide_leases (run_id text, holder text, expiry timestamptz)`,
		`INSERT INTO rv120hide.bide_steps VALUES ('r', 0)`,
	)
	for _, c := range rv120Hidden {
		t.Run(c.name, func(t *testing.T) {
			conn, err := admin.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, `SET search_path = rv120hide`); err != nil {
				t.Fatal(err)
			}
			args := []any{"r"}
			if strings.Contains(c.q, "$2") {
				args = append(args, "h")
			}
			_, err = conn.ExecContext(ctx, c.q, args...)
			if err == nil || !strings.Contains(err.Error(), "user code ran") {
				t.Fatalf("expected user code to run, got %v", err)
			}
			t.Logf("Postgres ran user code: %v", err)
		})
	}
}

// ---------------------------------------------------------------------------------------------
// 2. Operator hijack. Operators resolve through search_path too.
// ---------------------------------------------------------------------------------------------

// rv120EvilOps creates, in schema, an operator for every operator and type pair the store's
// statements and the next_seq body use, each raising "hijacked".
func rv120EvilOps(t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	ops := []struct{ op, l, r, ret string }{
		{"=", "text", "text", "boolean"}, {">", "text", "text", "boolean"}, {">=", "text", "text", "boolean"},
		{"<", "text", "text", "boolean"}, {">", "bigint", "bigint", "boolean"}, {"=", "bigint", "bigint", "boolean"},
		{"<", "timestamptz", "timestamptz", "boolean"}, {">=", "timestamptz", "timestamptz", "boolean"},
		{"+", "timestamptz", "interval", "timestamptz"}, {"*", "double precision", "interval", "interval"},
		{"+", "bigint", "integer", "bigint"}, {"=", "integer", "integer", "boolean"},
	}
	for i, o := range ops {
		fn := fmt.Sprintf("%s.op%d", schema, i)
		rv120Exec(t, admin,
			fmt.Sprintf(`CREATE FUNCTION %s(%s, %s) RETURNS %s LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'hijacked %s(%s, %s)'; END $$`, fn, o.l, o.r, o.ret, o.op, o.l, o.r),
			fmt.Sprintf(`CREATE OPERATOR %s.%s (LEFTARG = %s, RIGHTARG = %s, FUNCTION = %s)`, schema, o.op, o.l, o.r, fn))
	}
}

func rv120Exercise(ctx context.Context, s *Store) error {
	if _, _, err := s.Insert(ctx, "run", "a", []byte("a")); err != nil {
		return err
	}
	if _, _, err := s.Insert(ctx, "run", "b", []byte("b")); err != nil {
		return err
	}
	if _, _, err := s.Get(ctx, "run", "a"); err != nil {
		return err
	}
	for _, err := range s.Load(ctx, "run", -1) {
		if err != nil {
			return err
		}
	}
	for _, err := range s.Runs(ctx, agent.RunFilter{Prefix: "ru", ExcludeHolding: []string{"zz"}}) {
		if err != nil {
			return err
		}
	}
	if _, err := s.AcquireLease(ctx, "run", "h", 1e9); err != nil {
		return err
	}
	if _, err := s.RenewLease(ctx, "run", "h", 1e9); err != nil {
		return err
	}
	return s.ReleaseLease(ctx, "run", "h")
}

// With pg_catalog searched first (the default: it is implicitly first), an operator a role
// creates in a schema on the path, even one exactly matching the built-in's types, does not
// take over any of the store's statements.
func TestRV120_OperatorsOnPathDoNotHijack(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	rv120Schema(t, admin, "rv120app")
	rv120Schema(t, admin, "rv120evil")
	rv120EvilOps(t, admin, "rv120evil")
	s, err := Open(ctx, rv120DSN(t, base, "", "rv120app,rv120evil"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := rv120Exercise(ctx, s); err != nil {
		t.Fatalf("hijacked with pg_catalog implicitly first: %v", err)
	}
	// With pg_catalog explicitly after the evil schema, the Go-side statements are taken over
	// (the documented limit), but the next_seq body is not: it runs with its own search_path.
	s2, err := Open(ctx, rv120DSN(t, base, "", "rv120app,rv120evil,pg_catalog"))
	if err != nil {
		t.Logf("Open with pg_catalog last: %v", err)
	} else {
		defer s2.Close()
		t.Logf("store ops with pg_catalog after the evil schema: %v", rv120Exercise(ctx, s2))
	}
	// Call the function directly with the evil schema before pg_catalog.
	conn, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rv120Exec2(t, conn, `SET search_path = rv120evil, pg_catalog`)
	var n int64
	if err := conn.QueryRowContext(ctx, `SELECT rv120app.bide_next_seq_v1('run'::pg_catalog.text)`).Scan(&n); err != nil {
		t.Fatalf("next_seq body hijacked through the session's search_path: %v", err)
	}
	if n != 2 {
		t.Fatalf("next_seq = %d, want 2", n)
	}
	// The same body without SET search_path is not taken over either: the body writes every
	// operator OPERATOR(pg_catalog.<op>), so each of the two layers holds on its own. (Before the
	// body qualified its operators, this mutant was taken over, which showed the check above live.)
	rv120Exec2(t, conn, `CREATE FUNCTION rv120app.mutant(r pg_catalog.text) RETURNS pg_catalog.int8 LANGUAGE plpgsql VOLATILE AS $bide$`+
		(tables{steps: "bide_steps"}).nextSeqBody("rv120app")+`$bide$`)
	if err := conn.QueryRowContext(ctx, `SELECT rv120app.mutant('run'::pg_catalog.text)`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the body without SET search_path, through the evil schema's operators: n=%d err=%v; want 2", n, err)
	}
}

func rv120Exec2(t *testing.T, c *sql.Conn, q string) {
	t.Helper()
	if _, err := c.ExecContext(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// ---------------------------------------------------------------------------------------------
// 3. "$user" on the path: a role with CREATE on the database, and no CREATE on any schema that
// exists on the store's path, takes over every insert after Open checked the function.
// ---------------------------------------------------------------------------------------------

func TestRV120_UserSchemaTakesOverNextSeqAfterOpen(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	var dbname string
	if err := admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		t.Fatal(err)
	}
	rv120Role(t, admin, "rv120store")
	rv120Role(t, admin, "rv120attacker")
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS rv120store CASCADE`) })
	rv120Schema(t, admin, "rv120app")
	rv120Exec(t, admin,
		`ALTER SCHEMA rv120app OWNER TO rv120store`,
		// The attacker can create schemas in the database, and nothing on the store's schema.
		`GRANT CREATE ON DATABASE `+dbname+` TO rv120attacker`,
	)
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `REVOKE CREATE ON DATABASE `+dbname+` FROM rv120attacker`)
	})
	// The default shape of search_path: "$user" first, then the application's schema.
	s, err := Open(ctx, rv120DSN(t, base, "rv120store", `"$user",rv120app`))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.Insert(ctx, "run", "a", []byte("a")); err != nil {
		t.Fatal(err)
	}
	// The attacker, after Open.
	atk, err := sql.Open("pgx", rv120DSN(t, base, "rv120attacker", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer atk.Close()
	rv120Exec(t, atk,
		`CREATE SCHEMA rv120store`,
		`GRANT USAGE ON SCHEMA rv120store TO rv120store`,
		// Runs with the store role's privileges: it erases the run and hands out position 0.
		`CREATE FUNCTION rv120store.bide_next_seq_v1(r text) RETURNS bigint LANGUAGE plpgsql AS $$
		BEGIN DELETE FROM rv120app.bide_steps WHERE run_id = r; RETURN 0; END $$`,
	)
	e, ok, err := s.Insert(ctx, "run", "b", []byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	var left int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM rv120app.bide_steps WHERE run_id = 'run'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if e.Seq != 1 || left != 2 {
		t.Fatalf("insert after Open ran the attacker's rv120store.bide_next_seq_v1: seq=%d ok=%v, rows left for the run=%d (want seq 1, 2 rows)", e.Seq, ok, left)
	}
}
