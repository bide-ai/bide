package middleware_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/middleware"
)

// billed is the usage every scripted request in these tests reports: 120 tokens.
var billed = agent.Usage{InputTokens: 100, OutputTokens: 20}

var perInput = middleware.Rates{InputPer1M: 1e6} // $1 per input token, so costs are exact

func twice(u agent.Usage) agent.Usage {
	return agent.Usage{InputTokens: 2 * u.InputTokens, OutputTokens: 2 * u.OutputTokens}
}

// billedModel reports usage u on every call. Its first `bad` calls are cut off at max tokens
// (billed, but with truncated tool-call arguments); later calls answer "ok", or, with toolFirst,
// call "lookup" until the conversation has a tool result. With block set, a call after the bad
// ones calls block and then waits for its context to end instead.
type billedModel struct {
	u         agent.Usage
	bad       int32
	toolFirst bool
	calls     atomic.Int32
	block     func()
}

func (m *billedModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	n := m.calls.Add(1)
	ch := make(chan agent.Emit, 2)
	switch {
	case n <= m.bad:
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "cut", Name: "lookup", ArgsFragment: []byte(`{"q":`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "max_tokens", Usage: m.u}}
	case m.block != nil:
		m.block()
		<-ctx.Done()
		return nil, ctx.Err()
	case m.toolFirst && req.Messages[len(req.Messages)-1].Role != agent.RoleTool:
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use", Usage: m.u}}
	default:
		ch <- agent.Emit{Event: agent.TextDelta{Text: "ok"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop", Usage: m.u}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// Budget 200: the turn took two billed 120-token attempts, so the run must stop on the 240
// tokens spent, and the cost meter must report both attempts as spent and only the answer as
// the answer's usage.
func TestRetry_SpendCountsEveryAttempt(t *testing.T) {
	m := &billedModel{u: billed, bad: 1, toolFirst: true} // the tool call leaves a next turn to stop
	var meter middleware.CostMeter
	lookup := agent.Func("lookup", "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "x", nil })
	a := agenttest.Must(agenttest.MustNew(
		m,
		agenttest.MemJournal(),
		agent.WithTools(lookup),
		agent.WithMiddleware(middleware.Cost(&meter, perInput), middleware.Retry(1, middleware.WithBackoff(0, 0))),
	).With(agent.WithTokenBudget(200)))
	_, err := a.Run(context.Background(), "r", agent.UserText("q"))
	if !errors.Is(err, agent.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded (the first turn used 240 tokens)", err)
	}
	if n := m.calls.Load(); n != 2 {
		t.Fatalf("the model was called %d times, want 2", n)
	}
	if meter.Snapshot().Answer != billed {
		t.Fatalf("meter Usage = %+v, want the answer's %+v", meter.Snapshot().Answer, billed)
	}
	if meter.Snapshot().Spend != twice(billed) {
		t.Fatalf("meter Spent = %+v, want both attempts' %+v", meter.Snapshot().Spend, twice(billed))
	}
	if meter.Snapshot().SpendUSD != 200 || meter.Snapshot().AnswerUSD != 100 {
		t.Fatalf("meter SpentTotal = %v, Total = %v; want 200 and 100", meter.Snapshot().SpendUSD, meter.Snapshot().AnswerUSD)
	}
}

// A hedged target that failed was still billed: the run reports it as spend, the recorded
// answer keeps the winner's usage.
func TestHedge_SpendCountsFailedTargets(t *testing.T) {
	primary := &billedModel{u: billed, bad: 1}
	backup := &billedModel{u: billed}
	var meter middleware.CostMeter
	// A long delay: the backup fires as soon as the primary fails, so the order is fixed.
	a := agenttest.MustNew(
		primary,
		agenttest.MemJournal(),
		agent.WithMiddleware(middleware.Cost(&meter, perInput), middleware.Hedge(time.Hour, backup)),
	)
	res, err := a.Run(context.Background(), "r", agent.UserText("q"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage != billed || res.Spend != twice(billed) {
		t.Fatalf("Usage = %+v, Spend = %+v; want %+v and %+v", res.Usage, res.Spend, billed, twice(billed))
	}
	if meter.Snapshot().Answer != billed || meter.Snapshot().Spend != twice(billed) {
		t.Fatalf("meter Usage = %+v, Spent = %+v; want %+v and %+v", meter.Snapshot().Answer, meter.Snapshot().Spend, billed, twice(billed))
	}
}

// A retried turn cancelled while its second attempt is out still reports the first attempt's
// spend.
func TestRetry_SpendWhenCancelledMidAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &billedModel{u: billed, bad: 1, block: cancel}
	var meter middleware.CostMeter
	store := agenttest.MemJournal()
	a := agenttest.MustNew(
		m,
		store,
		agent.WithMiddleware(middleware.Cost(&meter, perInput), middleware.Retry(3, middleware.WithBackoff(0, 0))),
	)
	if _, err := a.Run(ctx, "r", agent.UserText("q")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if meter.Snapshot().Spend != billed || meter.Snapshot().Answer != (agent.Usage{}) {
		t.Fatalf("meter Spent = %+v, Usage = %+v; want %+v and none", meter.Snapshot().Spend, meter.Snapshot().Answer, billed)
	}
	// The cancelled call's spend is journaled all the same, for the run's budget.
	recs, _ := store.History(context.Background(), "r")
	var journaled agent.Usage
	for _, r := range recs {
		if r.DiscardedUsage != nil {
			journaled = *r.DiscardedUsage
		}
	}
	if journaled != billed {
		t.Fatalf("journaled spend = %+v, want %+v", journaled, billed)
	}
}

// Outside an agent, through agent.CallModel, Cost counts every request as spent, failed or not.
func TestCost_SpendOutsideAnAgent(t *testing.T) {
	var meter middleware.CostMeter
	m := &billedModel{u: billed, bad: 1}
	cost := middleware.Cost(&meter, perInput)
	if _, err := agent.CallModel(context.Background(), m, agent.Request{}, cost); err == nil {
		t.Fatal("the cut-off call succeeded")
	}
	if _, err := agent.CallModel(context.Background(), m, agent.Request{}, cost); err != nil {
		t.Fatal(err)
	}
	if meter.Snapshot().Spend != twice(billed) || meter.Snapshot().Answer != billed {
		t.Fatalf("meter Spent = %+v, Usage = %+v; want %+v and %+v", meter.Snapshot().Spend, meter.Snapshot().Answer, twice(billed), billed)
	}
}
