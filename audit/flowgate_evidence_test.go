package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
)

const flowRun = "flow-gate-run"

// flowGate drives a plan flow whose "pay" node is gated by g's policy: Need of the approvers
// sign (approve), and the node runs once.
func flowGate(t *testing.T, g *gate, approve ...string) {
	t.Helper()
	ctx := context.Background()
	b := plan.New[int, int]("refunds").WithApproverVerifiers(g.resolver())
	pay := b.Step("pay", func(_ context.Context, n int) (int, error) { g.charged++; return n, nil },
		plan.Approval(&g.policy))
	notify := b.Step("notify", func(_ context.Context, n int) (int, error) { return n, nil }, plan.ReadOnly())
	b.Edge(pay, notify)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Run(ctx, g.store, flowRun, 120)
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first drive = %v; want a pause at the gate", err)
	}
	for _, id := range approve {
		sig := ed25519.Sign(g.privs[id], agent.ApprovalDecisionBytes(pend.Subject(), id, true))
		if err := agent.SubmitDecision(ctx, g.store, agent.Decision{RunID: flowRun, ToolUseID: pend.ToolUseID, ApproverID: id, Approved: true, Alg: audit.AlgEd25519, Signature: sig},
			agent.WithDecisionCheck(g.resolver())); err != nil {
			t.Fatalf("SubmitDecision %s: %v", id, err)
		}
	}
	if _, err := f.Run(ctx, g.store, flowRun, 120); err != nil || g.charged != 1 {
		t.Fatalf("drive at quorum = %v, ran %d; want the node run once", err, g.charged)
	}
}

// A gated plan flow node's decisions verify offline exactly as a gated tool call's do: the
// evidence is the gate's request (the node and its input), the decisions, the tally, and the
// node's record, and VerifyApprovals recounts them against the policy the auditor expects. The
// evidence also verifies as an EvidencePackage.
func TestFlowGateEvidence_VerifiesOffline(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	flowGate(t, g, "alice", "carol")

	ts := time.Now().Add(-time.Hour).UnixNano()
	pkg, err := audit.Evidence(ctx, g.store, flowRun, edS(g.logPriv), ts)
	if err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, flowRun, "node:pay", pkg.STH)
	if err != nil {
		t.Fatalf("ApprovalEvidence: %v", err)
	}
	var kinds []audit.EvidenceKind
	for _, a := range acts {
		kinds = append(kinds, a.Kind)
	}
	want := []audit.EvidenceKind{audit.KindCall, audit.KindApproval, audit.KindApproval, audit.KindApprovalTally, audit.KindStep}
	if len(kinds) != len(want) {
		t.Fatalf("evidence kinds %v; want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("evidence kinds %v; want %v", kinds, want)
		}
	}
	v, err := audit.VerifyApprovals(acts, "node:pay", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK || v.ToolName != "pay" || string(v.Args) != "120" {
		t.Fatalf("VerifyApprovals = %+v, %v; want OK for pay(120)", v, err)
	}

	pkg.Actions = acts
	if err := pkg.Seal(edS(g.logPriv)); err != nil {
		t.Fatal(err)
	}
	if rep, err := pkg.Verify(edV(g.logPub)); err != nil || !rep.OK {
		t.Fatalf("the flow gate's evidence does not verify as a package: %+v (%v)", rep.Items, err)
	}

	// An omitted decision, or another expected policy, fails the verdict.
	if _, err := audit.VerifyApprovals(append(acts[:1:1], acts[2:]...), "node:pay", g.policy, g.resolver(), edV(g.logPub)); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("evidence without a decision: VerifyApprovals = %v; want ErrNotVerified", err)
	}
	strict := agent.ApprovalPolicy{Need: 3, Approvers: g.policy.Approvers}
	if _, err := audit.VerifyApprovals(acts, "node:pay", strict, g.resolver(), edV(g.logPub)); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("a 3-of-3 expected policy: VerifyApprovals = %v; want ErrNotVerified", err)
	}
	// Without the node's record (the "step" result) there is no proof the node ran after the tally,
	// and a later node's record does not stand in for it.
	if _, err := audit.VerifyApprovals(acts[:len(acts)-1], "node:pay", g.policy, g.resolver(), edV(g.logPub)); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("evidence without the node's record: VerifyApprovals = %v; want ErrNotVerified", err)
	}
	later, err := audit.ProveStep(ctx, g.store, flowRun, "node:notify", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	swapped := append(acts[:len(acts)-1:len(acts)-1], audit.EvidenceAction{Label: "node:notify", Kind: audit.KindStep, Ref: "node:notify", Bundle: later})
	if _, err := audit.VerifyApprovals(swapped, "node:pay", g.policy, g.resolver(), edV(g.logPub)); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("evidence with a later node's record as the result: VerifyApprovals = %v; want ErrNotVerified", err)
	}
}
