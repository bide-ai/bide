package audit_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/internal/taintfixture"
	"github.com/bide-ai/bide/plan"
)

// Probe: the evidence and the offline recount of a call and of its escalation instance are separate: each is
// recounted under its own gate, and a decision for one never counts on the other.
func TestApprovalEvidence_EscalationInstanceIsRecountedUnderItsOwnGate(t *testing.T) {
	ctx := context.Background()
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	if err := f.Drive(); !agent.IsPause(err) {
		t.Fatalf("drive 1 = %v", err)
	}
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	if err := f.Drive(); !agent.IsPause(err) {
		t.Fatalf("drive 2 = %v, want a pause on the escalation instance", err)
	}
	// A decision signed for the call is refused on the instance, and the reverse; written past the check it
	// is in the journal and the recount below ignores it.
	if err := f.Submit("c2#taint", "c2", "esc1", true); err == nil {
		t.Fatal("a decision signed for the call was recorded on its escalation instance")
	}
	if err := f.Submit("c2", "c2#taint", "own1", true); err == nil {
		t.Fatal("a decision signed for the instance was recorded on the call itself")
	}
	if err := f.SubmitUnchecked("c2#taint", "c2", "esc2", true); err != nil {
		t.Fatal(err)
	}
	if err := f.Submit("c2#taint", "c2#taint", "esc1", true); err != nil {
		t.Fatal(err)
	}
	// esc2's real decision is recorded under another name than the planted one.
	f.At = f.At.Add(time.Second)
	if err := f.Submit("c2#taint", "c2#taint", "esc2", true); err != nil {
		t.Fatal(err)
	}
	if err := f.Drive(); err != nil || f.Wrote != 1 {
		t.Fatalf("drive 3 = %v, writes %d", err, f.Wrote)
	}

	th, err := audit.NewTreeHead(ctx, f.Journal, taintfixture.Run, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	sth := signTH(t, th, f.LogPriv)
	verify := func(call string, pol agent.ApprovalPolicy) audit.ApprovalVerdict {
		t.Helper()
		acts, err := audit.ApprovalEvidence(ctx, f.Journal, taintfixture.Run, call, sth)
		if err != nil {
			t.Fatalf("ApprovalEvidence(%s): %v", call, err)
		}
		v, _ := audit.VerifyApprovals(acts, call, pol, f.Verifiers, edV(f.LogPub))
		return v
	}
	own, esc := *f.Own, *f.Esc
	if v := verify("c2", own); !v.OK || len(v.Counted) != 1 || v.Counted[0] != "own1" {
		t.Fatalf("the call under its own gate: %+v", v)
	}
	if v := verify("c2#taint", esc); !v.OK || len(v.Counted) != 2 {
		t.Fatalf("the instance under the escalation gate: %+v", v)
	}
	// The planted esc2 decision (signed for the call) is disclosed and ignored, not counted.
	if v := verify("c2#taint", esc); len(v.Ignored) == 0 {
		t.Fatalf("the decision signed for the call is not among the ignored: %+v", v)
	}
	// Under the wrong gate neither passes: the recount never counts a decision for the other instance.
	if v := verify("c2", esc); v.OK || len(v.Counted) != 0 {
		t.Fatalf("the call recounted under the escalation gate: %+v, want nothing counted", v)
	}
	if v := verify("c2#taint", own); v.OK || len(v.Counted) != 0 {
		t.Fatalf("the instance recounted under the tool's own gate: %+v, want nothing counted", v)
	}
}

// The escalation instance of a gated plan flow node ("node:pay:taint", docs/design/flow-taint.md) has the
// node's request and the node's own record as its request and result, and its own decisions and tally. The
// engine writes no instance yet, so the test drives the instance's gate through the flow hook, with the
// node already run.
func TestFlowGateEvidence_EscalationInstanceOfANode(t *testing.T) {
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
	const inst = "node:pay:taint"
	esc := &agent.ApprovalPolicy{Need: 1, Approvers: []string{"carol"}}
	args := json.RawMessage(`120`)
	if _, err := journalhook.Gate(ctx, g.store, flowRun, inst, "pay", args, esc, g.resolver()); !errors.As(err, &pend) {
		t.Fatalf("the instance's first evaluation = %v, want a pause", err)
	}
	sign := func(p *agent.ApprovalPending, key, id string) {
		t.Helper()
		sig := ed25519.Sign(g.privs[id], agent.ApprovalDecisionBytes(p.Subject(), id, true, testDecidedAt))
		if err := agent.SubmitDecision(ctx, g.store, agent.Decision{RunID: flowRun, ToolUseID: key, ApproverID: id, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig},
			agent.WithDecisionCheck(g.resolver())); err != nil {
			t.Fatalf("SubmitDecision %s on %s: %v", id, key, err)
		}
	}
	sign(pend, inst, "carol")
	if ok, err := journalhook.Gate(ctx, g.store, flowRun, inst, "pay", args, esc, g.resolver()); err != nil || !ok {
		t.Fatalf("the instance's gate after quorum = %v, %v", ok, err)
	}
	sign(nodePend, "node:pay", "alice")
	if _, err := f.Run(ctx, g.store, flowRun, 120); err != nil || g.charged != 1 {
		t.Fatalf("drive at both quorums = %v, ran %d", err, g.charged)
	}

	th, err := audit.NewTreeHead(ctx, g.store, flowRun, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	sth := signTH(t, th, g.logPriv)
	// The evidence of the instance proves its own tally and the node's request and record.
	acts, err := audit.ApprovalEvidence(ctx, g.store, flowRun, inst, sth)
	if err != nil {
		t.Fatalf("ApprovalEvidence(instance): %v", err)
	}
	v, err := audit.VerifyApprovals(acts, inst, *esc, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK || len(v.Counted) != 1 || v.Counted[0] != "carol" {
		t.Fatalf("the instance under its gate: %+v, %v", v, err)
	}
	if v.ToolName != "pay" || string(v.Args) != "120" {
		t.Errorf("the instance's call is %q(%s), want the node's pay(120)", v.ToolName, v.Args)
	}
	// Its approver does not count under the node's own gate, and the node's does not count under the
	// instance's: each is recounted under its own subject.
	if v, _ := audit.VerifyApprovals(acts, inst, g.policy, g.resolver(), edV(g.logPub)); v.OK {
		t.Fatalf("the instance recounted under the node's own gate verified: %+v", v)
	}
	nodeActs, err := audit.ApprovalEvidence(ctx, g.store, flowRun, "node:pay", sth)
	if err != nil {
		t.Fatalf("ApprovalEvidence(node): %v", err)
	}
	if v, _ := audit.VerifyApprovals(nodeActs, "node:pay", *esc, g.resolver(), edV(g.logPub)); v.OK {
		t.Fatalf("the node recounted under the instance's gate verified: %+v", v)
	}
}
