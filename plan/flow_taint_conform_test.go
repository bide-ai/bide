package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Flow.Conform holds a run's journal to the taint obligations of the flow (docs/design/flow-taint.md, A3): a
// node that ran after a source needed the escalation gate passed before its record, and a Refuse policy means
// it could not have run.

// completedTaintRun runs the taint flow to the end, approving the escalation gate, and returns it with its
// journal.
func completedTaintRun(t *testing.T) (*taintRun, *agent.Journal) {
	t.Helper()
	r := newTaintRun(t, flowOpts{policy: single()})
	ctx := context.Background()
	if _, err := r.run(); err == nil {
		t.Fatal("the flow did not pause")
	}
	if err := agent.Approve(ctx, r.j, "r1", "node:pay:taint", true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(); err != nil {
		t.Fatal(err)
	}
	return r, r.j
}

// without copies the journal of run r1 into a new one, leaving out the records skip names.
func without(t *testing.T, j *agent.Journal, skip func(name string) bool) *agent.Journal {
	t.Helper()
	ctx := context.Background()
	ms := agent.NewMemStore()
	for e, err := range j.Store().Load(ctx, "r1", -1) {
		if err != nil {
			t.Fatal(err)
		}
		if skip(e.Name) {
			continue
		}
		if _, _, err := ms.Insert(ctx, "r1", e.Name, e.Data); err != nil {
			t.Fatal(err)
		}
	}
	nj, err := agent.NewJournal(ms)
	if err != nil {
		t.Fatal(err)
	}
	return nj
}

func TestFlowTaintConform(t *testing.T) {
	ctx := context.Background()
	r, j := completedTaintRun(t)
	if ok, diffs, err := r.f.Conform(ctx, j, "r1"); err != nil || !ok {
		t.Fatalf("a run that met every obligation: Conform = %v, %q, %v", ok, diffs, err)
	}
	has := func(diffs []string, sub string) bool {
		for _, d := range diffs {
			if strings.Contains(d, sub) {
				return true
			}
		}
		return false
	}
	for what, c := range map[string]struct {
		skip func(name string) bool
		want string
	}{
		"the escalation gate's tally and decision are missing": {func(n string) bool {
			return strings.HasPrefix(n, "approval:") && strings.Contains(n, "taint") || strings.HasPrefix(n, "approval-tally:") && strings.Contains(n, "taint")
		}, "ran without a passed escalation gate"},
		"the gate-reason record is missing": {func(n string) bool { return strings.HasPrefix(n, "gate-reason:") }, "no gate-reason record"},
	} {
		forged := without(t, j, c.skip)
		ok, diffs, err := r.f.Conform(ctx, forged, "r1")
		if err != nil || ok || !has(diffs, c.want) {
			t.Errorf("%s: Conform = %v, %q, %v; want a divergence containing %q", what, ok, diffs, err, c.want)
		}
	}
	// A run with the source's record removed has no obligation at pay: the removal is another divergence (a
	// node's record is missing), but the taint check does not invent a gate for it.
	noSource := without(t, j, func(n string) bool { return n == "node:fetch" })
	_, diffs, _ := r.f.Conform(ctx, noSource, "r1")
	if has(diffs, "ran without a passed escalation gate") || has(diffs, "no gate-reason record") {
		t.Errorf("without the source's record: %q", diffs)
	}
}

func TestFlowTaintConform_RefusedNodeCannotHaveRun(t *testing.T) {
	ctx := context.Background()
	r := newTaintRun(t, flowOpts{policy: &agent.TaintPolicy{Refuse: true}})
	if _, err := r.run(); err == nil {
		t.Fatal("the flow was not refused")
	}
	// An honest refused run conforms (it is unfinished, as Conform allows).
	if ok, diffs, err := r.f.Conform(ctx, r.j, "r1"); err != nil || !ok {
		t.Fatalf("a refused run: Conform = %v, %q, %v", ok, diffs, err)
	}
	// A record for the node that was refused is a divergence.
	data, err := agent.JournalEntry("node:pay", agent.Record{Kind: agent.StepValue, Result: []byte(`1`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.j.Store().Insert(ctx, "r1", "node:pay", data); err != nil {
		t.Fatal(err)
	}
	ok, diffs, err := r.f.Conform(ctx, r.j, "r1")
	if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), "refuses it") {
		t.Errorf("a refused node with a record: Conform = %v, %q, %v", ok, diffs, err)
	}
}

func TestFlowTaintConform_RecordsInARunThatDidNotStartUnderTaint(t *testing.T) {
	ctx := context.Background()
	r := newTaintRun(t, flowOpts{})
	if _, err := r.run(); err != nil {
		t.Fatal(err)
	}
	if ok, diffs, err := r.f.Conform(ctx, r.j, "r1"); err != nil || !ok {
		t.Fatalf("a run with no policy: Conform = %v, %q, %v", ok, diffs, err)
	}
	// A gate-reason or taint-mark record planted in it is a divergence.
	for _, name := range []string{"gate-reason:node%3Apay", "taint-mark:node%3Apay:x"} {
		data, _ := agent.JournalEntry(name, agent.Record{Kind: agent.StepValue, Result: []byte(`1`)})
		j2 := agenttest.MemJournal()
		_ = j2
		ms := agent.NewMemStore()
		for e, err := range r.j.Store().Load(ctx, "r1", -1) {
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := ms.Insert(ctx, "r1", e.Name, e.Data); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := ms.Insert(ctx, "r1", name, data); err != nil {
			t.Fatal(err)
		}
		nj, _ := agent.NewJournal(ms)
		ok, diffs, err := r.f.Conform(ctx, nj, "r1")
		if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), "did not start under taint") {
			t.Errorf("%s: Conform = %v, %q, %v", name, ok, diffs, err)
		}
	}
}

func TestFlowTaintConform_DeclarationsAreTheFlows(t *testing.T) {
	ctx := context.Background()
	r, j := completedTaintRun(t)
	other := r.build(flowOpts{policy: &agent.TaintPolicy{Refuse: true}})
	ok, diffs, err := other.Conform(ctx, j, "r1")
	if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), "not this flow's") {
		t.Errorf("another policy: Conform = %v, %q, %v", ok, diffs, err)
	}
	policyless := r.build(flowOpts{})
	ok, diffs, err = policyless.Conform(ctx, j, "r1")
	if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), "no taint policy") {
		t.Errorf("no policy: Conform = %v, %q, %v", ok, diffs, err)
	}
}
