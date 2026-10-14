package approvals_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
)

// The pending call is read from the journal: what the approver will sign is shown with the
// digest the signature binds, which a client can recompute from the arguments it shows.
func TestPending_ShowsTheCallFromTheJournal(t *testing.T) {
	r := newRig(t)
	status, out := r.get("/pending?run=" + runID + "&call=c1")
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	if out["tool"] != "charge" || out["run"] != runID || out["call"] != "c1" || out["state"] != "waiting" {
		t.Fatalf("pending: %v", out)
	}
	args, _ := json.Marshal(out["args"])
	if !strings.Contains(string(args), `"amount":120`) {
		t.Fatalf("args %s", args)
	}
	s := r.subject()
	want := sha256.Sum256(canonical(t, s.Args))
	if out["args_sha256"] != hex.EncodeToString(want[:]) {
		t.Fatalf("args_sha256 %v, want %x", out["args_sha256"], want)
	}
	if out["decision_format"] != agent.DecisionFormat {
		t.Fatalf("decision_format %v", out["decision_format"])
	}
	gate := out["gate"].(map[string]any)
	if gate["need"] != float64(2) || len(gate["approvers"].([]any)) != 3 {
		t.Fatalf("gate %v", gate)
	}
}

// canonical re-serializes JSON with sorted keys and no spacing, as the signed digest does.
func canonical(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v) // map keys are sorted
	return b
}

func TestPending_UnknownIsOneNotFound(t *testing.T) {
	r := newRig(t)
	for _, path := range []string{"/pending?run=nope&call=c1", "/pending?run=" + runID + "&call=c9", "/pending?run=" + runID, "/pending"} {
		status, out := r.get(path)
		if status != http.StatusNotFound && status != http.StatusBadRequest {
			t.Errorf("%s: status %d %v", path, status, out)
		}
	}
	a, b := responseBody(t, r, "/pending?run=nope&call=c1"), responseBody(t, r, "/pending?run="+runID+"&call=c9")
	if a != b {
		t.Errorf("a missing run and a missing call answer differently:\n%s\n%s", a, b)
	}
}

