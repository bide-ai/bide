package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func evidenceFor(t *testing.T, g *gate) []audit.EvidenceAction {
	t.Helper()
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatalf("ApprovalEvidence: %v", err)
	}
	return acts
}

func without(acts []audit.EvidenceAction, drop func(audit.EvidenceAction) bool) []audit.EvidenceAction {
	var out []audit.EvidenceAction
	for _, a := range acts {
		if !drop(a) {
			out = append(out, a)
		}
	}
	return out
}

// Leaving out a decision the gate read is detected, even when enough approvals remain.
func TestVerifyApprovals_DetectsOmission(t *testing.T) {
	g := passedGate(t)
	acts := without(evidenceFor(t, g), func(a audit.EvidenceAction) bool {
		return a.Kind == audit.KindApproval && a.Label == "mallory"
	})
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), g.logPub)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || !hasProblem(v, "omits") {
		t.Fatalf("verdict = %+v, want not OK with an omission problem", v)
	}
}

// Hiding an approver's earlier valid denial is detected: the gate read it, so the evidence
// must carry it, and the recount would otherwise count the later approval.
func TestVerifyApprovals_DetectsHiddenDenial(t *testing.T) {
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	wantPaused(t, g.run(), 0)
	g.decide(t, "carol", false, false) // carol's counted decision: a denial
	g.decide(t, "carol", true, false)  // superseded
	g.decide(t, "alice", true, false)
	g.decide(t, "bob", true, false)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	acts := without(evidenceFor(t, g), func(a audit.EvidenceAction) bool {
		return a.Kind == audit.KindApproval && a.Label == "carol" && !a.Bundle.Record.Approved
	})
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), g.logPub)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || !hasProblem(v, "omits") || !hasProblem(v, "disagrees") {
		t.Fatalf("verdict = %+v, want the omitted denial and the resulting recount mismatch reported", v)
	}
}

// The auditor's expected policy is checked against what the gate recorded enforcing.
func TestVerifyApprovals_DetectsPolicyMismatch(t *testing.T) {
	g := passedGate(t)
	acts := evidenceFor(t, g)
	for name, expect := range map[string]agent.ApprovalPolicy{
		"stricter need":   {Need: 3, Approvers: g.policy.Approvers},
		"weaker need":     {Need: 1, Approvers: g.policy.Approvers},
		"other approvers": {Need: 2, Approvers: []string{"alice", "bob", "dave"}},
	} {
		v, err := audit.VerifyApprovals(acts, "c1", expect, g.resolver(), g.logPub)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.OK || !hasProblem(v, "enforced") {
			t.Fatalf("%s: verdict = %+v, want a policy mismatch problem", name, v)
		}
	}
}

// If the auditor's keys do not reproduce the gate's count, that is a problem, not a pass.
func TestVerifyApprovals_DetectsRecountMismatch(t *testing.T) {
	g := passedGate(t)
	delete(g.pubs, "bob") // the auditor has no key for bob
	v, err := audit.VerifyApprovals(evidenceFor(t, g), "c1", g.policy, g.resolver(), g.logPub)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || !hasProblem(v, "disagrees") || ignoredReason(v, "bob") != agent.ReasonNoKey {
		t.Fatalf("verdict = %+v, want bob unverifiable and the recount mismatch reported", v)
	}
}

// A decision recorded after the gate decided is shown as ignored and changes nothing.
func TestVerifyApprovals_LateDecisionIgnored(t *testing.T) {
	ctx := context.Background()
	g := passedGate(t)
	g.decide(t, "carol", true, false) // after the gate passed
	sth := g.sth(t, 1700000000)
	acts, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "c1", sth)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := g.store.History(ctx, gateRun)
	late := recs[len(recs)-1]
	pb, err := audit.ProveApproval(ctx, g.store, gateRun, late.Name, sth)
	if err != nil {
		t.Fatal(err)
	}
	acts = append(acts, audit.EvidenceAction{Label: "carol", Kind: audit.KindApproval, Ref: late.Name, Bundle: pb})
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), g.logPub)
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK || !slices.Equal(v.Counted, []string{"alice", "bob"}) {
		t.Fatalf("verdict = %+v, want the late decision to change nothing", v)
	}
	found := false
	for _, d := range v.Ignored {
		found = found || (d.Step == late.Name && d.Reason == "not read by the gate (recorded after it decided)")
	}
	if !found {
		t.Fatalf("ignored = %+v, want the late decision listed as not read by the gate", v.Ignored)
	}
}

