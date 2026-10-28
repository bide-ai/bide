package audit_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/plan"
)

// separated runs a gated node whose own gate alice decided, and whose escalation instance (a gate of alice and
// carol) both decided: alice first. sep says the instance is counted apart from the node's gate (the taint policy
// separates approvers), so alice's decision on it does not count, and carol's does.
func separated(t *testing.T, sep bool) (*gate, []audit.EvidenceAction, agent.ApprovalPolicy) {
	t.Helper()
	ctx := context.Background()
	g := newGate(1, []string{"alice", "bob"}, []string{"alice", "bob", "carol"})
	b := plan.New[int, int]("refunds").WithApproverVerifiers(g.resolver())
	b.Step("pay", func(_ context.Context, n int) (int, error) { g.charged++; return n, nil }, plan.Approval(&g.policy))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	var pend *agent.ApprovalPending
	if _, err := f.Run(ctx, g.store, flowRun, 120); !errors.As(err, &pend) {
		t.Fatalf("first drive = %v, want a pause at the node's gate", err)
	}
	nodePend := pend
	sign := func(p *agent.ApprovalPending, key, id string) {
		t.Helper()
		sig := ed25519.Sign(g.privs[id], agent.ApprovalDecisionBytes(p.Subject(), id, true, testDecidedAt))
		if err := agent.SubmitDecision(ctx, g.store, agent.Decision{RunID: flowRun, ToolUseID: key, ApproverID: id, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig},
			agent.WithDecisionCheck(g.resolver())); err != nil {
			t.Fatalf("SubmitDecision %s on %s: %v", id, key, err)
		}
	}
	args := json.RawMessage(`120`)
	// The node's own gate passes first (its terminal tally is recorded), as the engine orders them.
	sign(nodePend, "node:pay", "alice")
	if ok, err := journalhook.Gate(ctx, g.store, flowRun, "node:pay", "pay", args, &g.policy, g.resolver()); err != nil || !ok {
		t.Fatalf("the node's gate = %v, %v", ok, err)
	}
	const inst = "node:pay:taint"
	esc := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "carol"}}
	run := esc
	if sep {
		run = esc.SeparatedFrom("node:pay")
	}
	if _, err := journalhook.Gate(ctx, g.store, flowRun, inst, "pay", args, &run, g.resolver()); !errors.As(err, &pend) {
		t.Fatalf("the instance's first evaluation = %v, want a pause", err)
	}
	sign(pend, inst, "alice")
	if sep {
		if _, err := journalhook.Gate(ctx, g.store, flowRun, inst, "pay", args, &run, g.resolver()); !errors.As(err, &pend) {
			t.Fatalf("after the excluded approver's decision = %v, want the instance still waiting", err)
		}
	}
	sign(pend, inst, "carol")
	if ok, err := journalhook.Gate(ctx, g.store, flowRun, inst, "pay", args, &run, g.resolver()); err != nil || !ok {
		t.Fatalf("the instance's gate after quorum = %v, %v", ok, err)
	}
	if _, err := f.Run(ctx, g.store, flowRun, 120); err != nil || g.charged != 1 {
		t.Fatalf("drive at both quorums = %v, ran %d", err, g.charged)
	}
	th, err := audit.NewTreeHead(ctx, g.store, flowRun, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, flowRun, inst, signTH(t, th, g.logPriv))
	if err != nil {
		t.Fatalf("ApprovalEvidence(instance): %v", err)
	}
	return g, acts, esc
}

// The offline recount excludes the approvers the node's own gate counted, from the own gate's tally among the
// evidence: the instance's tally and the recount agree, and the excluded decision is listed as ignored.
func TestVerifyApprovals_ExcludesTheApproversTheOwnGateCounted(t *testing.T) {
	g, acts, esc := separated(t, true)
	hasOwn := false
	for _, a := range acts {
		hasOwn = hasOwn || a.Ref == agent.ApprovalTallyStep("node:pay")
	}
	if !hasOwn {
		t.Fatal("the evidence of the instance does not carry the own gate's tally")
	}
	v, err := audit.VerifyApprovals(acts, "node:pay:taint", esc.SeparatedFrom("node:pay"), g.resolver(), edV(g.logPub))
	if err != nil || !v.OK || len(v.Counted) != 1 || v.Counted[0] != "carol" {
		t.Fatalf("the separated instance: %+v, %v", v, err)
	}
	found := false
	for _, ig := range v.Ignored {
		found = found || ig.Approver == "alice" && ig.Reason == agent.ReasonSibling
	}
	if !found {
		t.Errorf("alice's decision on the instance is not ignored as counted on the own gate: %+v", v.Ignored)
	}
}

// A verifier that expects separation refuses evidence of a gate that counted the own gate's approver; one that
// expects none refuses evidence of a gate that excluded someone; and without the own gate's tally the recount
// counts no one.
func TestVerifyApprovals_SeparationIsHeldBothWays(t *testing.T) {
	// The gate did not separate: alice's decision counted on the instance.
	g, acts, esc := separated(t, false)
	if v, err := audit.VerifyApprovals(acts, "node:pay:taint", esc, g.resolver(), edV(g.logPub)); err != nil || !v.OK {
		t.Fatalf("an unseparated gate under the unseparated policy: %+v, %v", v, err)
	}
	v, err := audit.VerifyApprovals(acts, "node:pay:taint", esc.SeparatedFrom("node:pay"), g.resolver(), edV(g.logPub))
	if err == nil || v.OK || len(v.Problems) == 0 {
		t.Errorf("an unseparated gate verified under a policy that separates approvers: %+v, %v", v, err)
	}
	// The gate separated, the expected policy does not.
	g2, acts2, esc2 := separated(t, true)
	v, err = audit.VerifyApprovals(acts2, "node:pay:taint", esc2, g2.resolver(), edV(g2.logPub))
	if err == nil || v.OK || !strings.Contains(strings.Join(v.Problems, "|"), "excluded_by_sibling") {
		t.Errorf("a separated gate verified under a policy that does not separate: %+v, %v", v, err)
	}
	// The own gate's tally is left out: the recount cannot know whom to exclude, and counts no one.
	without := without(acts2, func(a audit.EvidenceAction) bool { return a.Ref == agent.ApprovalTallyStep("node:pay") })
	v, err = audit.VerifyApprovals(without, "node:pay:taint", esc2.SeparatedFrom("node:pay"), g2.resolver(), edV(g2.logPub))
	if err == nil || v.OK || !strings.Contains(strings.Join(v.Problems, "|"), "own gate") {
		t.Errorf("evidence without the own gate's tally verified: %+v, %v", v, err)
	}
}