func responseBody(t *testing.T, r *rig, path string) string {
	t.Helper()
	resp, err := http.Get(r.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// A valid decision is recorded through the application's own journal, so the AuditedStore signs a
// head over it before the response, and a replay changes nothing.
func TestDecision_RecordedSignedAndReplayIsANoOp(t *testing.T) {
	r := newRig(t)
	before := r.records()
	status, out := r.post(r.decision("alice", true))
	if status != http.StatusOK || out["recorded"] != true {
		t.Fatalf("status %d: %v", status, out)
	}
	if r.records() != before+1 {
		t.Fatalf("records %d, want %d", r.records(), before+1)
	}
	index := int(out["index"].(float64))
	largest := 0
	for _, e := range r.log.Entries() {
		largest = max(largest, e.STH.Size)
	}
	if largest <= index {
		t.Fatalf("decision at %d is under no signed head (largest %d)", index, largest)
	}
	gate := out["gate"].(map[string]any)
	if gate["approved"] != float64(1) || gate["passed"] != false {
		t.Fatalf("gate %v", gate)
	}
	// The response carries the decision's receipt, which verifies under the approver's key.
	raw, _ := json.Marshal(out["receipt"])
	var rc audit.DecisionReceipt
	if err := audit.UnmarshalStrict(raw, &rc); err != nil {
		t.Fatal(err)
	}
	if err := rc.Verify(audit.Ed25519Verifier{Pub: r.enrolled["alice"]}); err != nil || rc.Index != index || rc.Approver != "alice" {
		t.Fatalf("receipt: %v (%+v)", err, rc)
	}

	status, out = r.post(r.decision("alice", true))
	if status != http.StatusOK || out["recorded"] != false || int(out["index"].(float64)) != index {
		t.Fatalf("replay: status %d: %v", status, out)
	}
	if r.records() != before+1 {
		t.Fatal("a replay wrote a record")
	}
}

// Nothing a caller sends records unless it verifies against the call in the journal, from an
// approver the gate names.
func TestDecision_RefusalsRecordNothing(t *testing.T) {
	r := newRig(t)
	before := r.records()
	good := r.decision("alice", true)

	badSig := good
	badSig.Signature = r.decision("bob", true).Signature
	otherCall := r.decisionOn(agent.ApprovalSubject{RunID: runID, ToolUseID: "c1", ToolName: "charge", Args: []byte(`{"amount":1}`)}, "alice", true)
	flipped := good
	flipped.Approved = false
	notOnGate := r.decision("dave", true) // enrolled, but not an approver of this gate
	noKey := good
	noKey.Approver = "mallory"
	badAlg := good
	badAlg.Alg = "ml-dsa-65"
	badTime := good
	badTime.DecidedAt = "2026-10-13 12:00:01"
	noRun := good
	noRun.Run = "nope"
	noCall := good
	noCall.Call = "c9"

	for _, tc := range []struct {
		name   string
		b      body
		status int
		code   string
	}{
		{"signature of another approver", badSig, 422, "invalid_approval"},
		{"signed over other arguments", otherCall, 422, "invalid_approval"},
		{"verdict flipped after signing", flipped, 422, "invalid_approval"},
		{"approver not on the gate", notOnGate, 403, "not_an_approver"},
		{"approver with no key", noKey, 403, "not_an_approver"},
		{"wrong scheme", badAlg, 422, "invalid_approval"},
		{"decided_at not canonical", badTime, 400, "bad_request"},
		{"no such run", noRun, 404, "not_found"},
		{"no such call", noCall, 404, "not_found"},
	} {
		status, out := r.post(tc.b)
		if status != tc.status || code(out) != tc.code {
			t.Errorf("%s: status %d code %q, want %d %q (%v)", tc.name, status, code(out), tc.status, tc.code, out)
		}
	}
	if r.records() != before {
		t.Fatalf("a refused decision wrote %d records", r.records()-before)
	}
}

// Requests are read strictly and bounded, and a malformed one records nothing.
func TestDecision_StrictAndBounded(t *testing.T) {
	r := newRig(t, approvals.WithMaxBodyBytes(2048), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	before := r.records()
	good, _ := json.Marshal(r.decision("alice", true))
	withExtra := strings.Replace(string(good), "{", `{"admin":true,`, 1)
	dup := strings.Replace(string(good), "{", `{"approver":"bob",`, 1)
	for _, tc := range []struct {
		name, ctype, body string
		status            int
	}{
		{"unknown field", "application/json", withExtra, 400},
		{"duplicate key", "application/json", dup, 400},
		{"trailing data", "application/json", string(good) + `{}`, 400},
		{"not an object", "application/json", `[1]`, 400},
		{"empty", "application/json", ``, 400},
		{"wrong content type", "text/plain", string(good), 415},
		{"no content type", "", string(good), 415},
		{"too large", "application/json", `{"run":"` + strings.Repeat("a", 4096) + `"}`, 413},
		{"signature not base64", "application/json", strings.Replace(string(good), `"signature":"`, `"signature":"!!`, 1), 400},
		{"json with parameters", "application/json; charset=utf-8", string(good), 200},
	} {
		status, out := r.postRaw([]byte(tc.body), tc.ctype)
		if status != tc.status {
			t.Errorf("%s: status %d (%v), want %d", tc.name, status, out, tc.status)
		}
	}
	if r.records() != before+1 {
		t.Fatalf("records grew by %d, want 1 (only the well-formed request)", r.records()-before)
	}
	if status, _ := r.get("/decisions"); status != http.StatusMethodNotAllowed {
		t.Errorf("GET /decisions: %d", status)
	}
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/pending", strings.NewReader("{}"))
	if status, _ := r.do(req); status != http.StatusMethodNotAllowed {
		t.Errorf("POST /pending: %d", status)
	}
}

// A gate that already settled, and a run that ended, take no more decisions.
func TestDecision_ClosedGate(t *testing.T) {
	r := newRig(t)
	for _, id := range []string{"alice", "bob"} {
		if status, out := r.post(r.decision(id, true)); status != 200 {
			t.Fatalf("%s: %d %v", id, status, out)
		}
	}
	if err := r.run(); err != nil || r.charged.Load() != 1 {
		t.Fatalf("run at quorum: %v (charged %d)", err, r.charged.Load())
	}
	before := r.records()
	status, out := r.post(r.decision("carol", true))
	if status != http.StatusConflict || code(out) != "not_waiting" {
		t.Fatalf("status %d %v", status, out)
	}
	if r.records() != before {
		t.Fatal("recorded on a closed gate")
	}
	if status, out := r.get("/pending?run=" + runID + "&call=c1"); status != 200 || out["state"] == "waiting" {
		t.Fatalf("pending after the run: %d %v", status, out)
	}
}

// A second decision by the same approver, on a different time, does not count again: refused.
func TestDecision_AlreadyDecided(t *testing.T) {
	r := newRig(t)
	if status, _ := r.post(r.decision("alice", true)); status != 200 {
		t.Fatal("first")
	}
	s := r.subject()
	at := time.Date(2026, 10, 13, 12, 5, 0, 0, time.UTC)
	b := r.decisionOn(s, "alice", false)
	b.DecidedAt = agent.FormatTimestamp(at)
	b.Signature = sign(r, "alice", s, false, at)
	status, out := r.post(b)
	if status != http.StatusConflict || code(out) != "already_decided" {
		t.Fatalf("status %d %v", status, out)
	}
}

func sign(r *rig, id string, s agent.ApprovalSubject, approved bool, at time.Time) string {
	return b64(ed25519Sign(r, id, agent.ApprovalDecisionBytes(s, id, approved, at)))
}

// The hook that re-runs the run is called once for a recorded decision, with the root run, and
// not for a replay or a refusal.
func TestDecision_ResumeHook(t *testing.T) {
	var mu sync.Mutex
	var got []approvals.Recorded
	called := make(chan struct{}, 8)
	r := newRig(t, approvals.OnRecorded(func(_ context.Context, rec approvals.Recorded) {
		mu.Lock()
		got = append(got, rec)
		mu.Unlock()
		called <- struct{}{}
	}))
	r.post(r.decision("alice", true))
	<-called
	r.post(r.decision("alice", true)) // replay
	bad := r.decision("bob", true)
	bad.Signature = r.decision("alice", true).Signature
	r.post(bad)
	r.post(r.decision("bob", true))
	<-called
	r.h.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("hook called %d times, want 2: %+v", len(got), got)
	}
	if got[0].RunID != runID || got[0].RootRunID != runID || got[0].Approver != "alice" || got[0].Decided || !got[1].Decided {
		t.Fatalf("recorded: %+v", got)
	}
}

// End to end: two approvers decide over HTTP and the hook re-runs the run, which charges once.
func TestDecision_QuorumThenRerun(t *testing.T) {
	done := make(chan error, 4)
	var r *rig
	r = newRig(t, approvals.OnRecorded(func(ctx context.Context, rec approvals.Recorded) {
		if rec.Decided {
			done <- r.run()
		}
	}))
	r.post(r.decision("alice", true))
	r.post(r.decision("bob", true))
	select {
	case err := <-done:
		if err != nil || r.charged.Load() != 1 {
			t.Fatalf("rerun: %v (charged %d)", err, r.charged.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run was not re-run")
	}
	r.h.Wait()
}

// Concurrent identical submissions leave one record, and every caller is told it is recorded.
func TestDecision_ConcurrentIdentical(t *testing.T) {
	r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	before := r.records()
	b := r.decision("alice", true)
	var wg sync.WaitGroup
	recorded := make(chan bool, 16)
	for range 16 {
		wg.Go(func() {
			status, out := r.post(b)
			if status != 200 {
				t.Errorf("status %d %v", status, out)
			}
			recorded <- out["recorded"] == true
		})
	}
	wg.Wait()
	close(recorded)
	n := 0
	for rec := range recorded {
		if rec {
			n++
		}
	}
	if n < 1 {
		t.Fatal("no request reported the decision recorded")
	}
	if r.records() != before+1 {
		t.Fatalf("records grew by %d", r.records()-before)
	}
}

// The authorization hook sees every request before the journal does, and what it refuses reads
// and records nothing.
func TestAuth_HookDecides(t *testing.T) {
	var seen []approvals.Access
	var mu sync.Mutex
	r := newRig(t, approvals.WithAuth(func(req *http.Request, a approvals.Access) error {
		mu.Lock()
		seen = append(seen, a)
		mu.Unlock()
		switch req.Header.Get("X-Client") {
		case "alice":
			if a.Approver != "" && a.Approver != "alice" {
				return approvals.ErrForbidden
			}
			return nil
		case "":
			return approvals.ErrUnauthenticated
		}
		return approvals.ErrForbidden
	}))
	before := r.records()
	b, _ := json.Marshal(r.decision("alice", true))
	if status, out := r.postRaw(b, "application/json"); status != 401 || code(out) != "unauthenticated" {
		t.Fatalf("no credentials: %d %v", status, out)
	}
	if status, out := r.postRaw(b, "application/json", "X-Client", "mallory"); status != 403 || code(out) != "forbidden" {
		t.Fatalf("wrong client: %d %v", status, out)
	}
	bobBody, _ := json.Marshal(r.decision("bob", true))
	if status, _ := r.postRaw(bobBody, "application/json", "X-Client", "alice"); status != 403 {
		t.Fatalf("alice posting as bob: %d", status)
	}
	if r.records() != before {
		t.Fatal("a refused request recorded")
	}
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/pending?run="+runID+"&call=c1", nil)
	if status, _ := r.do(req); status != 401 {
		t.Fatalf("GET without credentials: %d", status)
	}
	req.Header.Set("X-Client", "alice")
	if status, _ := r.do(req); status != 200 {
		t.Fatalf("GET with credentials: %d", status)
	}
	if status, _ := r.postRaw(b, "application/json", "X-Client", "alice"); status != 200 {
		t.Fatalf("authorized decision: %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	last := seen[len(seen)-1]
	if last.Method != http.MethodPost || last.RunID != runID || last.ToolUseID != "c1" || last.Approver != "alice" {
		t.Fatalf("access: %+v", last)
	}
}

// Requests past the limit are refused with 429 and a Retry-After, per client and overall, before
// anything is read; a client's tokens come back with time.
func TestRateLimit(t *testing.T) {
	r := newRig(t, approvals.WithRateLimit(1, 3), approvals.WithGlobalRateLimit(100, 100),
		approvals.WithClientKey(func(req *http.Request) string { return req.Header.Get("X-Client") }))
	getAs := func(client string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/pending?run="+runID+"&call=c1", nil)
		req.Header.Set("X-Client", client)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Retry-After")
	}
	for i := range 3 {
		if status, _ := getAs("a"); status != 200 {
			t.Fatalf("request %d: %d", i, status)
		}
	}
	status, retry := getAs("a")
	if status != http.StatusTooManyRequests || retry == "" {
		t.Fatalf("over the limit: %d, Retry-After %q", status, retry)
	}
	if status, _ := getAs("b"); status != 200 {
		t.Fatalf("another client: %d", status)
	}
	r.advance(2 * time.Second)
	if status, _ := getAs("a"); status != 200 {
		t.Fatalf("after refill: %d", status)
	}
}

func TestRateLimit_Global(t *testing.T) {
	r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(1, 2),
		approvals.WithClientKey(func(req *http.Request) string { return req.Header.Get("X-Client") }))
	n429 := 0
	for _, c := range []string{"a", "b", "c", "d"} {
		req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/pending?run="+runID+"&call=c1", nil)
		req.Header.Set("X-Client", c)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode == 429 {
			n429++
		}
	}
	if n429 != 2 {
		t.Fatalf("%d refused, want 2 of 4 after a burst of 2", n429)
	}
}

// A limit that sees a flood of distinct clients keeps its memory bounded and still limits them.
func TestRateLimit_ManyClientsShareAnOverflowBucket(t *testing.T) {
	r := newRig(t, approvals.WithRateLimit(1, 1), approvals.WithGlobalRateLimit(0, 0), approvals.WithMaxClients(4),
		approvals.WithClientKey(func(req *http.Request) string { return req.Header.Get("X-Client") }))
	ok, refused := 0, 0
	for i := range 40 {
		req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/pending?run="+runID+"&call=c1", nil)
		req.Header.Set("X-Client", "client-"+string(rune('A'+i)))
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			ok++
		} else if resp.StatusCode == 429 {
			refused++
		}
	}
	if ok > 6 || refused < 30 {
		t.Fatalf("%d served, %d refused: a flood of new clients was not limited", ok, refused)
	}
}

func TestNew_Config(t *testing.T) {
	r := newRig(t)
	if _, err := approvals.New(nil, r.verifiers); err == nil {
		t.Error("nil journal accepted")
	}
	if _, err := approvals.New(r.app, nil); err == nil {
		t.Error("nil verifiers accepted")
	}
	if _, err := approvals.New(r.app, r.verifiers, approvals.WithMaxBodyBytes(0)); err == nil {
		t.Error("zero body limit accepted")
	}
	_ = audit.AlgEd25519
}

// A single-approver gate takes no signed decision, and a plan flow node's gate is not served: each
// is refused with its own code and records nothing.
func TestDecision_UnsupportedGates(t *testing.T) {
	single := newRigWith(t, "c1", agent.SingleApproval())
	before := single.records()
	b := single.decisionOn(single.subject(), "alice", true)
	if status, out := single.post(b); status != 422 || code(out) != "single_gate" {
		t.Errorf("single gate: %d %v", status, out)
	}
	if single.records() != before {
		t.Error("a decision on a single-approver gate was recorded")
	}

	node := newRigWith(t, "node:review", nil)
	before = node.records()
	b = node.decisionOn(node.subject(), "alice", true)
	if status, out := node.post(b); status != 422 || code(out) != "unsupported" {
		t.Errorf("node gate: %d %v", status, out)
	}
	if node.records() != before {
		t.Error("a decision on a node-key gate was recorded")
	}
}

// Work on the journal is capped: past the cap a request is told to retry, and the one in progress
// still completes.
func TestBusy(t *testing.T) {
	r := newRig(t, approvals.WithMaxInFlight(1), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	release := r.stall.arm()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // a failure must not leave the request holding the server open
	first := make(chan int, 1)
	go func() {
		status, _ := r.get("/pending?run=" + runID + "&call=c1")
		first <- status
	}()
	<-r.stall.entered
	resp, err := http.Get(r.srv.URL + "/pending?run=" + runID + "&call=c1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second request: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	unblock()
	if status := <-first; status != 200 {
		t.Fatalf("the request in progress: %d", status)
	}
}

// A client that sends headers and stalls holds a slot of the small read pool for at most the read
// timeout, never a journal-work slot: with the cap on journal work at one, a legitimate request is
// still served while stalled bodies are pending, and the stalled ones are cut off.
func TestDecision_StalledBodyHoldsNoJournalSlot(t *testing.T) {
	r := newRig(t, approvals.WithReadTimeout(300*time.Millisecond), approvals.WithMaxInFlight(1),
		approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	for range 4 {
		c, err := net.Dial("tcp", r.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("POST /decisions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 4000\r\n\r\n{"))
	}
	time.Sleep(100 * time.Millisecond)
	if status, out := r.get("/pending?run=" + runID + "&call=c1"); status != 200 {
		t.Fatalf("GET /pending while 4 bodies stall: %d %v", status, out)
	}
	time.Sleep(600 * time.Millisecond) // past the read timeout
	if status, out := r.post(r.decision("alice", true)); status != 200 {
		t.Fatalf("a decision after the stalled bodies timed out: %d %v", status, out)
	}
}

// With an authorization hook, a request that fails it is refused from the headers alone, before a
// body is read: stalled unauthenticated POSTs are answered 401 at once and hold nothing.
func TestDecision_AuthRunsBeforeTheBodyIsRead(t *testing.T) {
	r := newRig(t, approvals.WithMaxReaders(1, 1), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0),
		approvals.WithAuth(func(req *http.Request, a approvals.Access) error {
			if req.Header.Get("X-Client") == "" {
				return approvals.ErrUnauthenticated
			}
			return nil
		}))
	for i := range 3 { // more stalled requests than the one read slot
		c, err := net.Dial("tcp", r.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("POST /decisions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 4000\r\n\r\n{"))
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || !strings.Contains(line, "401") {
			t.Fatalf("stalled unauthenticated POST %d: %q %v, want an immediate 401", i, line, err)
		}
	}
	// An authenticated client still gets the one read slot.
	b, _ := json.Marshal(r.decision("alice", true))
	if status, out := r.postRaw(b, "application/json", "X-Client", "a"); status != 200 {
		t.Fatalf("authenticated: %d %v", status, out)
	}
}

// The read pool is bounded overall and per client: past either, a request is refused, and the slot is
// returned when its body arrives or times out.
func TestDecision_ReadPoolBounds(t *testing.T) {
	key := approvals.WithClientKey(func(req *http.Request) string { return req.Header.Get("X-Client") })
	r := newRig(t, approvals.WithMaxReaders(2, 1), approvals.WithReadTimeout(500*time.Millisecond), key,
		approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	stall := func(client string) net.Conn {
		c, err := net.Dial("tcp", r.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("POST /decisions HTTP/1.1\r\nHost: x\r\nX-Client: " + client + "\r\nContent-Type: application/json\r\nContent-Length: 4000\r\n\r\n{"))
		return c
	}
	status := func(client string) int {
		b, _ := json.Marshal(r.decision("alice", true))
		s, _ := r.postRaw(b, "application/json", "X-Client", client)
		return s
	}
	a := stall("a")
	defer a.Close()
	time.Sleep(100 * time.Millisecond)
	if s := status("a"); s != http.StatusTooManyRequests {
		t.Fatalf("a second concurrent read by client a: %d, want 429", s)
	}
	b := stall("b")
	defer b.Close()
	time.Sleep(100 * time.Millisecond)
	if s := status("c"); s != http.StatusServiceUnavailable {
		t.Fatalf("a third read with the pool of 2 full: %d, want 503", s)
	}
	time.Sleep(700 * time.Millisecond) // both stalled reads time out and return their slots
	if s := status("a"); s != 200 {
		t.Fatalf("after the stalled reads timed out: %d", s)
	}
}

// A replay of a decision after the gate settled answers from the gate's recorded tally, not a fresh
// recount: the numbers the gate enforced are shown even if the keys the handler resolves now are
// not the keys that counted then.
func TestDecision_ReplayAnswersFromTheRecordedTally(t *testing.T) {
	r := newRig(t)
	for _, id := range []string{"alice", "bob"} {
		if status, out := r.post(r.decision(id, true)); status != 200 {
			t.Fatalf("%s: %d %v", id, status, out)
		}
	}
	if err := r.run(); err != nil || r.charged.Load() != 1 {
		t.Fatalf("run at quorum: %v (charged %d)", err, r.charged.Load())
	}
	b := r.decision("alice", true)
	delete(r.enrolled, "alice") // a rotation: the key that counted is no longer the one resolved
	status, out := r.post(b)
	if status != 200 || out["recorded"] != false {
		t.Fatalf("replay: %d %v", status, out)
	}
	gate := out["gate"].(map[string]any)
	if gate["approved"] != float64(2) || gate["passed"] != true || out["counted"] != true {
		t.Fatalf("the replay did not answer from the recorded tally: gate %v counted %v", gate, out["counted"])
	}
	_, pend := r.get("/pending?run=" + runID + "&call=c1")
	if pend["state"] != "decided" || pend["gate"].(map[string]any)["approved"] != float64(2) {
		t.Fatalf("pending after the tally: %v", pend)
	}
}

// A decision recorded after the gate settled (by another writer: the endpoint itself refuses one) is
// not one the gate counted: a recount would count it, and the recorded tally says it was not.
func TestDecision_LateDecisionIsNotCountedAfterTheTally(t *testing.T) {
	r := newRig(t)
	for _, id := range []string{"alice", "bob"} {
		r.post(r.decision(id, true))
	}
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	b := r.decision("carol", true)
	at, _ := agent.ParseTimestamp(b.DecidedAt)
	sig, _ := base64.StdEncoding.DecodeString(b.Signature)
	d := agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: "carol", Approved: true, DecidedAt: at, Alg: agent.Alg(b.Alg), Signature: sig}
	if err := agent.SubmitDecision(context.Background(), r.app, d); err != nil {
		t.Fatal(err)
	}
	status, out := r.post(b)
	if status != 200 || out["recorded"] != false || out["counted"] != false {
		t.Fatalf("%d %v", status, out)
	}
	if gate := out["gate"].(map[string]any); gate["approved"] != float64(2) {
		t.Fatalf("the late decision changed the standing: %v", gate)
	}
}

// With separation of duties the standing excludes the requester, as the gate does: the requester's
// decision is recorded and does not count, and the response says so.
func TestDecision_ExcludePrincipalIsHonored(t *testing.T) {
	pol := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob", "carol"}, ExcludePrincipal: true}
	r := newRigAs(t, "c1", pol, agent.Identity{Actor: "agent@1", OnBehalfOf: "alice"})
	status, out := r.post(r.decision("alice", true))
	if status != 200 || out["counted"] != false || out["reason"] == nil {
		t.Fatalf("the requester's decision: %d %v", status, out)
	}
	if gate := out["gate"].(map[string]any); gate["approved"] != float64(0) {
		t.Fatalf("the requester's approval counted: %v", gate)
	}
	_, out = r.post(r.decision("bob", true))
	if out["counted"] != true || out["gate"].(map[string]any)["approved"] != float64(1) {
		t.Fatalf("bob: %v", out)
	}
}

// One approver's approve and reject sent at once: either the second is refused as already decided
// (it saw the first), or both are recorded and the first in journal order counts. In every case
// exactly one decision counts, no response claims counted for the other, and OnRecorded runs once
// for each decision that was written.
func TestDecision_ConcurrentApproveAndReject(t *testing.T) {
	for i := range 20 {
		var mu sync.Mutex
		hooks := 0
		r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0),
			approvals.OnRecorded(func(context.Context, approvals.Recorded) { mu.Lock(); hooks++; mu.Unlock() }))
		bodies := []body{r.decision("alice", true), r.decision("alice", false)}
		for j := range bodies { // a distinct decision time keeps the two names apart
			s := r.subject()
			at := time.Date(2026, 10, 13, 12, 0, 1+j, 0, time.UTC)
			bodies[j].DecidedAt = agent.FormatTimestamp(at)
			bodies[j].Signature = sign(r, "alice", s, bodies[j].Approved, at)
		}
		var wg sync.WaitGroup
		type answer struct{ counted, recorded bool }
		answers := make(chan answer, 2)
		start := make(chan struct{})
		for _, b := range bodies {
			wg.Go(func() {
				<-start
				status, out := r.post(b)
				if status != 200 && status != 409 {
					t.Errorf("status %d %v", status, out)
				}
				c, _ := out["counted"].(bool)
				rec, _ := out["recorded"].(bool)
				answers <- answer{c, rec}
			})
		}
		close(start)
		wg.Wait()
		close(answers)
		counted, recorded := 0, 0
		for a := range answers {
			if a.counted {
				counted++
			}
			if a.recorded {
				recorded++
			}
		}
		r.h.Wait()
		mu.Lock()
		if counted > 1 || recorded < 1 || hooks != recorded {
			t.Fatalf("iteration %d: %d say counted (want at most 1), %d recorded, %d hooks", i, counted, recorded, hooks)
		}
		mu.Unlock()
		// The journal holds the decisions the responses say were written, and the gate counts one.
		if _, out := r.get("/pending?run=" + runID + "&call=c1"); int(out["gate"].(map[string]any)["approved"].(float64))+int(out["gate"].(map[string]any)["denied"].(float64)) != 1 {
			t.Fatalf("iteration %d: the gate counts %v", i, out["gate"])
		}
	}
}

// Two identical requests at once: one writes (recorded true), and OnRecorded runs once.
func TestDecision_ConcurrentIdenticalFiresOnce(t *testing.T) {
	for i := range 20 {
		var mu sync.Mutex
		hooks := 0
		r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0),
			approvals.OnRecorded(func(context.Context, approvals.Recorded) { mu.Lock(); hooks++; mu.Unlock() }))
		b := r.decision("alice", true)
		var wg sync.WaitGroup
		recorded := make(chan bool, 8)
		start := make(chan struct{})
		for range 8 {
			wg.Go(func() {
				<-start
				_, out := r.post(b)
				rec, _ := out["recorded"].(bool)
				recorded <- rec
			})
		}
		close(start)
		wg.Wait()
		close(recorded)
		n := 0
		for rec := range recorded {
			if rec {
				n++
			}
		}
		r.h.Wait()
		mu.Lock()
		if n != 1 || hooks != 1 {
			t.Fatalf("iteration %d: %d requests say recorded (want 1), %d hooks (want 1)", i, n, hooks)
		}
		mu.Unlock()
	}
}

// A record another process wrote first, under the same name, is the one the journal holds: this
// request did not write it, so it says recorded false and OnRecorded does not run for it.
func TestDecision_RecordWrittenFirstByAnotherProcess(t *testing.T) {
	var mu sync.Mutex
	hooks := 0
	r := newRig(t, approvals.OnRecorded(func(context.Context, approvals.Recorded) { mu.Lock(); hooks++; mu.Unlock() }),
		approvals.WithOnError(func(err error) { t.Log(err) }))
	b := r.decision("alice", true)
	at, _ := agent.ParseTimestamp(b.DecidedAt)
	d := agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: "alice", Approved: true, DecidedAt: at, Alg: agent.Alg(b.Alg)}
	sig, _ := base64.StdEncoding.DecodeString(b.Signature)
	d.Signature = sig
	rec := agent.Record{Name: agent.DecisionStep(d), Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{
		Approver: "alice", ApproverAlg: d.Alg, Signature: sig, DecidedAt: b.DecidedAt, RecordedAt: "2026-10-13T11:00:00.000Z"}}
	data, err := agent.JournalEntry(agent.DecisionStep(d), rec)
	if err != nil {
		t.Fatal(err)
	}
	r.stall.mu.Lock()
	r.stall.preempt = &preemption{name: agent.DecisionStep(d), data: data}
	r.stall.mu.Unlock()
	status, out := r.post(b)
	if status != 200 || out["recorded"] != false || out["counted"] != true {
		t.Fatalf("status %d: %v", status, out)
	}
	r.h.Wait()
	mu.Lock()
	defer mu.Unlock()
	if hooks != 0 {
		t.Fatalf("OnRecorded ran %d times for a record this request did not write", hooks)
	}
}

// Two handlers over one store (two replicas of an application): identical decisions sent to both at
// once leave one record, exactly one response says recorded, and OnRecorded runs once in all.
func TestDecision_TwoHandlersOneStore(t *testing.T) {
	for i := range 20 {
		var mu sync.Mutex
		hooks := 0
		hook := approvals.OnRecorded(func(context.Context, approvals.Recorded) { mu.Lock(); hooks++; mu.Unlock() })
		off := []approvals.Option{approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0), hook}
		r := newRig(t, off...)
		j2, err := agent.NewJournal(r.app.Store())
		if err != nil {
			t.Fatal(err)
		}
		h2, err := approvals.New(j2, r.verifiers, append([]approvals.Option{approvals.WithClock(r.clock)}, off...)...)
		if err != nil {
			t.Fatal(err)
		}
		srv2 := httptest.NewServer(h2)
		b := r.decision("alice", true)
		raw, _ := json.Marshal(b)
		var wg sync.WaitGroup
		recorded := make(chan bool, 8)
		start := make(chan struct{})
		for k := range 8 {
			url := r.srv.URL
			if k%2 == 1 {
				url = srv2.URL
			}
			wg.Go(func() {
				<-start
				resp, err := http.Post(url+"/decisions", "application/json", bytes.NewReader(raw))
				if err != nil {
					t.Error(err)
					return
				}
				defer resp.Body.Close()
				var out map[string]any
				_ = json.NewDecoder(resp.Body).Decode(&out)
				rec, _ := out["recorded"].(bool)
				recorded <- rec
			})
		}
		close(start)
		wg.Wait()
		close(recorded)
		n := 0
		for rec := range recorded {
			if rec {
				n++
			}
		}
		r.h.Wait()
		h2.Wait()
		srv2.Close()
		mu.Lock()
		if n != 1 || hooks != 1 {
			t.Fatalf("iteration %d: %d responses say recorded (want 1), %d hooks (want 1)", i, n, hooks)
		}
		mu.Unlock()
	}
}

