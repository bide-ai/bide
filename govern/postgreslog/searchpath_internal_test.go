package postgreslog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Nothing the log sends resolves through the search path after Open: Open records the schema and
// every statement names it. The log's only connection is pointed, after Open, at a decoy schema
// holding a governed_events table and a next_seq that raises; appends and reads must still use
// the log's own schema, and leave the decoy untouched. Skips without PG_DSN.
func TestLog_NothingResolvesThroughSearchPathAfterOpen(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // an append into a decoy collides and retries
	defer cancel()
	sfx := time.Now().UnixNano()
	app, decoy := fmt.Sprintf("rv_logapp3_%d", sfx), fmt.Sprintf("rv_logdecoy_%d", sfx)
	for _, s := range []string{app, decoy} {
		if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+s); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+s+` CASCADE`) })
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE %[1]s.governed_events (entity text NOT NULL, seq bigint NOT NULL, event text NOT NULL, append_id text, PRIMARY KEY (entity, seq));
		CREATE UNIQUE INDEX governed_events_append_id ON %[1]s.governed_events (entity, append_id);
		CREATE FUNCTION %[1]s.governed_events_next_seq_v1(e text) RETURNS bigint LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'decoy next_seq ran'; END $$;`, decoy)); err != nil {
		t.Fatal(err)
	}
	l, err := Open(ctx, rvDSN(t, base, "", app))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.db.SetMaxOpenConns(1) // one connection, which the SET below points at the decoy
	if _, err := l.db.ExecContext(ctx, `SET search_path = `+decoy); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"a", "b"} {
		if seq, err := l.Append(ctx, "e", id, "e"+id); err != nil || seq != int64(i) {
			t.Fatalf("Append(%s) = %d, %v; want %d", id, seq, err, i)
		}
	}
	if seq, err := l.Append(ctx, "e", "a", "ea"); err != nil || seq != 0 {
		t.Fatalf("a repeated Append = %d, %v; want the recorded 0", seq, err)
	}
	if events, err := l.Events(ctx, "e", 0); err != nil || len(events) != 2 {
		t.Fatalf("Events = %v, %v; want 2", events, err)
	}
	var own, decoyRows int
	if err := admin.QueryRowContext(ctx, fmt.Sprintf(`SELECT (SELECT count(*) FROM %s.governed_events), (SELECT count(*) FROM %s.governed_events)`, app, decoy)).Scan(&own, &decoyRows); err != nil {
		t.Fatal(err)
	}
	if own != 2 || decoyRows != 0 {
		t.Fatalf("the log's schema holds %d events and the decoy %d; want 2 and 0", own, decoyRows)
	}
}

// Open refuses when the first relation on the search path named governed_events is not an
// ordinary or partitioned table (a view, a sequence): appends would go through it while Open
// checked another table. Skips without PG_DSN.
func TestOpen_RefusesANonTableGovernedEvents(t *testing.T) {
	admin, base := rvAdmin(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, ddl string }{
		{"view", `CREATE TABLE %[1]s.shadow (entity text, seq bigint, event text, append_id text); CREATE VIEW %[1]s.governed_events AS SELECT * FROM %[1]s.shadow`},
		{"sequence", `CREATE SEQUENCE %[1]s.governed_events`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sfx := time.Now().UnixNano()
			first, later := fmt.Sprintf("rv_logfirst_%d", sfx), fmt.Sprintf("rv_loglater_%d", sfx)
			for _, s := range []string{first, later} {
				if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+s); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+s+` CASCADE`) })
			}
			l, err := Open(ctx, rvDSN(t, base, "", later))
			if err != nil {
				t.Fatal(err)
			}
			l.Close()
			if _, err := admin.ExecContext(ctx, fmt.Sprintf(tc.ddl, first)); err != nil {
				t.Fatal(err)
			}
			l, err = Open(ctx, rvDSN(t, base, "", first+","+later))
			if err == nil {
				l.Close()
				t.Fatal("Open accepted a search path whose first governed_events is not a table")
			}
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "governed_events") {
				t.Fatalf("Open = %v, want an ErrConfig naming governed_events", err)
			}
		})
	}
}
