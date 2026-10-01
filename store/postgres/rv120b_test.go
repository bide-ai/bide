package postgres

// From the second review of #120 (store side). Skips without PG_DSN. Uses the helpers in rv120_test.go.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// rv120bAttackerSetup creates the store role and the attacker role, gives the store role
// rv120app, and gives the attacker CREATE on the database (and nothing on rv120app). It returns
// the attacker's pool.
func rv120bAttackerSetup(t *testing.T, admin *sql.DB, base string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	var dbname string
	if err := admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS rv120bstore CASCADE`) })
	rv120Role(t, admin, "rv120bstore")
	rv120Role(t, admin, "rv120batk")
	rv120Schema(t, admin, "rv120app")
	rv120Exec(t, admin,
		`ALTER SCHEMA rv120app OWNER TO rv120bstore`,
		`GRANT CREATE ON DATABASE `+dbname+` TO rv120batk`,
	)
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `REVOKE CREATE ON DATABASE `+dbname+` FROM rv120batk`)
	})
	atk, err := sql.Open("pgx", rv120DSN(t, base, "rv120batk", ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { atk.Close() })
	return atk
}

// 1. Open's catalog checks compare text[] with text[] (requiredUnique, expectedFunction) and oid
// with regtype (expectedFunction). pg_catalog has only =(anyarray, anyarray) and =(oid, oid) for
// those, so an operator with the exact argument types in any schema on the path matches better,
// whether pg_catalog is searched first or not. A role with CREATE on the database creates the
// "$user" schema of the store's role, which holds no table (so Open still picks rv120app), and
// its operators run as the store's role at every Open. Returning true they also blind the checks.
func TestRV120b_OpenRunsPathOperatorsAsTheStoreRole(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	atk := rv120bAttackerSetup(t, admin, base)
	// First Open, before the attacker does anything.
	s, err := Open(ctx, rv120DSN(t, base, "rv120bstore", `"$user",rv120app`))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	rv120Exec(t, atk,
		`CREATE SCHEMA rv120bstore`,
		`GRANT USAGE ON SCHEMA rv120bstore TO PUBLIC`,
		`CREATE TABLE rv120bstore.ran (who text, op text)`,
		`GRANT INSERT ON rv120bstore.ran TO PUBLIC`,
		`CREATE FUNCTION rv120bstore.arreq(a text[], b text[]) RETURNS boolean LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO rv120bstore.ran VALUES (current_user, 'text[] = text[]'); RETURN true; END $$`,
		`CREATE OPERATOR rv120bstore.= (LEFTARG = text[], RIGHTARG = text[], FUNCTION = rv120bstore.arreq)`,
		`CREATE FUNCTION rv120bstore.oidreg(a oid, b regtype) RETURNS boolean LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO rv120bstore.ran VALUES (current_user, 'oid = regtype'); RETURN true; END $$`,
		`CREATE OPERATOR rv120bstore.= (LEFTARG = oid, RIGHTARG = regtype, FUNCTION = rv120bstore.oidreg)`,
	)
	// The node restarts.
	s, err = Open(ctx, rv120DSN(t, base, "rv120bstore", `"$user",rv120app`))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var who, ops string
	if err := admin.QueryRowContext(ctx, `SELECT coalesce(string_agg(DISTINCT who, ','), ''), coalesce(string_agg(DISTINCT op, ' | '), '') FROM rv120bstore.ran`).Scan(&who, &ops); err != nil {
		t.Fatal(err)
	}
	if who != "" {
		t.Fatalf("Open ran the attacker's operators (%s) as role %s", ops, who)
	}
}

// Control for the test above: with rv120app alone on the path, the same operators do not run.
func TestRV120b_OpenOperatorsControl(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	atk := rv120bAttackerSetup(t, admin, base)
	rv120Exec(t, atk,
		`CREATE SCHEMA rv120bstore`,
		`GRANT USAGE ON SCHEMA rv120bstore TO PUBLIC`,
		`CREATE FUNCTION rv120bstore.arreq(a text[], b text[]) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'hijacked'; END $$`,
		`CREATE OPERATOR rv120bstore.= (LEFTARG = text[], RIGHTARG = text[], FUNCTION = rv120bstore.arreq)`,
	)
	s, err := Open(ctx, rv120DSN(t, base, "rv120bstore", `rv120app`))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, rv120DSN(t, base, "rv120bstore", `rv120app,"$user"`)); err == nil || !strings.Contains(err.Error(), "hijacked") {
		t.Fatalf("with the attacker's schema last on the path (pg_catalog implicitly first), want the operator to run, got %v", err)
	}
}

// 3. The schema choice is redone at every Open. After a node has written to rv120app, a role with
// CREATE on the database creates the store role's "$user" schema holding a steps table and a
// next_seq function with this version's exact body, both its own. A restarted node passes every
// check, moves to the attacker's schema (its run's entries are gone), and the attacker, owner of
// next_seq, then replaces it and runs code as the store's role on every insert.
func TestRV120b_RestartMovesToALaterEarlierSchema(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	atk := rv120bAttackerSetup(t, admin, base)
	dsn := rv120DSN(t, base, "rv120bstore", `"$user",rv120app`)
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if _, _, err := s.Insert(ctx, "run", n, []byte(n)); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	body := (tables{steps: "bide_steps"}).nextSeqBody("rv120bstore")
	rv120Exec(t, atk,
		`CREATE SCHEMA rv120bstore`,
		`GRANT USAGE, CREATE ON SCHEMA rv120bstore TO rv120bstore`,
		`CREATE TABLE rv120bstore.bide_steps (
			run_id pg_catalog.text COLLATE pg_catalog."C" NOT NULL, seq pg_catalog.int8 NOT NULL,
			name pg_catalog.text NOT NULL, data pg_catalog.bytea NOT NULL,
			PRIMARY KEY (run_id, name), UNIQUE (run_id, seq))`,
		`GRANT ALL ON rv120bstore.bide_steps TO rv120bstore`,
		`CREATE FUNCTION rv120bstore.bide_next_seq_v1(r pg_catalog.text) RETURNS pg_catalog.int8 LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp AS $bide$`+body+`$bide$`,
	)
	s, err = Open(ctx, dsn)
	if err != nil {
		t.Fatalf("restart refused: %v", err)
	}
	defer s.Close()
	var n int
	for _, err := range s.Load(ctx, "run", -1) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	rv120Exec(t, atk, `CREATE OR REPLACE FUNCTION rv120bstore.bide_next_seq_v1(r pg_catalog.text) RETURNS pg_catalog.int8 LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp AS $$
		BEGIN DELETE FROM rv120app.bide_steps; RETURN 7; END $$`)
	e, _, err := s.Insert(ctx, "run", "c", []byte("c"))
	var left int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM rv120app.bide_steps`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if n != 2 || left != 2 {
		t.Fatalf("restarted node: schema %q, sees %d of the run's 2 entries; then the attacker's next_seq ran as the store role (insert seq=%d err=%v) and left %d of 2 rows in rv120app.bide_steps", s.schema, n, e.Seq, err, left)
	}
}

