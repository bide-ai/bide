// Command proof-carrying-run is a worked example of a PROOF-CARRYING RUN: a run that ships a single,
// offline-checkable certificate asserting behavioral-property compliance over the whole run, checked
// against one signed tree head. It composes existing audit primitives (anchored policy and
// convergence leaves, the policy-used absence commitment, RFC 6962 inclusion proofs, a signed tree
// head) into one per-run certificate plus a verifier; there is no new cryptography.
//
// The example is offline: no LLM, no network. An agent with a scripted model
// (agent.NewScriptedModel) applies governed actions through attested govern.EventTools, each
// journaled by the run with the policy digest that admitted it; with a real model the governed
// guarantees are identical. It:
//
//  1. anchors a convergent policy and its convergence certificate, runs governed actions under it;
//  2. emits a RunCertificate asserting only-approved-policies and policies-convergence-certified;
//  3. verifies the certificate offline against the public key alone (PASS). The log key is a
//     hybrid ed25519 + ML-DSA-65 key (audit.HybridSigner): the same code signs under any
//     audit.Signer, and the verifier is whatever audit.Verifier matches the key the auditor holds;
//     the ed25519 half of the hybrid key alone does not verify the certificate;
//  4. shows the FAIL path: with the SAME certificate but a narrower approved allowlist (as if a
//     disallowed policy had been used), only-approved-policies fails and the completeness commitment
//     has teeth: dropping the used policy from the disclosed set breaks the signed absence root too.
//
// What the certificate proves, precisely: properties of the GOVERNED, COMMITTED boundary (which
// policies ran, that each is approved and has an anchored, oracle-checkable convergence certificate).
// It does NOT prove the model's judgment was correct or close the runtime-refinement gap.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

