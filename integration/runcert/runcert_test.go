package runcert_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// buildKYC returns a small convergent policy (an approval reverted when the case is flagged), its
// digest, serialized bytes, and portable convergence certificate bytes. It is the same shape the
// compliance example and the CLI convergence test use. A distinct varName produces a structurally
// distinct policy (hence a distinct digest, which is over the machine's semantics, not variable
// names), so a test can exercise two different used policies. Pass extra=true to add another event
// and variable, which changes the state space and thus the digest.
func buildKYC(t *testing.T, name string, extra bool) (digest string, policyBytes, certBytes []byte) {
	t.Helper()
	r := gsm.NewRegistry(name)
	approved := r.Bool("approved")
	flag := r.Bool("flagged")
	r.Rule("no_approve_when_flagged").
		Require(gsm.Or(gsm.Is(approved, 0), gsm.Is(flag, 0))).
		RepairWith(gsm.SetTo(approved, 0)).
		Add()
	r.On("flag").Does(gsm.SetTo(flag, 1)).Add()
	r.On("approve").Does(gsm.SetTo(approved, 1)).Add()
	if extra {
		hold := r.Bool("hold")
		r.On("hold").Does(gsm.SetTo(hold, 1)).Add()
	}
	_, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	digest, _ = r.PolicyDigest()
	policyBytes, _ = r.PolicyBytes()
	certBytes, _ = govern.CertifyConvergence(rep, digest).Marshal()
	return digest, policyBytes, certBytes
}

// anchorGovernedRun anchors a policy + its convergence certificate and records one governed action
// leaf under that policy (a StepToolResult whose result carries policy_digest, as
// govern.AttestedEventTool journals), so PolicyUsedKey picks it up as an exercised policy.
func anchorGovernedRun(t *testing.T, ctx context.Context, store agent.Durable, runID, digest string, policyBytes, certBytes []byte) {
	t.Helper()
	if _, err := audit.RecordPolicy(ctx, store, runID, policyBytes, digest); err != nil {
		t.Fatalf("RecordPolicy: %v", err)
	}
	if _, err := audit.RecordConvergence(ctx, store, runID, certBytes, digest); err != nil {
		t.Fatalf("RecordConvergence: %v", err)
	}
	// A governed-action leaf: a completed tool call whose result embeds the policy digest.
	_, err := store.Do(ctx, runID, "action:"+digest, func(context.Context) (agent.Record, error) {
		return agent.Record{
			Kind:      agent.StepToolResult,
			ToolUseID: "call_" + digest[:8],
			Result:    []byte(`{"event":"approve","applied":true,"policy_digest":"` + digest + `","state_digest":"abc"}`),
		}, nil
	})
	if err != nil {
		t.Fatalf("record governed action: %v", err)
	}
}

// TestCertifyAndVerifyRun is the happy path: a run with a governed action under an approved,
// convergence-certified policy produces a certificate that verifies offline under the public key.
func TestCertifyAndVerifyRun(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run-ok"

	digest, policyBytes, certBytes := buildKYC(t, "kyc-decision", false)
	anchorGovernedRun(t, ctx, store, runID, digest, policyBytes, certBytes)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth := audit.SignTreeHead(th, priv)

	cert, err := audit.CertifyRun(ctx, store, runID, sth, audit.RunCertSpec{ApprovedPolicies: []string{digest}}, priv, 2)
	if err != nil {
		t.Fatalf("CertifyRun: %v", err)
	}
	if len(cert.UsedPolicies) != 1 || cert.UsedPolicies[0] != digest {
		t.Fatalf("used policies = %v, want [%s]", cert.UsedPolicies, digest)
	}

	res, err := audit.VerifyRun(cert, []string{digest}, pub)
	if err != nil {
		t.Fatalf("VerifyRun: %v", err)
	}
	if !res.OK {
		t.Fatalf("run certificate should verify, got %+v", res)
	}
	if !res.OnlyApprovedPolicies || !res.ConvergenceCertified {
		t.Fatalf("both properties should hold, got %+v", res)
	}

	// Anchoring and proving the certificate itself.
	if _, err := audit.RecordRunCertificate(ctx, store, runID, cert); err != nil {
		t.Fatalf("RecordRunCertificate: %v", err)
	}
	th2, _ := audit.NewTreeHead(ctx, store, runID, 3)
	sth2 := audit.SignTreeHead(th2, priv)
	pb, err := audit.ProveRunCertificate(ctx, store, runID, sth2)
	if err != nil {
		t.Fatalf("ProveRunCertificate: %v", err)
	}
	if ok, err := pb.Verify(pub); err != nil || !ok {
		t.Fatalf("run certificate leaf should prove in the later tree (ok=%v err=%v)", ok, err)
	}
}

