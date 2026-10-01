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

// Open refuses a next_seq function whose definition is not the one this version creates. Each case
// changes one property of the exact definition, which pg_get_functiondef prints for the function
// Open created in another schema. Skips without PG_DSN.
func TestOpen_RefusesAnotherNextSeq(t *testing.T) {
	ctx := context.Background()
	db, scratch, dsn := migrationSchema(t, false)
	l, err := postgreslog.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	var exact string
	if err := db.QueryRow(`SELECT pg_get_functiondef($1::regprocedure)`, scratch+"."+nextSeqFn+"(text)").Scan(&exact); err != nil {
		t.Fatal(err)
	}
	exact = strings.Replace(exact, "CREATE OR REPLACE FUNCTION", "CREATE FUNCTION", 1)
	const setPath = " SET search_path TO 'pg_catalog', 'pg_temp'\n"
	for _, tc := range []struct{ name, old, new string }{
		{"no_lock", "pg_advisory_xact_lock", "pg_advisory_xact_lock_shared"},
		{"stable", "LANGUAGE plpgsql", "LANGUAGE plpgsql STABLE"},
		{"security_definer", "LANGUAGE plpgsql", "LANGUAGE plpgsql SECURITY DEFINER"},
		{"another_search_path", setPath, " SET search_path TO 'public'\n"},
		{"no_search_path", setPath, ""},
		{"returns_int", "RETURNS bigint", "RETURNS integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, schema, dsn := migrationSchema(t, false)
			def := strings.ReplaceAll(exact, scratch, schema)
			changed := strings.Replace(def, tc.old, tc.new, 1)
			if changed == def && !strings.Contains(tc.name, "search_path") {
				t.Fatalf("the definition holds no %q:\n%s", tc.old, def)
			}
			ddl := strings.ReplaceAll(`CREATE TABLE %s.governed_events (entity text NOT NULL, seq bigint NOT NULL, event text NOT NULL, append_id text, PRIMARY KEY (entity, seq));
				CREATE UNIQUE INDEX governed_events_append_id ON %s.governed_events (entity, append_id);`, "%s", schema)
			if _, err := db.Exec(ddl + changed); err != nil {
				t.Fatal(err)
			}
			l, err := postgreslog.Open(ctx, dsn)
			if err == nil {
				l.Close()
				t.Fatalf("Open accepted a next_seq function with another definition:\n%s", changed)
			}
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), nextSeqFn) {
				t.Fatalf("Open = %v, want an ErrConfig naming %s", err, nextSeqFn)
			}
		})
	}
}
