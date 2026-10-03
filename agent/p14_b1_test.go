package agent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// B1 (model 10's regress/options-from-caller and regress/limits-from-caller): a token budget and a
// system prompt passed per run survive RecoverLoop. The recovery drive passes no option
// (ResumeAgent) and runs under the run's journaled options, not the agent's defaults (no budget,
// no prompt), so the budget stops it.
func TestP14_B1_PerRunOptionsSurviveRecoverLoop(t *testing.T) {
	j, m := p14Journal(t)
	var pay counter
	model := &usageModel{turns: []p14Turn{
		{calls: []agent.ToolUse{call("c1", "pay")}},
		{calls: []agent.ToolUse{call("c2", "lookup")}},
		{text: "done"},
	}}
	a := p14Build(t, model, j, agent.WithTools(
		pay.tool("pay", agent.Safety{}, agent.WithApproval(agent.SingleApproval())),
		agent.Func("lookup", "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "ok", nil })))
	ctx := context.Background()
	_, err := a.Run(ctx, "r", agent.UserText("go"), agent.WithTokenBudget(30), agent.WithSystemPrompt("per-run"))
	if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatalf("first drive = %v, want the approval pause", err)
	}
	if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	// The process that started the run is gone; a worker's RecoverLoop takes the run over.
	lctx, cancel := context.WithCancel(ctx)
	errs := make(chan error, 8)
	done := make(chan error, 1)
	go func() {
		done <- agent.RecoverLoop(lctx, j, agent.ResumeAgent(a), agent.WithRecoverInterval(time.Hour),
			agent.WithRecoverErrors(func(err error) { errs <- err }))
	}()
	var got error
	select {
	case got = <-errs:
	case <-time.After(30 * time.Second):
		t.Fatal("RecoverLoop reported nothing")
	}
	cancel()
	<-done
	if !errors.Is(got, agent.ErrBudgetExceeded) {
		t.Fatalf("recovery drive = %v, want ErrBudgetExceeded under the journaled budget", got)
	}
	if pay.n.Load() != 1 {
		t.Fatalf("pay ran %d times, want once", pay.n.Load())
	}
	if has(t, m, "r", "run:complete") {
		t.Fatal("the recovery drive ran past the run's budget")
	}
	if s := systemText(model.m.lastReq(t)); s != "per-run" {
		t.Fatalf("the recovery drive's system prompt = %q, want the journaled one", s)
	}
}
