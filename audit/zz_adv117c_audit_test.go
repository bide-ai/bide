package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// A saga delegation began under a grant, fired a compensable charge in its sub-run, and paused on
// an approval. It is resumed under other authority (here: the root grant was renewed, so the bound
// parent is a different grant). The delegation's refusal is recorded as the saga step's failure;
// the rollback must still undo, or at least report, the charge its sub-run already made.
func TestAdv117c_RefusedResumeLeavesSubRunChargeUnaccounted(t *testing.T) {
	for _, name := range []string{"renewed root grant", "no grant bound"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := agenttest.MemJournal()
			var charged, undone atomic.Int32
			charge := agent.MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { charged.Add(1); return "charged", nil },
				func(context.Context, struct{}, string) error { undone.Add(1); return nil })
			confirm := agent.MustFunc("confirm", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithApproval(agent.SingleApproval()))
			sub := agenttest.MustNew(
				agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.ToolTurn("s2", "confirm", `{}`), agent.TextTurn("done")),
				store,
				agent.WithTools(charge, confirm),
			)
			exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
			parent := agenttest.MustNew(
				agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"x"}`), agent.TextTurn("done")),
				store,
				agent.WithTools(exec),
			)

			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			signer := Ed25519Signer{Priv: priv}
			root, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: time.Now().Add(time.Hour).Unix()}, signer)
			_, err := parent.Run(WithGrant(ctx, root, signer), "trip", agent.UserText("go"), agent.WithSaga())
			var pend *agent.ApprovalPending
			if !errors.As(err, &pend) || charged.Load() != 1 {
				t.Fatalf("first drive: %v (charged %d), want the sub-run's pause after the charge", err, charged.Load())
			}
			if err := agent.Approve(ctx, store, pend.RunID, pend.ToolUseID, true); err != nil {
				t.Fatal(err)
			}
			rctx := ctx
			if name == "renewed root grant" {
				renewed, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: time.Now().Add(2 * time.Hour).Unix()}, signer)
				rctx = WithGrant(ctx, renewed, signer)
			}
			// Resumed under other authority: the delegation refuses with ErrConfig and records
			// nothing, so the saga does not abort and the call is not failed for good.
			_, err = parent.Run(rctx, "trip", agent.UserText("go"), agent.WithSaga())
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
			_, err2 := parent.Run(WithGrant(ctx, root, signer), "trip", agent.UserText("go"), agent.WithSaga())
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
	j := agenttest.MustJournal(store)
	var undone atomic.Int32
	charge := agent.MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { return "charged", nil },
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	build := func(authStore *agent.Journal) *agent.Agent {
		sub := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.TextTurn("done")),
			j,
			agent.WithTools(charge),
		)
		exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: authStore, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
		gate := agent.MustFunc("gate", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithApproval(agent.SingleApproval()))
		boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) { return "", errors.New("sold out") })
		m := agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"pay"}`), agent.ToolTurn("c2", "gate", `{}`), agent.ToolTurn("c3", "boom", `{}`), agent.TextTurn("x"))
		return agenttest.MustNew(m, j, agent.WithTools(exec, gate, boom))
	}
	if _, err := build(agenttest.MemJournal()).Run(ctx, "trip", agent.UserText("go"), agent.WithSaga()); !agent.IsPause(err) {
		t.Fatalf("first drive (the old build): %v, want the approval pause", err)
	}
	if err := agent.Approve(ctx, j, "trip", "c2", true); err != nil {
		t.Fatal(err)
	}
	_, err := build(j).Run(ctx, "trip", agent.UserText("go"), agent.WithSaga()) // the upgraded build
	// Documented break (CHANGELOG, delegation guide): such a journal cannot be rolled back, because
	// its sub-run records no authority; the rollback stops rather than guess, and nothing is undone.
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) || !errors.Is(ab.CompensateErr, agent.ErrProtocol) || undone.Load() != 0 {
		t.Fatalf("a pre-change ungranted delegation: %v (undone %d); want the documented refusal (ErrProtocol), nothing undone", err, undone.Load())
	}
}

