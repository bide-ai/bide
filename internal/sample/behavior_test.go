package sample

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview"
)

// TestRunsAreInTheStateTheyClaim: the viewer reads every run Write journaled into the state group
// and kind its Run says, so the README and the run list cannot drift from the journal.
func TestRunsAreInTheStateTheyClaim(t *testing.T) {
	s := agent.NewMemStore()
	res, err := Write(context.Background(), s, "p-")
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	h := runview.Handler(runview.Options{Source: runview.JournalSource{J: j}})
	for _, r := range res.Runs {
		if !strings.HasPrefix(r.ID, "p-") {
			t.Errorf("run %q lacks the prefix", r.ID)
		}
		req := httptest.NewRequest("GET", "/runs.json?q="+r.ID, nil)
		req.Host = "127.0.0.1:8484"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(body, `"run_id": "`+r.ID+`"`) {
			t.Fatalf("%s: GET /runs.json = %d, lacks the run:\n%s", r.ID, rec.Code, body)
		}
		if !strings.Contains(body, `"group": "`+r.Group+`"`) {
			t.Errorf("%s: the viewer does not put it in group %q:\n%s", r.ID, r.Group, body)
		}
		if !strings.Contains(body, `"kind": "`+r.Kind+`"`) {
			t.Errorf("%s: the viewer does not read it as kind %q:\n%s", r.ID, r.Kind, body)
		}
		if r.Note == "" {
			t.Errorf("%s has no note", r.ID)
		}
	}
}

// TestEvidenceVerifies: the evidence packages Write exports verify under the published log key,
// and the approved run's package carries its gate's decisions.
func TestEvidenceVerifies(t *testing.T) {
	_, res := write(t)
	logV, err := audit.ParsePublicKey(res.LogPub)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evidence) != 2 {
		t.Fatalf("evidence for %d runs, want refund-1041 and refund-1049", len(res.Evidence))
	}
	for id, pkg := range res.Evidence {
		if _, err := pkg.Verify(logV); err != nil {
			t.Errorf("%s: evidence does not verify: %v", id, err)
		}
	}
	plain, gated := res.Evidence["refund-1041"], res.Evidence["refund-1049"]
	if len(gated.Actions) <= len(plain.Actions) {
		t.Errorf("the gated run's evidence has %d actions, the plain one's %d; want the gate's decisions on top", len(gated.Actions), len(plain.Actions))
	}
	for id := range res.ApproverKeys {
		if _, err := audit.ParsePublicKey(res.ApproverKeys[id]); err != nil {
			t.Errorf("approver %s key does not parse: %v", id, err)
		}
	}
}

// TestFlowsEmbedded: the three flow configs the README names are embedded, and a weakened gate is
// what refund-v2 differs in.
func TestFlowsEmbedded(t *testing.T) {
	v1, err1 := Flows.ReadFile("flows/refund-v1.yaml")
	v2, err2 := Flows.ReadFile("flows/refund-v2.yaml")
	bypass, err3 := Flows.ReadFile("flows/refund-bypass.yaml")
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("embedded flows: %v %v %v", err1, err2, err3)
	}
	if string(v1) == string(v2) || string(v1) == string(bypass) || len(v1) == 0 {
		t.Error("the three flow configs are not three different files")
	}
}

// TestWriteReportsFailure: Write names the run it failed on, and a store that cannot journal fails
// the sample rather than writing half of it silently.
func TestWriteReportsFailure(t *testing.T) {
	_, err := Write(context.Background(), failingStore{agent.NewMemStore()}, "")
	if err == nil || !strings.Contains(err.Error(), "sample: refund-0999") {
		t.Fatalf("Write over a failing store = %v, want it to name refund-0999", err)
	}
}

type failingStore struct{ agent.Store }

func (failingStore) Insert(context.Context, string, string, []byte) (agent.Entry, bool, error) {
	return agent.Entry{}, false, errors.New("full")
}

// TestWant: want accepts exactly the error type asked for and names what it got otherwise.
func TestWant(t *testing.T) {
	if err := want[*agent.OutcomeUnknown](&agent.OutcomeUnknown{}); err != nil {
		t.Errorf("want of the right type = %v", err)
	}
	err := want[*agent.OutcomeUnknown](errors.New("boom"))
	if err == nil || !strings.Contains(err.Error(), "run = boom, want *agent.OutcomeUnknown") {
		t.Errorf("want of another error = %v", err)
	}
	if err := want[*agent.OutcomeUnknown](nil); err == nil {
		t.Error("want of a nil error (the run did not fail) = nil")
	}
}

// TestLive: the live run pauses at its gate, takes two signed approvals and completes, on the wall
// clock; its journal verifies as one run that ended completed.
func TestLive(t *testing.T) {
	s := agent.NewMemStore()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Live(ctx, s, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	st, err := agent.Status(ctx, j, LiveRun)
	if err != nil || st.State != agent.RunCompleted {
		t.Fatalf("Status = %+v, %v, want completed", st, err)
	}
	recs, err := j.History(ctx, LiveRun)
	if err != nil {
		t.Fatal(err)
	}
	approvals := 0
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "approval:") {
			approvals++
		}
	}
	if approvals < 2 {
		t.Errorf("live run journaled %d approval records, want the two signed decisions", approvals)
	}
}

// TestLiveCancelled: a live run stopped between steps returns the context's error.
func TestLiveCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Live(ctx, agent.NewMemStore(), time.Hour)
	if err == nil {
		t.Fatal("Live with a cancelled context returned nil")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := Live(ctx, agent.NewMemStore(), 100*time.Millisecond); err == nil {
		t.Fatal("Live cut off mid-run returned nil")
	}
}

// TestVerifiers: an approver resolves to its key; an unknown approver or key text that does not
// parse resolves to nothing, so its signatures cannot count.
func TestVerifiers(t *testing.T) {
	_, res := write(t)
	v := verifiers(map[string]string{"ops": res.ApproverKeys["ops"], "junk": "not-a-key"})
	if got, ok := v("ops"); !ok || got == nil {
		t.Error("ops does not resolve")
	}
	if got, ok := v("finance"); ok || got != nil {
		t.Errorf("unknown approver resolved: %v", got)
	}
	if got, ok := v("junk"); ok || got != nil {
		t.Errorf("unparsable key resolved: %v", got)
	}
}
