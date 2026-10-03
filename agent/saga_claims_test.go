package agent_test

// Saga rollback's reading of not-started records and re-attempted calls (review of the claim protocol).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journalhook"
)

type sagaCounts struct{ fired, undone int }

func sagaTools(c *sagaCounts) []agent.Tool {
	charge := agent.CompensatedFunc("charge", "", agent.Safety{},
		func(context.Context, struct{}) (string, error) { c.fired++; return "charged", nil },
		func(context.Context, struct{}, string) error { c.undone++; return nil })
	boom := agent.Func("boom", "", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("boom")
	})
	return append([]agent.Tool{}, charge, boom)
}

// sagaFail records that a later call (c2) failed the saga, which starts the rollback.
func sagaFail(t *testing.T, m *agent.MemStore, runID string) {
	t.Helper()
	_, err := journalhook.Do(context.Background(), agenttest.MustJournal(m), runID, agent.ToolResultStep("c2"), func(context.Context) (any, error) {
		return agent.Record{Kind: agent.StepSagaFail, ToolUseID: "c2", Result: json.RawMessage(`"boom"`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An attempt voided by its not-started record changed nothing: the rollback neither compensates
// it nor halts on it.
func TestSagaSkipsVoidedAttempt(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	c := &sagaCounts{}
	model := func() agent.Model {
		return agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("done"))
	}
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "c"}}} // marker commits, errors: voided
	j, _ := agent.NewJournal(s)
	_, err1 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	t.Logf("drive 1: %v", err1)
	sagaFail(t, m, "r")
	_, err2 := agenttest.MustNew(
		model(),
		agenttest.MustJournal(m),
		agent.WithTools(sagaTools(c)...),
		agent.WithMaxConcurrency(1),
	).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	t.Logf("drive 2: %v; fired %d undone %d", err2, c.fired, c.undone)
	r3Dump(t, m, "r")
	var ab *agent.SagaAborted
	if !errors.As(err2, &ab) || ab.CompensateErr != nil || c.undone != 0 || c.fired != 0 {
		t.Fatalf("want a clean abort that compensates nothing")
	}
}

// A call that fired under a re-attempt after a claim whose writes failed and recorded its result is a write:
// the rollback compensates it.
func TestSagaCompensatesCallFiredUnderHeldClaim(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	c := &sagaCounts{}
	model := func() agent.Model {
		return agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("done"))
	}
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "nc"}, {"attempt:not-started:", "nc"}}}
	j, _ := agent.NewJournal(s)
	_, err1 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	_, err2 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	t.Logf("drive 1: %v\ndrive 2: %v; fired %d undone %d", err1, err2, c.fired, c.undone)
	r3Dump(t, m, "r")
	var ab *agent.SagaAborted
	if !errors.As(err2, &ab) || c.fired != 1 || c.undone != 1 {
		t.Fatalf("want the fired call compensated once")
	}
}

// A call that fired under a held claim whose result was lost may have taken effect: the rollback
// halts on it.
func TestSagaHaltsOnHeldClaimWithoutResult(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	c := &sagaCounts{}
	model := func() agent.Model {
		return agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.ToolTurn("c2", "boom", `{}`), agent.TextTurn("done"))
	}
	s := &r3Store{m: m, faults: []r3Fault{{"attempt:tool:c1", "nc"}, {"attempt:not-started:", "nc"}}}
	j, _ := agent.NewJournal(s)
	_, err1 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	s.faults = []r3Fault{{"tool:c1", "nc"}} // the result write is lost after the call fired
	_, err2 := agenttest.MustNew(model(), j, agent.WithTools(sagaTools(c)...), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	sagaFail(t, m, "r")
	_, err3 := agenttest.MustNew(
		model(),
		agenttest.MustJournal(m),
		agent.WithTools(sagaTools(c)...),
		agent.WithMaxConcurrency(1),
	).Run(ctx, "r", agent.UserText("hi"), agent.WithSaga())
	t.Logf("drive 1: %v\ndrive 2: %v\ndrive 3: %v; fired %d undone %d", err1, err2, err3, c.fired, c.undone)
	r3Dump(t, m, "r")
	var ab *agent.SagaAborted
	var halt *agent.ResumeHalt
	if c.fired != 1 || !errors.As(err3, &ab) || !errors.As(ab.CompensateErr, &halt) {
		t.Fatalf("want the rollback to halt on the call that fired with no result")
	}
}
