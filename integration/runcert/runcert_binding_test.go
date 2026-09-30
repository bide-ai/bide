package runcert_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// certRun anchors a run that used policies dA and dB, signs its journal head, and certifies it.
func certRun(t *testing.T, runID string) (agent.Durable, audit.RunCertificate, []string, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	dA, pbA, cbA := buildKYC(t, "kyc-A", false)
	dB, pbB, cbB := buildKYC(t, "kyc-B", true)
	anchorGovernedRun(t, ctx, store, runID, dA, pbA, cbA)
	anchorGovernedRun(t, ctx, store, runID, dB, pbB, cbB)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	approved := []string{dA, dB}
	cert, err := audit.CertifyRun(ctx, store, runID, audit.SignTreeHead(th, priv), audit.RunCertSpec{ApprovedPolicies: approved}, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := audit.VerifyRun(cert, approved, pub); err != nil || !res.OK {
		t.Fatalf("the genuine certificate does not verify: %+v %v", res, err)
	}
	return store, cert, approved, pub, priv
}

func mustFailRun(t *testing.T, what string, cert audit.RunCertificate, approved []string, pub ed25519.PublicKey) {
	t.Helper()
	res, err := audit.VerifyRun(cert, approved, pub)
	if err == nil && res.OK {
		t.Fatalf("%s: the certificate verifies", what)
	}
}

// TestVerifyRun_EveryBindingIsChecked: each field of a certificate is either verified or derived
// from verified data; changing any one of them fails the certificate.
func TestVerifyRun_EveryBindingIsChecked(t *testing.T) {
	_, cert, approved, pub, _ := certRun(t, "run-x")

	c := cert
	c.Properties = nil
	mustFailRun(t, "no properties", c, approved, pub)

	c = cert
	c.UsedPolicyAbsence.Signature = append([]byte(nil), cert.UsedPolicyAbsence.Signature...)
	c.UsedPolicyAbsence.Signature[0] ^= 1
	mustFailRun(t, "a forged used-policy signature", c, approved, pub)

	c = cert
	c.STH.Signature = append([]byte(nil), cert.STH.Signature...)
	c.STH.Signature[0] ^= 1
	mustFailRun(t, "a forged run STH signature", c, approved, pub)

	c = cert
	c.UsedPolicies = []string{cert.UsedPolicies[0], "not-" + cert.UsedPolicies[1]}
	mustFailRun(t, "a swapped-in digest", c, append(approved, "not-"+cert.UsedPolicies[1]), pub)

	c = cert
	c.UsedPolicies = []string{cert.UsedPolicies[1], cert.UsedPolicies[0]}
	mustFailRun(t, "the used set out of order", c, approved, pub)

	c = cert
	c.Convergence = append(append([]audit.PolicyConvergence(nil), cert.Convergence...), cert.Convergence[0])
	mustFailRun(t, "an extra convergence entry", c, approved, pub)

	c = cert
	c.Convergence = []audit.PolicyConvergence{cert.Convergence[1], cert.Convergence[1]}
	mustFailRun(t, "the second policy's evidence standing in for the first's", c, approved, pub)

	// A policy leaf and a convergence leaf decode as each other's content (both carry "digest"), so
	// the bundles must be the named leaves, not any record with the right digest.
	c = cert
	c.Convergence = append([]audit.PolicyConvergence(nil), cert.Convergence...)
	c.Convergence[0].PolicyLeaf = cert.Convergence[0].Certificate
	mustFailRun(t, "the convergence leaf presented as the policy leaf", c, approved, pub)
	c.Convergence[0] = cert.Convergence[0]
	c.Convergence[0].Certificate = cert.Convergence[0].PolicyLeaf
	mustFailRun(t, "the policy leaf presented as the convergence leaf", c, approved, pub)
}

// TestCertifyRun_CoversTheSignedPrefix: a run certificate is built for exactly the records its
// journal head commits to, not for whatever the journal holds when it is assembled.
func TestCertifyRun_CoversTheSignedPrefix(t *testing.T) {
	ctx := context.Background()
	store, cert, approved, pub, priv := certRun(t, "run-p")
	// After the head was signed, the run used a policy that is not approved.
	dC, pbC, cbC := buildKYC(t, "kyc-C", false)
	anchorGovernedRun(t, ctx, store, "run-p", dC+"x", pbC, cbC)
	again, err := audit.CertifyRun(ctx, store, "run-p", cert.STH, audit.RunCertSpec{ApprovedPolicies: approved}, priv, 3)
	if err != nil {
		t.Fatalf("certifying the signed prefix failed: %v", err)
	}
	if res, _ := audit.VerifyRun(again, approved, pub); !res.OK || len(again.UsedPolicies) != 2 {
		t.Fatalf("the prefix certificate: used %v, %+v", again.UsedPolicies, res)
	}
}
