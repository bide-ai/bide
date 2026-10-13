package runview

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// readOnlyStore is the store as the viewer sees it: every Insert fails and is counted.
type readOnlyStore struct {
	agent.Store
	inserts atomic.Int64
}

func (s *readOnlyStore) Insert(context.Context, string, string, []byte) (agent.Entry, bool, error) {
	s.inserts.Add(1)
	return agent.Entry{}, false, errReadOnly
}

func liveOf(t *testing.T, h http.Handler, runID string, after int64) LiveUpdate {
	t.Helper()
	code, body := get(t, h, "/run/live?id="+runID+"&after="+itoa(after))
	if code != 200 {
		t.Fatalf("GET /run/live?id=%s&after=%d = %d: %s", runID, after, code, body)
	}
	var u LiveUpdate
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestLiveFollowsRun follows a gated run from a store, as the page's script does: the run waits at
// its 2-of-3 gate, an approver decides, and the run completes. Each poll after the page's cursor
// returns only the records journaled after it, nothing when none were, and stops being live once
// the run has its end marker; the page then has no live indicator and no script. The viewer never
// writes.
func TestLiveFollowsRun(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	writer := agenttest.MustJournal(mem)
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	refund := agent.MustFunc("refund", "refund", func(context.Context, struct{ Order int }) (string, error) { return "refunded", nil }, agent.WithApproval(&policy))
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		return audit.Ed25519Verifier{Pub: key(id).Public().(ed25519.PublicKey)}, true
	}
	run := func(runID string) error {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{"Order":42}`), agenttest.TextTurn("done"))
		_, err := agenttest.MustNew(m, writer, agent.WithTools(refund), agent.WithApproverVerifiers(verifiers)).Run(ctx, runID, agent.UserText("go"))
		return err
	}
	decide := func(who string) {
		t.Helper()
		recs, _ := writer.History(ctx, "l1")
		_, call, _ := agent.FindToolCall(recs, "c1")
		subj := agent.ApprovalSubject{RunID: "l1", ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
		sig := ed25519.Sign(key(who), agent.ApprovalDecisionBytes(subj, who, true))
		if err := agent.SubmitDecision(ctx, writer, agent.Decision{RunID: "l1", ToolUseID: "c1", ApproverID: who, Approved: true, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run("l1"); !isPending(err) {
		t.Fatalf("Run = %v, want approval pending", err)
	}
	ro := &readOnlyStore{Store: mem}
	h := Handler(Options{Source: JournalSource{J: agenttest.MustJournal(ro)}})

	v := viewOf(t, h, "l1")
	if !v.Live || v.Cursor <= 0 || v.State != "waiting" {
		t.Fatalf("waiting run: live %v, cursor %d, state %q; want live, a cursor, waiting", v.Live, v.Cursor, v.State)
	}
	code, page := get(t, h, "/run?id=l1")
	if code != 200 || !strings.Contains(page, `id="live"`) || !strings.Contains(page, `data-cursor="`+itoa(v.Cursor)+`"`) || !strings.Contains(page, `<script src="/assets/live.js">`) {
		t.Fatalf("GET /run of a run in progress = %d, want the live indicator with the cursor and the script:\n%s", code, page)
	}
	// Without its script the page is complete: every record's row and the state are in the HTML,
	// and it says how to see new records.
	for _, e := range v.Events {
		if !strings.Contains(page, `<span class="seq">`+itoa(int64(e.Seq))+`</span>`) {
			t.Errorf("the page lacks timeline row %d (%s)", e.Seq, e.Name)
		}
	}
	if !strings.Contains(page, "waiting for approval") || !strings.Contains(page, "<noscript>") {
		t.Errorf("the page lacks its state or its no-script reload link")
	}

	// Nothing journaled: no change, still live, same cursor.
	if u := liveOf(t, h, "l1", v.Cursor); u.Changed || !u.Live || u.Cursor != v.Cursor || len(u.Events) != 0 {
		t.Fatalf("poll with nothing new = %+v, want unchanged and live at cursor %d", u, v.Cursor)
	}
	// Another run's records do not change this run.
	if err := run("other"); !isPending(err) {
		t.Fatalf("other Run = %v", err)
	}
	if u := liveOf(t, h, "l1", v.Cursor); u.Changed {
		t.Fatalf("another run's records changed this run: %+v", u)
	}

	// One decision: exactly its record, still live (waiting on a second approver).
	decide("ops")
	u := liveOf(t, h, "l1", v.Cursor)
	if !u.Changed || !u.Live || u.Cursor <= v.Cursor || u.State != "waiting" || len(u.Events) != 1 || u.Events[0].Seq != v.Records+1 || u.Events[0].Class != "approval" {
		t.Fatalf("poll after one decision = %+v, want the one approval record (row %d), live, waiting", u, v.Records+1)
	}
	v2 := viewOf(t, h, "l1")

	// The second decision and the resume: the records after the cursor, ending in run:complete;
	// the run is no longer live.
	decide("finance")
	if err := run("l1"); err != nil {
		t.Fatal(err)
	}
	u = liveOf(t, h, "l1", u.Cursor)
	v3 := viewOf(t, h, "l1")
	if !u.Changed || u.Live || u.State != "completed" || len(u.Events) != v3.Records-v2.Records {
		t.Fatalf("poll after the run ended = changed %v, live %v, state %q, %d events; want changed, not live, completed, %d events", u.Changed, u.Live, u.State, len(u.Events), v3.Records-v2.Records)
	}
	for i, e := range u.Events {
		if e.Seq != v2.Records+1+i {
			t.Errorf("event %d is row %d, want row %d: only the records after the cursor", i, e.Seq, v2.Records+1+i)
		}
	}
	if last := u.Events[len(u.Events)-1]; last.Name != "run:complete" {
		t.Errorf("last new record = %q, want run:complete", last.Name)
	}
	if v3.Live {
		t.Errorf("a completed run's view is live")
	}
	if _, page := get(t, h, "/run?id=l1"); strings.Contains(page, `id="live"`) || strings.Contains(page, "live.js") {
		t.Errorf("a completed run's page still has the live indicator or script")
	}
	// after -1 is every record.
	if u := liveOf(t, h, "l1", -1); len(u.Events) != v3.Records {
		t.Errorf("poll after -1 = %d events, want all %d", len(u.Events), v3.Records)
	}
	if n := ro.inserts.Load(); n != 0 {
		t.Fatalf("the viewer tried %d writes", n)
	}
}

// TestLiveStaticExport: an exported journal does not grow, so its page is not live, has no
// indicator or script, and /run/live is not found.
func TestLiveStaticExport(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	m := agenttest.NewScriptedModel(agenttest.TextTurn("hello"))
	if _, err := agenttest.MustNew(m, j).Run(ctx, "x1", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	recs, err := j.History(ctx, "x1")
	if err != nil {
		t.Fatal(err)
	}
	// A run cut before its end marker, as an export of a run in progress would be.
	h := Handler(Options{Source: Exports{"x1": recs[:len(recs)-1]}})
	if v := viewOf(t, h, "x1"); v.Live || v.Cursor != 0 || v.State != "started" {
		t.Fatalf("exported run in progress: live %v, cursor %d, state %q; want static and started", v.Live, v.Cursor, v.State)
	}
	if _, page := get(t, h, "/run?id=x1"); strings.Contains(page, `id="live"`) || strings.Contains(page, "live.js") {
		t.Errorf("an exported run's page has the live indicator or script")
	}
	if code, _ := get(t, h, "/run/live?id=x1&after=0"); code != http.StatusNotFound {
		t.Errorf("GET /run/live over exports = %d, want 404", code)
	}
}

// TestLiveGuarded: /run/live answers only GET and HEAD on a loopback host, and wants a cursor.
func TestLiveGuarded(t *testing.T) {
	h := Handler(Options{Source: JournalSource{J: agenttest.MemJournal()}})
	req := httptest.NewRequest("GET", "/run/live?id=x&after=0", nil)
	req.Host = "attacker.example:8484"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /run/live to another host = %d, want 403", rec.Code)
	}
	req = httptest.NewRequest("POST", "/run/live?id=x&after=0", nil)
	req.Host = "127.0.0.1:8484"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /run/live = %d, want 405", rec.Code)
	}
	if code, _ := get(t, h, "/run/live?id=x&after=soon"); code != http.StatusBadRequest {
		t.Errorf("GET /run/live with a bad cursor = %d, want 400", code)
	}
	if code, body := get(t, h, "/assets/live.js"); code != 200 || !strings.Contains(body, "/run/live") {
		t.Errorf("GET /assets/live.js = %d", code)
	}
}

// TestEnded: only an end marker ends a run; a halted or waiting run can still be resumed.
func TestEnded(t *testing.T) {
	for state, want := range map[string]bool{"completed": true, "aborted": true, "cancelled": true, "started": false, "waiting": false, "halted": false, "not_started": false} {
		if got := ended(state); got != want {
			t.Errorf("ended(%q) = %v, want %v", state, got, want)
		}
	}
}
