// Command delegation shows non-repudiable authority and the delegation chain across sub-agents.
// A root principal signs a grant to a desk; the desk attenuates it (a strictly narrower limit) to
// an execution agent; the execution agent attenuates it again to a sub-agent. Each hop is signed by
// its own issuer's key (not the log's), hash-linked to its parent, and anchored in the audit log.
// The sub-agent is then governed to the limit its grant seeded, and its action's leaf links to that
// grant, whose chain proves, unbroken and never widened, back to the root.
//
// Offline, no LLM or network. The keyring stands in for a PKI/IdP.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

func main() {
	ctx := context.Background()

	// A key per principal (PKI/IdP stand-in).
	keys := map[string]struct {
		pub  ed25519.PublicKey
		priv ed25519.PrivateKey
	}{}
	for _, who := range []string{"corp-treasury", "desk-EQ-US", "exec-agent@1.4.2"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		keys[who] = struct {
			pub  ed25519.PublicKey
			priv ed25519.PrivateKey
		}{pub, priv}
	}
	signer := func(issuer string) audit.Signer { return audit.Ed25519Signer{Priv: keys[issuer].priv} }
	verifier := func(issuer string) (audit.Verifier, bool) {
		k, ok := keys[issuer]
		return audit.Ed25519Verifier{Pub: k.pub}, ok
	}

	// The delegation chain: treasury -> desk -> exec-agent, each hop narrowing the limit.
	g0 := audit.Grant{ID: "g0", Issuer: "corp-treasury", Subject: "desk-EQ-US", Scope: map[string]string{"limit": "10"}}
	g1 := audit.Grant{ID: "g1", Issuer: "desk-EQ-US", Subject: "exec-agent@1.4.2",
		Scope: map[string]string{"limit": "7"}, ParentRef: g0.Digest()}
	g2 := audit.Grant{ID: "g2", Issuer: "exec-agent@1.4.2", Subject: "exec-subagent@1.0",
		Scope: map[string]string{"limit": "3"}, ParentRef: g1.Digest()}
	s0, _ := audit.SignGrant(g0, signer("corp-treasury"))
	s1, _ := audit.SignGrant(g1, signer("desk-EQ-US"))
	s2, _ := audit.SignGrant(g2, signer("exec-agent@1.4.2"))
	grantChain := []audit.SignedGrant{s0, s1, s2}

	ok, err := audit.VerifyDelegationChain(grantChain, verifier, audit.ScopeRules{"limit": audit.NumericAtMost})
	if err != nil || !ok {
		panic(fmt.Sprintf("delegation chain invalid: %v", err))
	}
	fmt.Println("delegation chain verified (each hop signed by its issuer, linked, never widened):")
	fmt.Println("  corp-treasury($10M) -> desk-EQ-US($7M) -> exec-subagent($3M)")

	// The sub-agent runs governed to the limit ITS grant delegated. Same policy pattern as
	// examples/govern/authority; the limit is seeded from the attenuated grant, not chosen by the agent.
	r := gsm.NewRegistry("execution-desk")
	exposure := r.Int("exposure", 0, 10)
	limit := r.Int("limit", 0, 10)
	r.Rule("within_delegated_limit").
		Require(gsm.AtMostVar(exposure, limit)).
		RepairWith(gsm.Do(gsm.Set(exposure, gsm.V(limit)))).
		Add()
	r.On("buy").OnlyIf(gsm.Lt(gsm.V(exposure), gsm.V(limit))).Does(gsm.Inc(exposure)).Add()
	m, _, err := r.Build()
	if err != nil {
		panic(err)
	}
	policyDigest, _ := r.PolicyDigest()

	subLimit, _ := strconv.Atoi(g2.Scope["limit"])
	gov := govern.New(m, m.NewState().SetInt(limit, subLimit))
	buy := govern.AttestedEventTool(gov, "buy", "buy $1M", "buy", policyDigest, agent.Safety{})
	// The sub-agent's identity carries its attenuated grant as the authority reference.
	subID := agent.Identity{Actor: g2.Subject, OnBehalfOf: g1.Subject, AuthorityRef: g2.Digest()}
	runCtx := agent.WithIdentity(ctx, subID)

	var lastLeaf json.RawMessage
	for i := 0; i < 6; i++ { // tries $6M, governed to its $3M delegated limit
		lastLeaf, err = buy.Call(runCtx, []byte(`{}`))
		if err != nil {
			panic(err)
		}
	}
	fmt.Printf("\nsub-agent tried to buy $6M, governed to $%dM by its delegated limit\n", gov.State().GetInt(exposure))

	// Anchor the whole chain and the sub-agent's action in one run, then prove it offline.
	store := agent.NewMemStore()
	const runID = "run/subagent"
	for _, sg := range grantChain {
		if _, err := audit.RecordGrant(ctx, store, runID, sg); err != nil {
			panic(err)
		}
	}
	if _, err := store.Do(ctx, runID, "buy/last", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "buy/last", Result: lastLeaf}, nil
	}); err != nil {
		panic(err)
	}

	logPub, logPriv, _ := ed25519.GenerateKey(rand.Reader) // the log operator's key, distinct from any issuer
	th, _ := audit.NewTreeHead(ctx, store, runID, 1)
	sth := audit.SignTreeHead(th, logPriv)

	actionPB, _ := audit.ProveToolCall(ctx, store, runID, "buy/last", sth)
	grantPB, _ := audit.ProveGrant(ctx, store, runID, g2.Digest(), sth)
	aOK, _ := actionPB.Verify(logPub)
	gOK, _ := grantPB.Verify(logPub)

	var leaf map[string]any
	_ = json.Unmarshal(actionPB.Record.Result, &leaf)
	linksToGrant := leaf["authority_ref"] == g2.Digest()

	fmt.Printf("\noffline proofs (verify with the log key alone):\n")
	fmt.Printf("  sub-agent action inclusion proof: %v\n", aOK)
	fmt.Printf("  attenuated grant g2 inclusion proof: %v\n", gOK)
	fmt.Printf("  action.authority_ref links to the anchored grant g2: %v\n", linksToGrant)

	fmt.Println("\nSo an auditor confirms, from public artifacts: this sub-agent acted under a grant")
	fmt.Println("its parent signed, which descends unbroken and never widened from the root principal,")
	fmt.Println("and governance held it to exactly the delegated limit.")
}
