package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// multiTurns is a replay-safe scripted model whose turns may call several tools at once: it picks
// the turn by counting the assistant messages already in the request.
type multiTurns [][][3]string

func (m multiTurns) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	i := 0
	for _, msg := range req.Messages {
		if msg.Role == agent.RoleAssistant {
			i++
		}
	}
	ch := make(chan agent.Emit, 16)
	if i < len(m) {
		for k, c := range m[i] {
			ch <- agent.Emit{Event: agent.ToolCallDelta{Index: k, ID: c[0], Name: c[1], ArgsFragment: []byte(c[2])}}
		}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "done"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// F1. An Unrecorded refusal is documented as a stop with no effect that a re-drive continues. It
// is returned from the errgroup like a tool fault, so it cancels every sibling in flight. A side
// effect sibling whose request already went out is cut off: its outcome is unknown, nothing is
// recorded for it, and the re-drive under a valid grant halts on it instead of continuing.
// Before round 4 the refusal was recorded as the delegation's failure and the sibling finished.
func TestRev117d_UnrecordedRefusalCutsOffSiblingSideEffect(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	var fired atomic.Int32
	started := make(chan struct{})
	charge := agent.MustFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		if fired.Add(1) == 1 {
			close(started) // the charge request has gone out
		}
		select {
		case <-ctx.Done(): // the response is lost with the cancellation
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return "charged", nil
		}
	})
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("sub done")), store)
	exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
	waitForCharge := func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
			if call.Use.Name == "exec" {
				<-started // the delegation is refused while the charge is in flight
			}
			return next(ctx, call)
		}
	}
	model := multiTurns{{{"c1", "exec", `{"task":"x"}`}, {"c2", "charge", `{}`}}}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	expired, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: time.Now().Add(-time.Hour).Unix()}, signer)
	_, err1 := agenttest.MustNew(model, store, agent.WithTools(exec, charge), agent.WithToolMiddleware(waitForCharge)).Run(WithGrant(ctx, expired, signer), "r", agent.UserText("go"))
	if !errors.Is(err1, agent.ErrConfig) {
		t.Fatalf("first drive: %v, want the delegation's unrecorded ErrConfig", err1)
	}
	// The operator binds a live grant and drives again, as the delegation guide says to.
	live, _ := SignGrant(Grant{ID: "g1", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: time.Now().Add(time.Hour).Unix()}, signer)
	_, err2 := agenttest.MustNew(model, store, agent.WithTools(exec, charge)).Run(WithGrant(ctx, live, signer), "r", agent.UserText("go"))
	var halt *agent.OutcomeUnknown
	if errors.As(err2, &halt) {
		t.Fatalf("the unrecorded refusal of c1 cancelled sibling c2 (charge) after its request went out (fired %d); the re-drive under a live grant halts on c2 instead of continuing: %v (first drive: %v)", fired.Load(), err2, err1)
	}
	if err2 != nil {
		t.Fatalf("re-drive: %v", err2)
	}
}

// F2. A delegation whose journaled child grant expired while its sub-run was paused can never
// continue and never be failed: every re-drive is an unrecorded ErrConfig, whatever grant is
// bound, so the saga neither finishes nor aborts, and the charge its sub-run already made is never
// compensated. The guide promises that a re-drive "with the right grant bound continues the
// delegation"; for an expired journaled grant no such grant exists.
func TestRev117d_ExpiredJournaledGrantWedgesTheSagaForever(t *testing.T) {
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
	childNotAfter := time.Now().Unix() + 1
	narrow := func(parent Grant, subAgent string) Grant {
		return Grant{ID: "grant/" + subAgent, Scope: map[string]string{"limit": "4"}, NotAfterUnix: childNotAfter}
	}
	exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrow, Rules: ScopeRules{"limit": NumericAtMost}})
	boom := agent.MustFunc("boom", "", func(context.Context, struct{}) (string, error) { return "", errors.New("sold out") })
	parent := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"x"}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("done")),
		store,
		agent.WithTools(exec, boom),
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
	for time.Now().Unix() <= childNotAfter { // the child grant expires while the approval waits
		time.Sleep(50 * time.Millisecond)
	}
	renewed, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: time.Now().Add(2 * time.Hour).Unix()}, signer)
	var errs []error
	for _, g := range []SignedGrant{root, renewed, root} {
		_, err := parent.Run(WithGrant(ctx, g, signer), "trip", agent.UserText("go"), agent.WithSaga())
		errs = append(errs, err)
	}
	_, errNone := parent.Run(ctx, "trip", agent.UserText("go"), agent.WithSaga())
	errs = append(errs, errNone)
	stuck := true
	for _, e := range errs {
		if _, u := errors.AsType[*agent.SagaAborted](e); u || e == nil || !errors.Is(e, agent.ErrConfig) {
			stuck = false
		}
	}
	if stuck {
		t.Fatalf("every re-drive (original root, renewed root, original again, no grant) stops with an unrecorded ErrConfig and nothing ever records the delegation's outcome: the saga can neither finish nor abort, and the sub-run's charge (charged %d) is never compensated (undone %d). errors: %v", charged.Load(), undone.Load(), errs)
	}
}

