package approvecmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/taintfixture"
)

// Probe: bide approve finds the escalation instance of a call as a gate of its own, shows the gate its pause
// note records, and refuses an approver of the other instance.
func TestApprove_DecidesTheEscalationInstanceByItsOwnGate(t *testing.T) {
	ctx := context.Background()
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	_ = f.Drive()
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	_ = f.Drive()

	esc, err := load(ctx, f.Journal, taintfixture.Run, "c2#taint")
	if err != nil {
		t.Fatalf("load(c2#taint): %v", err)
	}
	if esc.call.Name != "write" || esc.gate.Need != 2 || !slices.Equal(esc.gate.Approvers, []string{"esc1", "esc2"}) {
		t.Fatalf("the escalation gate = %s %d of %v, want write 2 of [esc1 esc2]", esc.call.Name, esc.gate.Need, esc.gate.Approvers)
	}
	for _, c := range []struct {
		as      string
		wantErr string
	}{{"esc1", ""}, {"esc2", ""}, {"own1", "not an approver"}, {"other", "not an approver"}} {
		err := esc.preflight(taintfixture.Run, c.as, false, f.Verifiers)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("preflight(%s) on the instance: %v", c.as, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("preflight(%s) on the instance = %v, want %q", c.as, err, c.wantErr)
		}
	}
	// The call's own gate, decided: it takes no more decisions, and its approver set is its own.
	own, err := load(ctx, f.Journal, taintfixture.Run, "c2")
	if err != nil {
		t.Fatalf("load(c2): %v", err)
	}
	if !slices.Equal(own.gate.Approvers, []string{"own1"}) {
		t.Fatalf("the call's gate approvers = %v", own.gate.Approvers)
	}
	if err := own.preflight(taintfixture.Run, "esc1", false, f.Verifiers); err == nil {
		t.Error("an escalation approver was accepted on the call's own gate")
	}
}
