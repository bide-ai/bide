// Command earned-authority shows authority that is earned from an agent's provable track record:
// it starts narrow, widens one rung after a clean streak (capped by a root ceiling), and resets to
// baseline the moment an anomaly is flagged. Two tiers cooperate, as designed:
//
//   - the controller (audit.EarnedAuthority) is a durable, sequential control loop: earning is
//     temporal and order-dependent, so it is not a convergent machine; it decides the current limit
//     and re-issues a signed grant that is a CHILD of the root grant, so the earned limit provably
//     never exceeds the root ceiling (VerifyDelegationChain);
//   - the enforcement is a convergent gsm invariant on the work machine (exposure <= limit), seeded
//     with whatever limit the controller currently grants.
//
// Offline, no LLM: the "compliant actions" and "anomaly" are driven directly so the loop is visible.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

func main() {
	ctx := context.Background()

	// The root principal authorizes the desk up to limit 10: the hard ceiling for anything earned.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := audit.Ed25519Signer{Priv: priv}
	verifier := func(string) (audit.Verifier, bool) { return audit.Ed25519Verifier{Pub: pub}, true }
	rootSG, err := audit.SignGrant(audit.Grant{ID: "root", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "10"}}, signer)
	if err != nil {
		panic(err)
	}

	// The work machine: exposure must not exceed the delegated limit. One proof over all limits.
	m, exposure, limit := buildWork()

	// The controller: baseline 2, then 5, then 10 (the ceiling), promoting every 3 compliant actions.
	ctrl, err := audit.NewEarnedAuthority([]int{2, 5, 10}, 3, rootSG, signer, "exec-agent")
	if err != nil {
		panic(err)
	}

	verify := func() string {
		ok, _ := audit.VerifyDelegationChain([]audit.SignedGrant{rootSG, ctrl.Grant()}, verifier, audit.AttenuatesNumericScope("limit"))
		return fmt.Sprintf("earned grant limit %d verifies within the root ceiling(10): %v", ctrl.Limit(), ok)
	}
	show := func(label string) {
		reached := tryBuys(ctx, m, exposure, limit, ctrl.Limit(), 8)
		fmt.Printf("%-32s limit=%-2d  agent tried $8M, governed to $%dM\n", label, ctrl.Limit(), reached)
	}

	fmt.Printf("root ceiling: $10M; ladder 2 -> 5 -> 10, promote every 3 clean actions\n\n")

	fmt.Println("== baseline ==")
	show("start (nothing earned)")
	fmt.Println("  " + verify())

	fmt.Println("\n== earning ==")
	recordClean(ctrl, 3) // three clean actions
	fmt.Printf("after 3 clean actions: promoted to limit %d\n", ctrl.Limit())
	show("operating at earned limit")
	fmt.Println("  " + verify())

	recordClean(ctrl, 3) // three more
	fmt.Printf("\nafter 6 clean actions: promoted to the ceiling limit %d\n", ctrl.Limit())
	show("operating at the ceiling")
	fmt.Println("  " + verify())

	recordClean(ctrl, 6) // cannot exceed the ceiling
	fmt.Printf("\nafter 12 clean actions: still capped at the ceiling limit %d (cannot exceed root)\n", ctrl.Limit())

	fmt.Println("\n== anomaly ==")
	if changed, _ := ctrl.FlagAnomaly(); changed {
		fmt.Printf("anomaly flagged: authority reset to baseline limit %d immediately (no gate)\n", ctrl.Limit())
	}
	show("operating after the reset")
	fmt.Println("  " + verify())

	fmt.Println("\nAuthority was earned from the trail and bounded by proof: it only ever widened on a")
	fmt.Println("clean streak, never past the root ceiling (each earned grant is a verified child of")
	fmt.Println("root), and collapsed to baseline the instant an anomaly appeared. The earning is a")
	fmt.Println("durable sequential loop; the limit it sets is enforced by a convergent gsm invariant.")
}

// buildWork returns the convergent work machine: exposure is capped at the delegated limit.
func buildWork() (*gsm.Machine, gsm.Var, gsm.Var) {
	r := gsm.NewRegistry("work")
	exposure := r.Int("exposure", 0, 10)
	limit := r.Int("limit", 0, 10)
	r.Rule("within_limit").
		Require(gsm.AtMostVar(exposure, limit)).
		RepairWith(gsm.Do(gsm.Set(exposure, gsm.V(limit)))).
		Add()
	r.On("buy").OnlyIf(gsm.Lt(gsm.V(exposure), gsm.V(limit))).Does(gsm.Inc(exposure)).Add()
	m, _, err := r.Build()
	if err != nil {
		panic(err)
	}
	return m, exposure, limit
}

// tryBuys seeds a work governor at the given limit and applies `attempts` buys; the invariant caps
// exposure at the limit. Returns the final exposure.
func tryBuys(ctx context.Context, m *gsm.Machine, exposure, limit gsm.Var, atLimit, attempts int) int {
	gov := govern.New(m, m.NewState().SetInt(limit, atLimit))
	for i := 0; i < attempts; i++ {
		gov.Apply(ctx, "buy")
	}
	return gov.State().GetInt(exposure)
}

func recordClean(ctrl *audit.EarnedAuthority, n int) {
	for i := 0; i < n; i++ {
		if _, err := ctrl.RecordCompliant(); err != nil {
			panic(err)
		}
	}
}
