package runview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// finishedRunStore returns a store holding one finished run "r".
func finishedRunStore(t *testing.T) *agent.MemStore {
	t.Helper()
	st := agent.NewMemStore()
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("answer")), agenttest.MustJournal(st))
	if _, err := a.Run(context.Background(), "r", agent.UserText("input")); err != nil {
		t.Fatal(err)
	}
	return st
}

func getPage(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "localhost"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// A legitimate redaction is shown, with its record named and no content; a tombstone at a name a
// redaction never replaces (the end marker here) makes the run page an error, never a run shown
// in some state.
func TestView_RedactedRecordsAndCorruption(t *testing.T) {
	ctx := context.Background()
	st := finishedRunStore(t)
	j := agenttest.MustJournal(st)
	plan, err := agent.PlanRedaction(ctx, j, "r", agent.RedactionSelector{Payloads: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Redact(ctx, j, plan, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := Handler(Options{Source: JournalSource{J: agenttest.MustJournal(st)}})
	w := getPage(h, "/run?id=r")
	if w.Code != 200 {
		t.Fatalf("the page of a redacted run = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "input") && strings.Contains(w.Body.String(), "answer") {
		t.Errorf("the page of a redacted run shows content")
	}

	// Forge a tombstone over run:complete.
	r := agent.NewMemStore()
	for e, err := range st.Load(ctx, "r", -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		if e.Name == "run:complete" {
			orig := finishedRunStore(t)
			o, _, _ := orig.Get(ctx, "r", "run:complete")
			data, _ = agent.NewTombstone("run:complete", o.Data, time.Now())
		}
		if _, _, err := r.Insert(ctx, "r", e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	w = getPage(Handler(Options{Source: JournalSource{J: agenttest.MustJournal(r)}}), "/run?id=r")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "redaction") {
		t.Fatalf("the page of a run with a forged tombstone = %d %q; want a 500 naming the redaction", w.Code, w.Body)
	}
}
