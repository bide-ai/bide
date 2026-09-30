package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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

// plainRun journals n value steps (no tool calls, so no policies) in runID and returns the records
// and the journal head.
func plainRun(t *testing.T, runID string, vals ...string) ([]agent.Record, audit.TreeHead) {
	t.Helper()
	ctx := context.Background()
	s := agent.NewMemStore()
	for i, v := range vals {
		v := v
		if _, err := s.Do(ctx, runID, "s"+string(rune('a'+i)), func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"` + v + `"`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := s.History(ctx, runID)
	th, err := audit.NewTreeHead(ctx, s, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return recs, th
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

// TestVerifyRun_UsedPolicyHeadIsBoundToRunAndJournal: a used-policy head signed by the right key
// still fails unless it is the used-policy set of this run's certified journal tree.
func TestVerifyRun_UsedPolicyHeadIsBoundToRunAndJournal(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	// Runs X and Y have byte-identical journals, so identical roots; only the run ID tells them apart.
	recsX, thX := plainRun(t, "X", "a")
	recsY, thY := plainRun(t, "Y", "a")
	absX, err := audit.SignAbsenceRoot(recsX, audit.PolicyUsedKeys, thX, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	absY, err := audit.SignAbsenceRoot(recsY, audit.PolicyUsedKeys, thY, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	props := []string{"only-approved-policies", "policies-convergence-certified"}
	genuine := audit.RunCertificate{Format: audit.RunCertificateFormat, RunID: "X", Properties: props, UsedPolicies: []string{}, UsedPolicyAbsence: absX, STH: audit.SignTreeHead(thX, priv)}
	if res, _ := audit.VerifyRun(genuine, nil, pub); !res.OK {
		t.Fatalf("the genuine certificate of a run that used no policy does not verify: %+v", res)
	}

	c := genuine
	c.UsedPolicyAbsence = absY
	mustFailRun(t, "run Y's used-policy head for run X", c, nil, pub)

	c = genuine
	c.STH = audit.SignTreeHead(thY, priv)
	mustFailRun(t, "run Y's journal head for run X", c, nil, pub)
	// The used-policy set is bound to the journal head, so it is not established either.
	if res, _ := audit.VerifyRun(c, nil, pub); res.OnlyApprovedPolicies {
		t.Fatal("only-approved-policies held against another run's journal head")
	}

	// The tool-use set of a run with no tool calls is empty too, but it is not the used-policy set.
	toolX, err := audit.SignAbsenceRoot(recsX, audit.ToolUseKeys, thX, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	c = genuine
	c.UsedPolicyAbsence = toolX
	mustFailRun(t, "run X's tool-use head as its used-policy head", c, nil, pub)

	// A different history of run X of the same length: the used-policy head names its journal root.
	recsX2, thX2 := plainRun(t, "X", "b")
	absX2, err := audit.SignAbsenceRoot(recsX2, audit.PolicyUsedKeys, thX2, priv, 2)
	if err != nil {
		t.Fatal(err)
	}
	c = genuine
	c.UsedPolicyAbsence = absX2
	mustFailRun(t, "a used-policy head of another history of run X", c, nil, pub)
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
