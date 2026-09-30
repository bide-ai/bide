package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// R117-4. Rollback now recurses through Unwrap into an attenuating sub-run, but it calls the
// sub-agent's rollback directly, skipping the wrapper's Call, which is what binds the child grant
// and the sub-agent's identity. The sub-run's forward write ran as "exec" under the attenuated
// child grant (limit 4); its compensation runs as the parent, under the parent's broader root
// grant (limit 7). A compensator (or a policy middleware) that checks authority from the context
// sees authority the delegation never granted, and the identity is the parent's.
func TestR117_AttenuatedSubRunIsCompensatedUnderTheParentsAuthority(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var fwdActor, undoActor, fwdLimit, undoLimit string
	charge := agent.CompensatedFunc("charge", "charge the card", agent.Safety{},
		func(ctx context.Context, _ struct{}) (string, error) {
			id, _ := agent.IdentityFrom(ctx)
			sg, _, _ := GrantFrom(ctx)
			fwdActor, fwdLimit = id.Actor, sg.Grant.Scope["limit"]
			return "charged", nil
		},
		func(ctx context.Context, _ struct{}, _ string) error {
			id, _ := agent.IdentityFrom(ctx)
			sg, _, _ := GrantFrom(ctx)
			undoActor, undoLimit = id.Actor, sg.Grant.Scope["limit"]
			return nil
		})
	sub := agent.New(agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.TextTurn("done")), store, charge)
	exec := AttenuatingSubAgent("exec", "execute within delegated authority", sub,
		AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
	boom := agent.Func("boom", "fails", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("hotel sold out")
	})
	parent := agent.New(agent.NewScriptedModel(
		agent.ToolTurn("c1", "exec", `{"task":"pay"}`),
		agent.ToolTurn("c2", "boom", `{}`),
		agent.TextTurn("unreachable")), store, exec, boom)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	rootSG, err := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	ctx = agent.WithIdentity(ctx, agent.Identity{Actor: "desk"})
	ctx = WithGrant(ctx, rootSG, signer)
	_, err = parent.RunSaga(ctx, "trip", "book the trip")
	var aborted *agent.SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	t.Logf("forward: actor %q limit %q; compensation: actor %q limit %q", fwdActor, fwdLimit, undoActor, undoLimit)
	if undoActor != fwdActor || undoLimit != fwdLimit {
		t.Fatalf("the sub-run's write ran as %q under limit %q, but was compensated as %q under limit %q", fwdActor, fwdLimit, undoActor, undoLimit)
	}
}
