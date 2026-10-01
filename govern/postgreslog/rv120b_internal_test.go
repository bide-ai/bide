package postgreslog

// From the second review of #120 (postgreslog side). Skips without PG_DSN. Uses the helpers in
// rv120_internal_test.go.

import (
	"context"
	"database/sql"
	"testing"
)

func rv120bSetup(t *testing.T, admin *sql.DB, base string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	var dbname string
	if err := admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS rv120blog CASCADE`) })
	rv120Role(t, admin, "rv120blog")
	rv120Role(t, admin, "rv120batk")
	rv120Schema(t, admin, "rv120b_app")
	rv120Exec(t, admin, `ALTER SCHEMA rv120b_app OWNER TO rv120blog`, `GRANT CREATE ON DATABASE `+dbname+` TO rv120batk`)
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

// checkSchema compares text[] with text[] and oid with regtype; an exact-type operator in any
// schema on the path beats pg_catalog's =(anyarray, anyarray) and =(oid, oid), pg_catalog first or
// not, and runs as the log's role at every Open.
func TestRV120b_OpenRunsPathOperatorsAsTheLogRole(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	atk := rv120bSetup(t, admin, base)
	dsn := rv120DSN(t, base, "rv120blog", `rv120b_app,"$user"`) // the log's schema first
	l, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	rv120Exec(t, atk,
		`CREATE SCHEMA rv120blog`,
		`GRANT USAGE ON SCHEMA rv120blog TO PUBLIC`,
		`CREATE TABLE rv120blog.ran (who text, op text)`,
		`GRANT INSERT ON rv120blog.ran TO PUBLIC`,
		`CREATE FUNCTION rv120blog.arreq(a text[], b text[]) RETURNS boolean LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO rv120blog.ran VALUES (current_user, 'text[] = text[]'); RETURN true; END $$`,
		`CREATE OPERATOR rv120blog.= (LEFTARG = text[], RIGHTARG = text[], FUNCTION = rv120blog.arreq)`,
		`CREATE FUNCTION rv120blog.oidreg(a oid, b regtype) RETURNS boolean LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO rv120blog.ran VALUES (current_user, 'oid = regtype'); RETURN true; END $$`,
		`CREATE OPERATOR rv120blog.= (LEFTARG = oid, RIGHTARG = regtype, FUNCTION = rv120blog.oidreg)`,
	)
	l, err = Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var who, ops string
	if err := admin.QueryRowContext(ctx, `SELECT coalesce(string_agg(DISTINCT who, ','), ''), coalesce(string_agg(DISTINCT op, ' | '), '') FROM rv120blog.ran`).Scan(&who, &ops); err != nil {
		t.Fatal(err)
	}
	if who != "" {
		t.Fatalf("Open ran the attacker's operators (%s) as role %s, with the log's schema first on the path", ops, who)
	}
}

// The schema is chosen again at every Open: a governed_events created later in an earlier schema
// (here the "$user" schema, by a role with CREATE on the database) moves a restarted process to it.
func TestRV120b_RestartMovesToALaterEarlierSchema(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	atk := rv120bSetup(t, admin, base)
	dsn := rv120DSN(t, base, "rv120blog", `"$user",rv120b_app`)
	l, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := l.Append(ctx, "e", id, id); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	rv120Exec(t, atk,
		`CREATE SCHEMA rv120blog`,
		`GRANT USAGE ON SCHEMA rv120blog TO rv120blog`,
		`CREATE TABLE rv120blog.governed_events (entity pg_catalog.text NOT NULL, seq pg_catalog.int8 NOT NULL,
			event pg_catalog.text NOT NULL, append_id pg_catalog.text, PRIMARY KEY (entity, seq))`,
		`CREATE UNIQUE INDEX governed_events_append_id ON rv120blog.governed_events (entity, append_id)`,
		`GRANT ALL ON rv120blog.governed_events TO rv120blog`,
		`CREATE FUNCTION rv120blog.governed_events_next_seq_v1(e pg_catalog.text) RETURNS pg_catalog.int8 LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp AS $bide$`+nextSeqBody("rv120blog")+`$bide$`,
	)
	l, err = Open(ctx, dsn)
	if err != nil {
		t.Fatalf("restart refused: %v", err)
	}
	defer l.Close()
	evs, err := l.Events(ctx, "e", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("restarted process moved to schema %q and sees %d of the entity's 2 events", l.schema, len(evs))
	}
}
