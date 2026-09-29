package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// resolver maps the run's registered approver keys (eligible or not) to verifiers, the same
// shape the gate uses at run time.
func (k *kofnRun) resolver() agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) {
		pub, ok := k.approvers[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	}
}

// everyone includes mallory, so her (ineligible) decision is in the evidence and must be
// reported rather than silently dropped.
var everyone = []string{"alice", "bob", "carol", "mallory"}

func reasons(v audit.ApprovalVerdict) map[string]string {
	out := map[string]string{}
	for _, d := range v.Ignored {
		out[d.Approver] = d.Reason
	}
	return out
}

// VerifyApprovals counts exactly the eligible, correctly signed approvals that precede the
// action, and says why every other decision did not count.
func TestVerifyApprovals_CountsAndExplains(t *testing.T) {
	k := buildKofnRun(t)
	acts, err := audit.ApprovalEvidence(context.Background(), k.store, "run-kofn", "c1", everyone, k.sth)
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts, "c1", *k.policy, k.resolver(), k.logPub)
	if err != nil {
		t.Fatalf("VerifyApprovals: %v", err)
	}
	if !v.OK || v.Need != 2 || !reflect.DeepEqual(v.Counted, []string{"alice", "bob"}) {
		t.Fatalf("verdict = %+v, want OK with alice and bob counted", v)
	}
	want := map[string]string{
		"carol":   "signature does not verify under the approver's key",
		"mallory": "not an eligible approver",
	}
	if got := reasons(v); !reflect.DeepEqual(got, want) {
		t.Fatalf("ignored = %v, want %v", got, want)
	}
}

// It works on a whole EvidencePackage (action first, decisions appended after), because it
// locates the action and decisions by kind and tool-use id rather than by position.
func TestVerifyApprovals_OnEvidencePackage(t *testing.T) {
	ctx := context.Background()
	k := buildKofnRun(t)
	pkg, err := audit.Evidence(ctx, k.store, "run-kofn", k.logPriv, 1700000001, audit.WithToolCall("c1"))
	if err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(ctx, k.store, "run-kofn", "c1", k.policy.Approvers, pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions = append(pkg.Actions, acts[:len(acts)-1]...) // the action is already packaged

	rep, err := pkg.Verify(k.logPub)
	if err != nil || !rep.OK {
		t.Fatalf("package Verify: ok=%v err=%v", rep.OK, err)
	}
	v, err := audit.VerifyApprovals(pkg.Actions, "c1", *k.policy, k.resolver(), k.logPub)
	if err != nil || !v.OK || !reflect.DeepEqual(v.Counted, []string{"alice", "bob"}) {
		t.Fatalf("verdict = %+v, err = %v, want alice and bob counted", v, err)
	}
}

// Below Need the verdict is not OK, and an unknown key counts nothing.
func TestVerifyApprovals_NotEnough(t *testing.T) {
	k := buildKofnRun(t)
	acts, err := audit.ApprovalEvidence(context.Background(), k.store, "run-kofn", "c1", everyone, k.sth)
	if err != nil {
		t.Fatal(err)
	}
	stricter := agent.ApprovalPolicy{Need: 3, Approvers: k.policy.Approvers}
	if v, err := audit.VerifyApprovals(acts, "c1", stricter, k.resolver(), k.logPub); err != nil || v.OK || len(v.Counted) != 2 {
		t.Fatalf("Need=3 verdict = %+v, err = %v, want not OK with 2 counted", v, err)
	}
	noKeys := func(string) (agent.ApproverVerifier, bool) { return nil, false }
	if v, err := audit.VerifyApprovals(acts, "c1", *k.policy, noKeys, k.logPub); err != nil || v.OK || len(v.Counted) != 0 {
		t.Fatalf("no-keys verdict = %+v, err = %v, want nothing counted", v, err)
	}
}

// A decision proven under a different signed tree head than the action does not count, even
// though its own proof is valid: the evidence must commit decisions and action together.
func TestVerifyApprovals_MixedTreeHeads(t *testing.T) {
	ctx := context.Background()
	k := buildKofnRun(t)
	acts, err := audit.ApprovalEvidence(ctx, k.store, "run-kofn", "c1", k.policy.Approvers, k.sth)
	if err != nil {
		t.Fatal(err)
	}
	th, err := audit.NewTreeHead(ctx, k.store, "run-kofn", 1700000099)
	if err != nil {
		t.Fatal(err)
	}
	other, err := audit.ProveApproval(ctx, k.store, "run-kofn", "c1", "bob", audit.SignTreeHead(th, k.logPriv))
	if err != nil {
		t.Fatal(err)
	}
	for i := range acts {
		if acts[i].Kind == "approval" && acts[i].Bundle.Record.Approver == "bob" {
			acts[i].Bundle = other
		}
	}
	v, err := audit.VerifyApprovals(acts, "c1", *k.policy, k.resolver(), k.logPub)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || reasons(v)["bob"] != "not proven under the action's signed tree head" {
		t.Fatalf("verdict = %+v, want bob rejected for a different tree head and not OK", v)
	}
}

// Evidence that cannot be evaluated is an error, not a verdict.
func TestVerifyApprovals_Errors(t *testing.T) {
	k := buildKofnRun(t)
	acts, err := audit.ApprovalEvidence(context.Background(), k.store, "run-kofn", "c1", k.policy.Approvers, k.sth)
	if err != nil {
		t.Fatal(err)
	}
	wrongLog, _, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]func() (audit.ApprovalVerdict, error){
		"wrong log key": func() (audit.ApprovalVerdict, error) {
			return audit.VerifyApprovals(acts, "c1", *k.policy, k.resolver(), wrongLog)
		},
		"unknown tool call": func() (audit.ApprovalVerdict, error) {
			return audit.VerifyApprovals(acts, "nope", *k.policy, k.resolver(), k.logPub)
		},
		"invalid policy": func() (audit.ApprovalVerdict, error) {
			return audit.VerifyApprovals(acts, "c1", agent.ApprovalPolicy{Need: 4, Approvers: k.policy.Approvers}, k.resolver(), k.logPub)
		},
		"nil resolver": func() (audit.ApprovalVerdict, error) {
			return audit.VerifyApprovals(acts, "c1", *k.policy, nil, k.logPub)
		},
	}
	for name, f := range cases {
		if _, err := f(); err == nil {
			t.Errorf("%s: err = nil, want an error", name)
		}
	}
}
