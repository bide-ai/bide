//go:build !bide_audit_only

package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/sample"
	"github.com/bide-ai/bide/store/sqlite"
)

// TestViewAttentionNotifyTrail: bide view -db reads the notify trail from the same SQLite file, so
// the Needs attention list says whether a halted run's notification was acknowledged.
func TestViewAttentionNotifyTrail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	w, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sample.Write(context.Background(), w, ""); err != nil {
		t.Fatal(err)
	}
	// The halted run's event, observed and acknowledged, in the trail beside the journal.
	t0 := sample.Base.Add(time.Hour)
	recs, err := agenttest.MustJournal(w).History(context.Background(), "refund-1042")
	if err != nil {
		t.Fatal(err)
	}
	var h runview.Halt
	if v := runview.Build(context.Background(), "refund-1042", recs, nil); len(v.Halts) != 1 {
		t.Fatalf("setup: %d halts", len(v.Halts))
	} else {
		h = v.Halts[0]
	}
	hk := agent.HaltedKey("refund-1042", agent.OpTool, h.ID, h.AttemptedAt.UnixMilli())
	tr := audit.NewNotifyTrail(w.EventStore())
	for i, r := range []agent.NotifyRecord{
		{Kind: agent.NotifyObserved, Key: hk, RunID: "refund-1042", At: t0, EventKind: string(agent.NotificationOutcomeUnknown), EventAt: t0, DeadlineAt: t0.Add(time.Hour)},
		{Kind: agent.NotifyClaimed, Key: hk, RunID: "refund-1042", At: t0, Holder: "n", Until: t0.Add(time.Minute)},
		{Kind: agent.NotifyAcked, Key: hk, RunID: "refund-1042", At: t0.Add(time.Second), Attempts: 1, Status: 200},
	} {
		if err := tr.Append(context.Background(), agent.NotifyTrailName("refund-1042"), i, r); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	s, c, err := stores.SQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ev := stores.Events(s)
	if ev == nil {
		t.Fatal("the SQLite opener's store has no event store")
	}
	hd := runview.Handler(runview.Options{Source: runview.JournalSource{J: agenttest.MustJournal(s)}, Events: ev})
	req := httptest.NewRequest("GET", "/attention.json", nil)
	req.Host = "127.0.0.1:8484"
	rec := httptest.NewRecorder()
	hd.ServeHTTP(rec, req)
	var l runview.AttentionList
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	if !l.Trail {
		t.Fatal("the list has no notify trail")
	}
	for _, it := range l.Items {
		if it.RunID == "refund-1042" {
			if it.Notify == nil || it.Notify.State != runview.NotifyAcked {
				t.Fatalf("refund-1042 notify = %+v, want acked", it.Notify)
			}
			return
		}
	}
	t.Fatal("refund-1042 is not on the list")
}
