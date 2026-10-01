package postgreslog

// From the review of #120 (postgreslog side). Skips without PG_DSN.
// Uses only Open and Append, so it also runs against earlier versions.

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
)

const rv120Password = "rv120-test"

func rv120Admin(t *testing.T) (*sql.DB, string) {
	t.Helper()
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, base
}

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

func rv120Schema(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+name+` CASCADE`)
	rv120Exec(t, admin, `CREATE SCHEMA `+name)
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+name+` CASCADE`) })
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

// A governed_events table from before the append_id index, in a schema after an empty one on the
// search path. #120 says the log "keeps using the governed_events table the search path finds when
// an empty schema comes before it", but the migration's unqualified CREATE TABLE IF NOT EXISTS
// creates a second, empty governed_events in the first schema, and the log moves to it: the
// entity's history stays behind and positions restart at 0.
func TestRV120_UpgradeInLaterSchemaSplitsTheLog(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	rv120Schema(t, admin, "rv120lg_app")
	rv120Schema(t, admin, "rv120lg_pub")
	rv120Exec(t, admin,
		`CREATE TABLE rv120lg_pub.governed_events (entity text NOT NULL, seq bigint NOT NULL, event text NOT NULL, PRIMARY KEY (entity, seq))`,
		`INSERT INTO rv120lg_pub.governed_events VALUES ('e', 0, 'x'), ('e', 1, 'y'), ('e', 2, 'z')`,
	)
	dsn := rv120DSN(t, base, "", "rv120lg_app,rv120lg_pub")
	var l *Log
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		if l, err = Open(ctx, dsn); err == nil {
			break
		}
		t.Logf("Open attempt %d: %v", attempt, err)
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	seq, err := l.Append(ctx, "e", "id-1", "w")
	if err != nil {
		t.Fatal(err)
	}
	var inApp bool
	if err := admin.QueryRowContext(ctx, `SELECT to_regclass('rv120lg_app.governed_events') IS NOT NULL`).Scan(&inApp); err != nil {
		t.Fatal(err)
	}
	if seq != 3 || inApp {
		t.Fatalf("Append after the upgrade returned seq %d (want 3); a second governed_events in rv120lg_app: %v", seq, inApp)
	}
}

// A view named governed_events in a schema before the log's table. logSchema skips it (it reads
// only relkind r and p), so Open checks the table in the later schema, but Append's unqualified
// INSERT resolves to the view: the events land in another table while next_seq reads the checked
// one. The parent of #120 read the table with to_regclass, found the view, found no unique
// index, and refused to open.
func TestRV120_ViewBeforeTheTableIsNotWhatOpenChecks(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	rv120Schema(t, admin, "rv120lv_app")
	rv120Schema(t, admin, "rv120lv_pub")
	l0, err := Open(ctx, rv120DSN(t, base, "", "rv120lv_pub"))
	if err != nil {
		t.Fatal(err)
	}
	l0.Close()
	rv120Exec(t, admin,
		`CREATE TABLE rv120lv_app.shadow (LIKE rv120lv_pub.governed_events INCLUDING ALL)`,
		`CREATE VIEW rv120lv_app.governed_events AS SELECT * FROM rv120lv_app.shadow`,
	)
	l, err := Open(ctx, rv120DSN(t, base, "", "rv120lv_app,rv120lv_pub"))
	if err != nil {
		t.Logf("Open refused: %v", err)
		return
	}
	defer l.Close()
	if _, err := l.Append(ctx, "e", "id-1", "w"); err != nil {
		t.Fatal(err)
	}
	var inChecked, inShadow int
	if err := admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM rv120lv_pub.governed_events), (SELECT count(*) FROM rv120lv_app.shadow)`).Scan(&inChecked, &inShadow); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("Open accepted; the append went to the table it checked: %d row(s), to another table through the view: %d row(s)", inChecked, inShadow)
}

// The "$user" schema: a role with CREATE on the database, and none on the log's schema, takes
// over every append after Open checked the function.
func TestRV120_UserSchemaTakesOverNextSeqAfterOpen(t *testing.T) {
	admin, base := rv120Admin(t)
	ctx := context.Background()
	var dbname string
	if err := admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		t.Fatal(err)
	}
	rv120Role(t, admin, "rv120log")
	rv120Role(t, admin, "rv120attacker")
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS rv120log CASCADE`) })
	rv120Schema(t, admin, "rv120lu_app")
	rv120Exec(t, admin, `ALTER SCHEMA rv120lu_app OWNER TO rv120log`, `GRANT CREATE ON DATABASE `+dbname+` TO rv120attacker`)
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `REVOKE CREATE ON DATABASE `+dbname+` FROM rv120attacker`)
	})
	l, err := Open(ctx, rv120DSN(t, base, "rv120log", `"$user",rv120lu_app`))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append(ctx, "e", "a", "x"); err != nil {
		t.Fatal(err)
	}
	atk, err := sql.Open("pgx", rv120DSN(t, base, "rv120attacker", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer atk.Close()
	rv120Exec(t, atk,
		`CREATE SCHEMA rv120log`,
		`GRANT USAGE ON SCHEMA rv120log TO rv120log`,
		`CREATE FUNCTION rv120log.governed_events_next_seq_v1(e text) RETURNS bigint LANGUAGE plpgsql AS $$
		BEGIN DELETE FROM rv120lu_app.governed_events WHERE entity = e; RETURN 0; END $$`,
	)
	seq, err := l.Append(ctx, "e", "b", "y")
	if err != nil {
		t.Fatal(err)
	}
	var left int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM rv120lu_app.governed_events WHERE entity = 'e'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if seq != 1 || left != 2 {
		t.Fatalf("append after Open ran the attacker's rv120log.governed_events_next_seq_v1: seq=%d, rows left=%d (want seq 1, 2 rows)", seq, left)
	}
}
