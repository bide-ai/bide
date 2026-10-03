// Command authority shows authority-as-governed-state: a principal's delegated limit is a state
// variable, so the compliance invariant is scoped to whoever is acting, "exposure must not exceed
// THIS desk's limit," and one machine-checked policy covers every possible limit at once. The limit
// is seeded at run start from the identity/authority the deployment binds (agent.WithIdentity), and
// every governed action's leaf commits to who acted, on whose behalf, under what authority, and the
// resulting state.
//
// No LLM or network: the agent's model is scripted (agent.NewScriptedModel) to call the governed
// buy tool six times, so the example runs offline and exercises the governance + identity + audit
// machinery through a real agent run. Swap the scripted model for a real one and the guarantees are
// identical.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
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

	store, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	for _, d := range desks {
		// The deployment seeds the delegated limit from the (verified) grant, and binds the acting
		// identity to the run. The agent never sets its own limit.
		gov := govern.New(m, m.NewState().SetInt(limit, d.limit))
		buy := govern.EventTool(gov, govern.EventToolConfig{Name: "buy", Description: "buy $1M", Event: "buy", PolicyDigest: policyDigest})
		id := agent.Identity{Actor: d.actor, OnBehalfOf: d.principal, AuthorityRef: d.grant}

		// The agent tries to buy $6M (six calls). Governance caps it at the desk's granted limit. The
		// scripted model stands in for an LLM: it calls buy six times, then answers. The run journals
		// every call as a tool-result leaf, the last one under the ID "buy/last".
		turns := make([]agenttest.ScriptedTurn, 0, 7)
		for i := 1; i < 6; i++ {
			turns = append(turns, agenttest.ToolTurn(fmt.Sprintf("buy/%d", i), "buy", `{}`))
		}
		turns = append(turns, agenttest.ToolTurn("buy/last", "buy", `{}`), agenttest.TextTurn("done"))
		a, err := agent.New(agenttest.NewScriptedModel(turns...), store, agent.WithTools(buy), agent.WithIdentity(id))
		if err != nil {
			log.Fatal(err)
		}
		runID := "run/" + d.principal
		if _, err := a.Run(ctx, runID, agent.UserText("buy $6M")); err != nil {
			panic(err)
		}
		final := gov.State().GetInt(exposure)
		fmt.Printf("%s (limit $%dM): tried to buy $6M, governed to $%dM\n", d.principal, d.limit, final)

		// The last governed action, read back from the journal: it binds identity, authority, policy,
		// state.
		lastLeaf := toolResult(ctx, store, runID, "buy/last")
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

// toolResult reads the result the run journaled for the tool call toolUseID.
func toolResult(ctx context.Context, store *agent.Journal, runID, toolUseID string) json.RawMessage {
	for rec, err := range store.Records(ctx, runID) {
		if err != nil {
			log.Fatal(err)
		}
		if rec.Kind == agent.StepToolResult && rec.ToolUseID == toolUseID {
			return rec.Result
		}
	}
	log.Fatalf("run %s has no result for tool call %s", runID, toolUseID)
	return nil
}