// Every refusal sent before the body is read ends the unread body's read at once, so a stalled client
// holds nothing: a rate-limited POST, a POST of the wrong content type, and one the hook refuses are
// each answered immediately.
func TestDecision_EarlyRefusalsDoNotWaitForTheBody(t *testing.T) {
	stalled := func(t *testing.T, r *rig, ctype string) string {
		t.Helper()
		c, err := net.Dial("tcp", r.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("POST /decisions HTTP/1.1\r\nHost: x\r\nContent-Type: " + ctype + "\r\nContent-Length: 4000\r\n\r\n{"))
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			t.Fatalf("no answer within 2s while the body stalls: %v", err)
		}
		return line
	}
	t.Run("rate limited", func(t *testing.T) {
		r := newRig(t, approvals.WithRateLimit(0.001, 1), approvals.WithGlobalRateLimit(0, 0))
		if status, _ := r.get("/pending?run=" + runID + "&call=c1"); status != 200 {
			t.Fatalf("setup: %d", status)
		}
		if line := stalled(t, r, "application/json"); !strings.Contains(line, "429") {
			t.Fatalf("%q", line)
		}
	})
	t.Run("globally rate limited", func(t *testing.T) {
		r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0.001, 1))
		if status, _ := r.get("/pending?run=" + runID + "&call=c1"); status != 200 {
			t.Fatalf("setup: %d", status)
		}
		if line := stalled(t, r, "application/json"); !strings.Contains(line, "429") {
			t.Fatalf("%q", line)
		}
	})
	t.Run("wrong content type", func(t *testing.T) {
		r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
		if line := stalled(t, r, "text/plain"); !strings.Contains(line, "415") {
			t.Fatalf("%q", line)
		}
	})
	t.Run("refused by the hook", func(t *testing.T) {
		r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0),
			approvals.WithAuth(func(*http.Request, approvals.Access) error { return approvals.ErrForbidden }))
		if line := stalled(t, r, "application/json"); !strings.Contains(line, "403") {
			t.Fatalf("%q", line)
		}
	})
	t.Run("read pool full", func(t *testing.T) {
		r := newRig(t, approvals.WithMaxReaders(1, 1), approvals.WithReadTimeout(3*time.Second), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
		hold, err := net.Dial("tcp", r.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Close()
		hold.Write([]byte("POST /decisions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 4000\r\n\r\n{"))
		time.Sleep(150 * time.Millisecond)
		if line := stalled(t, r, "application/json"); !strings.Contains(line, "429") && !strings.Contains(line, "503") {
			t.Fatalf("%q", line)
		}
	})
}

// The hook is told which of its two calls this is: the first, before the body, has no ids and only
// authenticates; the second carries the decision's. A hook that binds the approver to the
// credential (a client certificate, here a header) enforces the binding in the second only, and a
// hook with state sees each phase once per request.
func TestAuth_PhasesAreExplicit(t *testing.T) {
	var mu sync.Mutex
	var seen []approvals.Access
	r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0), approvals.WithOnError(func(err error) { t.Log(err) }),
		approvals.WithAuth(func(req *http.Request, a approvals.Access) error {
			mu.Lock()
			seen = append(seen, a)
			mu.Unlock()
			who := req.Header.Get("X-Cert-Subject") // stands for a verified client certificate
			if who == "" {
				return approvals.ErrUnauthenticated
			}
			if a.Phase == approvals.PhaseIdentity {
				if a.RunID != "" || a.ToolUseID != "" || a.Approver != "" {
					t.Errorf("the identity phase carries ids: %+v", a)
				}
				return nil
			}
			if a.Method == http.MethodPost && who != a.Approver {
				return approvals.ErrForbidden // a certificate binds one approver
			}
			return nil
		}))
	b, _ := json.Marshal(r.decision("alice", true))
	if status, _ := r.postRaw(b, "application/json"); status != 401 {
		t.Fatalf("no certificate: %d", status)
	}
	if status, _ := r.postRaw(b, "application/json", "X-Cert-Subject", "bob"); status != 403 {
		t.Fatalf("bob's certificate on alice's decision: %d", status)
	}
	if status, out := r.postRaw(b, "application/json", "X-Cert-Subject", "alice"); status != 200 {
		t.Fatalf("alice's own: %d %v", status, out)
	}
	mu.Lock()
	var phases []approvals.Phase
	for _, a := range seen {
		phases = append(phases, a.Phase)
	}
	seen = nil
	mu.Unlock()
	// 401: identity only; 403: identity, request; 200: identity, request.
	want := []approvals.Phase{approvals.PhaseIdentity, approvals.PhaseIdentity, approvals.PhaseRequest, approvals.PhaseIdentity, approvals.PhaseRequest}
	if !slices.Equal(phases, want) {
		t.Fatalf("phases %v, want %v", phases, want)
	}
	// A GET is one call, in the request phase.
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/pending?run="+runID+"&call=c1", nil)
	req.Header.Set("X-Cert-Subject", "alice")
	status, _ := r.do(req)
	mu.Lock()
	defer mu.Unlock()
	if status != 200 || len(seen) != 1 || seen[0].Phase != approvals.PhaseRequest {
		t.Fatalf("GET: %d, %+v", status, seen)
	}
}

