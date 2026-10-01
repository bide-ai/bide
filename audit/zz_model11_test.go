package audit

// Findings of model 11 (spec/tla/delegation): each test reproduces a counterexample and fails on
// the code as it stands.

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func m11Root(t *testing.T, signer Signer, id string) SignedGrant {
	t.Helper()
	sg, err := SignGrant(Grant{ID: id, Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "7"},
		NotAfterUnix: time.Now().Unix() + 3600}, signer)
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

// D1 (findings/d1-rotation): a saga whose delegations were minted under two bound grants (the
// first expired between them, and the refusal of the second mint asks for a live one; here the
// root grant is rotated between drives) can never finish its rollback: BindRollback verifies each
// journaled grant against the one grant bound now, and the walk stops at the first that does not
// match, whichever of the two is bound.
func TestModel11_D1_RollbackAcrossRotatedGrants(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	signer := rev117eSigner(t)
	p1, p2 := m11Root(t, signer, "p1"), m11Root(t, signer, "p2")
	var refunds atomic.Int32
	charge := func(name string) agent.Tool {
		return agent.CompensatedFunc(name, "a write", agent.Safety{},
			func(context.Context, struct{}) (string, error) { return "charged", nil },
			func(context.Context, struct{}, string) error { refunds.Add(1); return nil })
	}
	boom := agent.Func("boom", "fails", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("sold out")
	})
	cfg := AttenuationConfig{Store: store, Narrow: narrowLimitBy(1), Rules: rev117eRules}
	tools := func() []agent.Tool {
		sub1 := agent.New(agent.NewScriptedModel(agent.ToolTurn("s1", "charge1", `{}`), agent.TextTurn("d1 done")), store, charge("charge1"))
		sub2 := agent.New(agent.NewScriptedModel(agent.ToolTurn("s2", "charge2", `{}`), agent.ToolTurn("s3", "boom", `{}`), agent.TextTurn("x")),
			store, charge("charge2"), boom)
		return []agent.Tool{AttenuatingSubAgent("d1", "first", sub1, cfg), AttenuatingSubAgent("d2", "second", sub2, cfg)}
	}
	// Drive 1, under p1: d1 delegates and finishes; the next model turn fails (the run stops).
	parent1 := agent.New(agent.NewScriptedModel(agent.ToolTurn("c1", "d1", `{"task":"a"}`), agent.ErrorTurn(errors.New("provider down"))), store, tools()...)
	if _, err := parent1.RunSaga(WithGrant(ctx, p1, signer), "trip", "go"); err == nil {
		t.Fatal("drive 1: want the provider error")
	}
	// Drive 2, under p2 (the rotated grant): d2 mints from p2, its sub-run fails, the saga rolls back.
	script := func() agent.Model {
		return agent.NewScriptedModel(agent.ToolTurn("c1", "d1", `{"task":"a"}`), agent.ToolTurn("c2", "d2", `{"task":"b"}`), agent.TextTurn("x"))
	}
	drive := func(g SignedGrant) *agent.SagaAborted {
		_, err := agent.New(script(), store, tools()...).RunSaga(WithGrant(ctx, g, signer), "trip", "go")
		var ab *agent.SagaAborted
		if !errors.As(err, &ab) {
			t.Fatalf("RunSaga = %v, want *SagaAborted", err)
		}
		return ab
	}
	ab := drive(p2)
	t.Logf("under p2: compensated %v, uncompensated %v, err %v", ab.Compensated, ab.Uncompensated, ab.CompensateErr)
	for k := 0; k < 4 && ab.CompensateErr != nil; k++ {
		g := p1
		if k%2 == 1 {
			g = p2
		}
		ab = drive(g)
		t.Logf("re-drive under %s: compensated %v, uncompensated %v, err %v", g.Grant.ID, ab.Compensated, ab.Uncompensated, ab.CompensateErr)
	}
	if ab.CompensateErr != nil || refunds.Load() != 2 {
		t.Fatalf("the rollback never finished under either grant the delegations were minted from: refunds %d, last error %v",
			refunds.Load(), ab.CompensateErr)
	}
}

