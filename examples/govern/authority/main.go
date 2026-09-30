// Command authority shows authority-as-governed-state: a principal's delegated limit is a state
// variable, so the compliance invariant is scoped to whoever is acting, "exposure must not exceed
// THIS desk's limit," and one machine-checked policy covers every possible limit at once. The limit
// is seeded at run start from the identity/authority the deployment binds (agent.WithIdentity), and
// every governed action's leaf commits to who acted, on whose behalf, under what authority, and the
// resulting state.
//
// No LLM or network: the "agent" here just calls the governed buy tool in a loop, so the example
// runs offline and exercises the governance + identity + audit machinery. Swap the loop for a real
// agent and the guarantees are identical.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

func main() {
	ctx := context.Background()

	// One policy. The delegated limit is a STATE variable, so the invariant "exposure <= limit"
	// is verified exhaustively over every (exposure, limit) pair: one proof for all principals.
	r := gsm.NewRegistry("execution-desk")
	exposure := r.Int("exposure", 0, 10) // net long position, $1M units
	limit := r.Int("limit", 0, 10)       // the delegated authority, seeded from the grant

	r.Rule("within_delegated_limit").
		Require(gsm.AtMostVar(exposure, limit)).             // exposure <= this desk's limit
		RepairWith(gsm.Do(gsm.Set(exposure, gsm.V(limit)))). // compensate a breach down to the limit
		Add()

	// Buy $1M, blocked pre-trade once the delegated limit is reached (guard), with the invariant as
	// the post-trade backstop.
	r.On("buy").OnlyIf(gsm.Lt(gsm.V(exposure), gsm.V(limit))).Does(gsm.Inc(exposure)).Add()

	m, rep, err := r.Build()
	if err != nil {
		panic(fmt.Sprintf("policy did not verify: %v\n%s", err, rep))
	}
	policyDigest, _ := r.PolicyDigest()
	fmt.Printf("one policy, proven convergent over all %d (exposure,limit) states; digest %s\n\n",
		rep.StateCount, policyDigest[:16]+"...")

	// Two desks under the SAME policy, each granted a different limit by its principal.
	desks := []struct {
		actor, principal, grant string
		limit                   int
	}{
		{"exec-agent@1.4.2", "desk-EQ-US", "grant#a1b2", 3},
		{"exec-agent@1.4.2", "desk-RATES", "grant#c3d4", 7},
	}

	store := agent.NewMemStore()
	for _, d := range desks {
		// The deployment seeds the delegated limit from the (verified) grant, and binds the acting
		// identity to the run. The agent never sets its own limit.
		gov := govern.New(m, m.NewState().SetInt(limit, d.limit))
		buy := govern.EventTool(gov, govern.EventToolConfig{Name: "buy", Description: "buy $1M", Event: "buy", PolicyDigest: policyDigest})
		id := agent.Identity{Actor: d.actor, OnBehalfOf: d.principal, AuthorityRef: d.grant}
		runCtx := agent.WithIdentity(ctx, id)

		// The agent tries to buy $6M (six calls). Governance caps it at the desk's granted limit.
		var lastLeaf json.RawMessage
		for i := 0; i < 6; i++ {
			leaf, err := buy.Call(runCtx, []byte(`{}`))
			if err != nil {
				panic(err)
			}
			lastLeaf = leaf
		}
		final := gov.State().GetInt(exposure)
		fmt.Printf("%s (limit $%dM): tried to buy $6M, governed to $%dM\n", d.principal, d.limit, final)

		// The last governed action, journaled and shown: it binds identity, authority, policy, state.
		runID := "run/" + d.principal
		if _, err := store.Do(ctx, runID, "buy/last", func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepToolResult, ToolUseID: "buy/last", Result: lastLeaf}, nil
		}); err != nil {
			panic(err)
		}
		var leaf map[string]any
		_ = json.Unmarshal(lastLeaf, &leaf)
		fmt.Printf("  leaf: actor=%v on_behalf_of=%v authority=%v state=%v\n\n",
			leaf["actor"], leaf["on_behalf_of"], leaf["authority_ref"], leaf["state_digest"].(string)[:12]+"...")
	}

	// Prove one desk's action offline: the inclusion proof commits to who acted, under what
	// authority, and to what governed state, verifiable with the public key alone.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	runID := "run/desk-EQ-US"
	th, _ := audit.NewTreeHead(ctx, store, runID, 1)
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		panic(err)
	}
	pb, err := audit.ProveToolCall(ctx, store, runID, "buy/last", sth)
	if err != nil {
		panic(err)
	}
	if err := pb.Verify(audit.Ed25519Verifier{Pub: pub}); err != nil {
		panic(err)
	}
	fmt.Printf("offline proof for desk-EQ-US action verified: %v\n", true)
	fmt.Println("the proof commits to the identity, the authority grant, the policy, and the state:")
	rec, err := pb.Record()
	if err != nil {
		panic(err)
	}
	fmt.Printf("  %s\n", string(rec.Result))

	fmt.Println("\nOne proven policy governed two desks to their own delegated limits, and every")
	fmt.Println("action is provable to who acted, for whom, under what authority. Authority is state.")
}
