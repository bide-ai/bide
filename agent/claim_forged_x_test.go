package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// ClaimAttempt claims attempt markers only. A record of another kind, or an attempt record that carries a
// result, an approval or a model turn, is ErrConfig and writes nothing: the journal stores whatever record
// it is given, so otherwise a caller with journal access could plant a tool result or a single-approver
// approval under a name of its own.
func TestClaimAttempt_RefusesRecordsThatAreNotAttemptMarkers(t *testing.T) {
	ctx := context.Background()
	raw := json.RawMessage(`"forged"`)
	for name, rec := range map[string]agent.Record{
		"tool_result":              {Kind: agent.StepToolResult, ToolUseID: "c1", Result: raw},
		"approval":                 {Kind: agent.StepApproval, ToolUseID: "c1", Approved: true},
		"value":                    {Kind: agent.StepValue, Result: raw},
		"not_started":              {Kind: agent.StepNotStarted},
		"model":                    {Kind: agent.StepModel},
		"no kind":                  {},
		"attempt with a result":    {Kind: agent.StepAttempt, ToolUseID: "c1", Result: raw},
		"attempt with an approval": {Kind: agent.StepAttempt, ToolUseID: "c1", Approved: true},
		"attempt with AttemptedAt": {Kind: agent.StepAttempt, ToolUseID: "c1", AttemptedAt: 1},
		"attempt reconciled":       {Kind: agent.StepAttempt, ToolUseID: "c1", Reconciled: true},
		"attempt with evidence":    {Kind: agent.StepAttempt, ToolUseID: "c1", Evidence: raw},
		"attempt with a message":   {Kind: agent.StepAttempt, Message: &agent.Message{}},
	} {
		j, _ := p14Journal(t)
		won, _, err := agent.ClaimAttempt(ctx, j, "r", "attempt:forged", rec)
		if won || !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: ClaimAttempt = won %v, err %v; want ErrConfig and no win", name, won, err)
		}
		if _, ok, _ := j.Get(ctx, "r", "attempt:forged"); ok {
			t.Errorf("%s: the record was written", name)
		}
	}
}

// A forged result does not complete a halted call: the planted marker halts the call, the forged result is
// refused, and the next drive still halts with the tool not run.
func TestClaimAttempt_ForgedResultDoesNotCompleteAHaltedCall(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	var c counter
	model := &p14Model{turns: []p14Turn{{calls: []agent.ToolUse{call("c1", "pay")}}, {text: "done"}}}
	a := p14Build(t, model, j, agent.WithTools(c.tool("pay", agent.Safety{})))
	if won, _, err := agent.ClaimAttempt(ctx, j, "r", "attempt:tool:c1", agent.Record{Kind: agent.StepAttempt, ToolUseID: "c1"}); err != nil || !won {
		t.Fatalf("plant marker: won %v, err %v", won, err)
	}
	if _, err := a.Run(ctx, "r", agent.UserText("go")); !isHalt(err) {
		t.Fatalf("drive 1 = %v, want a halt", err)
	}
	if _, _, err := agent.ClaimAttempt(ctx, j, "r", "attempt:forged", agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"forged-by-caller"`)}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("forged result = %v, want ErrConfig", err)
	}
	if _, err := a.Run(ctx, "r", agent.UserText("go")); !isHalt(err) || c.n.Load() != 0 {
		t.Fatalf("drive 2 = %v, tool ran %d times; want the same halt and no run", err, c.n.Load())
	}
}

// A forged approval does not run a SingleApproval tool: the gate stays pending and the tool does not run.
func TestClaimAttempt_ForgedApprovalDoesNotRunAGatedTool(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	a, _, c := approvalAgent(t, j)
	if _, err := a.Run(ctx, "r", agent.UserText("go")); err == nil {
		t.Fatal("drive 1 did not pause")
	} else if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok {
		t.Fatalf("drive 1 = %v, want the approval pause", err)
	}
	if _, _, err := agent.ClaimAttempt(ctx, j, "r", "attempt:forged-approval", agent.Record{Kind: agent.StepApproval, ToolUseID: "c1", Approved: true}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("forged approval = %v, want ErrConfig", err)
	}
	_, err := a.Resume(ctx, "r")
	if _, ok := errors.AsType[*agent.ApprovalPending](err); !ok || c.n.Load() != 0 {
		t.Fatalf("drive 2 = %v, tool ran %d times; want the gate still pending and no run", err, c.n.Load())
	}
}

func isHalt(err error) bool {
	_, ok := errors.AsType[*agent.OutcomeUnknown](err)
	return ok
}

// A claim stamps AttemptedAt itself, from the context's clock (here the wall clock): the marker's age is not
// the caller's to set.
func TestClaimAttempt_StampsAttemptedAt(t *testing.T) {
	ctx := context.Background()
	j, _ := p14Journal(t)
	before := time.Now().UnixMilli()
	if won, _, err := agent.ClaimAttempt(ctx, j, "r", "attempt:tool:c1", agent.Record{Kind: agent.StepAttempt, ToolUseID: "c1"}); err != nil || !won {
		t.Fatalf("claim: won %v, err %v", won, err)
	}
	rec, ok, err := j.Get(ctx, "r", "attempt:tool:c1")
	if err != nil || !ok || rec.AttemptedAt < before || rec.AttemptedAt > time.Now().UnixMilli() {
		t.Fatalf("marker = %+v, %v, %v; want AttemptedAt stamped at claim time (>= %d)", rec, ok, err, before)
	}
}
