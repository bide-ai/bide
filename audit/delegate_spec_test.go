package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// attenuatingSaga runs a saga whose first turn delegates through AttenuatingSubAgent (the sub-run
// makes a compensable charge) and whose second turn calls a tool that fails, so the saga rolls
// back. withGrant binds a signed grant, so the delegation attenuates; without one it is a plain
// delegation. It returns the saga's abort and the number of refunds the rollback made.
func attenuatingSaga(t *testing.T, withGrant bool) (*agent.SagaAborted, int) {
	t.Helper()
	ctx := context.Background()
	store := agenttest.MemJournal()
	var charges, refunds int
	charge := agent.MustCompensatedFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) { charges++; return "charged", nil },
		func(context.Context, struct{}, string) error { refunds++; return nil })
	sub := agenttest.MustNew(
		agenttest.NewScriptedModel(agenttest.ToolTurn("s1", "charge", `{}`), agenttest.TextTurn("done")),
		store,
		agent.WithTools(charge),
	)
	exec := AttenuatingSubAgent("exec", "execute within delegated authority", sub,
		AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
	boom := agent.MustFunc("boom", "fails", func(context.Context, struct{}) (string, error) {
		return "", errors.New("hotel sold out")
	})
	parent := agenttest.MustNew(
		agenttest.NewScriptedModel(
			agenttest.ToolTurn("c1", "exec", `{"task":"pay"}`),
			agenttest.ToolTurn("c2", "boom", `{}`),
			agenttest.TextTurn("unreachable")),
		store,
		agent.WithTools(exec, boom),
	)
	if withGrant {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		signer := Ed25519Signer{Priv: priv}
		rootSG, err := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
		if err != nil {
			t.Fatal(err)
		}
		ctx = WithGrant(ctx, rootSG, signer)
	}
	_, err := parent.Run(ctx, "trip", agent.UserText("book the trip"), agent.WithSaga())
	var aborted *agent.SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("saga Run = %v, want *SagaAborted", err)
	}
	if charges != 1 {
		t.Fatalf("charged %d times, want 1", charges)
	}
	return aborted, refunds
}

// A saga rolled back after an attenuating delegation completed undoes the sub-run's writes, as it
// does for a plain SubAgent: the agent recognises the wrapper through Unwrap and recurses into its
// sub-run, rather than reporting the delegation as a write it cannot undo.
func TestAttenuatingSubAgent_SagaRollbackCompensatesTheSubRun(t *testing.T) {
	for _, withGrant := range []bool{false, true} {
		aborted, refunds := attenuatingSaga(t, withGrant)
		if refunds != 1 || !slices.Contains(aborted.Compensated, "charge") || len(aborted.Uncompensated) != 0 || aborted.CompensateErr != nil {
			t.Errorf("grant=%v: refunds %d, compensated %v, uncompensated %v, err %v; want the sub-run's charge refunded once and nothing left",
				withGrant, refunds, aborted.Compensated, aborted.Uncompensated, aborted.CompensateErr)
		}
	}
}

// The wrapper is described by the SubAgent it wraps, and Unwrap returns that tool.
func TestAttenuatingSubAgent_SpecAndUnwrap(t *testing.T) {
	store := agenttest.MemJournal()
	tool := AttenuatingSubAgent("exec", "execute", agenttest.MustNew(answerModel{"done"}, store),
		AttenuationConfig{Store: store, Narrow: narrowLimitBy(1)}, agent.WithTitle("Executor"))
	s := tool.Spec()
	if s.Name != "exec" || s.Title != "Executor" || !s.Safety.Idempotent || len(s.Input) == 0 {
		t.Fatalf("spec = %+v; want the wrapped SubAgent's spec with its title", s)
	}
	u, ok := tool.(interface{ Unwrap() agent.Tool })
	if !ok || u.Unwrap().Spec().Name != "exec" {
		t.Fatalf("Unwrap missing or wrong: %v", ok)
	}
}

// A nil Store or Narrow is a configuration error, raised when the tool is built.
func TestAttenuatingSubAgent_ConfigErrors(t *testing.T) {
	store := agenttest.MemJournal()
	sub := agenttest.MustNew(answerModel{"done"}, store)
	for name, cfg := range map[string]AttenuationConfig{
		"nil store":  {Narrow: narrowLimitBy(1)},
		"nil narrow": {Store: store},
	} {
		func() {
			defer func() {
				err, _ := recover().(error)
				if !errors.Is(err, agent.ErrConfig) {
					t.Errorf("%s: panic %v, want an error wrapping ErrConfig", name, err)
				}
			}()
			AttenuatingSubAgent("exec", "x", sub, cfg)
		}()
	}
}

// With WithApproval, the parent waits for a human before it delegates: the run pauses with no
// sub-run started, and delegates once the call is approved.
func TestAttenuatingSubAgent_WithApprovalPausesBeforeDelegating(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	sub := agenttest.MustNew(answerModel{"done"}, store)
	exec := AttenuatingSubAgent("exec", "execute", sub,
		AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}},
		agent.WithApproval(agent.SingleApproval()))
	parent := agenttest.MustNew(
		agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "exec", `{"task":"pay"}`), agenttest.TextTurn("ok")),
		store,
		agent.WithTools(exec),
	)

	_, err := parent.Run(ctx, "p", agent.UserText("go"))
	var pa *agent.ApprovalPending
	if !errors.As(err, &pa) || pa.ToolName != "exec" {
		t.Fatalf("Run = %v, want *ApprovalPending for exec", err)
	}
	if recs, _ := store.History(ctx, agent.SubRunID("p", "c1")); len(recs) != 0 {
		t.Fatalf("the sub-run has %d records before approval, want none", len(recs))
	}
	if err := agent.Approve(ctx, store, "p", "c1", true); err != nil {
		t.Fatal(err)
	}
	if out, err := agenttest.Answer(parent.Run(ctx, "p", agent.UserText("go"))); err != nil || out.Text() != "ok" {
		t.Fatalf("resume after Approve = %q, %v; want ok", out.Text(), err)
	}
	if recs, _ := store.History(ctx, agent.SubRunID("p", "c1")); len(recs) == 0 {
		t.Fatal("the approved delegation did not run its sub-run")
	}
}
