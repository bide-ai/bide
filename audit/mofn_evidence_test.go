package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// chargeModel calls the "charge" tool until a tool result is in the conversation, then
// answers. It is stateless, so a resumed run (which replays the journaled tool turn) needs no
// per-run script.
type chargeModel struct{}

func (chargeModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	if hasToolResult(req.Messages) {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "charged"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	} else {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "charge", ArgsFragment: json.RawMessage(`{"amount":120}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func hasToolResult(msgs []agent.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if _, ok := p.(agent.ToolResult); ok {
				return true
			}
		}
	}
	return false
}

const gateRun = "run-kofn"

// gate is a real agent run through an m-of-n gate on the "charge" tool, plus the keys an
// offline auditor holds: each registered approver's key and the log operator's key.
type gate struct {
	store   agent.Durable
	policy  agent.ApprovalPolicy
	pubs    map[string]ed25519.PublicKey
	privs   map[string]ed25519.PrivateKey
	logPub  ed25519.PublicKey
	logPriv ed25519.PrivateKey
	charged int
}

// newGate registers a key for every id in registered (eligible or not).
func newGate(need int, approvers, registered []string) *gate {
	g := &gate{
		store:  agent.NewMemStore(),
		policy: agent.ApprovalPolicy{Need: need, Approvers: approvers},
		pubs:   map[string]ed25519.PublicKey{},
		privs:  map[string]ed25519.PrivateKey{},
	}
	for _, id := range registered {
		g.pubs[id], g.privs[id], _ = ed25519.GenerateKey(rand.Reader)
	}
	g.logPub, g.logPriv, _ = ed25519.GenerateKey(rand.Reader)
	return g
}

// resolver maps approver ids to keys, the same shape the gate uses at run time.
func (g *gate) resolver() agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) {
		pub, ok := g.pubs[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	}
}

// run drives the agent once and returns its error (nil when the run completes).
func (g *gate) run() error {
	charge := agent.Func("charge", "charge the card", agent.Safety{Approval: &g.policy},
		func(context.Context, struct {
			Amount int `json:"amount"`
		}) (string, error) {
			g.charged++
			return "ok", nil
		})
	_, err := agent.New(chargeModel{}, g.store, charge).WithApproverVerifiers(g.resolver()).Run(context.Background(), gateRun, "pay")
	return err
}

// subject is the recorded call, which is what approvers sign.
func (g *gate) subject(t *testing.T) agent.ApprovalSubject {
	t.Helper()
	recs, err := g.store.History(context.Background(), gateRun)
	if err != nil {
		t.Fatal(err)
	}
	_, call, ok := agent.FindToolCall(recs, "c1")
	if !ok {
		t.Fatal("no recorded call c1")
	}
	return agent.ApprovalSubject{RunID: gateRun, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
}

// decide records id's signed decision; tamper corrupts the signature.
func (g *gate) decide(t *testing.T, id string, approved, tamper bool) {
	t.Helper()
	sig := ed25519.Sign(g.privs[id], agent.ApprovalDecisionBytes(g.subject(t), id, approved))
	if tamper {
		sig[0] ^= 0xff
	}
	d := agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: id, Approved: approved, Alg: audit.AlgEd25519, Signature: sig}
	if err := agent.SubmitDecision(context.Background(), g.store, d); err != nil {
		t.Fatalf("SubmitDecision %s: %v", id, err)
	}
}

// sth signs a tree head over the whole journal.
func (g *gate) sth(t *testing.T, ts int64) audit.SignedTreeHead {
	t.Helper()
	th, err := audit.NewTreeHead(context.Background(), g.store, gateRun, ts)
	if err != nil {
		t.Fatal(err)
	}
	return signTH(t, th, g.logPriv)
}

func wantPaused(t *testing.T, err error, approved int) {
	t.Helper()
	var pend *agent.PendingApproval
	if !errors.As(err, &pend) || pend.Quorum == nil || pend.Quorum.Approved != approved {
		t.Fatalf("err = %v, want an m-of-n pause at %d approved", err, approved)
	}
}

// passedGate: a Need=2 of {alice, bob, carol} gate. Before it passes, mallory (a registered
// key, not eligible) approves with a valid signature and carol approves with a tampered one;
// neither counts. alice then approves (1 of 2) and bob approves, and the charge runs.
func passedGate(t *testing.T) *gate {
	t.Helper()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol", "mallory"})
	wantPaused(t, g.run(), 0)
	g.decide(t, "mallory", true, false)
	g.decide(t, "carol", true, true)
	wantPaused(t, g.run(), 0)
	g.decide(t, "alice", true, false)
	wantPaused(t, g.run(), 1)
	g.decide(t, "bob", true, false)
	if err := g.run(); err != nil {
		t.Fatalf("run at quorum: %v", err)
	}
	if g.charged != 1 {
		t.Fatalf("charge ran %d times, want 1", g.charged)
	}
	return g
}

func hasProblem(v audit.ApprovalVerdict, substr string) bool {
	return slices.ContainsFunc(v.Problems, func(p string) bool { return strings.Contains(p, substr) })
}

func ignoredReason(v audit.ApprovalVerdict, approver string) string {
	for _, d := range v.Ignored {
		if d.Approver == approver {
			return d.Reason
		}
	}
	return ""
}

// The evidence carries, under one STH and in journal order, the request, every decision the
// gate read (including the ineligible and forged ones), the tally, and the result; the offline
// check recounts it to exactly alice and bob and explains the rest.
func TestMofnEvidence_KofNVerifiesOffline(t *testing.T) {
	g := passedGate(t)
	sth := g.sth(t, 1700000000)
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", sth)
	if err != nil {
		t.Fatalf("ApprovalEvidence: %v", err)
	}

	var kinds []audit.EvidenceKind
	for i, a := range acts {
		kinds = append(kinds, a.Kind)
		if err := a.Bundle.Verify(edV(g.logPub)); err != nil {
			t.Fatalf("evidence %d (%s %s) failed to verify (err=%v)", i, a.Kind, a.Label, err)
		}
		if !reflect.DeepEqual(a.Bundle.STH, sth) {
			t.Fatalf("evidence %d (%s) not under the one STH", i, a.Kind)
		}
		if i > 0 && a.Bundle.Inclusion.Index <= acts[i-1].Bundle.Inclusion.Index {
			t.Fatalf("evidence %d (%s) is not after evidence %d in the journal", i, a.Kind, i-1)
		}
	}
	want := []audit.EvidenceKind{audit.KindCall, audit.KindApproval, audit.KindApproval, audit.KindApproval, audit.KindApproval, audit.KindApprovalTally, audit.KindTool}
	if !slices.Equal(kinds, want) {
		t.Fatalf("evidence kinds = %v, want %v", kinds, want)
	}

	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err := reportErr(v.OK, err); err != nil {
		t.Fatalf("VerifyApprovals: %v", err)
	}
	if !v.OK || len(v.Problems) != 0 || !slices.Equal(v.Counted, []string{"alice", "bob"}) {
		t.Fatalf("verdict = %+v, want OK with alice and bob counted", v)
	}
	if v.ToolName != "charge" || string(v.Args) != `{"amount":120}` {
		t.Fatalf("verdict call = %s %s, want charge {\"amount\":120}", v.ToolName, v.Args)
	}
	if ignoredReason(v, "mallory") != agent.ReasonNotEligible || ignoredReason(v, "carol") != agent.ReasonBadSig {
		t.Fatalf("ignored = %+v, want mallory not eligible and carol's signature rejected", v.Ignored)
	}

	// Every decision also proves on its own, by its record name.
	for _, a := range acts {
		if a.Kind != audit.KindApproval {
			continue
		}
		pb, err := audit.ProveApproval(context.Background(), g.store, gateRun, a.Ref, sth)
		if err != nil || pb.Inclusion.Index != a.Bundle.Inclusion.Index {
			t.Fatalf("ProveApproval(%s): index %d err %v, want index %d", a.Ref, pb.Inclusion.Index, err, a.Bundle.Inclusion.Index)
		}
	}
}

// The approval evidence composes with a whole EvidencePackage: it survives a JSON round trip,
// the package verifies, and VerifyApprovals reads the package's actions directly.
func TestMofnEvidence_InPackage(t *testing.T) {
	ctx := context.Background()
	g := passedGate(t)
	pkg, err := audit.Evidence(ctx, g.store, gateRun, edS(g.logPriv), 1700000000, audit.WithToolCall("c1"))
	if err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "c1", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions = append(pkg.Actions, acts[:len(acts)-1]...) // the result is already packaged
	if rep, _ := pkg.Verify(edV(g.logPub)); rep.OK {
		t.Fatal("a package with actions appended after sealing verified")
	}
	if err := pkg.Seal(edS(g.logPriv)); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	var got audit.EvidencePackage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	rep, err := got.Verify(edV(g.logPub))
	if err != nil || !rep.OK {
		t.Fatalf("package Verify: ok=%v err=%v items=%+v", rep.OK, err, rep.Items)
	}
	counts := map[audit.EvidenceKind]int{}
	for _, it := range rep.Items {
		counts[it.Kind]++
	}
	if counts[audit.KindTool] != 1 || counts[audit.KindCall] != 1 || counts[audit.KindApproval] != 4 || counts[audit.KindApprovalTally] != 1 {
		t.Fatalf("report kinds = %v", counts)
	}
	v, err := audit.VerifyApprovals(got.Actions, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK || !slices.Equal(v.Counted, []string{"alice", "bob"}) {
		t.Fatalf("verdict = %+v, err = %v, want alice and bob counted", v, err)
	}
}

// Offline verification refuses a resolver under which two eligible approvers share a key: the
// holder of that key is one person in two seats, so the count would certify one approval as two.
func TestMofnEvidence_SharedKeyRefused(t *testing.T) {
	g := passedGate(t)
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatalf("ApprovalEvidence: %v", err)
	}
	shared := func(id string) (agent.ApproverVerifier, bool) {
		if id == "bob" {
			id = "alice"
		}
		return g.resolver()(id)
	}
	if _, err := audit.VerifyApprovals(acts, "c1", g.policy, shared, audit.Ed25519Verifier{Pub: g.logPub}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("VerifyApprovals with alice and bob on one key = %v, want ErrConfig", err)
	}
}
