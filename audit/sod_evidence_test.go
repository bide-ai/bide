package audit_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
	"github.com/bide-ai/bide/plan"
)

// Separation of duties, offline: the evidence for a gate that excludes the requester discloses the
// run's run:principal (which names the requesters, and nothing of the run's input), and
// VerifyApprovals recounts with the same exclusion.

// runAs drives g's agent once as requester (the run's principal on its first drive; a resume
// passes none and runs under the journaled one).
func (g *gate) runAs(requester string) error {
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Amount int `json:"amount"`
	}) (string, error) {
		g.charged++
		return "ok", nil
	}, agent.WithApproval(&g.policy))
	var opts []agent.RunOption
	if requester != "" {
		opts = append(opts, agent.WithIdentity(agent.Identity{Actor: "agent@1", OnBehalfOf: requester}))
	}
	a := agenttest.MustNew(chargeModel{}, g.store, agent.WithTools(charge), agent.WithApproverVerifiers(g.resolver()))
	_, err := a.Run(context.Background(), gateRun, agent.UserText("pay"), opts...)
	return err
}

// sodPassedGate: 2 of {alice, bob, carol}, requested by alice, who approves; so do bob and carol.
// alice's approval never counts; bob and carol pass the gate.
func sodPassedGate(t *testing.T, exclude bool) *gate {
	t.Helper()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.policy.ExcludePrincipal = exclude
	wantPaused(t, g.runAs("alice"), 0)
	g.decide(t, "alice", true, false)
	g.decide(t, "bob", true, false)
	if exclude {
		wantPaused(t, g.runAs(""), 1)
		g.decide(t, "carol", true, false)
	}
	if err := g.runAs(""); err != nil {
		t.Fatalf("run at quorum: %v", err)
	}
	if g.charged != 1 {
		t.Fatalf("charge ran %d times, want 1", g.charged)
	}
	return g
}