// A gate whose recorded tally passed without excluding the requester, under a policy that now
// excludes it (or on decisions that predate the signed format), asks again: it is waiting, not
// decided, so approvers can decide and the page and CLI show it as pending.
func TestDecision_ReaskedGateIsWaiting(t *testing.T) {
	pol := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob", "carol"}, ExcludePrincipal: true}
	for name, tally := range map[string]agent.ApprovalTally{
		"requester not excluded":   {Need: 2, Approvers: pol.Approvers, Approved: 2, ApprovedBy: []string{"alice", "bob"}, DecisionFormat: agent.DecisionFormat},
		"before the signed format": {Need: 2, Approvers: pol.Approvers, Approved: 2, ApprovedBy: []string{"bob", "carol"}, ExcludePrincipal: true, Requesters: []string{"alice"}, DecisionFormat: "bide.approval.v2"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRigAs(t, "c1", pol, agent.Identity{Actor: "agent@1", OnBehalfOf: "alice"}, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
			raw, _ := json.Marshal(tally)
			data, err := agent.JournalEntry(agent.ApprovalTallyStep("c1"), agent.Record{Kind: agent.StepValue, Result: raw})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.stall.Store.Insert(context.Background(), runID, agent.ApprovalTallyStep("c1"), data); err != nil {
				t.Fatal(err)
			}
			_, pend := r.get("/pending?run=" + runID + "&call=c1")
			if pend["state"] != "waiting" || pend["reason"] == nil || pend["gate"].(map[string]any)["approved"] != float64(0) {
				t.Fatalf("pending: %v", pend)
			}
			status, out := r.post(r.decision("bob", true))
			if status != 200 || out["recorded"] != true || out["counted"] != true {
				t.Fatalf("a decision on the re-asked gate: %d %v", status, out)
			}
		})
	}
}

