package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A node with an own gate under a policy that separates approvers is held under an instance that is kept apart from
// it: the engine passes that to the instance's gate, and the node's gate-reason record says so. A node with no own
// gate has nothing to separate from.
func TestSeparateApprovers_TheNodesInstanceIsKeptApartFromItsOwnGate(t *testing.T) {
	as, vf := approvers("ops", "fin", "sec1", "sec2")
	reasonOf := func(r *taintRun) string {
		recs, _ := r.j.History(context.Background(), "r1")
		for _, rec := range recs {
			if rec.Name == "gate-reason:node%3Apay" {
				return string(rec.Result)
			}
		}
		t.Fatal("no gate-reason record for node:pay")
		return ""
	}
	pol := &agent.TaintPolicy{Gate: gate(2, "sec1", "sec2"), SeparateApprovers: true}
	r := newTaintRun(t, flowOpts{policy: pol, pay: []NodeOption{Approval(gate(2, "ops", "fin"))}, verifiers: vf})
	_, err := r.run()
	pend := pending(t, err)
	as["ops"].decide(t, r.j, pend, true)
	if got := reasonOf(r); !strings.Contains(got, `"separate_approvers":true`) {
		t.Errorf("a node with an own gate: %s; want separate_approvers", got)
	}
	r2 := newTaintRun(t, flowOpts{policy: pol, verifiers: vf})
	_, err = r2.run()
	if p := pending(t, err); p.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q, want the instance", p.ToolUseID)
	}
	if got := reasonOf(r2); strings.Contains(got, "separate_approvers") {
		t.Errorf("a node with no own gate: %s; want no separate_approvers", got)
	}
}
