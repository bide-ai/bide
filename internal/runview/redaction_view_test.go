package runview

import (
	"context"
	"encoding/hex"
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

	// Forge a tombstone over run:limits:1, a record no redaction replaces.
	r := agent.NewMemStore()
	for e, err := range st.Load(ctx, "r", -1) {
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.Insert(ctx, "r", e.Name, e.Data); err != nil {
			t.Fatal(err)
		}
	}
	forged, err := agent.NewTombstone("run:limits:1", []byte(`{"name":"run:limits:1","kind":"value"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Insert(ctx, "r", "run:limits:1", forged); err != nil {
		t.Fatal(err)
	}
	w = getPage(Handler(Options{Source: JournalSource{J: agenttest.MustJournal(r)}}), "/run?id=r")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "redaction") {
		t.Fatalf("the page of a run with a forged tombstone = %d %q; want a 500 naming the redaction", w.Code, w.Body)
	}
}

// A run whose end marker was erased is shown as ended with the kind not attested, never as completed,
// and renaming the tombstone (completed to cancelled) changes nothing on the page.
func TestView_AnErasedEndMarkerIsEndedNotAttested(t *testing.T) {
	ctx := context.Background()
	st := finishedRunStore(t)
	j := agenttest.MustJournal(st)
	plan, err := agent.PlanRedaction(ctx, j, "r", agent.RedactionSelector{Names: []string{"run:complete"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Redact(ctx, j, plan, time.Now()); err != nil {
		t.Fatal(err)
	}
	renamed := agent.NewMemStore()
	for e, err := range st.Load(ctx, "r", -1) {
		if err != nil {
			t.Fatal(err)
		}
		name, data := e.Name, e.Data
		if name == "run:complete" {
			leaf, _ := agent.TombstoneLeafHash(data)
			name = "run:cancelled"
			data = []byte(`{"redacted":{"leaf_hash":"` + hex.EncodeToString(leaf) + `","at_ms":1,"name":"run:cancelled"}}`)
		}
		if _, _, err := renamed.Insert(ctx, "r", name, data); err != nil {
			t.Fatal(err)
		}
	}
	for name, s := range map[string]*agent.MemStore{"erased": st, "renamed": renamed} {
		w := getPage(Handler(Options{Source: JournalSource{J: agenttest.MustJournal(s)}}), "/run?id=r")
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "ENDED (ERASED)") || strings.Contains(body, ">COMPLETED<") || strings.Contains(body, ">FAILED<") {
			t.Errorf("%s: the page = %d; want ENDED (ERASED), neither COMPLETED nor FAILED", name, w.Code)
		}
		if !strings.Contains(body, "end (erased; how it ended is not attested)") {
			t.Errorf("%s: the timeline does not say how the run ended is not attested", name)
		}
	}
}