// F3. The guide says "A delegation cannot run past its grant's NotAfterUnix". Expiry is checked
// only when the delegation is entered: a sub-run that started under a live child grant keeps
// running its side effects after that grant expired, in the same drive.
func TestRev117d_SubRunFiresEffectsAfterItsGrantExpired(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	childNotAfter := time.Now().Unix() + 1
	var firedAt atomic.Int64
	slow := agent.MustFunc("slow", "", func(context.Context, struct{}) (string, error) {
		for time.Now().Unix() <= childNotAfter { // a long read outlives the grant
			time.Sleep(50 * time.Millisecond)
		}
		return "ok", nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		firedAt.Store(time.Now().Unix())
		return "charged", nil
	})
	sub := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("s1", "slow", `{}`), agent.ToolTurn("s2", "charge", `{}`), agent.TextTurn("done")),
		store,
		agent.WithTools(slow, charge),
	)
	narrow := func(parent Grant, subAgent string) Grant {
		return Grant{ID: "grant/" + subAgent, Scope: map[string]string{"limit": "4"}, NotAfterUnix: childNotAfter}
	}
	exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrow, Rules: ScopeRules{"limit": NumericAtMost}})
	parent := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "exec", `{"task":"x"}`), agent.TextTurn("done")),
		store,
		agent.WithTools(exec),
	)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	root, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: time.Now().Add(time.Hour).Unix()}, signer)
	if _, err := parent.Run(WithGrant(ctx, root, signer), "r", agent.UserText("go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if at := firedAt.Load(); at > childNotAfter {
		t.Fatalf("the sub-run charged at %d under a child grant that expired at %d: the delegation ran past its grant's NotAfterUnix", at, childNotAfter)
	}
}

// (b) An unrecorded refusal and a sibling's pause (an Interrupt) in one turn: both surface, neither is
// hidden (errors.Join): the caller sees the ErrConfig to fix and the pause to answer.
func TestRev117d_UnrecordedAndSiblingPauseBothSurface(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("sub done")), store)
	exec := AttenuatingSubAgent("exec", "", sub, AttenuationConfig{Store: store, Narrow: narrowLimitBy(3), Rules: ScopeRules{"limit": NumericAtMost}})
	gated := agent.MustFunc("gated", "", func(ctx context.Context, _ struct{}) (string, error) {
		return agent.Interrupt[string](ctx, "confirm", "go ahead?") // a pause while the call runs
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	model := multiTurns{{{"c1", "exec", `{"task":"x"}`}, {"c2", "gated", `{}`}}}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	expired, _ := SignGrant(Grant{ID: "g0", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"}, NotAfterUnix: time.Now().Add(-time.Hour).Unix()}, signer)
	_, err := agenttest.MustNew(model, store, agent.WithTools(exec, gated)).Run(WithGrant(ctx, expired, signer), "r", agent.UserText("go"))
	if _, pause := agent.AsPause(err); !errors.Is(err, agent.ErrConfig) || !pause {
		t.Fatalf("Run = %v; want both the delegation's ErrConfig and the sibling's pause", err)
	}
}
