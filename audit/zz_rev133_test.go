package audit

// The adversarial review of #133 (model 11 D1 to D3): its probes, kept as regression tests.

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/middleware"
)

// D1, nested: d1 (minted from p1) delegates to e (minted from d1's child). Drive 2 under p2 with
// p1 bound for the rollback: e's grant must verify against d1's child, not against p1 or p2.
func TestRev133_D1_NestedAcrossRotation(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
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
		inner := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("i1", "chargeE", `{}`), agent.TextTurn("e done")),
			store,
			agent.WithTools(charge("chargeE")),
		)
		e := AttenuatingSubAgent("e", "inner", inner, cfg)
		sub1 := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("s0", "e", `{"task":"x"}`), agent.ToolTurn("s1", "charge1", `{}`), agent.TextTurn("d1 done")),
			store,
			agent.WithTools(charge("charge1"), e),
		)
		sub2 := agenttest.MustNew(
			agent.NewScriptedModel(agent.ToolTurn("s2", "charge2", `{}`), agent.ToolTurn("s3", "boom", `{}`), agent.TextTurn("x")),
			store,
			agent.WithTools(charge("charge2"), boom),
		)
		return []agent.Tool{AttenuatingSubAgent("d1", "first", sub1, cfg), AttenuatingSubAgent("d2", "second", sub2, cfg)}
	}
	parent1 := agenttest.MustNew(
		agent.NewScriptedModel(agent.ToolTurn("c1", "d1", `{"task":"a"}`), agent.ErrorTurn(errors.New("provider down"))),
		store,
		agent.WithTools(tools()...),
	)
	if _, err := parent1.Run(WithGrant(ctx, p1, signer), "trip", agent.UserText("go"), agent.WithSaga()); err == nil {
		t.Fatal("drive 1: want the provider error")
	}
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "d1", `{"task":"a"}`), agent.ToolTurn("c2", "d2", `{"task":"b"}`), agent.TextTurn("x"))
	_, err := agenttest.MustNew(m, store, agent.WithTools(tools()...)).Run(WithRollbackGrants(WithGrant(ctx, p2, signer), signer, p1), "trip", agent.UserText("go"), agent.WithSaga())
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v", err)
	}
	t.Logf("compensated %v, uncompensated %v, unknown %v, err %v, refunds %d", ab.Compensated, ab.Uncompensated, ab.UnknownOutcome, ab.CompensateErr, refunds.Load())
	if ab.CompensateErr != nil || refunds.Load() != 3 {
		t.Fatalf("nested rollback across rotation did not finish: refunds %d, err %v", refunds.Load(), ab.CompensateErr)
	}
}

// D2 with the library's own ToolRetry on the delegated sub-agent: the guard refusal is retried
// and still must come out as an unknown outcome, never a rollback that stops.
func TestRev133_D2_GuardRefusalThroughToolRetry(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
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
	boom := agent.Func("boom", "fails", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		for time.Now().Unix() <= notAfter {
			time.Sleep(20 * time.Millisecond)
		}
		return "", errors.New("sold out")
	})
	sub, err := agent.New(rev117eMultiModel{calls: [][2]string{{"s1", "boom"}, {"s2", "idem"}}}, store,
		agent.WithTools(boom, idem), agent.WithToolMiddleware(middleware.ToolRetry(1, middleware.WithBackoff(time.Millisecond, time.Millisecond))),
		agent.WithMaxConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	deleg := AttenuatingSubAgent("deleg", "d", sub, AttenuationConfig{Store: store, Narrow: narrow, Rules: rev117eRules})
	parent := agenttest.MustNew(rev117eMultiModel{calls: [][2]string{{"c1", "deleg"}}}, store, agent.WithTools(deleg))
	_, err = parent.Run(WithGrant(ctx, m11Root(t, signer, "p"), signer), "trip", agent.UserText("go"), agent.WithSaga())
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v", err)
	}
	t.Logf("unknown %v, err %v, calls %d", ab.UnknownOutcome, ab.CompensateErr, calls.Load())
	if calls.Load() != 0 || ab.CompensateErr != nil || !slices.Contains(ab.UnknownOutcome, "idem") {
		t.Fatalf("guard refusal through ToolRetry: calls %d, unknown %v, err %v", calls.Load(), ab.UnknownOutcome, ab.CompensateErr)
	}
}

// D3, double compensation: the failed sub-agent call in a plain sub-run is a sub-agent that ran
// as a saga and rolled itself back. The outer rollback recurses into it (D3) and must not undo
// its write a second time.
func TestRev133_D3_NoDoubleCompensation(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	var undone atomic.Int32
	charge := agent.CompensatedFunc("charge", "a write", agent.Safety{},
		func(context.Context, struct{}) (string, error) { return "charged", nil },
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	bad := agent.Func("bad", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("no") })
	build := func(m agent.Model, tools ...agent.Tool) *agent.Agent {
		a, err := agent.New(m, store, agent.WithTools(tools...))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	grand := build(agent.NewScriptedModel(agent.ToolTurn("g1", "charge", `{}`), agent.ToolTurn("g2", "bad", `{}`), agent.TextTurn("y")), charge, bad)
	// The grand sub-agent is a saga of its own: the child runs it through a programmatic RunSaga.
	gstarter := agent.Func("gstart", "", agent.Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		res, err := grand.Run(ctx, info.SubRunFor("grand"), agent.UserText("work"), agent.WithSaga())
		var msg agent.Message
		if res != nil {
			msg = res.Message
		}
		var ab *agent.SagaAborted
		if errors.As(err, &ab) {
			return "", errors.New("grand aborted") // a plain failure for the plain child run
		}
		return msg.Text(), err
	}, agent.WithSubRuns(func(name string) *agent.Agent {
		if name == "grand" {
			return grand
		}
		return nil
	}))
	child := build(agent.NewScriptedModel(agent.ToolTurn("k1", "worker", `{"task":"w"}`), agent.TextTurn("child done")),
		agent.SubAgent("worker", "w", build(agent.NewScriptedModel(agent.ToolTurn("w1", "gstart", `{}`), agent.ErrorTurn(errors.New("provider rejected"))), gstarter)))
	starter := agent.Func("starter", "", agent.Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := agent.RunInfoFrom(ctx)
		res, err := child.Run(ctx, info.SubRunFor("child"), agent.UserText("work"))
		var msg agent.Message
		if res != nil {
			msg = res.Message
		}
		return msg.Text(), err
	}, agent.WithSubRuns(func(name string) *agent.Agent {
		if name == "child" {
			return child
		}
		return nil
	}))
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("boom") })
	parent := build(agent.NewScriptedModel(agent.ToolTurn("c1", "starter", `{}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("x")), starter, boom)
	_, err := parent.Run(ctx, "p", agent.UserText("go"), agent.WithSaga())
	var ab *agent.SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v", err)
	}
	t.Logf("compensated %v, uncompensated %v, unknown %v, err %v, undone %d", ab.Compensated, ab.Uncompensated, ab.UnknownOutcome, ab.CompensateErr, undone.Load())
	if undone.Load() > 1 {
		t.Fatalf("charge compensated %d times", undone.Load())
	}
}
