package postgreslog_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern/postgreslog"
)

const nextSeqFn = "governed_events_next_seq_v1"

// nextSeqSource returns the source of schema's next_seq function, or "" if there is none.
func nextSeqSource(t *testing.T, db *sql.DB, schema string) string {
	t.Helper()
	var src sql.NullString
	if err := db.QueryRow(`SELECT prosrc FROM pg_proc WHERE oid = to_regprocedure($1)`, schema+"."+nextSeqFn+"(text)").Scan(&src); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return src.String
}

// Open creates the next_seq function Append calls, in a new schema and beside a current table
// from an earlier version (whose migration Open otherwise skips). Skips without PG_DSN.
func TestOpen_CreatesNextSeq(t *testing.T) {
	ctx := context.Background()
	for _, current := range []bool{false, true} {
		db, schema, dsn := migrationSchema(t, false)
		if current {
			if _, err := db.Exec(strings.ReplaceAll(`CREATE TABLE %s.governed_events (entity text NOT NULL, seq bigint NOT NULL, event text NOT NULL, append_id text, PRIMARY KEY (entity, seq));
				CREATE UNIQUE INDEX governed_events_append_id ON %s.governed_events (entity, append_id);`, "%s", schema)); err != nil {
				t.Fatal(err)
			}
		}
		l, err := postgreslog.Open(ctx, dsn)
		if err != nil {
			t.Fatalf("current=%v: %v", current, err)
		}
		if src := nextSeqSource(t, db, schema); !strings.Contains(src, "pg_advisory_xact_lock") {
			t.Fatalf("current=%v: %s = %q, want a function that locks the entity", current, nextSeqFn, src)
		}
		if _, err := l.Append(ctx, "e", "a", "ea"); err != nil {
			t.Fatalf("current=%v: Append through the created function: %v", current, err)
		}
		l.Close()
	}
}

// Open refuses a next_seq function whose definition is not the one this version creates. Skips
// without PG_DSN.
func TestOpen_RefusesAnotherNextSeq(t *testing.T) {
	ctx := context.Background()
	db, schema, dsn := migrationSchema(t, false)
	l, err := postgreslog.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	want := nextSeqSource(t, db, schema)
	if want == "" {
		t.Fatalf("Open created no %s", nextSeqFn)
	}
	quote := func(src string) string { return "$src$" + src + "$src$" }
	noLock := `BEGIN RETURN (SELECT COALESCE(MAX(seq), -1) + 1 FROM governed_events WHERE entity = e); END`
	for _, tc := range []struct{ name, def string }{
		{"no_lock", `CREATE FUNCTION %s.` + nextSeqFn + `(e text) RETURNS bigint LANGUAGE plpgsql VOLATILE AS ` + quote(noLock)},
		{"stable", `CREATE FUNCTION %s.` + nextSeqFn + `(e text) RETURNS bigint LANGUAGE plpgsql STABLE AS ` + quote(want)},
		{"security_definer", `CREATE FUNCTION %s.` + nextSeqFn + `(e text) RETURNS bigint LANGUAGE plpgsql VOLATILE SECURITY DEFINER AS ` + quote(want)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, schema, dsn := migrationSchema(t, false)
			ddl := `CREATE TABLE %s.governed_events (entity text NOT NULL, seq bigint NOT NULL, event text NOT NULL, append_id text, PRIMARY KEY (entity, seq));
				CREATE UNIQUE INDEX governed_events_append_id ON %s.governed_events (entity, append_id);` + tc.def
			if _, err := db.Exec(strings.ReplaceAll(ddl, "%s", schema)); err != nil {
				t.Fatal(err)
			}
			l, err := postgreslog.Open(ctx, dsn)
			if err == nil {
				l.Close()
				t.Fatal("Open accepted a next_seq function with another definition")
			}
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), nextSeqFn) {
				t.Fatalf("Open = %v, want an ErrConfig naming %s", err, nextSeqFn)
			}
		})
	}
}
