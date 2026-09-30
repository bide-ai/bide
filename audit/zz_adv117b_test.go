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
)

// ADV117b-4 (R117-4). An AttenuateFunc that gives each child grant its own ID (Grant.ID is "unique
// identifier for this grant") mints a new grant every time the delegation's Call runs. A sub-run
// that paused (here on an approval inside it) is re-entered on resume, so the delegation's Call
// runs twice and journals two grant leaves in the one sub-run. BindRollback then refuses the
// sub-run (two grants), and a later saga failure cannot compensate the delegation's charge: the
// rollback stops, although nothing about the run was wrong.
func TestAdv117b_ResumedDelegationWithFreshGrantIDsCannotRollBack(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var undos int
	charge := agent.CompensatedFunc("charge", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "ok", nil },
		func(context.Context, struct{}, string) error { undos++; return nil })
	confirm := agent.Func("confirm", "", agent.Safety{ReadOnly: true},
		func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	sub := agent.New(agent.NewScriptedModel(agent.ToolTurn("s1", "charge", `{}`), agent.ToolTurn("s2", "confirm", `{}`), agent.TextTurn("done")), store, charge, confirm)
	var minted int
	narrow := func(parent Grant, subAgent string) Grant {
		minted++
		l, _ := strconv.Atoi(parent.Scope["limit"])
		return Grant{ID: fmt.Sprintf("grant/%s/%d", subAgent, minted), Scope: map[string]string{"limit": strconv.Itoa(l - 3)}}
	}
	exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrow, Rules: ScopeRules{"limit": NumericAtMost}})
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("sold out") })
	parent := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"pay"}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("x")), store, exec, boom)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	rootSG, err := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}}, signer)
	if err != nil {
		t.Fatal(err)
	}
	gctx := WithGrant(agent.WithIdentity(ctx, agent.Identity{Actor: "desk"}), rootSG, signer)
	_, err = parent.RunSaga(gctx, "trip", "go")
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first drive: %v, want the sub-run's approval pause", err)
	}
	if err := agent.Approve(ctx, store, pend.RunID, pend.ToolUseID, true); err != nil {
		t.Fatal(err)
	}
	_, err = parent.RunSaga(gctx, "trip", "go")
	var aborted *agent.SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("resume: %v, want *SagaAborted", err)
	}
	t.Logf("grants minted %d; SagaAborted: compensated %v, uncompensated %v, compensateErr %v", minted, aborted.Compensated, aborted.Uncompensated, aborted.CompensateErr)
	if undos != 1 || aborted.CompensateErr != nil {
		t.Fatalf("the delegation's charge was compensated %d times; rollback err %v", undos, aborted.CompensateErr)
	}
}