// The evidence starts with the run's run:principal, proven under the same head, and VerifyApprovals
// recounts bob and carol, lists alice as ignored for being the requester, and names her.
func TestSoDEvidence_ExcludedDecisionVerifiesOffline(t *testing.T) {
	g := sodPassedGate(t, true)
	sth := g.sth(t, 1700000000)
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", sth)
	if err != nil {
		t.Fatalf("ApprovalEvidence: %v", err)
	}
	if acts[0].Kind != audit.KindStep || acts[0].Ref != "run:principal" {
		t.Fatalf("evidence starts with %s %q, want the run's run:principal", acts[0].Kind, acts[0].Ref)
	}
	// The evidence proves the requesters without the run's input: run:start is not in it, and the
	// run:principal record holds no input.
	for _, a := range acts {
		r, err := a.Bundle.Record()
		if err != nil {
			t.Fatal(err)
		}
		if r.Name == "run:start" || strings.Contains(string(r.Result), "pay") && r.Name == "run:principal" {
			t.Fatalf("the evidence discloses the run's input in %s: %s", r.Name, r.Result)
		}
	}
	for i := 1; i < len(acts); i++ {
		if acts[i].Bundle.Inclusion.Index <= acts[i-1].Bundle.Inclusion.Index {
			t.Fatalf("evidence %d (%s) is not after evidence %d in the journal", i, acts[i].Kind, i-1)
		}
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK {
		t.Fatalf("VerifyApprovals = %+v, %v", v, err)
	}
	if !slices.Equal(v.Counted, []string{"bob", "carol"}) || !slices.Equal(v.Requesters, []string{"alice"}) {
		t.Fatalf("verdict = %+v, want bob and carol counted and requester alice", v)
	}
	if got := ignoredReason(v, "alice"); got != agent.ReasonRequester {
		t.Fatalf("alice ignored for %q, want %q", got, agent.ReasonRequester)
	}

	// The evidence also verifies as a package.
	pkg, err := audit.Evidence(context.Background(), g.store, gateRun, edS(g.logPriv), 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	acts, err = audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions = append(pkg.Actions, acts...)
	if err := pkg.Seal(edS(g.logPriv)); err != nil {
		t.Fatal(err)
	}
	if rep, err := pkg.Verify(edV(g.logPub)); err != nil || !rep.OK {
		t.Fatalf("package Verify: %v %+v", err, rep.Items)
	}
	if v, err := audit.VerifyApprovals(pkg.Actions, "c1", g.policy, g.resolver(), edV(g.logPub)); err != nil || !v.OK {
		t.Fatalf("VerifyApprovals over the package = %+v, %v", v, err)
	}
}

// Evidence that omits the run's run:principal cannot show who the requester was: the recount counts no
// one and the verdict fails, with a problem saying why.
func TestSoDEvidence_WithoutRunStartFails(t *testing.T) {
	g := sodPassedGate(t, true)
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts[1:], "c1", g.policy, g.resolver(), edV(g.logPub))
	if !errors.Is(err, audit.ErrNotVerified) || v.OK || len(v.Counted) != 0 || !hasProblem(v, "run:principal") {
		t.Fatalf("verdict = %+v, %v; want not verified, nothing counted, a problem naming run:principal", v, err)
	}
}

// A run:principal from another run, proven under another head, does not name this run's requesters.
func TestSoDEvidence_ForeignRunStartFails(t *testing.T) {
	g := sodPassedGate(t, true)
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	other := sodPassedGate(t, true)
	other.logPub, other.logPriv = g.logPub, g.logPriv // the same log operator
	foreign, err := audit.ApprovalEvidence(context.Background(), other.store, gateRun, "c1", other.sth(t, 1700000001))
	if err != nil {
		t.Fatal(err)
	}
	acts[0] = foreign[0]
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if !errors.Is(err, audit.ErrNotVerified) || v.OK || !hasProblem(v, "run:principal") {
		t.Fatalf("verdict = %+v, %v; want a problem with the run's run:principal", v, err)
	}
}

// An auditor who expects separation of duties fails a gate that did not enforce it (alice, the
// requester, counted), and one who does not expect it fails a gate that did (the gate enforced
// another policy).
func TestSoDEvidence_PolicyMismatch(t *testing.T) {
	g := sodPassedGate(t, false)
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	if acts[0].Kind == audit.KindStep && acts[0].Ref == "run:principal" {
		t.Fatal("the evidence of a gate that does not exclude the requester discloses the run's run:principal")
	}
	want := g.policy
	want.ExcludePrincipal = true
	v, err := audit.VerifyApprovals(acts, "c1", want, g.resolver(), edV(g.logPub))
	if !errors.Is(err, audit.ErrNotVerified) || v.OK || !hasProblem(v, "requester") {
		t.Fatalf("expecting exclusion of a gate without it: %+v, %v; want a problem", v, err)
	}

	g = sodPassedGate(t, true)
	acts, err = audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	want = g.policy
	want.ExcludePrincipal = false
	v, err = audit.VerifyApprovals(acts, "c1", want, g.resolver(), edV(g.logPub))
	if !errors.Is(err, audit.ErrNotVerified) || v.OK || !hasProblem(v, "requester") {
		t.Fatalf("not expecting the exclusion of a gate with it: %+v, %v; want a problem", v, err)
	}
}

// A tally written by someone with journal write access, claiming another requester and counting
// alice, makes the gate run the call (a recorded tally is authoritative), but the offline recount
// from the run's run:principal excludes alice, and VerifyApprovals reports both disagreements.
func TestSoDEvidence_ForgedTallyDetected(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.policy.ExcludePrincipal = true
	wantPaused(t, g.runAs("alice"), 0)
	g.decide(t, "alice", true, false)
	g.decide(t, "bob", true, false)
	recs, err := g.store.History(ctx, gateRun)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range recs {
		if agent.IsApprovalDecision(r, "c1") {
			names = append(names, r.Name)
		}
	}
	forged := agent.ApprovalTally{Need: 2, Approvers: g.policy.Approvers, Approved: 2, ApprovedBy: []string{"alice", "bob"},
		Pending: []string{"carol"}, Records: names, DecisionFormat: agent.DecisionFormat, ExcludePrincipal: true, Requesters: []string{"zed"}}
	b, _ := json.Marshal(forged)
	if _, err := journaltest.Put(ctx, g.store, gateRun, agent.ApprovalTallyStep("c1"), agent.Record{Kind: agent.StepValue, Result: b}); err != nil {
		t.Fatal(err)
	}
	if err := g.runAs(""); err != nil || g.charged != 1 {
		t.Fatalf("run on the forged tally = %v (charged %d)", err, g.charged)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if !errors.Is(err, audit.ErrNotVerified) || v.OK {
		t.Fatalf("forged tally verified: %+v", v)
	}
	if !hasProblem(v, `"zed"`) || !hasProblem(v, "disagrees") || !slices.Equal(v.Counted, []string{"bob"}) {
		t.Fatalf("problems = %q, counted %v; want the requester and the recount reported", v.Problems, v.Counted)
	}
}

// A gated plan flow node: the flow run's principal is the requester, and the evidence verifies
// offline with the requester's approval ignored.
func TestSoDEvidence_FlowGate(t *testing.T) {
	ctx, alice := context.Background(), agent.WithIdentity(agent.Identity{OnBehalfOf: "alice"})
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.policy.ExcludePrincipal = true
	b := plan.New[int, int]("refunds").WithApproverVerifiers(g.resolver())
	pay := b.Step("pay", func(_ context.Context, n int) (int, error) { g.charged++; return n, nil }, plan.Approval(&g.policy))
	notify := b.Step("notify", func(_ context.Context, n int) (int, error) { return n, nil }, plan.ReadOnly())
	b.Edge(pay, notify)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Run(ctx, g.store, flowRun, 120, alice)
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first drive = %v", err)
	}
	for _, id := range []string{"alice", "bob", "carol"} {
		sig := ed25519.Sign(g.privs[id], agent.ApprovalDecisionBytes(pend.Subject(), id, true, testDecidedAt))
		if err := agent.SubmitDecision(ctx, g.store, agent.Decision{RunID: flowRun, ToolUseID: pend.ToolUseID, ApproverID: id, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Run(context.Background(), g.store, flowRun, 120); err != nil || g.charged != 1 {
		t.Fatalf("drive at quorum = %v, ran %d", err, g.charged)
	}
	pkg, err := audit.Evidence(ctx, g.store, flowRun, edS(g.logPriv), time.Now().Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, flowRun, "node:pay", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts, "node:pay", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK || !slices.Equal(v.Counted, []string{"bob", "carol"}) || ignoredReason(v, "alice") != agent.ReasonRequester {
		t.Fatalf("verdict = %+v, %v; want bob and carol counted, alice ignored as the requester", v, err)
	}
	if !strings.Contains(acts[0].Ref, "run:principal") {
		t.Fatalf("flow evidence starts with %q, want the run's run:principal", acts[0].Ref)
	}
}

// A gate in a sub-run delegated through AttenuatingSubAgent excludes the root run's principal too:
// alice started the root run, the delegate acts for the grant's subject, and alice's approval of
// the sub-run's call never counts. The sub-run's own run:principal proves the chain offline.
func TestSoDEvidence_DelegatedSubRun(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.policy.ExcludePrincipal = true
	_, opPriv, _ := ed25519.GenerateKey(nil)
	signer := audit.Ed25519Signer{Priv: opPriv}
	root := audit.Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}
	rootSG, err := audit.SignGrant(root, signer)
	if err != nil {
		t.Fatal(err)
	}
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Amount int `json:"amount"`
	}) (string, error) {
		g.charged++
		return "ok", nil
	}, agent.WithApproval(&g.policy))
	drive := func(first bool) error {
		sub := agenttest.MustNew(chargeModel{}, g.store, agent.WithTools(charge), agent.WithApproverVerifiers(g.resolver()))
		tool := audit.AttenuatingSubAgent("exec", "execute", sub, audit.AttenuationConfig{Store: g.store,
			Narrow: func(parent audit.Grant, name string) audit.Grant {
				return audit.Grant{ID: "g/" + name, Scope: parent.Scope}
			}})
		model := agenttest.NewScriptedModel(agenttest.ToolTurn("p1", "exec", `{"task":"pay"}`), agenttest.TextTurn("ok"))
		if !first {
			model = agenttest.NewScriptedModel(agenttest.TextTurn("ok"))
		}
		parent := agenttest.MustNew(model, g.store, agent.WithTools(tool))
		var opts []agent.RunOption
		if first {
			opts = append(opts, agent.WithIdentity(agent.Identity{Actor: "desk-agent", OnBehalfOf: "alice", AuthorityRef: root.Digest()}))
		}
		_, err := parent.Run(audit.WithGrant(ctx, rootSG, signer), "root", agent.UserText("go"), opts...)
		return err
	}
	var pend *agent.ApprovalPending
	if err := drive(true); !errors.As(err, &pend) {
		t.Fatalf("first drive = %v; want the sub-run's gate to pause", err)
	}
	if !slices.Equal(pend.Quorum.Requesters, []string{"alice", "desk"}) {
		t.Fatalf("requesters = %q; want alice (the root's) and desk (the delegate's)", pend.Quorum.Requesters)
	}
	sub := pend.RunID
	for _, id := range []string{"alice", "bob", "carol"} {
		sig := ed25519.Sign(g.privs[id], agent.ApprovalDecisionBytes(pend.Subject(), id, true, testDecidedAt))
		if err := agent.SubmitDecision(ctx, g.store, agent.Decision{RunID: sub, ToolUseID: pend.ToolUseID, ApproverID: id, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	if err := drive(false); err != nil || g.charged != 1 {
		t.Fatalf("drive at quorum = %v, charged %d", err, g.charged)
	}
	th, err := audit.NewTreeHead(ctx, g.store, sub, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, sub, pend.ToolUseID, signTH(t, th, g.logPriv))
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts, pend.ToolUseID, g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK || !slices.Equal(v.Counted, []string{"bob", "carol"}) || !slices.Equal(v.Requesters, []string{"alice", "desk"}) || ignoredReason(v, "alice") != agent.ReasonRequester {
		t.Fatalf("verdict = %+v, %v; want bob and carol counted, alice ignored as an ancestor's requester", v, err)
	}
}

// A run:principal journaled after the call's request (a run an earlier version started, whose
// principal a later drive recorded) does not name who requested the call: the gate refuses the
// run, and an audit of a tally and result written into such a journal reports the order.
func TestSoDEvidence_PrincipalAfterRequestFails(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.policy.ExcludePrincipal = true
	wantPaused(t, g.runAs("alice"), 0)
	recs, err := g.store.History(ctx, gateRun)
	if err != nil {
		t.Fatal(err)
	}
	legacy := agenttest.MemJournal() // the run as an earlier version journaled it: no principal
	for _, r := range recs {
		if r.Name == "run:start" || r.Name == "run:principal" {
			continue
		}
		if _, err := journaltest.Put(ctx, legacy, gateRun, r.Name, r); err != nil {
			t.Fatal(err)
		}
	}
	g.store = legacy
	if err := g.runAs("alice"); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("drive of the legacy run = %v; want ErrConfig", err)
	}
	g.decide(t, "bob", true, false)
	g.decide(t, "carol", true, false)
	var names []string
	all, _ := legacy.History(ctx, gateRun)
	for _, r := range all {
		if agent.IsApprovalDecision(r, "c1") {
			names = append(names, r.Name)
		}
	}
	forged := agent.ApprovalTally{Need: 2, Approvers: g.policy.Approvers, Approved: 2, ApprovedBy: []string{"bob", "carol"},
		Excluded: []string{"alice"}, Records: names, DecisionFormat: agent.DecisionFormat, ExcludePrincipal: true, Requesters: []string{"alice"}}
	b, _ := json.Marshal(forged)
	if _, err := journaltest.Put(ctx, legacy, gateRun, agent.ApprovalTallyStep("c1"), agent.Record{Kind: agent.StepValue, Result: b}); err != nil {
		t.Fatal(err)
	}
	if _, err := journaltest.Put(ctx, legacy, gateRun, agent.ToolResultStep("c1"), agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"ok"`)}); err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(ctx, legacy, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if !errors.Is(err, audit.ErrNotVerified) || v.OK || !hasProblem(v, "not before the request") {
		t.Fatalf("verdict = %+v, %v; want a problem: the run:principal is not before the request", v, err)
	}
}
