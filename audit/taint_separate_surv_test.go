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

// sepOpts shapes the run of sepRun.
type sepOpts struct {
	sep           bool // the instance's gate counts apart from the node's own gate
	aliceDecides  bool // alice, whom the node's own gate counted, also decides on the instance
	instanceFirst bool // the instance is counted before the node's own gate has an outcome
}

// sepRun is separated (taint_separate_test.go) with the order and the deciders chosen: a node whose own gate alice
// decided, and an instance of alice and carol. It returns the evidence of the instance and the signed head of its
// run, at a later time than the evidence's.
func sepRun(t *testing.T, o sepOpts) (*gate, []audit.EvidenceAction, agent.ApprovalPolicy, func(ts int64) audit.SignedTreeHead) {
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
	const inst = "node:pay:taint"
	esc := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "carol"}}
	run := esc
	if o.sep {
		run = esc.SeparatedFrom("node:pay")
	}
	instance := func() {
		t.Helper()
		if o.instanceFirst {
			// With no outcome of the node's own gate to separate from, nobody counts: the instance is denied outright.
			if ok, err := journalhook.Gate(ctx, g.store, flowRun, inst, "pay", args, &run, g.resolver()); err != nil || ok {
				t.Fatalf("the instance before the node's gate = %v, %v; want a denial", ok, err)
			}
			return
		}
		if _, err := journalhook.Gate(ctx, g.store, flowRun, inst, "pay", args, &run, g.resolver()); !errors.As(err, &pend) {
			t.Fatalf("the instance's first evaluation = %v, want a pause", err)
		}
		if o.aliceDecides {
			sign(pend, inst, "alice")
		}
		sign(pend, inst, "carol")
		if _, err := journalhook.Gate(ctx, g.store, flowRun, inst, "pay", args, &run, g.resolver()); err != nil {
			t.Fatalf("the instance's gate = %v", err)
		}
	}
	own := func() {
		t.Helper()
		sign(nodePend, "node:pay", "alice")
		if ok, err := journalhook.Gate(ctx, g.store, flowRun, "node:pay", "pay", args, &g.policy, g.resolver()); err != nil || !ok {
			t.Fatalf("the node's gate = %v, %v", ok, err)
		}
	}
	if o.instanceFirst {
		instance()
		own()
	} else {
		own()
		instance()
	}
	if !o.instanceFirst {
		if _, err := f.Run(ctx, g.store, flowRun, 120); err != nil || g.charged != 1 {
			t.Fatalf("drive at both quorums = %v, ran %d", err, g.charged)
		}
	} else if _, err := f.Run(ctx, g.store, flowRun, 120); err != nil {
		t.Fatalf("drive after the node's gate = %v", err)
	}
	sth := func(ts int64) audit.SignedTreeHead {
		th, err := audit.NewTreeHead(ctx, g.store, flowRun, ts)
		if err != nil {
			t.Fatal(err)
		}
		return signTH(t, th, g.logPriv)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, flowRun, inst, sth(1700000000))
	if err != nil {
		t.Fatalf("ApprovalEvidence(instance): %v", err)
	}
	return g, acts, esc, sth
}

func ownTallyAction(acts []audit.EvidenceAction) int {
	for i, a := range acts {
		if a.Kind == audit.KindApprovalTally && a.Ref == agent.ApprovalTallyStep("node:pay") {
			return i
		}
	}
	return -1
}

func hasProblemText(v audit.ApprovalVerdict, text string) bool {
	return strings.Contains(strings.Join(v.Problems, "\n"), text)
}

// A gate that separated, verified against a policy that does not, is refused for the exclusion it recorded, not
// only for the recount that follows from it.
func TestVerifyApprovals_ASeparatedGateUnderAPolicyThatDoesNotSeparate(t *testing.T) {
	g, acts, esc, _ := sepRun(t, sepOpts{sep: true, aliceDecides: true})
	v, err := audit.VerifyApprovals(acts, "node:pay:taint", esc, g.resolver(), edV(g.logPub))
	if err == nil || v.OK || !hasProblemText(v, "does not separate approvers") {
		t.Errorf("verdict %+v, %v; want the expected policy named as not separating", v, err)
	}
}

// A gate that did not exclude someone the policy excludes, though they did not decide: the recount's exclusion
// differs from the gate's journaled one alone (the approved and denied lists agree).
func TestVerifyApprovals_TheExcludedSetMustMatchTheTallys(t *testing.T) {
	g, acts, esc, _ := sepRun(t, sepOpts{sep: false, aliceDecides: false})
	if v, err := audit.VerifyApprovals(acts, "node:pay:taint", esc, g.resolver(), edV(g.logPub)); err != nil || !v.OK {
		t.Fatalf("unseparated under the unseparated policy: %+v, %v", v, err)
	}
	v, err := audit.VerifyApprovals(acts, "node:pay:taint", esc.SeparatedFrom("node:pay"), g.resolver(), edV(g.logPub))
	if err == nil || v.OK || !hasProblemText(v, "journaled tally names") {
		t.Errorf("verdict %+v, %v; want the excluded sets named as different", v, err)
	}
	if hasProblemText(v, "disagrees with the gate's journaled tally") {
		t.Errorf("the approved and denied lists should agree here: %+v", v.Problems)
	}
}

// The own gate's tally must itself be proven, under the same head and before the instance's tally.
func TestVerifyApprovals_TheOwnGatesTallyMustBeProvenInPlace(t *testing.T) {
	// Tampered: its proof does not verify.
	g, acts, esc, sth := sepRun(t, sepOpts{sep: true, aliceDecides: true})
	pol := esc.SeparatedFrom("node:pay")
	i := ownTallyAction(acts)
	if i < 0 {
		t.Fatal("no own gate tally among the evidence")
	}
	tampered := append([]audit.EvidenceAction(nil), acts...)
	editRec(t, &tampered[i].Bundle, func(r *agent.Record) { r.Result = json.RawMessage(`{"approved_by":["bob"]}`) })
	if v, err := audit.VerifyApprovals(tampered, "node:pay:taint", pol, g.resolver(), edV(g.logPub)); err == nil || v.OK || !hasProblemText(v, "own gate's tally does not verify") {
		t.Errorf("a tampered own gate tally: %+v, %v; want its proof refused", v, err)
	}
	// Under another head than the instance's.
	other, err := audit.ApprovalEvidence(context.Background(), g.store, flowRun, "node:pay:taint", sth(1700000099))
	if err != nil {
		t.Fatal(err)
	}
	mixed := append([]audit.EvidenceAction(nil), acts...)
	mixed[i] = other[ownTallyAction(other)]
	if v, err := audit.VerifyApprovals(mixed, "node:pay:taint", pol, g.resolver(), edV(g.logPub)); err == nil || v.OK || !hasProblemText(v, "same signed tree head") {
		t.Errorf("an own gate tally under another head: %+v, %v; want it refused", v, err)
	}
	// Recorded after the instance's own tally: the exclusion cannot rest on it.
	g2, acts2, esc2, _ := sepRun(t, sepOpts{sep: true, aliceDecides: true, instanceFirst: true})
	if v, err := audit.VerifyApprovals(acts2, "node:pay:taint", esc2.SeparatedFrom("node:pay"), g2.resolver(), edV(g2.logPub)); err == nil || v.OK || !hasProblemText(v, "not before the instance's tally") {
		t.Errorf("an own gate tally after the instance's: %+v, %v; want it refused", v, err)
	}
}
