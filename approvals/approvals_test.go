package approvals_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
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
	r := newRig(t, approvals.WithMaxBodyBytes(2048))
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

// A stalled body holds one in-flight slot for at most the read timeout, and the slot is taken before
// the body is read, so stalled clients cannot pile up handlers.
func TestDecision_StalledBodyIsCutOff(t *testing.T) {
	r := newRig(t, approvals.WithReadTimeout(300*time.Millisecond), approvals.WithMaxInFlight(2),
		approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	var conns []net.Conn
	for range 2 {
		c, err := net.Dial("tcp", r.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("POST /decisions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 4000\r\n\r\n{"))
		conns = append(conns, c)
	}
	time.Sleep(100 * time.Millisecond)
	if status, _ := r.post(r.decision("alice", true)); status != http.StatusServiceUnavailable {
		t.Fatalf("with both slots held by stalled bodies: status %d, want 503", status)
	}
	time.Sleep(600 * time.Millisecond) // past the read timeout: the stalled requests are cut off
	if status, out := r.post(r.decision("alice", true)); status != 200 {
		t.Fatalf("after the stalled bodies timed out: %d %v", status, out)
	}
	_ = conns
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