// TestCertifyRunRejectsDisallowedPolicy: a run that exercised a policy NOT in the approved allowlist
// cannot even be certified (CertifyRun fails), and a certificate whose used set is not a subset of a
// verifier's allowlist fails VerifyRun.
func TestCertifyRunRejectsDisallowedPolicy(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run-bad"

	digest, policyBytes, certBytes := buildKYC(t, "kyc-decision", false)
	anchorGovernedRun(t, ctx, store, runID, digest, policyBytes, certBytes)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, _ := audit.NewTreeHead(ctx, store, runID, 1)
	sth := audit.SignTreeHead(th, priv)

	// CertifyRun refuses when the used policy is not approved.
	if _, err := audit.CertifyRun(ctx, store, runID, sth, audit.RunCertSpec{ApprovedPolicies: []string{"some-other-digest"}}, priv, 2); err == nil {
		t.Fatalf("CertifyRun must refuse a run that used a disallowed policy")
	}

	// A well-formed certificate whose allowlist is then narrowed (as a strict verifier would) fails
	// VerifyRun on only-approved-policies.
	pub, priv2, _ := ed25519.GenerateKey(rand.Reader)
	th2, _ := audit.NewTreeHead(ctx, store, runID, 1)
	sth2 := audit.SignTreeHead(th2, priv2)
	cert, err := audit.CertifyRun(ctx, store, runID, sth2, audit.RunCertSpec{ApprovedPolicies: []string{digest}}, priv2, 2)
	if err != nil {
		t.Fatalf("CertifyRun (approved): %v", err)
	}
	// The verifier's allowlist excludes the used one.
	res, err := audit.VerifyRun(cert, []string{"only-this-other-policy"}, pub)
	if err != nil {
		t.Fatalf("VerifyRun: %v", err)
	}
	if res.OK || res.OnlyApprovedPolicies {
		t.Fatalf("certificate must fail only-approved-policies when the used policy is not allowed, got %+v", res)
	}
}

// TestVerifyRunDetectsHiddenPolicy: the completeness teeth. If a producer drops a used policy from
// UsedPolicies (to sneak it past the allowlist), the recomputed absence root no longer matches the
// signed root, so VerifyRun fails only-approved-policies.
func TestVerifyRunDetectsHiddenPolicy(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run-two"

	dA, pbA, cbA := buildKYC(t, "kyc-A", false)
	dB, pbB, cbB := buildKYC(t, "kyc-B", true)
	anchorGovernedRun(t, ctx, store, runID, dA, pbA, cbA)
	anchorGovernedRun(t, ctx, store, runID, dB, pbB, cbB)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, _ := audit.NewTreeHead(ctx, store, runID, 1)
	sth := audit.SignTreeHead(th, priv)

	cert, err := audit.CertifyRun(ctx, store, runID, sth, audit.RunCertSpec{ApprovedPolicies: []string{dA, dB}}, priv, 2)
	if err != nil {
		t.Fatalf("CertifyRun: %v", err)
	}
	if len(cert.UsedPolicies) != 2 {
		t.Fatalf("expected 2 used policies, got %v", cert.UsedPolicies)
	}

	// Tamper: drop one used policy from the disclosed set (leaving the signed absence STH intact).
	tampered := cert
	tampered.UsedPolicies = []string{cert.UsedPolicies[0]}
	res, err := audit.VerifyRun(tampered, []string{dA, dB}, pub)
	if err != nil {
		t.Fatalf("VerifyRun: %v", err)
	}
	if res.OnlyApprovedPolicies {
		t.Fatalf("dropping a used policy must break the absence-root binding, but only-approved-policies held")
	}
}
