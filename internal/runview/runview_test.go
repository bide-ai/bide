package runview

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
)

// crashStore fails the insert of one record name while armed: the write a crash loses.
type crashStore struct {
	agent.Store
	lose  string
	armed bool
}

var errCrash = errors.New("simulated crash")

func (s *crashStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if s.armed && name == s.lose {
		return agent.Entry{}, false, errCrash
	}
	return s.Store.Insert(ctx, runID, name, data)
}

func (s *crashStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return s.Store.Load(ctx, runID, after)
}

func (s *crashStore) Unwrap() agent.Store { return s.Store }

type order struct {
	ID     string `json:"id"`
	Amount int    `json:"amount"`
}

// refundFlow is validate -> Switch(amount > 100: charge -> receipt, else: skip).
func refundFlow(t *testing.T, charges *int) *plan.Flow[order, string] {
	t.Helper()
	b := plan.New[order, string]("refund")
	validate := b.Step("validate", func(_ context.Context, o order) (order, error) { return o, nil }, plan.ReadOnly())
	charge := b.Step("charge", func(_ context.Context, o order) (order, error) { *charges++; return o, nil })
	receipt := b.Step("receipt", func(_ context.Context, o order) (string, error) { return "refunded " + o.ID, nil }, plan.ReadOnly())
	skip := b.Step("skip", func(_ context.Context, o order) (string, error) { return "nothing to refund", nil }, plan.ReadOnly())
	b.Switch(validate, plan.When(func(o order) bool { return o.Amount > 100 }, charge).Named("large"), plan.Else(skip))
	b.Edge(charge, receipt)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func get(t *testing.T, h http.Handler, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Host = "127.0.0.1:8484"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

func viewOf(t *testing.T, h http.Handler, runID string) View {
	t.Helper()
	code, body := get(t, h, "/run.json?id="+runID)
	if code != 200 {
		t.Fatalf("GET /run.json?id=%s = %d: %s", runID, code, body)
	}
	var v View
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func node(v View, name string) GraphNode {
	for _, n := range v.Graph.Nodes {
		if n.Name == name {
			return n
		}
	}
	return GraphNode{}
}

// TestFlowRunHaltThenResolve views a plan flow run that crashed mid-charge: the topology with the
// path taken, the halted node and its HaltRef, and conformance; then resolves the halt and views it
// complete.
func TestFlowRunHaltThenResolve(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	cs := &crashStore{Store: mem, lose: "node:charge", armed: true}
	charges := 0
	flow := refundFlow(t, &charges)
	if _, err := flow.Run(ctx, agenttest.MustJournal(cs), "r1", order{ID: "o-1", Amount: 250}); !errors.Is(err, errCrash) {
		t.Fatalf("crashing Run = %v, want the simulated crash", err)
	}
	cs.armed = false
	j := agenttest.MustJournal(cs)
	if _, err := flow.Run(ctx, j, "r1", order{ID: "o-1", Amount: 250}); !errorsAsHalt(err) {
		t.Fatalf("resumed Run = %v, want *agent.OutcomeUnknown", err)
	}

	h := Handler(Options{Source: JournalSource{J: j}, Flows: map[string]*Flow{"refund": {Topology: flow.Topology(), Digest: flow.Digest(), Conform: flow.Conform}}})
	v := viewOf(t, h, "r1")
	if v.Kind != "flow" || v.Flow != "refund" || v.State != "halted" || v.StateText != "halted: outcome unknown" {
		t.Fatalf("kind/flow/state = %q/%q/%q", v.Kind, v.Flow, v.State)
	}
	if len(v.Halts) != 1 || v.Halts[0].ID != "node:charge" || v.Halts[0].Op != "step" || !strings.Contains(v.Halts[0].Ref, `ID: "node:charge"`) {
		t.Fatalf("halts = %+v", v.Halts)
	}
	for name, want := range map[string]string{"validate": "done", "charge": "halted", "receipt": "not-reached", "skip": "not-reached"} {
		if got := node(v, name).Status; got != want {
			t.Errorf("node %s = %q, want %q", name, got, want)
		}
	}
	if !node(v, "charge").Last {
		t.Error("charge is not marked as the last node")
	}
	for _, e := range v.Graph.Edges {
		if want := e.From == "validate" && e.To == "charge"; e.Taken != want {
			t.Errorf("edge %s->%s taken = %v, want %v", e.From, e.To, e.Taken, want)
		}
	}
	if v.Conform == nil || !v.Conform.OK || v.Conform.Method != "plan.Conform" || v.Conform.Digest != "match" {
		t.Fatalf("conformance = %+v", v.Conform)
	}
	if code, body := get(t, h, "/run?id=r1"); code != 200 || !strings.Contains(body, "<svg") || !strings.Contains(body, "Outcome unknown") {
		t.Fatalf("GET /run = %d, page lacks the graph or the halt", code)
	}

	// A topology the run did not follow: the digest and the structural check both flag it.
	other := plan.New[order, string]("refund")
	other.Step("validate", func(_ context.Context, o order) (string, error) { return o.ID, nil })
	of, err := other.Build()
	if err != nil {
		t.Fatal(err)
	}
	hv := Handler(Options{Source: JournalSource{J: j}, Flows: map[string]*Flow{"refund": {Topology: of.Topology(), Digest: of.Digest()}}})
	if c := viewOf(t, hv, "r1").Conform; c == nil || c.OK || c.Digest != "mismatch" || c.Method != "structural" {
		t.Fatalf("conformance against another topology = %+v", c)
	}

	ref := agent.HaltRef{RunID: "r1", Op: agent.OpRef{Kind: agent.OpStep, ID: "node:charge"}, Cause: agent.HaltCrashed}
	if err := agent.ResolveHalt(ctx, j, ref, agent.Outcome{Result: order{ID: "o-1", Amount: 250}}); err != nil {
		t.Fatalf("ResolveHalt: %v", err)
	}
	if out, err := flow.Run(ctx, j, "r1", order{ID: "o-1", Amount: 250}); err != nil || out != "refunded o-1" {
		t.Fatalf("Run after resolve = %q, %v", out, err)
	}
	v = viewOf(t, h, "r1")
	if v.State != "completed" || len(v.Halts) != 0 || node(v, "receipt").Status != "done" || !v.Conform.OK {
		t.Fatalf("after resolve: state %q, halts %v, receipt %q, conform %+v", v.State, v.Halts, node(v, "receipt").Status, v.Conform)
	}
	if charges != 1 {
		t.Fatalf("charge ran %d times, want 1", charges)
	}
}

func errorsAsHalt(err error) bool {
	_, ok := errors.AsType[*agent.OutcomeUnknown](err)
	return ok
}

func key(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("runview test " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

// TestAgentRunTimeline views an agent run on an anchored store: a 2-of-3 approval gate, a refund
// whose result a crash lost (a halt), then the halt resolved, and the anchored head verifying.
func TestAgentRunTimeline(t *testing.T) {
	ctx := context.Background()
	anchor := audit.NewMemAnchorLog()
	logKey := audit.Ed25519Signer{Priv: key("log")}
	mem := agent.NewMemStore()
	cs := &crashStore{Store: mem, lose: "tool:c1"}
	as, err := audit.NewAuditedStore(cs, logKey, anchor)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(as)
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		return audit.Ed25519Verifier{Pub: key(id).Public().(ed25519.PublicKey)}, true
	}
	refunds := 0
	refund := agent.MustFunc("refund", "refund an order", func(context.Context, struct{ Order int }) (string, error) {
		refunds++
		return "refunded", nil
	}, agent.WithApproval(&policy))
	lookup := agent.MustFunc("lookup", "look up an order", func(context.Context, struct{ Order int }) (string, error) {
		return "order 42: $120", nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	newAgent := func() *agent.Agent {
		model := agenttest.NewScriptedModel(
			agenttest.ToolTurn("c0", "lookup", `{"Order":42}`),
			agenttest.ToolTurn("c1", "refund", `{"Order":42}`),
			agenttest.TextTurn("Refunded order 42."))
		return agenttest.MustNew(model, j, agent.WithTools(lookup, refund), agent.WithApproverVerifiers(verifiers))
	}
	if _, err := newAgent().Run(ctx, "a1", agent.UserText("Refund order 42.")); !isPending(err) {
		t.Fatalf("Run = %v, want approval pending", err)
	}
	recs, err := j.History(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	_, call, _ := agent.FindToolCall(recs, "c1")
	for _, who := range []string{"ops", "finance"} {
		subj := agent.ApprovalSubject{RunID: "a1", ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
		sig := ed25519.Sign(key(who), agent.ApprovalDecisionBytes(subj, who, true))
		if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "a1", ToolUseID: "c1", ApproverID: who, Approved: true, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	cs.armed = true
	if _, err := newAgent().Run(ctx, "a1", agent.UserText("Refund order 42.")); !errors.Is(err, errCrash) {
		t.Fatalf("crashing Run = %v, want the simulated crash", err)
	}
	cs.armed = false
	if _, err := newAgent().Run(ctx, "a1", agent.UserText("Refund order 42.")); !errorsAsHalt(err) {
		t.Fatalf("resumed Run = %v, want *agent.OutcomeUnknown", err)
	}

	latest := func() audit.SignedTreeHead {
		e := anchor.Entries()
		return e[len(e)-1].STH
	}
	logPub := audit.Ed25519Verifier{Pub: key("log").Public().(ed25519.PublicKey)}
	h := Handler(Options{Source: JournalSource{J: j}, Provers: []Prover{HeadProver(latest(), logPub, Approvals{})}})
	v := viewOf(t, h, "a1")
	if v.Kind != "agent" || v.State != "halted" || v.StateText != "halted: outcome unknown" {
		t.Fatalf("kind/state = %q/%q", v.Kind, v.State)
	}
	if len(v.Halts) != 1 || v.Halts[0].Op != "tool" || v.Halts[0].ID != "c1" || v.Halts[0].ToolName != "refund" {
		t.Fatalf("halts = %+v", v.Halts)
	}
	if len(v.Gates) != 1 || v.Gates[0].Need != 2 || strings.Join(v.Gates[0].ApprovedBy, ",") != "ops,finance" || !v.Gates[0].Tallied {
		t.Fatalf("gates = %+v", v.Gates)
	}
	if got, want := v.Gates[0].Status, "approved (2 of 3 approvers; 2 needed)"; got != want {
		t.Fatalf("gate status = %q, want %q", got, want)
	}
	var sawReadOnly, sawAttempt bool
	for _, e := range v.Events {
		sawReadOnly = sawReadOnly || e.Class == "tool" && strings.Contains(strings.Join(e.Badges, ","), "read-only")
		sawAttempt = sawAttempt || e.Class == "attempt" && strings.Contains(e.Title, "refund")
	}
	if !sawReadOnly || !sawAttempt {
		t.Fatalf("timeline lacks the read-only lookup result (%v) or the refund's attempt (%v)", sawReadOnly, sawAttempt)
	}
	if v.Proof == nil || !v.Proof.OK {
		t.Fatalf("proof = %+v", v.Proof)
	}

	ref := agent.HaltRef{RunID: "a1", Op: agent.OpRef{Kind: agent.OpTool, ID: "c1", ToolName: "refund"}, Cause: agent.HaltCrashed}
	if err := agent.ResolveHalt(ctx, j, ref, agent.Outcome{Result: "refunded", Evidence: "provider ref r-9"}); err != nil {
		t.Fatalf("ResolveHalt: %v", err)
	}
	if _, err := newAgent().Run(ctx, "a1", agent.UserText("Refund order 42.")); err != nil {
		t.Fatalf("Run after resolve: %v", err)
	}
	h = Handler(Options{Source: JournalSource{J: j}, Provers: []Prover{HeadProver(latest(), logPub, Approvals{})}})
	v = viewOf(t, h, "a1")
	if v.State != "completed" || len(v.Halts) != 0 || v.Proof == nil || !v.Proof.OK || v.FinalText != "Refunded order 42." {
		t.Fatalf("after resolve: state %q, halts %v, proof %+v, text %q", v.State, v.Halts, v.Proof, v.FinalText)
	}
	if refunds != 1 {
		t.Fatalf("refund ran %d times, want 1", refunds)
	}

	// A head signed by another key does not verify.
	wrong := audit.Ed25519Verifier{Pub: key("someone else").Public().(ed25519.PublicKey)}
	if p := viewOf(t, Handler(Options{Source: JournalSource{J: j}, Provers: []Prover{HeadProver(latest(), wrong, Approvals{})}}), "a1").Proof; p == nil || p.OK {
		t.Fatalf("proof under the wrong key = %+v", p)
	}
	if code, body := get(t, h, "/"); code != 200 || !strings.Contains(body, "a1") {
		t.Fatalf("run list = %d %s", code, body)
	}
}

func isPending(err error) bool {
	_, ok := errors.AsType[*agent.ApprovalPending](err)
	return ok
}

// TestListenLoopbackOnly: the viewer listens on a loopback address and refuses any other.
func TestListenLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		ln, err := Listen(addr)
		if err != nil {
			t.Fatalf("Listen(%q): %v", addr, err)
		}
		ip := ln.Addr().(*net.TCPAddr).IP
		ln.Close()
		if !ip.IsLoopback() {
			t.Fatalf("Listen(%q) bound %v", addr, ip)
		}
	}
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "192.168.1.10:0", "example.com:0"} {
		if ln, err := Listen(addr); err == nil {
			ln.Close()
			t.Errorf("Listen(%q) succeeded; want a refusal", addr)
		}
	}
}

// TestHandlerRefusesOtherHosts: a request addressed to a non-loopback host (a rebound DNS name) is
// refused, and the handler answers only reads.
func TestHandlerRefusesOtherHosts(t *testing.T) {
	h := Handler(Options{Source: Exports{}})
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "attacker.example:8484"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("request to another host = %d, want 403", rec.Code)
	}
	req = httptest.NewRequest("POST", "/run?id=x", nil)
	req.Host = "127.0.0.1:8484"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}

// TestGateStatus: each gate state names its counts, the approver set and k.
func TestGateStatus(t *testing.T) {
	three := []string{"ops", "finance", "risk"}
	for _, c := range []struct {
		g    Gate
		want string
	}{
		{Gate{Need: 2, Approvers: three, ApprovedBy: []string{"ops", "finance"}, Tallied: true}, "approved (2 of 3 approvers; 2 needed)"},
		{Gate{Need: 2, Approvers: three, ApprovedBy: []string{"ops"}}, "pending (1 of 3 approvers; 2 needed; signatures are checked when the run resumes)"},
		{Gate{Need: 2, Approvers: three, ApprovedBy: []string{"ops"}, DeniedBy: []string{"finance", "risk"}, Tallied: true}, "denied (1 of 3 approvers, 2 denied; 2 needed)"},
		{Gate{Single: true, Decisions: []Decision{{Approved: false}}}, "denied (single decision)"},
		{Gate{ApprovedBy: []string{"ops"}}, "pending (1 approved, 0 denied so far; the policy is journaled with the call's result or the gate's tally)"},
	} {
		if got := gateStatus(c.g); got != c.want {
			t.Errorf("gateStatus(%+v) = %q, want %q", c.g, got, c.want)
		}
	}
}

// TestWaitingForApproval: a run paused at a gate with no decision shows "waiting for approval",
// and lists the gate with 0 decided and every approver pending.
func TestWaitingForApproval(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	refund := agent.MustFunc("refund", "refund an order", func(context.Context, struct{ Order int }) (string, error) {
		return "refunded", nil
	}, agent.WithApproval(&policy))
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		return audit.Ed25519Verifier{Pub: key(id).Public().(ed25519.PublicKey)}, true
	}
	model := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{"Order":42}`), agenttest.TextTurn("done"))
	a := agenttest.MustNew(model, j, agent.WithTools(refund), agent.WithApproverVerifiers(verifiers))
	if _, err := a.Run(ctx, "w1", agent.UserText("Refund order 42.")); !isPending(err) {
		t.Fatalf("Run = %v, want approval pending", err)
	}
	h := Handler(Options{Source: JournalSource{J: j}})
	v := viewOf(t, h, "w1")
	if v.State != "waiting" || v.StateText != "waiting for approval" {
		t.Fatalf("state = %q (%q), want waiting for approval", v.State, v.StateText)
	}
	if len(v.Gates) != 1 {
		t.Fatalf("gates = %+v, want the refund's gate listed before any decision", v.Gates)
	}
	g := v.Gates[0]
	if !g.Waiting || g.Need != 2 || len(g.Approvers) != 3 || len(g.Decisions) != 0 || strings.Join(g.Pending, ",") != "ops,finance,risk" {
		t.Fatalf("gate = %+v", g)
	}
	if want := "pending (0 of 3 approvers; 2 needed; signatures are checked when the run resumes)"; g.Status != want {
		t.Fatalf("gate status = %q, want %q", g.Status, want)
	}
	if code, body := get(t, h, "/run?id=w1"); code != 200 || !strings.Contains(body, "waiting for approval") {
		t.Fatalf("GET /run = %d, page does not say waiting for approval", code)
	}
}

// TestWaitingSingleGate: a one-decision gate shows waiting until its decision is recorded.
func TestWaitingSingleGate(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	refund := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	newAgent := func() *agent.Agent {
		return agenttest.MustNew(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done")), j, agent.WithTools(refund))
	}
	if _, err := newAgent().Run(ctx, "s1", agent.UserText("go")); !isPending(err) {
		t.Fatalf("Run = %v, want approval pending", err)
	}
	h := Handler(Options{Source: JournalSource{J: j}})
	if v := viewOf(t, h, "s1"); v.StateText != "waiting for approval" || len(v.Gates) != 1 || !v.Gates[0].Single || v.Gates[0].Status != "pending (one decision; none recorded yet)" {
		t.Fatalf("view = %q, gates %+v", v.StateText, v.Gates)
	}
	if err := agent.Approve(ctx, j, "s1", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgent().Run(ctx, "s1", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if v := viewOf(t, h, "s1"); v.State != "completed" || v.Gates[0].Waiting || v.Gates[0].Status != "approved (single decision)" {
		t.Fatalf("after approval: state %q, gates %+v", v.State, v.Gates)
	}
}

// TestMainUsage: -h prints the synopsis and every flag and exits 0; a bad flag combination is a
// usage error (2) and serves nothing.
func TestMainUsage(t *testing.T) {
	var out, errb strings.Builder
	if code := Main([]string{"-h"}, &out, &errb, Stores{}); code != 0 {
		t.Fatalf("-h exit = %d", code)
	}
	for _, want := range []string{"bide-view -db <file>", "loopback", "-db", "-journal", "-run", "-topology", "-digest", "-sth", "-evidence", "-pubkey", "-approver-keys", "-need", "-approvers", "-postgres", "-addr"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("-h output lacks %q", want)
		}
	}
	for _, args := range [][]string{{}, {"-db", "x", "-journal", "y"}, {"-journal", "y", "-sth", "s"}, {"-journal", "y", "-digest", "d"}, {"-journal", "y", "-approver-keys", "k"}, {"-db", "x", "-postgres", "p"}, {"-postgres", "p", "-journal", "y"},
		{"-journal", "y", "-sth", "s", "-pubkey", "k", "-approver-keys", "a", "-need", "2"},
		{"-journal", "y", "-sth", "s", "-pubkey", "k", "-need", "2", "-approvers", "a,b"},
		{"-journal", "y", "-sth", "s", "-pubkey", "k", "-approver-keys", "a", "-need", "3", "-approvers", "a,b"}} {
		if code := Main(args, io.Discard, io.Discard, Stores{}); code != 2 {
			t.Errorf("Main(%q) = %d, want 2", args, code)
		}
	}
}

// gatedRun drives a run through a 2-of-3 gate with the given decisions (approver -> approved),
// to its end, and returns its journal and a head over all of it signed by the log key.
func gatedRun(t *testing.T, decisions map[string]bool) (*agent.Journal, audit.SignedTreeHead) {
	t.Helper()
	ctx := context.Background()
	j := agenttest.MemJournal()
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	refund := agent.MustFunc("refund", "refund", func(context.Context, struct{ Order int }) (string, error) { return "refunded", nil }, agent.WithApproval(&policy))
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		return audit.Ed25519Verifier{Pub: key(id).Public().(ed25519.PublicKey)}, true
	}
	run := func() error {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{"Order":42}`), agenttest.TextTurn("done"))
		_, err := agenttest.MustNew(m, j, agent.WithTools(refund), agent.WithApproverVerifiers(verifiers)).Run(ctx, "g1", agent.UserText("go"))
		return err
	}
	if err := run(); !isPending(err) {
		t.Fatalf("Run = %v, want approval pending", err)
	}
	recs, _ := j.History(ctx, "g1")
	_, call, _ := agent.FindToolCall(recs, "c1")
	for _, who := range []string{"ops", "finance", "risk"} {
		approved, ok := decisions[who]
		if !ok {
			continue
		}
		subj := agent.ApprovalSubject{RunID: "g1", ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
		sig := ed25519.Sign(key(who), agent.ApprovalDecisionBytes(subj, who, approved))
		if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "g1", ToolUseID: "c1", ApproverID: who, Approved: approved, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(); err != nil {
		t.Fatalf("Run after the decisions: %v", err)
	}
	th, err := audit.NewTreeHead(ctx, j, "g1", time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: key("log")})
	if err != nil {
		t.Fatal(err)
	}
	return j, sth
}

// approverKeys resolves each approver to a key: its own, unless swap names another key's seed.
func approverKeys(swap map[string]string) agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) {
		seed := id
		if s, ok := swap[id]; ok {
			seed = s
		}
		return audit.Ed25519Verifier{Pub: key(seed).Public().(ed25519.PublicKey)}, true
	}
}

