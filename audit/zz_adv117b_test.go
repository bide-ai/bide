package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// ADV117b-4 (R117-4). An AttenuateFunc that gives each child grant its own ID (Grant.ID is "unique
// identifier for this grant") mints a new grant every time the delegation's Call runs. A sub-run
// that paused (here on an approval inside it) is re-entered on resume, so the delegation's Call
// runs twice and journals two grant leaves in the one sub-run. BindRollback then refuses the
// sub-run (two grants), and a later saga failure cannot compensate the delegation's charge: the
// rollback stops, although nothing about the run was wrong.
func TestAdv117b_ResumedDelegationWithFreshGrantIDsCannotRollBack(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	var undos int
	charge := agent.MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil },
		func(context.Context, struct{}, string) error { undos++; return nil })
	confirm := agent.MustFunc("confirm", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithApproval(agent.SingleApproval()))
	sub := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.ToolTurn("s2", "confirm", `{}`), agent.TextTurn("done")),
		store,
		agent.WithTools(charge, confirm),
	)
	var minted int
	narrow := func(parent Grant, subAgent string) Grant {
		minted++
		l, _ := strconv.Atoi(parent.Scope["limit"])
		return Grant{ID: fmt.Sprintf("grant/%s/%d", subAgent, minted), Scope: map[string]string{"limit": strconv.Itoa(l - 3)}}
	}
	exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrow, Rules: ScopeRules{"limit": NumericAtMost}})
	boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) { return "", errors.New("sold out") })
	parent := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"pay"}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("x")),
		store,
		agent.WithTools(exec, boom),
	)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	rootSG, err := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	gctx := WithGrant(ctx, rootSG, signer)
	_, err = parent.Run(gctx, "trip", agent.UserText("go"), agent.WithSaga(), agent.WithIdentity(agent.Identity{Actor: "desk"}))
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first drive: %v, want the sub-run's approval pause", err)
	}
	if err := agent.Approve(ctx, store, pend.RunID, pend.ToolUseID, true); err != nil {
		t.Fatal(err)
	}
	_, err = parent.Run(gctx, "trip", agent.UserText("go"), agent.WithSaga(), agent.WithIdentity(agent.Identity{Actor: "desk"}))
	var aborted *agent.SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("resume: %v, want *SagaAborted", err)
	}
	t.Logf("grants minted %d; SagaAborted: compensated %v, uncompensated %v, compensateErr %v", minted, aborted.Compensated, aborted.Uncompensated, aborted.CompensateErr)
	if minted != 1 {
		t.Fatalf("Narrow minted %d grants; want 1, reused when the delegation resumed", minted)
	}
	if undos != 1 || aborted.CompensateErr != nil {
		t.Fatalf("the delegation's charge was compensated %d times; rollback err %v", undos, aborted.CompensateErr)
	}
}
