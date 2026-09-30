package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A saga delegation began under a grant, fired a compensable charge in its sub-run, and paused on
// an approval. It is resumed under other authority (here: the root grant was renewed, so the bound
// parent is a different grant). The delegation's refusal is recorded as the saga step's failure;
// the rollback must still undo, or at least report, the charge its sub-run already made.
func TestAdv117c_RefusedResumeLeavesSubRunChargeUnaccounted(t *testing.T) {
	for _, name := range []string{"renewed root grant", "no grant bound"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := agent.NewMemStore()
			var charged, undone atomic.Int32
			charge := agent.CompensatedFunc("charge", "", agent.Safety{},
				func(context.Context, struct{}) (string, error) { charged.Add(1); return "charged", nil },
				func(context.Context, struct{}, string) error { undone.Add(1); return nil })
			confirm := agent.Func("confirm", "", agent.Safety{ReadOnly: true},
				func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
			sub := agent.New(agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.ToolTurn("s2", "confirm", `{}`), agent.TextTurn("done")), store, charge, confirm)
			exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
			parent := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"x"}`), agent.TextTurn("done")), store, exec)

			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			signer := Ed25519Signer{Priv: priv}
			root, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: 1000}, signer)
			_, err := parent.RunSaga(WithGrant(ctx, root, signer), "trip", "go")
			var pend *agent.ApprovalPending
			if !errors.As(err, &pend) || charged.Load() != 1 {
				t.Fatalf("first drive: %v (charged %d), want the sub-run's pause after the charge", err, charged.Load())
			}
			if err := agent.Approve(ctx, store, pend.RunID, pend.ToolUseID, true); err != nil {
				t.Fatal(err)
			}
			rctx := ctx
			if name == "renewed root grant" {
				renewed, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: 2000}, signer)
				rctx = WithGrant(ctx, renewed, signer)
			}
			// Resumed under other authority: the delegation refuses with ErrConfig and records
			// nothing, so the saga does not abort and the call is not failed for good.
			_, err = parent.RunSaga(rctx, "trip", "go")
			if !errors.Is(err, agent.ErrConfig) {
				t.Fatalf("resume under other authority: %v, want ErrConfig", err)
			}
			recs, herr := store.History(ctx, "trip")
			if herr != nil {
				t.Fatal(herr)
			}
			for _, r := range recs {
				if r.ToolUseID == "c1" && (r.Kind == agent.StepToolResult || r.Kind == agent.StepSagaFail) {
					t.Fatalf("the refused resume recorded %s for the delegation", r.Kind)
				}
			}
			// The operator binds the right grant and drives again: the delegation continues.
			_, err2 := parent.RunSaga(WithGrant(ctx, root, signer), "trip", "go")
			if err2 != nil || charged.Load() != 1 || undone.Load() != 0 {
				t.Fatalf("re-drive under the original grant: %v (charged %d, undone %d); want the saga to finish, the charge made once and kept", err2, charged.Load(), undone.Load())
			}
		})
	}
}

// A journal written before this change (v0.8.0 or an earlier v1-dev build) holds an ungranted
// delegation's sub-run without the audit:delegation:ungranted leaf. The first drive here writes
// that leaf to a throwaway store, which leaves exactly such a journal. A saga that aborts after
// the upgrade cannot roll the delegation back: "journaled no authority". This is a documented
// break (journals are not promised across pre-releases); the refusal is kept.
func TestAdv117c_PreChangeUngrantedJournalCannotRollBack(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var undone atomic.Int32
	charge := agent.CompensatedFunc("charge", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "charged", nil },
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	build := func(authStore agent.Durable) *agent.Agent {
		sub := agent.New(agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.TextTurn("done")), store, charge)
		exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: authStore, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
		gate := agent.Func("gate", "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
		boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("sold out") })
		m := agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"pay"}`), agent.ToolTurn("c2", "gate", `{}`), agent.ToolTurn("c3", "boom", `{}`), agent.TextTurn("x"))
		return agent.New(m, store, exec, gate, boom)
	}
	if _, err := build(agent.NewMemStore()).RunSaga(ctx, "trip", "go"); !agent.IsPause(err) {
		t.Fatalf("first drive (the old build): %v, want the approval pause", err)
	}
	if err := agent.Approve(ctx, store, "trip", "c2", true); err != nil {
		t.Fatal(err)
	}
	_, err := build(store).RunSaga(ctx, "trip", "go") // the upgraded build
	// Documented break (CHANGELOG, delegation guide): such a journal cannot be rolled back, because
	// its sub-run records no authority; the rollback stops rather than guess, and nothing is undone.
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) || !errors.Is(ab.CompensateErr, agent.ErrProtocol) || undone.Load() != 0 {
		t.Fatalf("a pre-change ungranted delegation: %v (undone %d); want the documented refusal (ErrProtocol), nothing undone", err, undone.Load())
	}
}
