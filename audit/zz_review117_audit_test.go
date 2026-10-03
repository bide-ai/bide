package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// R117-4. Rollback now recurses through Unwrap into an attenuating sub-run, but it calls the
// sub-agent's rollback directly, skipping the wrapper's Call, which is what binds the child grant
// and the sub-agent's identity. The sub-run's forward write ran as "exec" under the attenuated
// child grant (limit 4); its compensation runs as the parent, under the parent's broader root
// grant (limit 7). A compensator (or a policy middleware) that checks authority from the context
// sees authority the delegation never granted, and the identity is the parent's.
func TestR117_AttenuatedSubRunIsCompensatedUnderTheParentsAuthority(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
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
	sub := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.TextTurn("done")),
		store,
		agent.WithTools(charge),
	)
	exec := AttenuatingSubAgent("exec", "execute within delegated authority", sub,
		AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
	boom := agent.Func("boom", "fails", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("hotel sold out")
	})
	parent := agenttest.MustNew(
		agent.NewScriptedModel(
			agent.ToolTurn("c1", "exec", `{"task":"pay"}`),
			agent.ToolTurn("c2", "boom", `{}`),
			agent.TextTurn("unreachable")),
		store,
		agent.WithTools(exec, boom),
	)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	rootSG, err := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithGrant(ctx, rootSG, signer)
	_, err = parent.Run(ctx, "trip", agent.UserText("book the trip"), agent.WithSaga(), agent.WithIdentity(agent.Identity{Actor: "desk"}))
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
	store := agenttest.MemJournal()
	var undoHadGrant, fwdHadGrant bool
	charge := agent.CompensatedFunc("charge", "", agent.Safety{},
		func(ctx context.Context, _ struct{}) (string, error) {
			_, _, fwdHadGrant = GrantFrom(ctx)
			return "ok", nil
		},
		func(ctx context.Context, _ struct{}, _ string) error { _, _, undoHadGrant = GrantFrom(ctx); return nil })
	sub := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.TextTurn("done")),
		store,
		agent.WithTools(charge),
	)
	exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
	var gated int32
	gate := agent.Func("gate", "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { gated++; return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("sold out") })
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"pay"}`), agent.ToolTurn("c2", "gate", `{}`), agent.ToolTurn("c3", "boom", `{}`), agent.TextTurn("x"))
	parent := agenttest.MustNew(m, store, agent.WithTools(exec, gate, boom))
	if _, err := parent.Run(ctx, "trip", agent.UserText("go"), agent.WithSaga()); !agent.IsPause(err) {
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
	if _, err := parent.Run(WithGrant(ctx, rootSG, signer), "trip", agent.UserText("go"), agent.WithSaga()); !errors.As(err, &aborted) || len(aborted.Compensated) != 1 {
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
	store := agenttest.MemJournal()
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
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("x")), store)
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
	store := agenttest.MemJournal()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	parent, _ := SignGrant(Grant{ID: "p", Issuer: "corp", Subject: "desk"}, signer)
	other, _ := SignGrant(Grant{ID: "o", Issuer: "corp", Subject: "desk", Scope: map[string]string{"x": "1"}}, signer)
	child, _ := SignGrant(Grant{ID: "c", Issuer: "desk", Subject: "exec", ParentRef: parent.Grant.Digest()}, signer)
	if _, err := RecordGrant(ctx, store, "sub", child); err != nil {
		t.Fatal(err)
	}
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("x")), store)
	b := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(1)}).(interface {
		BindRollback(context.Context, string) (context.Context, error)
	})
	if _, err := b.BindRollback(WithGrant(ctx, other, signer), "sub"); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("BindRollback under a foreign parent = %v, want ErrNotVerified", err)
	}
	// A bound set of several grants (model 11 D1) that holds no parent of the child is refused too.
	third, _ := SignGrant(Grant{ID: "t", Issuer: "corp", Subject: "desk", Scope: map[string]string{"x": "2"}}, signer)
	if _, err := b.BindRollback(WithRollbackGrants(WithGrant(ctx, other, signer), signer, third), "sub"); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("BindRollback under a bound set without the parent = %v, want ErrNotVerified", err)
	}
	// The parent among the bound grants, beside a foreign acting grant: accepted.
	if _, err := b.BindRollback(WithRollbackGrants(WithGrant(ctx, other, signer), signer, third, parent), "sub"); err != nil {
		t.Fatalf("BindRollback with the parent among the bound grants: %v", err)
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

// The parent's rollback also resumes a sub-run's own rollback (the sub-agent's saga failed, and its
// rollback stopped at a failing compensator): that resumed compensation runs under the child grant
// too, not the parent's.
func TestR117_ResumedSubRollbackRunsUnderTheChildGrant(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	var undos int
	var lastLimit string
	charge := agent.CompensatedFunc("charge", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "ok", nil },
		func(ctx context.Context, _ struct{}, _ string) error {
			undos++
			sg, _, _ := GrantFrom(ctx)
			lastLimit = sg.Grant.Scope["limit"]
			if undos == 1 {
				return errors.New("refund service unavailable") // the sub-run's own rollback stops here
			}
			return nil
		})
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("sold out") })
	sub := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.ToolTurn("s2", "boom", `{}`)),
		store,
		agent.WithTools(charge, boom),
	)
	exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
	parent := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"pay"}`)),
		store,
		agent.WithTools(exec),
	)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	rootSG, err := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = parent.Run(WithGrant(ctx, rootSG, signer), "trip", agent.UserText("go"), agent.WithSaga())
	if undos != 2 || lastLimit != "4" {
		t.Fatalf("compensations %d, the resumed one under limit %q; want 2, the second under the child grant's 4", undos, lastLimit)
	}
}

