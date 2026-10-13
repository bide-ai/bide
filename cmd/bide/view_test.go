//go:build !bide_audit_only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/runview/runlistdemo"
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

// runList serves the run list through the store opener bide view uses and returns it.
func runList(t *testing.T, open func(string) (agent.Store, io.Closer, error), where, query string) runview.RunList {
	t.Helper()
	s, c, err := open(where)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer c.Close()
	h := runview.Handler(runview.Options{Source: runview.JournalSource{J: agenttest.MustJournal(s)}})
	req := httptest.NewRequest("GET", "/runs.json"+query, nil)
	req.Host = "127.0.0.1:8484"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var l runview.RunList
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &l) != nil {
		t.Fatalf("GET /runs.json = %d: %s", rec.Code, rec.Body)
	}
	return l
}

// checkRunList: the list holds each demo run in its state group, with its kind.
func checkRunList(t *testing.T, l runview.RunList, runs []runlistdemo.Run) {
	t.Helper()
	if l.Err != "" || len(l.Rows) != len(runs) {
		t.Fatalf("list: err %q, %d rows, want %d", l.Err, len(l.Rows), len(runs))
	}
	got := map[string]runview.RunRow{}
	for _, r := range l.Rows {
		got[r.ID] = r
	}
	for _, want := range runs {
		if r, ok := got[want.ID]; !ok || r.Group != want.Group || r.Kind != want.Kind {
			t.Errorf("%s: listed %v as %q %q, want %q %q", want.ID, ok, r.Group, r.Kind, want.Group, want.Kind)
		}
	}
}

// TestRunListDemo: bide view lists a SQLite journal's runs, opened read-only, each in its state.
// With BIDE_RUNLIST_DEMO set to a path it writes the demo journal there, the one the run viewer
// guide's screenshots are taken of (bide view -db <path>).
func TestRunListDemo(t *testing.T) {
	path := os.Getenv("BIDE_RUNLIST_DEMO")
	if path == "" {
		path = filepath.Join(t.TempDir(), "runs.db")
	} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	w, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runlistdemo.Write(context.Background(), w, "")
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	checkRunList(t, runList(t, stores.SQLite, path, ""), runs)
	if l := runList(t, stores.SQLite, path, "?state=halted"); len(l.Rows) != 2 {
		t.Fatalf("halted runs = %+v, want 2", l.Rows)
	}
}

// TestPostgresRunList: bide view lists a Postgres journal's runs, read in a read-only session.
// It runs against the CI Postgres (PG_DSN), whose other runs the run ID filter leaves out.
func TestPostgresRunList(t *testing.T) {
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
	prefix := "bide-view-list-" + time.Now().Format("150405.000000000") + "-"
	runs, err := runlistdemo.Write(ctx, w, prefix)
	if err != nil {
		t.Fatal(err)
	}
	checkRunList(t, runList(t, stores.Postgres, dsn, "?q="+url.QueryEscape(prefix)), runs)
	if l := runList(t, stores.Postgres, dsn, "?kind=flow&q="+url.QueryEscape(prefix)); len(l.Rows) != 3 {
		t.Fatalf("flow runs = %+v, want 3", l.Rows)
	}
}