// D2 (findings/d2-rerun-expired): BindRollback rebinds the journaled grant without the delegated
// mark, so CallGuard does not apply to the rollback's re-run of a retry-safe write in the
// sub-run: the tool is called after the delegation's grant expired. The sub-run's own rollback,
// under the delegated grant, is refused by CallGuard for the same call.
func TestModel11_D2_RollbackRerunAfterGrantExpiry(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	signer := rev117eSigner(t)
	notAfter := time.Now().Unix() + 1
	narrow := func(parent Grant, sub string) Grant {
		g := narrowLimitBy(1)(parent, sub)
		g.NotAfterUnix = notAfter
		return g
	}
	var calls, undone atomic.Int32
	idem := agent.CompensatedFunc("idem", "a retry-safe write", agent.Safety{Idempotent: true},
		func(context.Context, struct{}) (string, error) { calls.Add(1); return "set", nil },
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	// boom fails once the grant has expired, so the sub-run's own saga rolls back after expiry.
	boom := agent.Func("boom", "fails", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		for time.Now().Unix() <= notAfter {
			time.Sleep(20 * time.Millisecond)
		}
		return "", errors.New("sold out")
	})
	sub := agent.New(rev117eMultiModel{calls: [][2]string{{"s1", "boom"}, {"s2", "idem"}}}, store, boom, idem).SetMaxConcurrency(1)
	deleg := AttenuatingSubAgent("deleg", "d", sub, AttenuationConfig{Store: store, Narrow: narrow, Rules: rev117eRules})
	parent := agent.New(rev117eMultiModel{calls: [][2]string{{"c1", "deleg"}}}, store, deleg)
	_, err := parent.RunSaga(WithGrant(ctx, m11Root(t, signer, "p"), signer), "trip", "go")
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	t.Logf("compensated %v, unknown %v, err %v; idem called %d times, undone %d", ab.Compensated, ab.UnknownOutcome, ab.CompensateErr, calls.Load(), undone.Load())
	if calls.Load() != 0 {
		t.Fatalf("idem's tool was called %d time(s) by the rollback after the delegation's grant expired at %d", calls.Load(), notAfter)
	}
}

// D3 (findings/d3-plain-deleg): a saga's call starts a plain programmatic sub-run (Run, not
// RunSaga); in it, a sub-agent's run makes a compensable write and then fails, so the plain run
// records the sub-agent call as an error result and goes on. The saga's rollback walks the
// sub-run, but rollbackRun skips a call with an error result before its sub-agent recursion, so
// the write is neither compensated nor listed.
func TestModel11_D3_FailedSubAgentInPlainSubRunSkipped(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	var undone atomic.Int32
	charge := agent.CompensatedFunc("charge", "a write", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "charged", nil },
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	build := func(m agent.Model, tools ...agent.Tool) *agent.Agent {
		a, err := agent.Build(m, store.Journal(), agent.WithTools(tools...))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	grand := build(agent.NewScriptedModel(agent.ToolTurn("g1", "charge", `{}`), agent.ErrorTurn(errors.New("provider rejected the request"))), charge)
	child := build(agent.NewScriptedModel(agent.ToolTurn("k1", "worker", `{"task":"w"}`), agent.TextTurn("child done")),
		agent.SubAgent("worker", "w", grand))
	starter := agent.Func("starter", "", agent.Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		msg, err := child.Run(ctx, info.SubRunFor("child"), "work")
		return msg.Text(), err
	}, agent.WithSubRuns(func(name string) *agent.Agent {
		if name == "child" {
			return child
		}
		return nil
	}))
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	parent := build(agent.NewScriptedModel(agent.ToolTurn("c1", "starter", `{}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("x")), starter, boom)
	_, err := parent.RunSaga(ctx, "p", "go")
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	t.Logf("compensated %v, uncompensated %v, unknown %v, err %v", ab.Compensated, ab.Uncompensated, ab.UnknownOutcome, ab.CompensateErr)
	listed := slices.Contains(ab.Uncompensated, "charge") || slices.Contains(ab.Uncompensated, "worker")
	if ab.CompensateErr == nil && undone.Load() == 0 && !listed {
		t.Fatalf("the sub-agent's write was neither compensated nor listed (compensated %v, uncompensated %v)", ab.Compensated, ab.Uncompensated)
	}
}
