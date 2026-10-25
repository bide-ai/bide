//go:build !bide_audit_only

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/flowedit"
	"github.com/bide-ai/bide/internal/editrun"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/store/sqlite"
)

const editRunCfg = `{"version":1,"flow":"pay","in":"int","out":"int","entry":"a",
 "nodes":[{"name":"a","block":"inc"},{"name":"b","block":"dbl"}],
 "wiring":[{"edge":["a","b"]}]}`

// bide edit -run reads a SQLite journal through the read-only opener: the store it is handed rejects a
// write, a writer that keeps writing is not blocked by the editor reading (and re-reading) the run, and
// the comparison it makes is with the digest the run journaled.
func TestEditRunReadsASQLiteJournalReadOnly(t *testing.T) {
	reg := plan.NewRegistry()
	if err := reg.RegisterStep("inc", func(_ context.Context, n int) (int, error) { return n + 1, nil }); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterStep("dbl", func(_ context.Context, n int) (int, error) { return n * 2, nil }); err != nil {
		t.Fatal(err)
	}
	flow, err := plan.Load[int, int]([]byte(editRunCfg), reg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "j.db")
	w, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	j, err := agent.NewJournal(w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(context.Background(), j, "r1", 1); err != nil {
		t.Fatal(err)
	}

	op, err := runview.StoreFlags{DB: db}.Open(stores)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Closer.Close()
	if _, _, err := op.Store.Insert(context.Background(), "r1", "x", []byte(`{}`)); err == nil {
		t.Fatal("the store bide edit opened accepted a write")
	}

	cfg := filepath.Join(dir, "flow.json")
	if err := os.WriteFile(cfg, []byte(editRunCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := flowedit.New(cfg, flowedit.WithRegistry(reg), editrun.NewOption(editrun.Run{ID: "r1", Source: op.Source}).(flowedit.Option))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ask := func(reload bool) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"text": editRunCfg, "reload": reload})
		req := httptest.NewRequest("POST", "http://127.0.0.1:8585/api/run", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1:8585")
		req.Header.Set(flowedit.TokenHeader, srv.Token())
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/api/run: %d %s", rec.Code, rec.Body)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := ask(false); m["status"] != "same" || m["run_digest"] != flow.Digest() {
		t.Fatalf("%v", m)
	}
	// The writer writes on, without waiting for the reader, while the run is read again.
	done := make(chan error, 1)
	go func() {
		_, _, err := w.Insert(context.Background(), "r1", "late-record", []byte(`{}`))
		done <- err
	}()
	if m := ask(true); m["status"] != "same" {
		t.Fatalf("after a reload: %v", m)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the writer failed while the editor read: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the writer was blocked by the editor's read")
	}
}