// A denied gate yields consistent evidence that does not pass: the verdict reports who denied.
func TestVerifyApprovals_DeniedGate(t *testing.T) {
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	wantPaused(t, g.run(), 0)
	g.decide(t, "alice", false, false)
	g.decide(t, "bob", false, false)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	if g.charged != 0 {
		t.Fatalf("denied gate charged %d times", g.charged)
	}
	v, err := audit.VerifyApprovals(evidenceFor(t, g), "c1", g.policy, g.resolver(), g.logPub)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || len(v.Problems) != 0 || !slices.Equal(v.DeniedBy, []string{"alice", "bob"}) {
		t.Fatalf("verdict = %+v, want consistent evidence of a denial by alice and bob", v)
	}
}

// Tampering with a disclosed decision breaks its proof, which is reported; tampering with the
// request, tally, or result, or mixing tree heads, makes the evidence unevaluable.
func TestVerifyApprovals_Tampering(t *testing.T) {
	g := passedGate(t)

	acts := evidenceFor(t, g)
	for i := range acts {
		if acts[i].Kind == audit.KindApproval && acts[i].Label == "alice" {
			acts[i].Bundle.Record.Approved = false
		}
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), g.logPub)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || !hasProblem(v, "omits, or cannot prove") {
		t.Fatalf("tampered decision: verdict = %+v, want a proof problem", v)
	}

	for _, kind := range []string{audit.KindCall, audit.KindApprovalTally, audit.KindTool} {
		acts := evidenceFor(t, g)
		for i := range acts {
			if acts[i].Kind == kind {
				switch kind {
				case audit.KindCall:
					acts[i].Bundle.Record.Message.Parts = []agent.Part{agent.ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{"amount":999}`)}}
				default:
					acts[i].Bundle.Record.Result = json.RawMessage(`"edited"`)
				}
			}
		}
		if _, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), g.logPub); err == nil {
			t.Fatalf("tampered %s: VerifyApprovals returned a verdict, want an error", kind)
		}
	}

	mixed := evidenceFor(t, g)
	other, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000099))
	if err != nil {
		t.Fatal(err)
	}
	for i := range mixed {
		if mixed[i].Kind == audit.KindApprovalTally {
			mixed[i] = other[i]
		}
	}
	if _, err := audit.VerifyApprovals(mixed, "c1", g.policy, g.resolver(), g.logPub); err == nil {
		t.Fatal("tally under a different tree head: VerifyApprovals returned a verdict, want an error")
	}
}

// Evidence that cannot be evaluated at all is an error.
func TestVerifyApprovals_Errors(t *testing.T) {
	g := passedGate(t)
	acts := evidenceFor(t, g)
	wrongLog, _, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]func() error{
		"wrong log key": func() error {
			_, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), wrongLog)
			return err
		},
		"unknown call": func() error {
			_, err := audit.VerifyApprovals(acts, "nope", g.policy, g.resolver(), g.logPub)
			return err
		},
		"no request": func() error {
			_, err := audit.VerifyApprovals(without(acts, func(a audit.EvidenceAction) bool { return a.Kind == audit.KindCall }), "c1", g.policy, g.resolver(), g.logPub)
			return err
		},
		"no tally": func() error {
			_, err := audit.VerifyApprovals(without(acts, func(a audit.EvidenceAction) bool { return a.Kind == audit.KindApprovalTally }), "c1", g.policy, g.resolver(), g.logPub)
			return err
		},
		"invalid policy": func() error {
			_, err := audit.VerifyApprovals(acts, "c1", agent.ApprovalPolicy{Need: 4, Approvers: g.policy.Approvers}, g.resolver(), g.logPub)
			return err
		},
		"nil resolver": func() error {
			_, err := audit.VerifyApprovals(acts, "c1", g.policy, nil, g.logPub)
			return err
		},
	}
	for name, f := range cases {
		if f() == nil {
			t.Errorf("%s: err = nil, want an error", name)
		}
	}
}
