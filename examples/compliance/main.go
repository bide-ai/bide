// Command compliance is a worked example of the accountability stack on a KYC-shaped flow:
//
//  1. run independent compliance checks in PARALLEL, each a durable, independently provable step;
//  2. reach a GOVERNED decision whose compliance rule ("do not approve a flagged case") is
//     enforced by compensation, so the outcome is the same regardless of the order events arrive;
//  3. PROVE the whole run offline against a signed tree head.
//
// There is no LLM or network here: the checks are stubs standing in for external screening calls,
// so the example runs as-is and exercises the durability + governance + audit machinery. In a real
// agent the checks would be tool calls or sub-agents and the governed events would be applied
// through govern.AttestedEventTool (which journals each one); the guarantees are identical.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	agent "github.com/blackwell-systems/bide"
	"github.com/blackwell-systems/bide/audit"
	"github.com/blackwell-systems/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

type checkResult struct {
	Name string `json:"name"`
	Pass bool   `json:"pass"`
}

type decision struct {
	Approved     bool   `json:"approved"`
	Flagged      bool   `json:"flagged"`
	PolicyDigest string `json:"policy_digest"`
	StateDigest  string `json:"state_digest"`
}

func main() {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "kyc-applicant-42"

	// 1) Parallel, durable, independently-provable compliance checks. adverse_media flags.
	checks := []agent.Task[checkResult]{
		{Name: "sanctions_check", Fn: func(context.Context) (checkResult, error) { return checkResult{"sanctions", true}, nil }},
		{Name: "pep_check", Fn: func(context.Context) (checkResult, error) { return checkResult{"pep", true}, nil }},
		{Name: "adverse_media_check", Fn: func(context.Context) (checkResult, error) { return checkResult{"adverse_media", false}, nil }},
	}
	results, err := agent.Parallel(ctx, store, runID, 0, checks...)
	if err != nil {
		panic(err)
	}
	flagged := false
	fmt.Println("compliance checks (parallel, durable, each independently provable):")
	for _, r := range results {
		fmt.Printf("  %-20s pass=%v\n", r.Name, r.Pass)
		if !r.Pass {
			flagged = true
		}
	}

	// 2) Governed decision. The compliance rule is an invariant: approved implies not flagged.
	// The compensation reverts an approval on a flagged case, so applying {flag, approve} in ANY
	// order converges to the same compliant outcome (order-independence is the gsm guarantee).
	r := gsm.NewRegistry("kyc-decision")
	approved := r.Bool("approved")
	flag := r.Bool("flagged")
	r.Rule("no_approve_when_flagged").
		Require(gsm.Or(gsm.Is(approved, 0), gsm.Is(flag, 0))). // not approved, or not flagged
		RepairWith(gsm.SetTo(approved, 0)).                    // compensate an invalid approval
		Add()
	r.On("flag").Does(gsm.SetTo(flag, 1)).Add()
	r.On("approve").Does(gsm.SetTo(approved, 1)).Add()

	m, rep, err := r.Build() // Build succeeding is the proof the rule converges (WFC + CC)
	if err != nil {
		panic(fmt.Sprintf("build: %v\n%s", err, rep))
	}
	policyDigest, _ := r.PolicyDigest()
	policyBytes, _ := r.PolicyBytes()

	// Carry gsm's build-time convergence proof across as a portable certificate, and anchor both
	// the policy and the certificate as journal leaves so a verifier can later prove, from the
	// signed tree alone, that the anchored policy was certified convergent (not merely used).
	cert := govern.CertifyConvergence(rep, policyDigest)
	fmt.Printf("\n%s\n", cert.String())
	if _, err := audit.RecordPolicy(ctx, store, runID, policyBytes, policyDigest); err != nil {
		panic(err)
	}
	certBytes, _ := cert.Marshal()
	if _, err := audit.RecordConvergence(ctx, store, runID, certBytes, policyDigest); err != nil {
		panic(err)
	}

	gov := govern.New(m, m.NewState())
	// The case is submitted for approval; a failed check flags it. Order does not matter.
	gov.Apply(ctx, "approve")
	if flagged {
		gov.Apply(ctx, "flag")
	}
	st := gov.State()

	// Record the decision as a durable, provable step binding the outcome, the policy, and the
	// resulting governed state.
	dec, err := agent.Step(ctx, store, runID, "decision", func(context.Context) (decision, error) {
		return decision{
			Approved:     st.GetBool(approved),
			Flagged:      st.GetBool(flag),
			PolicyDigest: policyDigest,
			StateDigest:  st.Digest(),
		}, nil
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("\ngoverned decision: approved=%v flagged=%v\n", dec.Approved, dec.Flagged)
	fmt.Printf("  under policy %s\n", dec.PolicyDigest[:16]+"...")
	fmt.Printf("  the compliance rule (no approval when flagged) held by compensation, not by luck of order\n")

	// 3) Prove the whole run offline: sign a tree head, then prove each stage against it. A
	// verifier checks these with only the public key, trusting neither this process nor its store.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		panic(err)
	}
	sth := audit.SignTreeHead(th, priv)

	fmt.Println("\noffline proofs (verify with the public key alone):")
	for _, name := range []string{"sanctions_check", "pep_check", "adverse_media_check", "decision"} {
		b, err := audit.ProveStep(ctx, store, runID, name, sth)
		if err != nil {
			panic(err)
		}
		ok, err := b.Verify(pub)
		if err != nil {
			panic(err)
		}
		fmt.Printf("  %-20s inclusion proof verified: %v\n", name, ok)
	}
	// The convergence certificate is provable from the same signed tree: a verifier confirms the
	// anchored policy was certified convergent, in the same committed tree as the actions.
	cb, err := audit.ProveConvergence(ctx, store, runID, policyDigest, sth)
	if err != nil {
		panic(err)
	}
	ok, err := cb.Verify(pub)
	if err != nil {
		panic(err)
	}
	fmt.Printf("  %-20s inclusion proof verified: %v\n", "convergence_cert", ok)

	fmt.Println("\nEvery check ran durably (at-most-once on resume), the decision was governed by a")
	fmt.Println("machine-checked-convergent policy, and every stage is provable to a third party.")
}
