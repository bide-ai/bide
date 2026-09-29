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
)

// A tool result whose JSON carries invalid UTF-8 inside a string is recorded, as MemStore and
// SQLite record it. Postgres text columns reject such bytes (SQLSTATE 22021), and failing here
// would fail the step after the tool's side effect already happened: a non-retriable tool would
// then halt on every resume. Skips without PG_DSN.
func TestPostgres_RecordsInvalidUTF8AfterTheEffect(t *testing.T) {
	s, ctx := openTestStore(t)
	runID := uniqueID(t, "pg-utf8-")
	result := json.RawMessage("{\"body\":\"caf\xe9 \xff\"}")
	live, err := s.Do(ctx, runID, "charge", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "charge", Result: result}, nil
	})
	if err != nil {
		t.Fatalf("Do after the effect: %v", err)
	}
	hist, err := s.History(ctx, runID)
	if err != nil || len(hist) != 1 {
		t.Fatalf("History = %d records, %v", len(hist), err)
	}
	if string(live.Result) != string(result) || string(hist[0].Result) != string(result) {
		t.Fatalf("recorded %q; Do returned %q, History %q", result, live.Result, hist[0].Result)
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

// createTextJournal creates bide_steps as it was before data became bytea, with one row.
func createTextJournal(t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	if _, err := admin.ExecContext(context.Background(), `
		CREATE TABLE `+schema+`.bide_steps (
			run_id text   NOT NULL,
			seq    bigint NOT NULL,
			name   text   NOT NULL,
			data   text   NOT NULL,
			PRIMARY KEY (run_id, name),
			UNIQUE (run_id, seq)
		);
		INSERT INTO `+schema+`.bide_steps VALUES ('old', 0, 'step', '{"name":"step","kind":"value","result":"caf`+"\xc3\xa9"+`"}');`); err != nil {
		t.Fatal(err)
	}
}

// Nodes that open the store at the same time over an old text journal all succeed: the
// migration runs once, under a lock, rather than each node converting a column another node
// already converted. Skips without PG_DSN.
func TestPostgres_ConcurrentOpensMigrateOnce(t *testing.T) {
	dsn, schema, admin := freshSchema(t)
	createTextJournal(t, admin, schema)
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

// A bide_steps table created before data became bytea is converted in place when the store
// opens, keeping its rows, so invalid UTF-8 can be recorded in it afterwards. Skips without
// PG_DSN.
func TestPostgres_MigratesTextJournalToBytea(t *testing.T) {
	ctx := context.Background()
	dsn, schema, admin := freshSchema(t)
	createTextJournal(t, admin, schema)
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open over a text journal: %v", err)
	}
	defer s.Close()
	var typ string
	if err := s.db.QueryRowContext(ctx, `SELECT data_type FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'bide_steps' AND column_name = 'data'`, schema).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != "bytea" {
		t.Fatalf("data column is %s after Open, want bytea", typ)
	}
	hist, err := s.History(ctx, "old")
	if err != nil || len(hist) != 1 || string(hist[0].Result) != "\"caf\xc3\xa9\"" {
		t.Fatalf("History of the migrated run = %v, %v; want the one recorded step", hist, err)
	}
	if _, err := s.Do(ctx, "old", "next", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage("\"\xff\"")}, nil
	}); err != nil {
		t.Fatalf("Do with invalid UTF-8 after the migration: %v", err)
	}
	// Opening again finds bytea and leaves it alone.
	s2, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	s2.Close()
}
