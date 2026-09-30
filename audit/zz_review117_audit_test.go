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

// A delegation that ran with no grant is compensated with none, even when the rollback runs under
// a grant bound later (a resume that binds one): the compensation never gets authority the
// delegation did not have.
func TestR117_UngrantedDelegationIsCompensatedWithoutAGrant(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var undoHadGrant, fwdHadGrant bool
	charge := agent.CompensatedFunc("charge", "", agent.Safety{},
		func(ctx context.Context, _ struct{}) (string, error) {
			_, _, fwdHadGrant = GrantFrom(ctx)
			return "ok", nil
		},
		func(ctx context.Context, _ struct{}, _ string) error { _, _, undoHadGrant = GrantFrom(ctx); return nil })
	sub := agent.New(agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.TextTurn("done")), store, charge)
	exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
	var gated int32
	gate := agent.Func("gate", "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { gated++; return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("sold out") })
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"pay"}`), agent.ToolTurn("c2", "gate", `{}`), agent.ToolTurn("c3", "boom", `{}`), agent.TextTurn("x"))
	parent := agent.New(m, store, exec, gate, boom)
	if _, err := parent.RunSaga(ctx, "trip", "go"); !agent.IsPause(err) {
		t.Fatalf("first drive: %v, want the approval pause", err)
	}
	if err := agent.Approve(ctx, store, "trip", "c2", true); err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	rootSG, err := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	var aborted *agent.SagaAborted
	if _, err := parent.RunSaga(WithGrant(ctx, rootSG, signer), "trip", "go"); !errors.As(err, &aborted) || len(aborted.Compensated) != 1 {
		t.Fatalf("resume: %v, want *SagaAborted compensating the charge", err)
	}
	if fwdHadGrant || undoHadGrant {
		t.Fatalf("forward ran with a grant %v, compensation with a grant %v; want neither", fwdHadGrant, undoHadGrant)
	}
}

// A sub-run with more than one grant leaf is refused: the rollback cannot tell which grant the
// delegation ran under, so it does not guess.
func TestR117_BindRollbackRefusesTwoGrants(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	for _, id := range []string{"a", "b"} {
		sg, err := SignGrant(Grant{ID: id, Issuer: "desk", Subject: "exec"}, signer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RecordGrant(ctx, store, "sub", sg); err != nil {
			t.Fatal(err)
		}
	}
	sub := agent.New(agent.NewScriptedModel(agent.TextTurn("x")), store)
	tool := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(1)})
	b, ok := tool.(interface {
		BindRollback(context.Context, string) (context.Context, error)
	})
	if !ok {
		t.Fatal("AttenuatingSubAgent does not bind rollbacks")
	}
	if _, err := b.BindRollback(ctx, "sub"); !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("BindRollback = %v, want ErrProtocol", err)
	}
}

// A journaled child grant whose parent is not the grant bound to the rollback is refused: the
// rollback does not compensate under a chain it cannot establish.
func TestR117_BindRollbackRefusesAForeignParent(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	parent, _ := SignGrant(Grant{ID: "p", Issuer: "corp", Subject: "desk"}, signer)
	other, _ := SignGrant(Grant{ID: "o", Issuer: "corp", Subject: "desk", Scope: map[string]string{"x": "1"}}, signer)
	child, _ := SignGrant(Grant{ID: "c", Issuer: "desk", Subject: "exec", ParentRef: parent.Grant.Digest()}, signer)
	if _, err := RecordGrant(ctx, store, "sub", child); err != nil {
		t.Fatal(err)
	}
	sub := agent.New(agent.NewScriptedModel(agent.TextTurn("x")), store)
	b := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(1)}).(interface {
		BindRollback(context.Context, string) (context.Context, error)
	})
	if _, err := b.BindRollback(WithGrant(ctx, other, signer), "sub"); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("BindRollback under a foreign parent = %v, want ErrConfig", err)
	}
	bound, err := b.BindRollback(WithGrant(ctx, parent, signer), "sub")
	if err != nil {
		t.Fatal(err)
	}
	if sg, _, ok := GrantFrom(bound); !ok || sg.Grant.ID != "c" {
		t.Fatalf("bound grant %+v (ok %v), want the child", sg.Grant, ok)
	}
	if id, _ := agent.IdentityFrom(bound); id.Actor != "exec" || id.OnBehalfOf != "desk" || id.AuthorityRef != child.Grant.Digest() {
		t.Fatalf("bound identity %+v", id)
	}
}