// The read deadline set for the body does not outlive it: a decision whose handling takes longer than
// the read timeout still completes, and a refusal does not poison the connection it was sent on (a
// deadline left in the past cancels the context of every later request on a kept-alive connection).
func TestDecision_ReadDeadlineEndsWithTheBody(t *testing.T) {
	r := newRig(t, approvals.WithReadTimeout(300*time.Millisecond), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	release := r.stall.arm()
	go func() {
		time.Sleep(800 * time.Millisecond)
		close(release)
	}()
	status, out := r.post(r.decision("alice", true))
	if status != 200 {
		t.Fatalf("a decision handled for longer than the read timeout: %d %v", status, out)
	}
}

// A refused request on a kept-alive connection leaves the next request served: the refusal must not
// cancel the connection's context (it does when the read deadline is set to the past on a connection
// whose background read is pending), which made the next request on it fail.
func TestRefusal_DoesNotPoisonTheConnection(t *testing.T) {
	r := newRig(t, approvals.WithRateLimit(0.001, 2), approvals.WithGlobalRateLimit(0, 0),
		approvals.WithClientKey(func(req *http.Request) string { return req.Header.Get("X-Client") }))
	tr := &http.Transport{MaxIdleConnsPerHost: 1}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}
	get := func(client string) int {
		req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/pending?run="+runID+"&call=c1", nil)
		req.Header.Set("X-Client", client)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := range 40 {
		a := fmt.Sprint("a", i)
		if get(a) != 200 || get(a) != 200 {
			t.Fatal("setup")
		}
		if got := get(a); got != http.StatusTooManyRequests {
			t.Fatalf("a's third request: %d, want 429", got)
		}
		if got := get(fmt.Sprint("b", i)); got != 200 {
			t.Fatalf("iteration %d: the request after a refusal on the same transport: %d, want 200", i, got)
		}
	}
}

// stalledStatus sends a request line and headers that announce a 4000-byte body, sends one byte of it,
// and returns the status line the server answers within 2s ("" if it does not).
func stalledStatus(t *testing.T, r *rig, method, path, ctype string) string {
	t.Helper()
	c, err := net.Dial("tcp", r.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(method + " " + path + " HTTP/1.1\r\nHost: x\r\nContent-Type: " + ctype + "\r\nContent-Length: 4000\r\n\r\n{"))
	c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}

// Every method and path, whatever the handler answers, answers a request whose body stalls without
// waiting for the body: net/http reads up to 256 KiB of an unread body before it sends any response,
// 200 included, so a path that does not read the body must not leave the connection open for it. The
// table is every method against every path (the routes, the paths that route to nothing, a bad query,
// the root), so a route added later is covered by adding its path.
func TestEveryPathAnswersAStalledBody(t *testing.T) {
	r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	paths := []string{
		"/pending?run=" + runID + "&call=c1", // a valid call
		"/pending",                           // bad query
		"/pending?run=nope&call=c9",          // not found
		"/decisions",                         // the route that reads a body (text/plain: refused before it)
		"/nothing", "/", "/page/", "/page/page.js",
	}
	var wg sync.WaitGroup
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE"} {
		for _, path := range paths {
			wg.Go(func() {
				if got := stalledStatus(t, r, method, path, "text/plain"); got == "" {
					t.Errorf("%s %s with a stalled body: no response within 700ms", method, path)
				}
			})
		}
	}
	wg.Wait()
}

// Requests that route to nothing are rate limited like the rest: a flood of 404s and 405s from one
// client does not escape the limit.
func TestRateLimitCoversEveryPath(t *testing.T) {
	for _, path := range []string{"/nothing", "/pending/x"} { // each path routes to nothing: a 404
		r := newRig(t, approvals.WithRateLimit(0.001, 2), approvals.WithGlobalRateLimit(0, 0))
		for i := range 2 {
			if status, _ := r.get(path); status == http.StatusTooManyRequests {
				t.Fatalf("%s: limited at request %d of a burst of 2", path, i+1)
			}
		}
		if status, _ := r.get(path); status != http.StatusTooManyRequests {
			t.Fatalf("%s: %d, want 429 once the client has spent its burst on paths that route to nothing", path, status)
		}
	}
	// A path with the wrong method (a 405) is counted too.
	r := newRig(t, approvals.WithRateLimit(0.001, 2), approvals.WithGlobalRateLimit(0, 0))
	for range 2 {
		r.get("/decisions")
	}
	if status, _ := r.get("/decisions"); status != http.StatusTooManyRequests {
		t.Fatalf("a 405 path: %d, want 429", status)
	}
}
