package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// answerModel is a stub model that returns a fixed final answer in one turn, so a wrapped sub-agent
// runs its real loop without a network or tools.
type answerModel struct{ text string }

func (m answerModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: m.text}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

// narrowLimitBy returns an AttenuateFunc that lowers the numeric "limit" scope by delta.
func narrowLimitBy(delta int) AttenuateFunc {
	return func(parent Grant, subAgent string) Grant {
		l, _ := strconv.Atoi(parent.Scope["limit"])
		return Grant{ID: "grant/" + subAgent, Scope: map[string]string{"limit": strconv.Itoa(l - delta)}}
	}
}

// isGrant detects a recorded SignedGrant leaf: it has a signature and a subject (fields the tool
// always fills), which distinguishes it from the sub-agent's other journal records.
func isGrant(sg SignedGrant) bool { return len(sg.Sig) > 0 && sg.Grant.Subject != "" }

// findGrant returns the single SignedGrant leaf recorded in a run, or fails.
func findGrant(t *testing.T, store agent.Durable, runID string) SignedGrant {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatalf("History %s: %v", runID, err)
	}
	for _, r := range recs {
		var sg SignedGrant
		if r.Kind == agent.StepValue && json.Unmarshal(r.Result, &sg) == nil && isGrant(sg) {
			return sg
		}
	}
	t.Fatalf("no grant leaf recorded in run %s", runID)
	return SignedGrant{}
}

// TestAttenuatingSubAgent_Default confirms that delegating through AttenuatingSubAgent mints a
// narrower, signed child grant automatically, anchors it, rebinds identity to it, and yields a
// delegation chain that verifies and never widens, all without the caller wiring the grant per hop.
func TestAttenuatingSubAgent_Default(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()

	// One operator key vouches for the whole chain; issuers name the logical delegators.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	opVerifier := func(string) (Verifier, bool) { return Ed25519Verifier{Pub: pub}, true }

	// Root grant: the desk may act up to limit 7.
	root := Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}
	rootSG, err := SignGrant(root, signer)
	if err != nil {
		t.Fatalf("SignGrant root: %v", err)
	}

	// The desk delegates to an execution sub-agent, narrowing the limit by 3 automatically.
	sub := agent.New(answerModel{"done"}, store)
	tool := AttenuatingSubAgent("exec", "execute within delegated authority", sub, store, narrowLimitBy(3), ScopeRules{"limit": NumericAtMost})
	parent := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"do the thing"}`), agent.TextTurn("ok")), store, tool)

	ctx = agent.WithIdentity(ctx, agent.Identity{Actor: "desk-agent", OnBehalfOf: "desk", AuthorityRef: root.Digest()})
	ctx = WithGrant(ctx, rootSG, signer)

	if _, err := parent.Run(ctx, "p1", "go"); err != nil {
		t.Fatalf("delegating run: %v", err)
	}

	// The child grant was minted and anchored under the call's own sub-run (parent run / tool-use
	// id), attenuated and linked to the parent.
	child := findGrant(t, store, agent.SubRunID("p1", "c1"))
	if child.Grant.ParentRef != root.Digest() {
		t.Fatalf("child parent_ref %q does not link to root %q", child.Grant.ParentRef, root.Digest())
	}
	if got := child.Grant.Scope["limit"]; got != "4" {
		t.Fatalf("child limit = %q, want 4 (7 narrowed by 3)", got)
	}
	if child.Grant.Issuer != "desk" || child.Grant.Subject != "exec" {
		t.Fatalf("child issuer/subject = %q/%q, want desk/exec", child.Grant.Issuer, child.Grant.Subject)
	}

	// The chain verifies: each hop signed, linked, and strictly narrowing.
	ok, err := VerifyDelegationChain([]SignedGrant{rootSG, child}, opVerifier, ScopeRules{"limit": NumericAtMost})
	if err != nil || !ok {
		t.Fatalf("delegation chain did not verify: ok=%v err=%v", ok, err)
	}
}

// TestAttenuatingSubAgent_NoGrant confirms that with no grant on ctx the tool is a plain sub-agent
// (it runs and inherits identity), so it is safe to use without the grant context.
func TestAttenuatingSubAgent_NoGrant(t *testing.T) {
	store := agent.NewMemStore()
	sub := agent.New(answerModel{"done"}, store)
	tool := AttenuatingSubAgent("exec", "execute", sub, store, narrowLimitBy(3), ScopeRules{"limit": NumericAtMost})

	if _, err := tool.Call(context.Background(), []byte(`{"task":"go"}`)); err != nil {
		t.Fatalf("plain delegation should work without a grant: %v", err)
	}
	// No grant leaf should have been recorded.
	recs, _ := store.History(context.Background(), "sub/exec")
	for _, r := range recs {
		var sg SignedGrant
		if r.Kind == agent.StepValue && json.Unmarshal(r.Result, &sg) == nil && isGrant(sg) {
			t.Fatalf("no grant should be minted when none is on ctx, found %+v", sg.Grant)
		}
	}
}