// A delegation's authority is checked on entry. Minting from an expired bound grant is a
// wrong-authority refusal: ErrConfig, unrecorded, so a re-drive under a live grant continues. An
// expired journaled grant, or one for another subject, is permanent: the delegation's failure is
// recorded (in a saga, it rolls back).
func TestAdv117c_ExpiredOrForeignGrantIsRefusedUnrecorded(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	past, future := time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix()
	live, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: future}, signer)
	expired, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: past}, signer)
	for name, tc := range map[string]struct {
		root     SignedGrant
		journal  *Grant // a child grant already in the sub-run
		want     error  // the run's error when unrecorded
		recorded bool   // the refusal is recorded as the delegation's failure
	}{
		"expired bound grant":     {root: expired, want: agent.ErrConfig},
		"expired journaled grant": {root: live, journal: &Grant{ID: "c", Issuer: "desk", Subject: "exec", Scope: map[string]string{"limit": "4"}, NotAfterUnix: past}, recorded: true},
		"foreign subject":         {root: live, journal: &Grant{ID: "c", Issuer: "desk", Subject: "other", Scope: map[string]string{"limit": "4"}, NotAfterUnix: future}, recorded: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := agenttest.MemJournal()
			if tc.journal != nil {
				g := *tc.journal
				g.ParentRef = tc.root.Grant.Digest()
				sg, _ := SignGrant(g, signer)
				if _, err := RecordGrant(ctx, store, agent.SubRunID("r", "c1"), sg); err != nil {
					t.Fatal(err)
				}
			}
			var ran atomic.Int32
			sub := agenttest.MustNew(
				agent.NewScriptedModel(agent.TextTurn("done")),
				store,
				agent.WithTools(agent.MustFunc("noop", "", func(context.Context, struct{}) (string, error) { ran.Add(1); return "", nil })),
			)
			exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
			parent := agenttest.MustNew(
				agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"x"}`), agent.TextTurn("done")),
				store,
				agent.WithTools(exec),
			)
			_, err := parent.Run(WithGrant(ctx, tc.root, signer), "r", agent.UserText("go"))
			var result *agent.Record
			recs, _ := store.History(ctx, "r")
			for i := range recs {
				if recs[i].Kind == agent.StepToolResult && recs[i].ToolUseID == "c1" {
					result = &recs[i]
				}
			}
			if tc.recorded {
				if err != nil || result == nil || !result.IsError {
					t.Fatalf("Run = %v, result %+v; want the refusal recorded as the delegation's failure", err, result)
				}
				return
			}
			if !errors.Is(err, tc.want) || result != nil {
				t.Fatalf("Run = %v, result %+v; want %v, unrecorded", err, result, tc.want)
			}
		})
	}
}

// An AttenuateFunc that gives the child an expiry already past, or another subject than the
// sub-agent's name, is refused before anything is signed or journaled.
func TestAdv117c_MintRefusesExpiredOrForeignChild(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	root, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
	for name, narrow := range map[string]AttenuateFunc{
		"expired child": func(Grant, string) Grant {
			return Grant{ID: "c", Scope: map[string]string{"limit": "4"}, NotAfterUnix: time.Now().Add(-time.Hour).Unix()}
		},
		"foreign subject": func(Grant, string) Grant {
			return Grant{ID: "c", Subject: "someone-else", Scope: map[string]string{"limit": "4"}}
		},
	} {
		ctx := context.Background()
		store := agenttest.MemJournal()
		sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("done")), store)
		exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrow, Rules: ScopeRules{"limit": NumericAtMost}})
		parent := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"x"}`), agent.TextTurn("done")),
			store,
			agent.WithTools(exec),
		)
		_, err := parent.Run(WithGrant(ctx, root, signer), "r", agent.UserText("go"))
		recs, _ := store.History(ctx, agent.SubRunID("r", "c1"))
		if len(recs) != 0 {
			t.Errorf("%s: Run = %v with %d sub-run records; want the delegation refused before anything is journaled", name, err, len(recs))
		}
		{
			res, _ := store.History(ctx, "r")
			found := false
			for _, r := range res {
				if r.Kind == agent.StepToolResult && r.ToolUseID == "c1" && r.IsError {
					found = true
				}
			}
			if !found {
				t.Errorf("foreign subject: want the refusal recorded as the delegation's failure (%v)", err)
			}
		}
	}
}
