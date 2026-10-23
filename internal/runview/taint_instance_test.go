package runview

import (
	"context"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/taintfixture"
)

// Probe: the run viewer lists the call's own gate and its escalation instance as two gates, each with
// the policy and the decisions of its own.
func TestBuild_EscalationInstanceIsAGateOfItsOwn(t *testing.T) {
	ctx := context.Background()
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	_ = f.Drive()
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	_ = f.Drive()
	if err := f.Submit("c2#taint", "c2#taint", "esc1", true); err != nil {
		t.Fatal(err)
	}
	recs, err := f.Journal.History(ctx, taintfixture.Run)
	if err != nil {
		t.Fatal(err)
	}
	v := Build(ctx, taintfixture.Run, recs, nil)
	byID := map[string]Gate{}
	for _, g := range v.Gates {
		byID[g.ToolUseID] = g
	}
	own, esc := byID["c2"], byID["c2#taint"]
	if own.ToolUseID == "" || esc.ToolUseID == "" {
		t.Fatalf("gates = %v, want both c2 and c2#taint", slices.Collect(func(yield func(string) bool) {
			for _, g := range v.Gates {
				if !yield(g.ToolUseID) {
					return
				}
			}
		}))
	}
	if own.ToolName != "write" || own.Need != 1 || !slices.Equal(own.Approvers, []string{"own1"}) || !slices.Equal(own.ApprovedBy, []string{"own1"}) {
		t.Errorf("the call's gate = %+v", own)
	}
	if esc.ToolName != "write" || esc.Need != 2 || !slices.Equal(esc.Approvers, []string{"esc1", "esc2"}) || !esc.Waiting {
		t.Errorf("the escalation gate = %+v, want write, 2 of [esc1 esc2], waiting", esc)
	}
	for _, d := range esc.Decisions {
		if d.Approver == "own1" {
			t.Errorf("the tool's own approver's decision is listed on the escalation gate: %+v", d)
		}
	}
	for _, d := range own.Decisions {
		if d.Approver == "esc1" || d.Approver == "esc2" {
			t.Errorf("an escalation approver's decision is listed on the call's own gate: %+v", d)
		}
	}
}

// Probe: once the call has run, neither of its gates is waiting: the instance's result is its call's.
func TestBuild_EscalationInstanceIsDoneWhenItsCallHasRun(t *testing.T) {
	ctx := context.Background()
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	_ = f.Drive()
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	_ = f.Drive()
	for _, id := range []string{"esc1", "esc2"} {
		if err := f.Submit("c2#taint", "c2#taint", id, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Drive(); err != nil || f.Wrote != 1 {
		t.Fatalf("drive = %v, writes %d", err, f.Wrote)
	}
	recs, err := f.Journal.History(ctx, taintfixture.Run)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range Build(ctx, taintfixture.Run, recs, nil).Gates {
		if g.Waiting {
			t.Errorf("gate %s is still waiting after its call ran", g.ToolUseID)
		}
	}
}
