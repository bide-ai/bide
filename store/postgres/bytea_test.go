package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// A tool result whose JSON carries invalid UTF-8 inside a string is recorded, as MemStore and
// SQLite record it. Postgres text columns reject such bytes (SQLSTATE 22021), and failing here
// would fail the step after the tool's side effect already happened: a non-retriable tool would
// then halt on every resume. Skips without PG_DSN.
func TestPostgres_RecordsInvalidUTF8AfterTheEffect(t *testing.T) {
	s, ctx := openTestStore(t)
	j := agenttest.MustJournal(s)
	runID := uniqueID(t, "pg-utf8-")
	result := json.RawMessage("{\"body\":\"caf\xe9 \xff\"}")
	live, err := journaltest.Do(ctx, j, runID, "charge", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "charge", Result: result}, nil
	})
	if err != nil {
		t.Fatalf("Do after the effect: %v", err)
	}
	hist, err := j.History(ctx, runID)
	if err != nil || len(hist) != 2 { // the journal header, then the step
		t.Fatalf("History = %d records, %v", len(hist), err)
	}
	if string(live.Result) != string(result) || string(hist[1].Result) != string(result) {
		t.Fatalf("recorded %q; Do returned %q, History %q", result, live.Result, hist[1].Result)
	}
}

// freshSchema creates an empty schema for one test and returns a DSN whose connections use it, so
// the test sees its own bide_steps table. Skips without PG_DSN.
func freshSchema(t *testing.T) (dsn, schema string, admin *sql.DB) {
	t.Helper()
	base := os.Getenv("PG_DSN")
	if base == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	schema = fmt.Sprintf("bide_migrate_%d", time.Now().UnixNano())
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "search_path=" + schema, schema, admin
}

// Nodes that open the store at the same time over an empty database all succeed: the schema is
// created once, under a lock, rather than each node racing to create the same tables. Skips
// without PG_DSN.
func TestPostgres_ConcurrentOpensCreateTheSchemaOnce(t *testing.T) {
	for range 5 {
		dsn, _, _ := freshSchema(t)
		const nodes = 8
		errs := make(chan error, nodes)
		start := make(chan struct{})
		for range nodes {
			go func() {
				<-start
				s, err := Open(context.Background(), dsn)
				if err == nil {
					s.Close()
				}
				errs <- err
			}()
		}
		close(start)
		for range nodes {
			if err := <-errs; err != nil {
				t.Fatalf("a concurrent Open failed: %v", err)
			}
		}
	}
}

// Open creates bide_steps with a bytea data column, and never rewrites an existing bide_steps
// table: altering a column takes the table's exclusive lock and changes what the nodes already
// running on it write, so a node that opens the store during a rolling deploy would break them.
// Skips without PG_DSN.
func TestPostgres_OpenCreatesByteaAndNeverAltersTheJournal(t *testing.T) {
	ctx := context.Background()
	dataType := func(admin *sql.DB, schema string) string {
		t.Helper()
		var typ string
		if err := admin.QueryRowContext(ctx, `SELECT data_type FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'bide_steps' AND column_name = 'data'`, schema).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		return typ
	}
	dsn, schema, admin := freshSchema(t)
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if typ := dataType(admin, schema); typ != "bytea" {
		t.Fatalf("Open created bide_steps.data as %s, want bytea", typ)
	}

	dsn, schema, admin = freshSchema(t)
	if _, err := admin.ExecContext(ctx, `CREATE TABLE `+schema+`.bide_steps (
		run_id text NOT NULL, seq bigint NOT NULL, name text NOT NULL, data text NOT NULL,
		PRIMARY KEY (run_id, name), UNIQUE (run_id, seq))`); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if typ := dataType(admin, schema); typ != "text" {
		t.Fatalf("Open rewrote an existing bide_steps.data column from text to %s", typ)
	}
}
