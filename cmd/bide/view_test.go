//go:build !bide_audit_only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// followRun checks live mode through the store opener bide view uses: a run paused at a gate is
// live, a poll after its cursor is unchanged until the run goes on, and then returns only the new
// records and stops being live once the run completes.
func followRun(t *testing.T, w *agent.Journal, open func(string) (agent.Store, io.Closer, error), where, runID string) {
	t.Helper()
	ctx := context.Background()
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	run := func() error {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
		_, err := agenttest.MustNew(m, w, agent.WithTools(tool)).Run(ctx, runID, agent.UserText("go"))
		return err
	}
	if _, ok := errors.AsType[*agent.ApprovalPending](run()); !ok {
		t.Fatal("the run did not pause at its gate")
	}
	s, c, err := open(where)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer c.Close()
	h := runview.Handler(runview.Options{Source: runview.JournalSource{J: agenttest.MustJournal(s)}})
	getJSON := func(target string, v any) {
		t.Helper()
		req := httptest.NewRequest("GET", target, nil)
		req.Host = "127.0.0.1:8484"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatal(err)
		}
	}
	var v runview.View
	getJSON("/run.json?id="+runID, &v)
	if !v.Live || v.State != "waiting" {
		t.Fatalf("paused run: live %v, state %q; want live and waiting", v.Live, v.State)
	}
	live := "/run/live?id=" + runID + "&after=" + strconv.FormatInt(v.Cursor, 10)
	var u runview.LiveUpdate
	getJSON(live, &u)
	if u.Changed || !u.Live {
		t.Fatalf("poll with nothing new = %+v", u)
	}
	if err := agent.Approve(ctx, w, runID, "c1", true); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	var end runview.View
	getJSON("/run.json?id="+runID, &end)
	u = runview.LiveUpdate{}
	getJSON(live, &u)
	if !u.Changed || u.Live || u.State != "completed" || len(u.Events) != end.Records-v.Records || u.Events[0].Seq != v.Records+1 {
		t.Fatalf("poll after the run completed = changed %v, live %v, state %q, %d events; want the %d new records, completed", u.Changed, u.Live, u.State, len(u.Events), end.Records-v.Records)
	}
}

// TestSQLiteLive: live mode over a SQLite journal opened read-only, written by another handle.
func TestSQLiteLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	w, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	followRun(t, agenttest.MustJournal(w), stores.SQLite, path, "l1")
}

// TestPostgresLive: live mode over a Postgres journal read in read-only sessions (PG_DSN).
func TestPostgresLive(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	w, err := postgres.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	followRun(t, agenttest.MustJournal(w), stores.Postgres, dsn, "bide-view-live-"+time.Now().Format("150405.000000000"))
}
