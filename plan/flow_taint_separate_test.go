package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// TaintPolicy.SeparateApprovers in a plan flow (docs/design/flow-taint.md, "Disjoint approvers").

func gate(need int, ids ...string) *agent.ApprovalPolicy {
	return &agent.ApprovalPolicy{Need: need, Approvers: ids}
}

func TestSeparateApprovers_BuildChecksTheNodesOwnGates(t *testing.T) {
	_, vf := approvers("ops", "fin", "sec1", "sec2", "sec3")
	pol := agent.TaintPolicy{Gate: gate(2, "sec1", "sec2", "sec3"), SeparateApprovers: true}
	build := func(p agent.TaintPolicy, opts ...NodeOption) error {
		b := New[int, int]("f").WithApproverVerifiers(vf).WithTaintPolicy(p)
		b.Step[int, int]("pay", stepInt, opts...)
		_, err := b.Build()
		return err
	}
	if err := build(pol, Approval(gate(2, "ops", "fin"))); err != nil {
		t.Errorf("disjoint gates: %v", err)
	}
	err := build(pol, Approval(gate(2, "ops", "sec2")))
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"pay"`) || !strings.Contains(err.Error(), "sec2") {
		t.Errorf("a shared approver: %v, want ErrConfig naming the node and sec2", err)
	}
	if err := build(pol, Approval(gate(2, "ops", "SEC3"))); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a shared approver in another case: %v, want ErrConfig", err)
	}
	err = build(pol, Approval(agent.SingleApproval()))
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"pay"`) {
		t.Errorf("a single own gate: %v, want ErrConfig naming the node", err)
	}
	// Not escalated: no own gate, read-only, or exempt.
	if err := build(pol); err != nil {
		t.Errorf("no own gate: %v", err)
	}
	if err := build(pol, ReadOnly(), Approval(agent.SingleApproval())); err != nil {
		t.Errorf("a read-only node: %v", err)
	}
	if err := build(pol, TaintExempt("reviewed"), Approval(gate(2, "ops", "sec2"))); err != nil {
		t.Errorf("an exempt node: %v", err)
	}
	// Without the field the overlap is allowed; with it, the policy needs an m-of-n Gate.
	if err := build(agent.TaintPolicy{Gate: gate(2, "sec1", "sec2", "sec3")}, Approval(gate(2, "ops", "sec2"))); err != nil {
		t.Errorf("no separation: %v", err)
	}
	for what, p := range map[string]agent.TaintPolicy{
		"Refuse":        {Refuse: true, SeparateApprovers: true},
		"a single gate": {Gate: agent.SingleApproval(), SeparateApprovers: true},
	} {
		if err := build(p); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", what, err)
		}
	}
	// A config-lowered node is checked the same way (the check runs at Build, on the final node set).
	reg := NewRegistry()
	if err := reg.RegisterStep[int, int]("pay", stepInt); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"version":1,"flow":"f","nodes":[{"name":"pay","block":"pay","approval":{"need":2,"approvers":["ops", "sec2"]}}],"wiring":[]}`)
	_, err = Load[int, int](cfg, reg, WithLoadedApproverVerifiers(vf), WithLoadedTaintPolicy(pol))
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "sec2") {
		t.Errorf("a loaded node with a shared approver: %v, want ErrConfig naming sec2", err)
	}
}

// A run under a policy that separates approvers goes through both gates with disjoint approvers as before, and the
// instance's tally excludes nobody.
func TestSeparateApprovers_ARunWithDisjointApproversPasses(t *testing.T) {
	as, vf := approvers("ops", "fin", "sec1", "sec2")
	pol := &agent.TaintPolicy{Gate: gate(2, "sec1", "sec2"), SeparateApprovers: true}
	r := newTaintRun(t, flowOpts{policy: pol, pay: []NodeOption{Approval(gate(2, "ops", "fin"))}, verifiers: vf})
	_, err := r.run()
	pend := pending(t, err)
	if pend.ToolUseID != "node:pay" {
		t.Fatalf("paused at %q, want the node's own gate", pend.ToolUseID)
	}
	as["ops"].decide(t, r.j, pend, true)
	as["fin"].decide(t, r.j, pend, true)
	_, err = r.run()
	pend = pending(t, err)
	if pend.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q, want the instance", pend.ToolUseID)
	}
	as["sec1"].decide(t, r.j, pend, true)
	as["sec2"].decide(t, r.j, pend, true)
	if _, err := r.run(); err != nil || r.runs["pay"] != 1 {
		t.Fatalf("after both gates: %v, pay ran %d times", err, r.runs["pay"])
	}
	recs, _ := r.j.History(context.Background(), "r1")
	for _, rec := range recs {
		if strings.HasPrefix(rec.Name, "approval-tally:node%3Apay%3Ataint") && strings.Contains(string(rec.Result), "excluded_by_sibling") {
			t.Errorf("the instance's tally excludes someone: %s", rec.Result)
		}
	}
}

// The declarations a run starts under hold the field: a flow that dropped it is refused on the next drive.
func TestSeparateApprovers_AFlowThatDroppedItIsRefusedOnTheNextDrive(t *testing.T) {
	as, vf := approvers("ops", "fin", "sec1", "sec2")
	own := gate(2, "ops", "fin")
	r := newTaintRun(t, flowOpts{policy: &agent.TaintPolicy{Gate: gate(2, "sec1", "sec2"), SeparateApprovers: true}, pay: []NodeOption{Approval(own)}, verifiers: vf})
	_, err := r.run()
	pend := pending(t, err)
	as["ops"].decide(t, r.j, pend, true)
	dropped := r.build(flowOpts{policy: &agent.TaintPolicy{Gate: gate(2, "sec1", "sec2")}, pay: []NodeOption{Approval(own)}, verifiers: vf})
	if _, err := dropped.Run(context.Background(), r.j, "r1", 1); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a flow that dropped SeparateApprovers: %v, want ErrConfig", err)
	}
}
