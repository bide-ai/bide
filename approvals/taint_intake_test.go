package approvals_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/plan"
)

// The escalation gate of a tainted call is a second gate instance on the call, under the subject
// "<call>#taint". The intake decides it by the gate its pause note records (the taint policy's), never by
// the tool's own gate, which WithGatePolicy names by tool; this is the end-to-end test of that rule through
// the exported option.

// taintRig is an application whose "write" tool is gated by its own gate (or none) and whose taint
// policy's gate is 2 of {esc1, esc2}, over a signed journal, with the intake handler mounted.
type taintRig struct {
	t      *testing.T
	app    *agent.Journal
	privs  map[string]ed25519.PrivateKey
	own    *agent.ApprovalPolicy // the write tool's own gate, nil for none
	esc    *agent.ApprovalPolicy
	wrote  atomic.Int32
	h      *approvals.Handler
	srv    *httptest.Server
	live   func(string) (*agent.ApprovalPolicy, bool)
	verify agent.ApproverVerifierFor
	pubs   map[string]ed25519.PublicKey
	runID  string
}

// readThenWrite reads (an untrusted, read-only tool) and then writes.
type readThenWrite struct{}

func (readThenWrite) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	results := map[string]bool{}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if tr, ok := p.(agent.ToolResult); ok {
				results[tr.ToolUseID] = true
			}
		}
	}
	ch := make(chan agent.Emit, 2)
	switch {
	case !results["c1"]:
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "read", ArgsFragment: json.RawMessage(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	case !results["c2"]:
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c2", Name: "write", ArgsFragment: json.RawMessage(`{"amount":120,"to":"acme"}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	default:
		ch <- agent.Emit{Event: agent.TextDelta{Text: "done"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func newTaintRig(t *testing.T, own *agent.ApprovalPolicy) *taintRig {
	t.Helper()
	r := &taintRig{t: t, own: own, runID: "run-taint", privs: map[string]ed25519.PrivateKey{}, pubs: map[string]ed25519.PublicKey{},
		esc: &agent.ApprovalPolicy{Need: 2, Approvers: []string{"esc1", "esc2"}}}
	for _, id := range []string{"own1", "esc1", "esc2", "other"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		r.privs[id], r.pubs[id] = priv, pub
	}
	r.verify = func(id string) (agent.ApproverVerifier, bool) {
		pub, ok := r.pubs[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	}
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	as, err := audit.NewAuditedStore(agent.NewMemStore(), audit.Ed25519Signer{Priv: logPriv}, audit.NewMemAnchorLog())
	if err != nil {
		t.Fatal(err)
	}
	r.app = agenttest.MustJournal(as)
	r.live = func(tool string) (*agent.ApprovalPolicy, bool) { return r.own, tool == "write" }
	return r
}

func (r *taintRig) drive() error {
	r.t.Helper()
	read := agent.MustFunc("read", "read", func(context.Context, struct{}) (string, error) { return "text", nil },
		agent.WithSafety(agent.Safety{ReadOnly: true}), agent.Untrusted("crm"))
	var wopts []agent.ToolOption
	if r.own != nil {
		wopts = append(wopts, agent.WithApproval(r.own))
	}
	write := agent.MustFunc("write", "write", func(context.Context, struct {
		Amount int    `json:"amount"`
		To     string `json:"to"`
	}) (string, error) {
		r.wrote.Add(1)
		return "ok", nil
	}, wopts...)
	a, err := agent.New(readThenWrite{}, r.app, agent.WithTools(read, write), agent.WithApproverVerifiers(r.verify),
		agent.WithTaintPolicy(agent.TaintPolicy{Gate: r.esc}))
	if err != nil {
		r.t.Fatal(err)
	}
	_, err = a.Run(context.Background(), r.runID, agent.UserText("pay"))
	return err
}

func (r *taintRig) serve() {
	r.t.Helper()
	h, err := approvals.New(r.app, r.verify, approvals.WithOnError(func(err error) { r.t.Log(err) }), approvals.WithGatePolicy(r.live))
	if err != nil {
		r.t.Fatal(err)
	}
	r.h = h
	r.srv = httptest.NewServer(h)
	r.t.Cleanup(r.srv.Close)
}

func (r *taintRig) subject(call string) agent.ApprovalSubject {
	r.t.Helper()
	recs, err := r.app.History(context.Background(), r.runID)
	if err != nil {
		r.t.Fatal(err)
	}
	_, tu, ok := agent.FindToolCall(recs, call)
	if !ok {
		r.t.Fatalf("no call %s", call)
	}
	return agent.ApprovalSubject{RunID: r.runID, ToolUseID: call, ToolName: tu.Name, Args: tu.Args}
}

// decision is the body of approver id's decision on subject call, signed over signedFor (which is call
// unless the test signs for another subject).
func (r *taintRig) decision(call, signedFor, id string, approved bool) map[string]any {
	r.t.Helper()
	at := time.Date(2026, 10, 24, 12, 0, 1, 0, time.UTC)
	sig := ed25519.Sign(r.privs[id], agent.ApprovalDecisionBytes(r.subject(signedFor), id, approved, at))
	return map[string]any{"run": r.runID, "call": call, "approver": id, "approved": approved, "decided_at": agent.FormatTimestamp(at),
		"alg": string(audit.AlgEd25519), "signature": base64.StdEncoding.EncodeToString(sig)}
}

func (r *taintRig) do(method, path string, body any) (int, map[string]any) {
	r.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (r *taintRig) pending(call string) (int, map[string]any) {
	return r.do(http.MethodGet, "/pending?run="+r.runID+"&call="+call, nil)
}

func (r *taintRig) decide(call, signedFor, id string, approved bool) (int, map[string]any) {
	return r.do(http.MethodPost, "/decisions", r.decision(call, signedFor, id, approved))
}

func gateOf(t *testing.T, out map[string]any) (need int, approvers []string, passed bool) {
	t.Helper()
	g, _ := out["gate"].(map[string]any)
	need = int(g["need"].(float64))
	for _, a := range g["approvers"].([]any) {
		approvers = append(approvers, a.(string))
	}
	passed, _ = g["passed"].(bool)
	return need, approvers, passed
}

// With a tool gate of its own: the call pauses on it, then on the escalation instance; each is decided
// by its own approvers and never by the other's.
func TestTaintIntake_EachInstanceIsDecidedByItsOwnGate(t *testing.T) {
	r := newTaintRig(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	if err := r.drive(); !agent.IsPause(err) {
		t.Fatalf("first drive = %v, want a pause on the tool's own gate", err)
	}
	r.serve()

	// The tool's own gate is served as before: 1 of {own1}.
	if status, out := r.pending("c2"); status != http.StatusOK || out["tool"] != "write" {
		t.Fatalf("own gate: %d %v", status, out)
	} else if need, appr, _ := gateOf(t, out); need != 1 || len(appr) != 1 || appr[0] != "own1" {
		t.Fatalf("own gate = %d of %v", need, appr)
	}
	// An escalation approver is not an approver of the tool's own gate.
	if status, _ := r.decide("c2", "c2", "esc1", true); status == http.StatusOK {
		t.Fatalf("esc1 was accepted on the tool's own gate")
	}
	if status, out := r.decide("c2", "c2", "own1", true); status != http.StatusOK || out["counted"] != true {
		t.Fatalf("own1 on the own gate: %d %v", status, out)
	}
	if err := r.drive(); !agent.IsPause(err) {
		t.Fatalf("second drive = %v, want a pause on the escalation instance", err)
	}
	if r.wrote.Load() != 0 {
		t.Fatal("the write ran on the own gate alone")
	}

	// The escalation instance is served by the gate its pause note records: 2 of {esc1, esc2}.
	status, out := r.pending("c2%23taint")
	if status != http.StatusOK || out["tool"] != "write" || out["call"] != "c2#taint" || out["state"] != "waiting" {
		t.Fatalf("escalation instance: %d %v", status, out)
	}
	if need, appr, _ := gateOf(t, out); need != 2 || len(appr) != 2 || appr[0] != "esc1" || appr[1] != "esc2" {
		t.Fatalf("escalation gate = %d of %v, want 2 of [esc1 esc2]", need, appr)
	}
	// The tool's own approver is not an approver of the instance, and a decision signed for the call
	// itself does not count on the instance.
	if status, _ := r.decide("c2#taint", "c2#taint", "own1", true); status == http.StatusOK {
		t.Fatal("own1 was accepted on the escalation instance")
	}
	if status, _ := r.decide("c2#taint", "c2", "esc1", true); status == http.StatusOK {
		t.Fatal("a decision signed for the call was accepted on its escalation instance")
	}
	if status, _ := r.decide("c2", "c2#taint", "esc1", true); status == http.StatusOK {
		t.Fatal("a decision signed for the instance was accepted on the call itself")
	}
	// The instance's own approvers decide it.
	for _, id := range []string{"esc1", "esc2"} {
		if status, out := r.decide("c2#taint", "c2#taint", id, true); status != http.StatusOK || out["counted"] != true {
			t.Fatalf("%s on the escalation instance: %d %v", id, status, out)
		}
	}
	if err := r.drive(); err != nil {
		t.Fatalf("after both gates: %v", err)
	}
	if r.wrote.Load() != 1 {
		t.Fatalf("writes = %d, want 1", r.wrote.Load())
	}
}

// A tool with no gate of its own, whose live gate is "none" (WithGatePolicy answers nil for it): the
// escalation instance is still served and decided by the gate its note records, not by "no gate".
func TestTaintIntake_AToolWithNoGateStillHasItsEscalationInstanceServed(t *testing.T) {
	r := newTaintRig(t, nil)
	if err := r.drive(); !agent.IsPause(err) {
		t.Fatalf("first drive = %v, want a pause on the escalation instance", err)
	}
	r.serve()
	status, out := r.pending("c2%23taint")
	if status != http.StatusOK || out["state"] != "waiting" {
		t.Fatalf("escalation instance of a tool with no gate: %d %v (the tool's live gate is none; the instance must not be 'decided')", status, out)
	}
	if need, _, _ := gateOf(t, out); need != 2 {
		t.Fatalf("escalation gate need = %d, want 2", need)
	}
	// A decision for a subject the journal holds no pause note for is not served.
	if status, _ := r.pending("c9%23taint"); status != http.StatusNotFound {
		t.Fatalf("an escalation subject of an unknown call: %d, want 404", status)
	}
	for _, id := range []string{"esc1", "esc2"} {
		if status, out := r.decide("c2#taint", "c2#taint", id, true); status != http.StatusOK || out["counted"] != true {
			t.Fatalf("%s: %d %v", id, status, out)
		}
	}
	if err := r.drive(); err != nil || r.wrote.Load() != 1 {
		t.Fatalf("after the escalation approvals: %v, writes %d", err, r.wrote.Load())
	}
}

// The gate-reason record says which gate the call was held under. An escalation subject is served only
// when that record agrees with the pause note (the snapshot is the note's policy, and the digest is the
// snapshot's); a record that disagrees, or none, means the journal was not written by the engine.
func TestTaintIntake_AnEscalationSubjectIsServedOnlyWhenItsReasonRecordAgreesWithTheNote(t *testing.T) {
	ctx := context.Background()
	base := newTaintRig(t, nil)
	if err := base.drive(); !agent.IsPause(err) {
		t.Fatalf("drive = %v, want a pause on the escalation instance", err)
	}
	// edit copies the journal into a plain store, changing the gate-reason record by fn (nil drops it).
	decoyLast := false
	var decoy func(m map[string]any) // when set, a second gate-reason record, built from the engine's, written ahead of it
	edit := func(fn func(m map[string]any)) int {
		t.Helper()
		ms := agent.NewMemStore()
		for e, err := range base.app.Store().Load(ctx, base.runID, -1) {
			if err != nil {
				t.Fatal(err)
			}
			data := e.Data
			if strings.HasPrefix(e.Name, "gate-reason:") {
				if fn == nil {
					continue
				}
				var rec map[string]any
				if err := json.Unmarshal(data, &rec); err != nil {
					t.Fatal(err)
				}
				if decoy != nil && !decoyLast {
					var d map[string]any
					_ = json.Unmarshal(data, &d)
					decoy(d["result"].(map[string]any))
					d["name"] = "gate-reason:decoy"
					dd, _ := json.Marshal(d)
					if _, _, err := ms.Insert(ctx, base.runID, "gate-reason:decoy", dd); err != nil {
						t.Fatal(err)
					}
				}
				fn(rec["result"].(map[string]any))
				data, _ = json.Marshal(rec)
			}
			if _, _, err := ms.Insert(ctx, base.runID, e.Name, data); err != nil {
				t.Fatal(err)
			}
			if decoy != nil && decoyLast && strings.HasPrefix(e.Name, "gate-reason:") {
				var d map[string]any
				_ = json.Unmarshal(e.Data, &d)
				decoy(d["result"].(map[string]any))
				d["name"] = "gate-reason:decoy"
				dd, _ := json.Marshal(d)
				if _, _, err := ms.Insert(ctx, base.runID, "gate-reason:decoy", dd); err != nil {
					t.Fatal(err)
				}
			}
		}
		h, err := approvals.New(agenttest.MustJournal(ms), base.verify, approvals.WithGatePolicy(base.live), approvals.WithOnError(func(err error) { t.Log(err) }))
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pending?run="+base.runID+"&call=c2%23taint", nil))
		return rec.Code
	}
	if got := edit(func(map[string]any) {}); got != http.StatusOK {
		t.Fatalf("an unchanged record: %d, want 200 (the harness itself is wrong)", got)
	}
	cases := map[string]func(m map[string]any){
		"a snapshot with other approvers": func(m map[string]any) {
			m["gate"].(map[string]any)["approvers"] = []any{"own1", "other"}
			redigest(t, m)
		},
		"a snapshot with another need": func(m map[string]any) {
			m["gate"].(map[string]any)["need"] = float64(1)
			redigest(t, m)
		},
		"a digest that is not the snapshot's": func(m map[string]any) {
			m["policy_digest"] = strings.Repeat("0", 64)
		},
		"no snapshot": func(m map[string]any) { delete(m, "gate") },
		"no snapshot and the digest of nothing": func(m map[string]any) {
			delete(m, "gate")
			sum := sha256.Sum256([]byte("null"))
			m["policy_digest"] = hex.EncodeToString(sum[:])
		},
		"a snapshot that does not exclude the requester": func(m map[string]any) {
			m["gate"].(map[string]any)["exclude_principal"] = true
			redigest(t, m)
		},
	}
	for name, fn := range cases {
		if got := edit(fn); got != http.StatusNotFound {
			t.Errorf("%s: %d, want 404 (not served)", name, got)
		}
	}
	// A record that names another call is not this call's: it neither refuses nor serves it.
	decoy = func(m map[string]any) {
		m["tool_use_id"] = "c9"
		m["gate"].(map[string]any)["need"] = float64(1)
		redigest(t, m)
	}
	if got := edit(func(map[string]any) {}); got != http.StatusOK {
		t.Errorf("a record for another call ahead of the engine's: %d, want 200", got)
	}
	// A second record that names this call and disagrees (a decoy ahead of the engine's) means not served.
	decoy = func(m map[string]any) {
		m["gate"].(map[string]any)["need"] = float64(1)
		redigest(t, m)
	}
	if got := edit(func(map[string]any) {}); got != http.StatusNotFound {
		t.Errorf("a disagreeing record for this call ahead of the engine's: %d, want 404", got)
	}
	decoyLast = true
	if got := edit(func(map[string]any) {}); got != http.StatusNotFound {
		t.Errorf("a disagreeing record for this call behind the engine's: %d, want 404", got)
	}
	decoy, decoyLast = nil, false
	if got := edit(nil); got != http.StatusNotFound {
		t.Errorf("no reason record: %d, want 404 (not served)", got)
	}
}

// redigest makes the record's digest the digest of its (edited) snapshot, as a writer who knows the rule would.
func redigest(t *testing.T, m map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(m["gate"])
	var p agent.ApprovalPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(&p)
	sum := sha256.Sum256(b)
	m["policy_digest"] = hex.EncodeToString(sum[:])
}

// A plan node named foo#taint is a node, not the escalation instance of a node foo: the intake serves its
// gate as it serves a node named foo, and does not read it as an instance (which would resolve to a node
// that is not there).
func TestTaintIntake_APlanNodeNamedLikeAnInstanceIsANode(t *testing.T) {
	ctx := context.Background()
	servedAs := func(name string) int {
		t.Helper()
		j := agenttest.MemJournal()
		pol := agent.ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
		b := plan.New[int, string]("pay").WithApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false })
		pay := b.Step(name, func(_ context.Context, n int) (string, error) { return "ok", nil }, plan.Approval(&pol))
		_ = pay
		f, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		var pend *agent.ApprovalPending
		if _, err := f.Run(ctx, j, "f1", 1); !errors.As(err, &pend) {
			t.Fatalf("flow with node %q: Run = %v, want a pause", name, err)
		}
		h, err := approvals.New(j, func(string) (agent.ApproverVerifier, bool) { return nil, false },
			approvals.WithGatePolicy(func(string) (*agent.ApprovalPolicy, bool) { return nil, false }))
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pending?run=f1&call="+url.QueryEscape(pend.ToolUseID), nil))
		return rec.Code
	}
	plain, named := servedAs("foo"), servedAs("foo#taint")
	if named != plain {
		t.Errorf("a node named foo#taint is answered %d, a node named foo %d: the name must not change how the gate is read", named, plain)
	}
}

// The intake says why the escalation instance asks: the taint reason from the gate-reason record, on the
// instance and not on the tool's own gate; a decoy record that names the call and disagrees is shown as a
// conflict in place of the reasons.
func TestTaintIntake_SaysWhyTheInstanceAsks(t *testing.T) {
	r := newTaintRig(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	if err := r.drive(); !agent.IsPause(err) {
		t.Fatalf("first drive = %v", err)
	}
	r.serve()
	if status, out := r.pending("c2"); status != http.StatusOK || out["why"] != nil {
		t.Fatalf("the own gate: %d why=%v, want no reason", status, out["why"])
	}
	if status, _ := r.decide("c2", "c2", "own1", true); status != http.StatusOK {
		t.Fatalf("own gate decision: %d", status)
	}
	if err := r.drive(); !agent.IsPause(err) {
		t.Fatalf("second drive = %v", err)
	}
	status, out := r.pending("c2%23taint")
	why, _ := out["why"].([]any)
	if status != http.StatusOK || len(why) != 1 {
		t.Fatalf("instance: %d why=%v, want one reason", status, out["why"])
	}
	w := why[0].(map[string]any)
	if w["kind"] != "taint" || w["source"] != "crm" || !strings.HasPrefix(w["ref"].(string), "tool:read/c1") {
		t.Fatalf("reason = %v, want the taint reason from crm read by tool read, call c1", w)
	}
	if status, out := r.pending("c2"); status != http.StatusOK || out["why"] != nil {
		t.Fatalf("the own gate after the instance asks: %d why=%v, want no reason", status, out["why"])
	}
}

// The escalation instance of a plan flow node ("node:pay:taint", docs/design/flow-taint.md) is not served by
// the intake, as the node's own gate is not: a decision for it records nothing, whatever it is signed for, and
// it is not decided under the node's own gate. The engine writes no instance yet, so the instance's gate is
// driven through the flow hook.
func TestIntake_DoesNotDecideAFlowNodesInstance(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		if id != "carol" {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	}
	j := agenttest.MustJournal(agent.NewMemStore())
	own := agent.ApprovalPolicy{Need: 1, Approvers: []string{"carol"}}
	b := plan.New[int, int]("refunds").WithApproverVerifiers(verifiers)
	b.Step("pay", func(_ context.Context, n int) (int, error) { return n, nil }, plan.Approval(&own))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "f1", 120); err == nil {
		t.Fatal("the flow did not pause")
	}
	const inst = "node:pay:taint"
	esc := &agent.ApprovalPolicy{Need: 1, Approvers: []string{"carol"}}
	var pend *agent.ApprovalPending
	if _, err := journalhook.Gate(ctx, j, "f1", inst, "pay", json.RawMessage(`120`), esc, agent.ApproverVerifierFor(verifiers)); !errors.As(err, &pend) {
		t.Fatalf("the instance's first evaluation = %v, want a pause", err)
	}
	h, err := approvals.New(j, verifiers, approvals.WithGatePolicy(func(string) (*agent.ApprovalPolicy, bool) { return &own, true }))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	count := func() int {
		recs, _ := j.History(ctx, "f1")
		return len(recs)
	}
	at := time.Date(2026, 10, 13, 12, 0, 1, 0, time.UTC)
	for _, call := range []string{inst, "node:pay"} {
		subj := agent.ApprovalSubject{RunID: "f1", ToolUseID: call, ToolName: "pay", Args: json.RawMessage(`120`)}
		sig := ed25519.Sign(priv, agent.ApprovalDecisionBytes(subj, "carol", true, at))
		raw, _ := json.Marshal(map[string]any{"run": "f1", "call": call, "approver": "carol", "approved": true,
			"decided_at": agent.FormatTimestamp(at), "alg": string(audit.AlgEd25519), "signature": base64.StdEncoding.EncodeToString(sig)})
		before := count()
		resp, err := http.Post(srv.URL+"/decisions", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s: a decision on a flow gate was accepted: %d", call, resp.StatusCode)
		}
		if count() != before {
			t.Errorf("%s: a decision on a flow gate was recorded", call)
		}
	}
}

// The instance of a node with no gate of its own is found by the intake (it has a request of its own) and
// refused as a node gate is: the response is the refusal of plan flow gates, not "no such gate", and nothing is
// recorded. The engine writes the instance's gate-reason record before it asks; the test plants the same.
func TestIntake_FindsAndRefusesTheInstanceOfAnUngatedNode(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		if id != "carol" {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	}
	j := agenttest.MustJournal(agent.NewMemStore())
	const inst = "node:pay:taint"
	esc := &agent.ApprovalPolicy{Need: 1, Approvers: []string{"carol"}}
	var pend *agent.ApprovalPending
	if _, err := journalhook.Gate(ctx, j, "f1", inst, "pay", json.RawMessage(`120`), esc, agent.ApproverVerifierFor(verifiers)); !errors.As(err, &pend) {
		t.Fatalf("the instance's first evaluation = %v, want a pause", err)
	}
	gb, _ := json.Marshal(esc)
	sum := sha256.Sum256(gb)
	body, _ := json.Marshal(map[string]any{"format": "bide.gate-reason.v1", "tool_use_id": "node:pay", "policy_digest": hex.EncodeToString(sum[:]),
		"gate": esc, "reasons": []any{map[string]any{"kind": "taint", "source": "inbound-email", "ref": "node:fetch", "pos": 2}}, "recorded_at": "2026-10-27T00:00:00.000Z"})
	data, err := agent.JournalEntry("gate-reason:node%3Apay", agent.Record{Kind: agent.StepValue, Result: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Store().Insert(ctx, "f1", "gate-reason:node%3Apay", data); err != nil {
		t.Fatal(err)
	}
	h, err := approvals.New(j, verifiers, approvals.WithGatePolicy(func(string) (*agent.ApprovalPolicy, bool) { return nil, false }))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	recs, _ := j.History(ctx, "f1")
	before := len(recs)
	at := time.Date(2026, 10, 13, 12, 0, 1, 0, time.UTC)
	subj := agent.ApprovalSubject{RunID: "f1", ToolUseID: inst, ToolName: "pay", Args: json.RawMessage(`120`)}
	sig := ed25519.Sign(priv, agent.ApprovalDecisionBytes(subj, "carol", true, at))
	raw, _ := json.Marshal(map[string]any{"run": "f1", "call": inst, "approver": "carol", "approved": true,
		"decided_at": agent.FormatTimestamp(at), "alg": string(audit.AlgEd25519), "signature": base64.StdEncoding.EncodeToString(sig)})
	resp, err := http.Post(srv.URL+"/decisions", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if e, _ := out["error"].(map[string]any); resp.StatusCode != 422 || e["code"] != "unsupported" {
		t.Errorf("a decision on the instance of an ungated node: %d %v, want 422 unsupported (found, then refused as a flow gate)", resp.StatusCode, out)
	}
	recs, _ = j.History(ctx, "f1")
	if len(recs) != before {
		t.Error("a decision on a flow gate was recorded")
	}
}