func main() {
	ctx := context.Background()
	store, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	const runID = "kyc-applicant-42"

	// A convergent compliance policy: an approval is reverted when the case is flagged, so applying
	// {approve, flag} in ANY order converges to the same compliant outcome. Build succeeding is gsm's
	// certificate that it converges (WFC + CC over the enumerated state space).
	r := gsm.NewRegistry("kyc-decision")
	approved := r.Bool("approved")
	flag := r.Bool("flagged")
	r.Rule("no_approve_when_flagged").
		Require(gsm.Or(gsm.Is(approved, 0), gsm.Is(flag, 0))).
		RepairWith(gsm.SetTo(approved, 0)).
		Add()
	r.On("flag").Does(gsm.SetTo(flag, 1)).Add()
	r.On("approve").Does(gsm.SetTo(approved, 1)).Add()
	m, rep, err := r.Build()
	if err != nil {
		panic(fmt.Sprintf("policy did not verify: %v\n%s", err, rep))
	}
	policyDigest, _ := r.PolicyDigest()
	policyBytes, _ := r.PolicyBytes()

	// Anchor the policy and its portable convergence certificate as journal leaves, so a verifier can
	// later prove from the signed tree alone that the anchored policy was certified convergent.
	cert := govern.CertifyConvergence(rep, policyDigest)
	if _, err := audit.RecordPolicy(ctx, store, runID, policyBytes, policyDigest); err != nil {
		panic(err)
	}
	certBytes, _ := cert.Marshal()
	if _, err := audit.RecordConvergence(ctx, store, runID, certBytes, policyDigest); err != nil {
		panic(err)
	}
	fmt.Printf("policy %s anchored and certified convergent over %d states\n\n", short(policyDigest), rep.StateCount)

	// Run governed actions through attested EventTools: each tool call applies an event to the shared
	// governed state and journals a leaf carrying the policy digest that admitted it. This is what
	// makes the run's policy-used set provable.
	gov := govern.New(m, m.NewState())
	approveTool := govern.EventTool(gov, govern.EventToolConfig{Name: "approve", Description: "approve the case", Event: "approve", PolicyDigest: policyDigest})
	flagTool := govern.EventTool(gov, govern.EventToolConfig{Name: "flag", Description: "flag the case", Event: "flag", PolicyDigest: policyDigest})

	//
	// The agent's model is scripted (no LLM): it calls approve, then flag, then answers. The run
	// journals each call as a tool-result leaf, the way it records every tool call.
	model := agent.NewScriptedModel(
		agent.ToolTurn("call_approve", "approve", `{}`),
		agent.ToolTurn("call_flag", "flag", `{}`),
		agent.TextTurn("case decided"),
	)
	a, err := agent.New(model, store, agent.WithTools(approveTool, flagTool))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := a.Run(ctx, runID, agent.UserText("decide the KYC case")); err != nil {
		panic(err)
	}
	fmt.Println("governed actions (each journaled with the policy that admitted it):")
	for _, toolUseID := range []string{"call_approve", "call_flag"} {
		fmt.Printf("  %-12s applied\n", toolUseID)
	}
	st := gov.State()
	fmt.Printf("  final governed state: approved=%v flagged=%v (the invariant held by compensation)\n\n",
		st.GetBool(approved), st.GetBool(flag))

	// Sign one tree head over the whole run, then emit the RunCertificate against it. The certificate
	// recomputes the used-policy set from the committed leaves, confirms it is a subset of the
	// approved allowlist, signs the used-policy absence commitment, and assembles the anchored
	// policy + convergence proofs for each used policy.
	//
	// The log key is a hybrid of ed25519 and post-quantum ML-DSA-65: a signature holds only if both
	// halves verify, so the certificate stays sound as long as either scheme is unbroken. Any
	// audit.Signer works here (audit.Ed25519Signer on its own, audit.MLDSASigner, or this hybrid).
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	mlPriv, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		panic(err)
	}
	signer := audit.HybridSigner{Ed: audit.Ed25519Signer{Priv: edPriv}, ML: audit.MLDSASigner{Priv: mlPriv}}
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		panic(err)
	}
	sth, err := audit.SignTreeHead(th, signer)
	if err != nil {
		panic(err)
	}

	approvedAllowlist := []string{policyDigest}
	runCert, err := audit.CertifyRun(ctx, store, runID, sth, audit.RunCertSpec{ApprovedPolicies: approvedAllowlist, Signer: signer, TimestampNanos: 2})
	if err != nil {
		panic(err)
	}
	fmt.Printf("emitted RunCertificate for run %q\n", runCert.RunID)
	fmt.Printf("  properties asserted: %v\n", runCert.Properties)
	fmt.Printf("  policies used: %d, all in the approved allowlist\n", len(runCert.UsedPolicies))
	fmt.Printf("  signed tree size %d, used-policy set (bound to that tree) committed to %d distinct policies\n\n",
		runCert.STH.Size, runCert.UsedPolicyAbsence.Size)

	// Anchor the certificate itself so it is provable in the run, then verify offline. The verifier
	// trusts only the out-of-band public key: it re-derives every property from the disclosed proofs.
	if _, err := audit.RecordRunCertificate(ctx, store, runID, runCert); err != nil {
		panic(err)
	}
	// The auditor holds the log's public key out of band; here it is derived from the signer.
	logKey, err := audit.VerifierOf(signer)
	if err != nil {
		panic(err)
	}
	fmt.Printf("verifying offline with the %s public key alone (%d bytes, sha256 %s):\n", logKey.Alg(), len(logKey.PublicKey()), short(fingerprint(logKey.PublicKey())))
	res, err := audit.VerifyRun(runCert, approvedAllowlist, logKey)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  only-approved-policies:          %v\n", res.OnlyApprovedPolicies)
	fmt.Printf("  policies-convergence-certified:  %v\n", res.ConvergenceCertified)
	fmt.Printf("  => PASS: %v\n\n", res.OK)
	if !res.OK {
		panic("expected a correctly-produced certificate to verify")
	}

	// The ed25519 half of the hybrid key is not the log key: a certificate signed under the hybrid
	// scheme does not verify under ed25519 alone.
	edOnly := audit.Ed25519Verifier{Pub: edPriv.Public().(ed25519.PublicKey)}
	if _, err := audit.VerifyRun(runCert, approvedAllowlist, edOnly); !errors.Is(err, audit.ErrNotVerified) {
		panic(fmt.Sprintf("expected the ed25519 half alone not to verify the certificate, got %v", err))
	}
	fmt.Println("  the ed25519 half of the key alone does NOT verify it (the signed scheme is ed25519+ml-dsa-65)")
	fmt.Println()

	// FAIL path: a stricter auditor whose allowlist does NOT include the policy this run used. The
	// certificate's used set is still bound to the signed absence root (completeness), so it cannot be
	// quietly narrowed; only-approved-policies fails because a used policy is outside the allowlist.
	fmt.Println("now a stricter auditor whose allowlist EXCLUDES the policy this run used:")
	// A certificate that does not hold is an error wrapping audit.ErrNotVerified, with the verdict.
	failRes, err := audit.VerifyRun(runCert, []string{"a-different-approved-policy-digest"}, logKey)
	if !errors.Is(err, audit.ErrNotVerified) {
		panic(fmt.Sprintf("expected ErrNotVerified, got %v", err))
	}
	fmt.Printf("  only-approved-policies:          %v\n", failRes.OnlyApprovedPolicies)
	for _, reason := range failRes.Reasons {
		fmt.Printf("    reason: %s\n", reason)
	}
	fmt.Printf("  => PASS: %v (correctly rejected)\n\n", failRes.OK)
	if failRes.OK {
		panic("expected the certificate to fail under an allowlist that excludes the used policy")
	}

	// The completeness teeth: try to HIDE the used policy by dropping it from the disclosed set. The
	// recomputed absence root no longer matches the signed root, so it fails regardless of allowlist.
	fmt.Println("and a producer who tries to HIDE the used policy by dropping it from the disclosed set:")
	hidden := runCert
	hidden.UsedPolicies = nil
	hiddenRes, err := audit.VerifyRun(hidden, approvedAllowlist, logKey)
	if !errors.Is(err, audit.ErrNotVerified) {
		panic(fmt.Sprintf("expected ErrNotVerified, got %v", err))
	}
	fmt.Printf("  only-approved-policies:          %v (the signed used-policy root no longer matches)\n", hiddenRes.OnlyApprovedPolicies)
	fmt.Printf("  => PASS: %v (a used policy cannot be hidden)\n\n", hiddenRes.OK)
	if hiddenRes.OK {
		panic("expected hiding a used policy to break the absence-root binding")
	}

	fmt.Println("The run carries its own proof: which governed policies ran, that each is approved and")
	fmt.Println("has an anchored convergence certificate, checkable offline against one signed tree head.")
}

// fingerprint is the hex SHA-256 of a public key, a short name for it.
func fingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

func short(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "..."
}