// 2. The check allows the spelling of an operator, not what it resolves to. Each statement below
// passes newSelect, uses only allowlisted operator tokens or a keyword operator (LIKE, IS DISTINCT
// FROM) the token list never sees, and with the attacker's schema LAST on the path (pg_catalog
// implicitly first) Postgres runs the attacker's operator, because pg_catalog has no operator with
// the exact argument types (only anyarray, or name ~~ text).
func TestRV120b_AcceptedStatementsRunPathOperators(t *testing.T) {
	admin, _ := rv120Admin(t)
	ctx := context.Background()
	rv120Schema(t, admin, "rv120bapp")
	rv120Schema(t, admin, "rv120bevil")
	rv120Exec(t, admin,
		`CREATE TABLE rv120bapp.bide_steps (run_id text, seq bigint)`,
		`INSERT INTO rv120bapp.bide_steps VALUES ('r', 0)`,
		`CREATE FUNCTION rv120bevil.boom(anyelement, anyelement) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'user code ran'; END $$`,
		`CREATE FUNCTION rv120bevil.arr(text[], text[]) RETURNS boolean LANGUAGE sql AS $$ SELECT rv120bevil.boom(1, 1) $$`,
		`CREATE OPERATOR rv120bevil.= (LEFTARG = text[], RIGHTARG = text[], FUNCTION = rv120bevil.arr)`,
		`CREATE FUNCTION rv120bevil.nl(name, name) RETURNS boolean LANGUAGE sql AS $$ SELECT rv120bevil.boom(1, 1) $$`,
		`CREATE OPERATOR rv120bevil.~~ (LEFTARG = name, RIGHTARG = name, FUNCTION = rv120bevil.nl)`,
	)
	for _, q := range []string{
		`SELECT seq FROM "rv120bapp".bide_steps WHERE ARRAY[run_id] = $1::pg_catalog.text[]`,
		`SELECT seq FROM "rv120bapp".bide_steps WHERE ARRAY[run_id] IS DISTINCT FROM $1::pg_catalog.text[]`,
		`SELECT c.relname FROM pg_catalog.pg_class AS c WHERE c.relname LIKE $1`,
	} {
		t.Run(q, func(t *testing.T) {
			sel, err := newSelect(q)
			if err != nil {
				t.Skipf("refused by the check: %v", err)
			}
			conn, err := admin.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, `SET search_path = rv120bapp, rv120bevil`); err != nil {
				t.Fatal(err)
			}
			arg := any("bide%")
			if strings.Contains(q, "text[]") {
				arg = []string{"r"}
			}
			rows, err := conn.QueryContext(ctx, string(sel), arg)
			if err == nil {
				err = rows.Close()
			}
			if err != nil {
				t.Fatalf("the check accepted it and Postgres ran user code: %v", err)
			}
		})
	}
}
