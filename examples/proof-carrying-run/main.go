// Command proof-carrying-run is a worked example of a PROOF-CARRYING RUN: a run that ships a single,
// offline-checkable certificate asserting behavioral-property compliance over the whole run, checked
// against one signed tree head. It composes existing audit primitives (anchored policy and
// convergence leaves, the policy-used absence commitment, RFC 6962 inclusion proofs, a signed tree
// head) into one per-run certificate plus a verifier; there is no new cryptography.
//
// The example is offline: no LLM, no network. The "agent" applies governed actions through
// govern.AttestedEventTool (each journaled with the policy digest that admitted it), exactly as a
// real agent loop would; the governed guarantees are identical. It:
//
//  1. anchors a convergent policy and its convergence certificate, runs governed actions under it;
//  2. emits a RunCertificate asserting only-approved-policies and policies-convergence-certified;
//  3. verifies the certificate offline against the public key alone (PASS);
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
	"crypto/rand"
	"encoding/hex"
	"fmt"

	gsm "github.com/blackwell-systems/gsm"
	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/audit"
	"github.com/dayna/go-agents/govern"
)

func main() {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "kyc-applicant-42"

	// A convergent compliance policy: an approval is reverted when the case is flagged, so applying
	// {approve, flag} in ANY order converges to the same compliant outcome. Build succeeding IS the
	// proof it converges (WFC + CC over the enumerated state space).
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

	// Run governed actions through AttestedEventTool: each tool call applies an event to the shared
	// governed state and journals a leaf carrying the policy digest that admitted it. This is what
	// makes the run's policy-used set provable.
	gov := govern.New(m, m.NewState())
	approveTool := govern.AttestedEventTool(gov, "approve", "approve the case", "approve", policyDigest, agent.Safety{})
	flagTool := govern.AttestedEventTool(gov, "flag", "flag the case", "flag", policyDigest, agent.Safety{})

	fmt.Println("governed actions (each journaled with the policy that admitted it):")
	callGoverned(ctx, store, runID, "call_approve", approveTool)
	callGoverned(ctx, store, runID, "call_flag", flagTool)
	st := gov.State()
	fmt.Printf("  final governed state: approved=%v flagged=%v (the invariant held by compensation)\n\n",
		st.GetBool(approved), st.GetBool(flag))

	// Sign one tree head over the whole run, then emit the RunCertificate against it. The certificate
	// recomputes the used-policy set from the committed leaves, confirms it is a subset of the
	// approved allowlist, signs the used-policy absence commitment, and assembles the anchored
	// policy + convergence proofs for each used policy.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		panic(err)
	}
	sth := audit.SignTreeHead(th, priv)

	approvedAllowlist := []string{policyDigest}
	runCert, err := audit.CertifyRun(ctx, store, runID, sth, audit.RunCertSpec{ApprovedPolicies: approvedAllowlist}, priv, 2)
	if err != nil {
		panic(err)
	}
	fmt.Printf("emitted RunCertificate for run %q\n", runCert.RunID)
	fmt.Printf("  properties asserted: %v\n", runCert.Properties)
	fmt.Printf("  policies used: %d, all in the approved allowlist\n", len(runCert.UsedPolicies))
	fmt.Printf("  signed tree size %d, used-policy absence root committed to %d distinct policies\n\n",
		runCert.STH.Size, runCert.UsedPolicyAbsence.Size)

	// Anchor the certificate itself so it is provable in the run, then verify offline. The verifier
	// trusts only the out-of-band public key: it re-derives every property from the disclosed proofs.
	if _, err := audit.RecordRunCertificate(ctx, store, runID, runCert); err != nil {
		panic(err)
	}
	fmt.Printf("verifying offline with the public key alone (%s):\n", short(hex.EncodeToString(pub)))
	res, err := audit.VerifyRun(runCert, pub)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  only-approved-policies:          %v\n", res.OnlyApprovedPolicies)
	fmt.Printf("  policies-convergence-certified:  %v\n", res.ConvergenceCertified)
	fmt.Printf("  => PASS: %v\n\n", res.OK)
	if !res.OK {
		panic("expected a correctly-produced certificate to verify")
	}

	// FAIL path: a stricter auditor whose allowlist does NOT include the policy this run used. The
	// certificate's used set is still bound to the signed absence root (completeness), so it cannot be
	// quietly narrowed; only-approved-policies fails because a used policy is outside the allowlist.
	fmt.Println("now a stricter auditor whose allowlist EXCLUDES the policy this run used:")
	strict := runCert
	strict.ApprovedPolicies = []string{"a-different-approved-policy-digest"}
	failRes, err := audit.VerifyRun(strict, pub)
	if err != nil {
		panic(err)
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
	hiddenRes, err := audit.VerifyRun(hidden, pub)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  only-approved-policies:          %v (the signed absence root no longer matches)\n", hiddenRes.OnlyApprovedPolicies)
	fmt.Printf("  => PASS: %v (a used policy cannot be hidden)\n\n", hiddenRes.OK)
	if hiddenRes.OK {
		panic("expected hiding a used policy to break the absence-root binding")
	}

	fmt.Println("The run carries its own proof: which governed policies ran, that each is approved and")
	fmt.Println("has an anchored convergence certificate, checkable offline against one signed tree head.")
}

// callGoverned invokes a governed tool as a durable, journaled tool-result leaf, the way an agent
// loop records a tool call, so the action's policy digest lands in the run's committed history.
func callGoverned(ctx context.Context, store agent.Durable, runID, toolUseID string, tool agent.Tool) {
	out, err := tool.Call(ctx, nil)
	if err != nil {
		panic(err)
	}
	if _, err := store.Do(ctx, runID, toolUseID, func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: toolUseID, Result: out}, nil
	}); err != nil {
		panic(err)
	}
	fmt.Printf("  %-12s applied\n", toolUseID)
}

func short(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "..."
}