// TestApprovalSignatures: with approver keys the viewer runs audit.VerifyApprovals on each m-of-n
// gate, from a signed head or an evidence package; a wrong key or a count below k fails, naming
// why; without keys the gate says its signatures were not checked.
func TestApprovalSignatures(t *testing.T) {
	ctx := context.Background()
	logPub := audit.Ed25519Verifier{Pub: key("log").Public().(ed25519.PublicKey)}
	gate := func(p Prover, j *agent.Journal) (Gate, *Proof) {
		v := viewOf(t, Handler(Options{Source: JournalSource{J: j}, Provers: []Prover{p}}), "g1")
		if len(v.Gates) != 1 {
			t.Fatalf("gates = %+v", v.Gates)
		}
		return v.Gates[0], v.Proof
	}

	j, sth := gatedRun(t, map[string]bool{"ops": true, "finance": true})
	g, p := gate(HeadProver(sth, logPub, Approvals{Keys: approverKeys(nil)}), j)
	if want := "2 of 3: signatures verified (ops, finance) (against the recorded policy)"; !g.SignaturesOK || g.Signatures != want || !p.Approvals["c1"].OK {
		t.Fatalf("verified gate: %q (ok %v), proof %+v; want %q", g.Signatures, g.SignaturesOK, p.Approvals, want)
	}

	pkg, err := audit.Evidence(ctx, j, "g1", audit.Ed25519Signer{Priv: key("log")}, time.Now().UnixNano(), audit.WithToolCall("c1"))
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := audit.ApprovalEvidence(ctx, j, "g1", "c1", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions = append(pkg.Actions, decisions[:len(decisions)-1]...)
	if err := pkg.Seal(audit.Ed25519Signer{Priv: key("log")}); err != nil {
		t.Fatal(err)
	}
	if g, _ := gate(EvidenceProver(pkg, logPub, Approvals{Keys: approverKeys(nil)}), j); !g.SignaturesOK {
		t.Fatalf("evidence package: %q, want verified", g.Signatures)
	}

	g, _ = gate(HeadProver(sth, logPub, Approvals{Keys: approverKeys(map[string]string{"finance": "mallory"})}), j)
	if g.SignaturesOK || !strings.HasPrefix(g.Signatures, "signatures NOT verified") || !strings.Contains(g.Signatures, "finance") {
		t.Fatalf("wrong key for finance: %q, want a failure naming finance", g.Signatures)
	}

	jd, sthd := gatedRun(t, map[string]bool{"ops": true, "finance": false, "risk": false})
	g, _ = gate(HeadProver(sthd, logPub, Approvals{Keys: approverKeys(nil)}), jd)
	if g.SignaturesOK || !strings.Contains(g.Signatures, "only 1 of the 2 needed approvals verified") {
		t.Fatalf("count below k: %q", g.Signatures)
	}

	all3 := []string{"ops", "finance", "risk"}
	g, _ = gate(HeadProver(sth, logPub, Approvals{Keys: approverKeys(nil), Expected: &agent.ApprovalPolicy{Need: 2, Approvers: all3}}), j)
	if want := "2 of 3: signatures verified (ops, finance) (against the expected policy)"; !g.SignaturesOK || g.Signatures != want {
		t.Fatalf("expected policy that matches: %q, want %q", g.Signatures, want)
	}
	g, _ = gate(HeadProver(sth, logPub, Approvals{Keys: approverKeys(nil), Expected: &agent.ApprovalPolicy{Need: 1, Approvers: all3}}), j)
	if want := "recorded policy 2 of 3 (ops, finance, risk) differs from expected 1 of 3 (ops, finance, risk)"; g.SignaturesOK || !strings.Contains(g.Signatures, want) || !strings.HasSuffix(g.Signatures, "(against the expected policy)") {
		t.Fatalf("expected policy that differs: %q, want a failure saying %q", g.Signatures, want)
	}
	if strings.Contains(g.Signatures, "the gate enforced need") {
		t.Fatalf("the mismatch is reported twice: %q", g.Signatures)
	}

	g, _ = gate(HeadProver(sth, logPub, Approvals{}), j)
	if g.SignaturesOK || g.Signatures != "included in the signed log; signatures not checked (pass -approver-keys)" {
		t.Fatalf("no keys: %q", g.Signatures)
	}
	g, _ = gate(func(context.Context, string, []agent.Record) *Proof { return nil }, j)
	if g.Signatures != "signatures not checked (pass -approver-keys, with -sth or -evidence)" {
		t.Fatalf("no proof: %q", g.Signatures)
	}
}

// TestFlowFromJournal: a flow run is drawn with no flags, from the topology it journaled, its
// digest checked; a run journaled without one says so and draws nothing; -topology overrides.
func TestFlowFromJournal(t *testing.T) {
	ctx := context.Background()
	charges := 0
	flow := refundFlow(t, &charges)
	j := agenttest.MemJournal()
	if _, err := flow.Run(ctx, j, "f1", order{ID: "o-1", Amount: 250}); err != nil {
		t.Fatal(err)
	}
	v := viewOf(t, Handler(Options{Source: JournalSource{J: j}}), "f1")
	if v.Graph == nil || v.Conform == nil {
		t.Fatalf("no graph or conformance from the journaled topology: notes %v", v.Notes)
	}
	if c := v.Conform; !c.OK || c.Digest != "match" || c.Source != "from journal (digest verified)" {
		t.Fatalf("conformance = %+v", c)
	}
	if node(v, "charge").Status != "done" || node(v, "skip").Status != "not-reached" {
		t.Fatalf("nodes: charge %q, skip %q", node(v, "charge").Status, node(v, "skip").Status)
	}
	if v := viewOf(t, Handler(Options{Source: JournalSource{J: j}, Flows: map[string]*Flow{"refund": {Topology: flow.Topology(), Source: "from -topology"}}}), "f1"); v.Conform == nil || v.Conform.Source != "from -topology" {
		t.Fatalf("an override was not used: %+v", v.Conform)
	}

	// A run journaled before flow:topology existed: its records but that one.
	recs, err := j.History(ctx, "f1")
	if err != nil {
		t.Fatal(err)
	}
	old := Exports{"f1": slices.DeleteFunc(slices.Clone(recs), func(r agent.Record) bool { return r.Name == plan.TopologyStep })}
	v = viewOf(t, Handler(Options{Source: old}), "f1")
	if v.Graph != nil || !slices.ContainsFunc(v.Notes, func(n string) bool { return strings.Contains(n, "journaled no topology") }) {
		t.Fatalf("an old run: graph %v, notes %v", v.Graph != nil, v.Notes)
	}

	// A topology record another flow's: not drawn, and said so.
	other := plan.New[order, string]("refund")
	other.Step("validate", func(_ context.Context, o order) (string, error) { return o.ID, nil })
	of, err := other.Build()
	if err != nil {
		t.Fatal(err)
	}
	tb, _ := json.Marshal(of.Topology())
	forged := slices.Clone(old["f1"])
	r, err := agent.DecodeStoredRecord("f1", plan.TopologyStep, []byte(`{"name":"`+plan.TopologyStep+`","kind":"value","result":`+string(tb)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	forged = append(forged, r)
	v = viewOf(t, Handler(Options{Source: Exports{"f1": forged}}), "f1")
	if v.Graph != nil || !slices.ContainsFunc(v.Notes, func(n string) bool { return strings.Contains(n, "journaled topology is not drawn") }) {
		t.Fatalf("a mismatched topology record: graph %v, notes %v", v.Graph != nil, v.Notes)
	}
}
