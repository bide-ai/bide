//go:build !bide_audit_only

package main

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

// drive runs a one-tool agent run to completion on j.
func drive(t *testing.T, j *agent.Journal, runID string) {
	t.Helper()
	tool := agent.MustFunc("lookup", "look up", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "lookup", `{}`), agenttest.TextTurn("done"))
	if _, err := agenttest.MustNew(m, j, agent.WithTools(tool)).Run(context.Background(), runID, agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
}

// page serves the run through the store opener bide view uses and returns the run page.
func page(t *testing.T, open func(string) (agent.Store, io.Closer, error), where, runID string) string {
	t.Helper()
	s, c, err := open(where)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer c.Close()
	h := runview.Handler(runview.Options{Source: runview.JournalSource{J: agenttest.MustJournal(s)}})
	req := httptest.NewRequest("GET", "/run?id="+runID, nil)
	req.Host = "127.0.0.1:8484"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /run = %d: %s", rec.Code, rec.Body)
	}
	if _, _, err := s.Insert(context.Background(), runID, "x", []byte(`{}`)); err == nil {
		t.Fatal("the store bide view opened accepted a write")
	}
	return rec.Body.String()
}

// TestSQLite: bide view's SQLite opener reads a journal read-only.
func TestSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	w, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	drive(t, agenttest.MustJournal(w), "s1")
	w.Close()
	if body := page(t, stores.SQLite, path, "s1"); !strings.Contains(body, "completed") {
		t.Fatal("the run page does not show the completed run")
	}
}

// TestPostgres: bide view's Postgres opener reads a journal in a read-only session. It runs
// against the CI Postgres (PG_DSN).
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	w, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	runID := "bide-view-" + time.Now().Format("150405.000000000")
	drive(t, agenttest.MustJournal(w), runID)
	if body := page(t, stores.Postgres, dsn, runID); !strings.Contains(body, "completed") {
		t.Fatal("the run page does not show the completed run")
	}
}