// bindOf returns the rollback-binding hook of an AttenuatingSubAgent over store.
func bindOf(t *testing.T, store *agent.Journal) interface {
	BindRollback(context.Context, string) (context.Context, error)
} {
	t.Helper()
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("x")), store)
	return AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: ScopeRules{"limit": NumericAtMost}}).(interface {
		BindRollback(context.Context, string) (context.Context, error)
	})
}

// A journaled grant is used only once verified against the bound grant and signer: with none bound
// the rollback stops (ErrConfig); a grant signed by another key, or one that widens its parent, is
// refused (ErrNotVerified); a sub-run with records but no journaled authority is refused.
func TestR117_BindRollbackVerifiesTheJournaledGrant(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	signer, forger := Ed25519Signer{Priv: priv}, Ed25519Signer{Priv: other}
	parent, _ := SignGrant(Grant{ID: "p", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
	child := Grant{ID: "c", Issuer: "desk", Subject: "exec", ParentRef: parent.Grant.Digest(), Scope: map[string]string{"limit": "4"}}
	wide := child
	wide.Scope = map[string]string{"limit": "9"}
	for name, tc := range map[string]struct {
		grant  Grant
		by     Signer
		bound  bool
		reason error
	}{
		"no grant bound":   {child, signer, false, agent.ErrConfig},
		"forged signature": {child, forger, true, ErrNotVerified},
		"widening grant":   {wide, signer, true, ErrNotVerified},
	} {
		store := agenttest.MemJournal()
		sg, err := SignGrant(tc.grant, tc.by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RecordGrant(ctx, store, "sub", sg); err != nil {
			t.Fatal(err)
		}
		bctx := ctx
		if tc.bound {
			bctx = WithGrant(ctx, parent, signer)
		}
		if _, err := bindOf(t, store).BindRollback(bctx, "sub"); !errors.Is(err, tc.reason) {
			t.Errorf("%s: BindRollback = %v, want %v", name, err, tc.reason)
		}
	}
	store := agenttest.MemJournal()
	if _, err := journaltest.Do(ctx, store, "sub", "some-step", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`1`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bindOf(t, store).BindRollback(WithGrant(ctx, parent, signer), "sub"); !errors.Is(err, agent.ErrProtocol) {
		t.Errorf("no journaled authority: BindRollback = %v, want ErrProtocol", err)
	}
}

// A delegation resumed under different authority than it began with is refused with ErrConfig,
// unrecorded: nothing is journaled for the call, so a re-drive under the authority it began with
// continues it.
func TestR117_ResumedDelegationKeepsItsAuthority(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	rootSG, err := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	withGrant := func(ctx context.Context) context.Context { return WithGrant(ctx, rootSG, signer) }
	plain := func(ctx context.Context) context.Context { return ctx }
	for name, drives := range map[string][2]func(context.Context) context.Context{
		"began without a grant, resumed with one": {plain, withGrant},
		"began under a grant, resumed without":    {withGrant, plain},
	} {
		ctx := context.Background()
		store := agenttest.MemJournal()
		confirm := agent.Func("confirm", "", agent.Safety{ReadOnly: true},
			func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
		sub := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("s1", "confirm", `{}`), agent.TextTurn("done")),
			store,
			agent.WithTools(confirm),
		)
		exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
		parent := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"x"}`), agent.TextTurn("done")),
			store,
			agent.WithTools(exec),
		)
		_, err := parent.Run(drives[0](ctx), "r", agent.UserText("go"))
		var pend *agent.ApprovalPending
		if !errors.As(err, &pend) {
			t.Fatalf("%s: first drive %v, want the sub-run's pause", name, err)
		}
		if err := agent.Approve(ctx, store, pend.RunID, pend.ToolUseID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := parent.Run(drives[1](ctx), "r", agent.UserText("go")); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("%s: resume = %v, want ErrConfig", name, err)
		}
		recs, err := store.History(ctx, "r")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Kind == agent.StepToolResult && r.ToolUseID == "c1" {
				t.Fatalf("%s: the refusal was recorded as the delegation's result %s", name, r.Result)
			}
		}
		if _, err := parent.Run(drives[0](ctx), "r", agent.UserText("go")); err != nil {
			t.Fatalf("%s: re-drive under the authority it began with: %v", name, err)
		}
	}
}
