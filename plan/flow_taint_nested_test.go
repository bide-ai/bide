package plan

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// A flow started inside a run under taint (docs/design/flow-taint.md, "Nested flows and inherited taint"), by
// the body of a tainted node: the child inherits the parent's status and the node's read set, as an agent child
// does.

// childRun is how a parent node runs a child flow: its error and the child's journal are kept for the test.
type nested struct {
	t    *testing.T
	j    *agent.Journal
	errs map[string]error
}

func (n *nested) runChild(ctx context.Context, f *Flow[int, int], run string) {
	_, err := f.Run(ctx, n.j, run, 1)
	n.errs[run] = err
}

// parent builds the flow fetch (a source) -> call (a read-only node whose body runs the child flows under the
// node's context) with the given taint policy for the parent.
func (n *nested) parent(src string, pol *agent.TaintPolicy, children map[string]*Flow[int, int]) *Flow[int, int] {
	b := New[int, int]("parent")
	if pol != nil {
		b.WithTaintPolicy(*pol)
	}
	fetch := b.Step("fetch", func(_ context.Context, v int) (int, error) { return v, nil }, Untrusted(src))
	call := b.Step("call", func(ctx context.Context, v int) (int, error) {
		for run, c := range children {
			n.runChild(ctx, c, run)
		}
		return v, nil
	}, ReadOnly())
	b.Edge(fetch, call)
	f, err := b.Build()
	if err != nil {
		n.t.Fatal(err)
	}
	return f
}

func child(t *testing.T, name string, pol *agent.TaintPolicy, readOnly bool, extra ...NodeOption) *Flow[int, int] {
	t.Helper()
	b := New[int, int](name)
	if pol != nil {
		b.WithTaintPolicy(*pol)
	}
	opts := extra
	if readOnly {
		opts = append([]NodeOption{ReadOnly()}, opts...)
	}
	b.Step("pay", func(_ context.Context, v int) (int, error) { return v, nil }, opts...)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func startOf(t *testing.T, j *agent.Journal, run string) agent.RunStart {
	t.Helper()
	s, ok, err := agent.RecordedStart(context.Background(), j, run)
	if err != nil || !ok {
		t.Fatalf("no recorded start for %s: %v", run, err)
	}
	return s
}

func inherited(t *testing.T, s agent.RunStart) []map[string]string {
	t.Helper()
	raw, ok := s.Ext["taint_inherited"]
	if !ok {
		return nil
	}
	var l []map[string]string
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestFlowTaint_ANestedFlowWithItsOwnPolicyInheritsTheMarks(t *testing.T) {
	ctx := context.Background()
	n := &nested{t: t, j: agenttest.MemJournal(), errs: map[string]error{}}
	c := child(t, "child", single(), false)
	p := n.parent("inbound-email", single(), map[string]*Flow[int, int]{"c1": c})
	// The parent's call node is read-only: its body runs, under the parent's taint, and starts the child.
	if _, err := p.Run(ctx, n.j, "p1", 1); err != nil {
		t.Fatalf("parent: %v", err)
	}
	// The child's side effect, after nothing of its own, waits at its escalation gate: the mark came down.
	if err := n.errs["c1"]; err == nil || !strings.Contains(err.Error(), "node:pay:taint") && !isPendingAt(err, "node:pay:taint") {
		t.Fatalf("child: %v, want a pause at its escalation gate", err)
	}
	s := startOf(t, n.j, "c1")
	got := inherited(t, s)
	if len(got) != 1 || got[0]["source"] != "inbound-email" || got[0]["ref"] != "parent:node:fetch" {
		t.Fatalf("inherited marks %v, want one: inbound-email, parent:node:fetch", got)
	}
	// Driven again with no parent (a recovery pass, a direct call), it runs under the recorded marks: still gated.
	if _, err := c.Run(ctx, n.j, "c1", 1); !isPendingAt(err, "node:pay:taint") {
		t.Fatalf("a direct drive of the child: %v, want the same pause", err)
	}
	// A child run that a parent shows other marks, while it is unfinished, is refused.
	n2 := &nested{t: t, j: n.j, errs: map[string]error{}}
	p2 := n2.parent("web", single(), map[string]*Flow[int, int]{"c1": c})
	if _, err := p2.Run(ctx, n.j, "p2", 1); err != nil {
		t.Fatalf("parent 2: %v", err)
	}
	if err := n2.errs["c1"]; !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "inherited") {
		t.Errorf("the child driven under other marks: %v, want ErrConfig naming inherited marks", err)
	}
	if err := agent.Approve(ctx, n.j, "c1", "node:pay:taint", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, n.j, "c1", 1); err != nil {
		t.Fatalf("after approval: %v", err)
	}
}

func isPendingAt(err error, key string) bool {
	var p *agent.ApprovalPending
	return errors.As(err, &p) && p.ToolUseID == key
}

func TestFlowTaint_ANestedFlowWithNoPolicyRunsOnlyNodesThatCannotHaveASideEffect(t *testing.T) {
	ctx := context.Background()
	n := &nested{t: t, j: agenttest.MemJournal(), errs: map[string]error{}}
	side := child(t, "side", nil, false)
	reads := child(t, "reads", nil, true, Approval(agent.SingleApproval()))
	p := n.parent("inbound-email", single(), map[string]*Flow[int, int]{"side": side, "reads": reads})
	if _, err := p.Run(ctx, n.j, "p1", 1); err != nil {
		t.Fatalf("parent: %v", err)
	}
	// A policyless child with a side effect is refused at Begin: nothing is written for its run.
	if err := n.errs["side"]; !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "pay") {
		t.Fatalf("the policyless child with a side effect: %v, want ErrConfig naming the step", err)
	}
	if recs, _ := n.j.History(ctx, "side"); len(recs) != 0 {
		t.Errorf("the refused child wrote %d records", len(recs))
	}
	// One of read-only nodes runs, and records the parent's status and marks with no declarations.
	if err := n.errs["reads"]; !isPendingAt(err, "node:pay") {
		t.Fatalf("the child of read-only nodes: %v, want it to run to its own gate", err)
	}
	s := startOf(t, n.j, "reads")
	if _, has := s.Ext["taint_declarations"]; has || len(inherited(t, s)) != 1 {
		t.Errorf("start ext %v; want the parent's digest and one inherited mark, no declarations", s.Ext)
	}
	// It is not driven by a flow that has a policy now.
	withPolicy := child(t, "reads", single(), true, Approval(agent.SingleApproval()))
	if _, err := withPolicy.Run(ctx, n.j, "reads", 1); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a flow with a policy driving a run that started with none inside a tainted run: %v, want ErrConfig", err)
	}
	// Outside a tainted run a policyless flow is as it was: nothing recorded, nothing gated.
	if _, err := side.Run(ctx, agenttest.MemJournal(), "alone", 1); err != nil {
		t.Errorf("a policyless flow outside a tainted run: %v", err)
	}
}

// flow -> node -> flow -> node: the inherited list does not grow down the chain: one entry per source, in
// canonical order, the first ref kept ("parent:" once, never repeated).
func TestFlowTaint_TheInheritedListDoesNotGrowDownAChain(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	n := &nested{t: t, j: j, errs: map[string]error{}}
	third := child(t, "third", single(), false)
	// The middle flow has its own source (crm) and a read-only node that runs the third.
	mb := New[int, int]("middle").WithTaintPolicy(*single())
	look := mb.Step("look", func(_ context.Context, v int) (int, error) { return v, nil }, Untrusted("crm"), ReadOnly())
	call := mb.Step("call", func(c context.Context, v int) (int, error) { n.runChild(c, third, "third"); return v, nil }, ReadOnly())
	mb.Edge(look, call)
	middle, err := mb.Build()
	if err != nil {
		t.Fatal(err)
	}
	p := n.parent("inbound-email", single(), map[string]*Flow[int, int]{"middle": middle})
	if _, err := p.Run(ctx, j, "p1", 1); err != nil {
		t.Fatal(err)
	}
	if err := n.errs["middle"]; err != nil {
		t.Fatalf("middle: %v", err)
	}
	mid := inherited(t, startOf(t, j, "middle"))
	if len(mid) != 1 || mid[0]["source"] != "inbound-email" || mid[0]["ref"] != "parent:node:fetch" {
		t.Fatalf("the middle flow inherited %v", mid)
	}
	got := inherited(t, startOf(t, j, "third"))
	var sources, refs []string
	for _, m := range got {
		sources, refs = append(sources, m["source"]), append(refs, m["ref"])
	}
	if !slices.Equal(sources, []string{"crm", "inbound-email"}) || !slices.Equal(refs, []string{"parent:node:look", "parent:node:fetch"}) {
		t.Errorf("the third flow inherited sources %v with refs %v; want one entry per source, the first ref kept, no doubled prefix", sources, refs)
	}
	for _, r := range refs {
		if strings.HasPrefix(r, "parent:parent:") {
			t.Errorf("a ref grew down the chain: %q", r)
		}
	}
}

// The body of a read-only node in a run under taint runs under its status even though the node needs no gate:
// what it starts inherits.
func TestFlowTaint_ABodyRunsUnderTheRunsTaintWhetherOrNotTheNodeIsGated(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	n := &nested{t: t, j: j, errs: map[string]error{}}
	reads := child(t, "reads", nil, true)
	// A parent that is not under taint (no policy): its body starts a child that inherits nothing.
	b := New[int, int]("untainted")
	b.Step("call", func(c context.Context, v int) (int, error) { n.runChild(c, reads, "r"); return v, nil }, ReadOnly())
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "p0", 1); err != nil {
		t.Fatal(err)
	}
	if s := startOf(t, j, "r"); len(s.Ext) != 0 {
		t.Errorf("a child of an untainted parent recorded %v", s.Ext)
	}
}

// An agent started by the body of a tainted node inherits what a flow child does: with a taint policy its first
// turn is tainted, so its side effect waits at its escalation gate; with none, a side effect is refused.
func TestFlowTaint_AnAgentStartedByATaintedNodeInheritsTheTaint(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	ran := 0
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { ran++; return "ok", nil })
	var withPolicy, without error
	b := New[int, int]("parent").WithTaintPolicy(*single())
	fetch := b.Step("fetch", func(_ context.Context, v int) (int, error) { return v, nil }, Untrusted("inbound-email"))
	call := b.Step("call", func(c context.Context, v int) (int, error) {
		m1 := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
		_, withPolicy = agenttest.MustNew(m1, j, agent.WithTools(tool), agent.WithTaintPolicy(agent.TaintPolicy{Gate: agent.SingleApproval()})).Run(c, "agent-with-policy", agent.UserText("go"), agent.WithTrustedInput("the flow's own"))
		m2 := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
		_, without = agenttest.MustNew(m2, j, agent.WithTools(tool)).Run(c, "agent-without-policy", agent.UserText("go"))
		return v, nil
	}, ReadOnly())
	b.Edge(fetch, call)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "p1", 1); err != nil {
		t.Fatal(err)
	}
	var pend *agent.ApprovalPending
	if !errors.As(withPolicy, &pend) || pend.ToolUseID != "c1#taint" {
		t.Errorf("the agent with a taint policy: %v, want a pause at its escalation gate c1#taint", withPolicy)
	}
	if !errors.Is(without, agent.ErrConfig) || ran != 0 {
		t.Errorf("the agent with no policy: %v (the tool ran %d times), want its side effect refused as ErrConfig", without, ran)
	}
}

// Conform recomputes a nested flow's obligations with the marks it inherited: a node that ran after nothing of
// the flow's own needed its gate because of them.
func TestFlowTaintConform_ANestedFlow(t *testing.T) {
	ctx := context.Background()
	n := &nested{t: t, j: agenttest.MemJournal(), errs: map[string]error{}}
	c := child(t, "child", single(), false)
	p := n.parent("inbound-email", single(), map[string]*Flow[int, int]{"c1": c})
	if _, err := p.Run(ctx, n.j, "p1", 1); err != nil {
		t.Fatal(err)
	}
	if err := agent.Approve(ctx, n.j, "c1", "node:pay:taint", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, n.j, "c1", 1); err != nil {
		t.Fatal(err)
	}
	if ok, diffs, err := c.Conform(ctx, n.j, "c1"); err != nil || !ok {
		t.Fatalf("a nested run that met its obligations: Conform = %v, %q, %v", ok, diffs, err)
	}
	// Without the escalation gate's records the node ran with nothing to show for the inherited mark.
	forged := copyWithout(t, n.j, "c1", func(name string) bool {
		return strings.HasPrefix(name, "approval:") && strings.Contains(name, "taint") || strings.HasPrefix(name, "approval-tally:") && strings.Contains(name, "taint")
	})
	ok, diffs, err := c.Conform(ctx, forged, "c1")
	if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), "ran without a passed escalation gate") {
		t.Errorf("Conform without the gate's records = %v, %q, %v", ok, diffs, err)
	}
	// With no record of why it was gated either, the inherited mark alone is the obligation: the recompute reads it.
	bare := copyWithout(t, n.j, "c1", func(name string) bool {
		return strings.HasPrefix(name, "gate-reason:") || strings.HasPrefix(name, "approval") && strings.Contains(name, "taint")
	})
	ok, diffs, err = c.Conform(ctx, bare, "c1")
	if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), "no gate-reason record") {
		t.Errorf("Conform without the gate-reason record and the gate's records = %v, %q, %v; want the inherited obligation", ok, diffs, err)
	}
	// A policyless child that started inside a tainted run: a node that can have a side effect cannot have run.
	reads := child(t, "reads", nil, true)
	n2 := &nested{t: t, j: n.j, errs: map[string]error{}}
	p2 := n2.parent("inbound-email", single(), map[string]*Flow[int, int]{"reads": reads})
	if _, err := p2.Run(ctx, n.j, "p2", 1); err != nil {
		t.Fatal(err)
	}
	if ok, diffs, err := reads.Conform(ctx, n.j, "reads"); err != nil || !ok {
		t.Fatalf("a policyless nested run of read-only nodes: Conform = %v, %q, %v", ok, diffs, err)
	}
	side := child(t, "reads", nil, false) // the same flow name and run, with a node that can have a side effect
	ok, diffs, err = side.Conform(ctx, n.j, "reads")
	if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), "side effect ran in a run that started inside a tainted run") && !strings.Contains(strings.Join(diffs, "|"), "ran in a run that started inside a tainted run") {
		t.Errorf("a side-effecting node in a policyless nested run: Conform = %v, %q, %v", ok, diffs, err)
	}
	withPolicy := child(t, "reads", single(), true)
	ok, diffs, err = withPolicy.Conform(ctx, n.j, "reads")
	if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), "this flow has one") {
		t.Errorf("a flow with a policy over a run that started with none inside a tainted run: Conform = %v, %q, %v", ok, diffs, err)
	}
}

// copyWithout is without (flow_taint_conform_test.go) for any run.
func copyWithout(t *testing.T, j *agent.Journal, run string, skip func(name string) bool) *agent.Journal {
	t.Helper()
	ctx := context.Background()
	ms := agent.NewMemStore()
	for e, err := range j.Store().Load(ctx, run, -1) {
		if err != nil {
			t.Fatal(err)
		}
		if skip(e.Name) {
			continue
		}
		if _, _, err := ms.Insert(ctx, run, e.Name, e.Data); err != nil {
			t.Fatal(err)
		}
	}
	nj, err := agent.NewJournal(ms)
	if err != nil {
		t.Fatal(err)
	}
	return nj
}

// A loop body node's body runs under the taint too, in every iteration.
func TestFlowTaint_ALoopBodyRunsUnderTheRunsTaint(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	n := &nested{t: t, j: j, errs: map[string]error{}}
	reads := child(t, "reads", nil, true)
	calls := 0
	b := New[int, int]("loop").WithTaintPolicy(*single())
	seed := b.Step("seed", func(_ context.Context, v int) (int, error) { return v, nil }, Untrusted("feed"))
	refine := b.Step("refine", func(c context.Context, v int) (int, error) {
		calls++
		n.runChild(c, reads, "lc"+string(rune('0'+calls)))
		return v + 1, nil
	}, ReadOnly())
	check := b.Step("check", func(_ context.Context, v int) (int, error) { return v, nil }, ReadOnly())
	done := b.Step("done", func(_ context.Context, v int) (int, error) { return v, nil }, ReadOnly())
	b.Edge(seed, refine)
	b.Edge(refine, check)
	b.Switch(check, LoopBack(3, func(v int) bool { return v < 2 }, refine), Else(done))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "p1", 0); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("the loop body ran %d times, want at least 2", calls)
	}
	for i := 1; i <= calls; i++ {
		run := "lc" + string(rune('0'+i))
		if l := inherited(t, startOf(t, j, run)); len(l) != 1 || l[0]["source"] != "feed" {
			t.Errorf("the child of iteration %d inherited %v, want the feed mark", i, l)
		}
	}
}

// A flow that a tool of an agent under a taint policy runs, with no taint policy of its own and a node that can
// have a side effect, is refused at Begin: the agent's escalation gate on the tool call is not the flow's
// control, and a flow cannot run a side effect under taint without a policy to enforce.
func TestFlowTaint_APolicylessFlowRunAsATaintedAgentsToolIsRefused(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	var flowErr error
	ran := 0
	cb := New[int, int]("tool-flow")
	cb.Step("pay", func(_ context.Context, v int) (int, error) { ran++; return v, nil })
	cf, err := cb.Build()
	if err != nil {
		t.Fatal(err)
	}
	read := agent.MustFunc("read", "reads", func(context.Context, struct{}) (string, error) { return "text", nil },
		agent.WithSafety(agent.Safety{ReadOnly: true, Untrusted: "inbox"}))
	runflow := agent.MustFunc("runflow", "runs a flow", func(c context.Context, _ struct{}) (string, error) {
		_, flowErr = cf.Run(c, j, "child", 1)
		return "ok", nil
	}, agent.TaintExempt("the flow is the control under test"))
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "read", `{}`), agenttest.ToolTurn("c2", "runflow", `{}`), agenttest.TextTurn("done"))
	a := agenttest.MustNew(m, j, agent.WithTools(read, runflow), agent.WithTaintPolicy(agent.TaintPolicy{Gate: agent.SingleApproval()}))
	if _, err = a.Run(ctx, "agent", agent.UserText("go"), agent.WithTrustedInput("the host's own")); err != nil {
		t.Fatalf("the agent run: %v", err)
	}
	if !errors.Is(flowErr, agent.ErrConfig) || ran != 0 {
		t.Errorf("the flow %v, pay ran %d; want it refused at Begin", flowErr, ran)
	}
}

// A node declared Idempotent has no claim, but it can have a side effect: in a policyless flow started inside a run
// under taint it is refused at Begin, as a plain side effect is, with nothing written.
func TestFlowTaint_AnIdempotentNodeInAPolicylessNestedFlowIsRefusedAtBegin(t *testing.T) {
	ctx := context.Background()
	n := &nested{t: t, j: agenttest.MemJournal(), errs: map[string]error{}}
	idem := child(t, "idem", nil, false, Idempotent())
	p := n.parent("inbound-email", single(), map[string]*Flow[int, int]{"idem": idem})
	if _, err := p.Run(ctx, n.j, "p1", 1); err != nil {
		t.Fatalf("parent: %v", err)
	}
	if err := n.errs["idem"]; !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "pay") {
		t.Fatalf("the policyless child with an idempotent side effect: %v, want ErrConfig naming the step", err)
	}
	if recs, _ := n.j.History(ctx, "idem"); len(recs) != 0 {
		t.Errorf("the refused child wrote %d records", len(recs))
	}
}

// The marks a parent passes are its node's read set at the node's gating position: a retry-safe node whose body
// ran the child and failed is driven again, passes the same list, and the child (unfinished, so checked on
// every drive) accepts it, with one entry.
func TestFlowTaint_ARetrySafeNodeDrivenAgainPassesTheSameMarks(t *testing.T) {
	ctx := context.Background()
	n := &nested{t: t, j: agenttest.MemJournal(), errs: map[string]error{}}
	c := child(t, "c", single(), true, Approval(agent.SingleApproval()))
	attempts := 0
	b := New[int, int]("parent").WithTaintPolicy(*single())
	fetch := b.Step("fetch", func(_ context.Context, v int) (int, error) { return v, nil }, Untrusted("inbound-email"))
	call := b.Step("call", func(ctx context.Context, v int) (int, error) {
		attempts++
		n.runChild(ctx, c, "c1")
		if attempts == 1 {
			return 0, errors.New("the body fails once, after the child ran")
		}
		return v, nil
	}, Idempotent(), Approval(agent.SingleApproval()))
	b.Edge(fetch, call)
	p, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	for range 8 {
		_, err = p.Run(ctx, n.j, "p1", 1)
		var pend *agent.ApprovalPending
		switch {
		case errors.As(err, &pend):
			if err := agent.Approve(ctx, n.j, "p1", pend.ToolUseID, true); err != nil {
				t.Fatal(err)
			}
		case err != nil && !failed:
			failed = true // the body's failure: drive again
		default:
			err = nil
		}
		if err == nil || attempts >= 2 && !errors.As(err, &pend) {
			break
		}
	}
	if attempts < 2 {
		t.Fatalf("the body ran %d times, want it driven again", attempts)
	}
	if err := n.errs["c1"]; !isPendingAt(err, "node:pay") {
		t.Errorf("the child driven again: %v, want it to accept the marks and wait at its own gate", err)
	}
	if l := inherited(t, startOf(t, n.j, "c1")); len(l) != 1 || l[0]["source"] != "inbound-email" {
		t.Errorf("the child holds %v, want one entry: inbound-email", l)
	}
}

// A run that started inside a tainted run under a flow with no policy runs no node that can have a side effect on
// any drive: a flow redeployed with a node that now has one is refused before its read-only prefix runs.
func TestFlowTaint_ARedeployedFlowWithASideEffectIsRefusedOnEveryDrive(t *testing.T) {
	ctx := context.Background()
	n := &nested{t: t, j: agenttest.MemJournal(), errs: map[string]error{}}
	looks := 0
	mk := func(payReadOnly bool) *Flow[int, int] {
		b := New[int, int]("reads")
		look := b.Step("look", func(_ context.Context, v int) (int, error) { looks++; return v, nil }, ReadOnly(), Approval(agent.SingleApproval()))
		var opts []NodeOption
		if payReadOnly {
			opts = append(opts, ReadOnly())
		}
		pay := b.Step("pay", func(_ context.Context, v int) (int, error) { return v, nil }, opts...)
		b.Edge(look, pay)
		f, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	p := n.parent("inbound-email", single(), map[string]*Flow[int, int]{"c1": mk(true)})
	if _, err := p.Run(ctx, n.j, "p1", 1); err != nil {
		t.Fatalf("parent: %v", err)
	}
	if err := n.errs["c1"]; !isPendingAt(err, "node:look") || looks != 0 {
		t.Fatalf("the first drive: %v, look ran %d times; want a pause at look's gate before it ran", err, looks)
	}
	if err := agent.Approve(ctx, n.j, "c1", "node:look", true); err != nil {
		t.Fatal(err)
	}
	// The flow is redeployed with a node that can have a side effect after the read-only prefix.
	if _, err := mk(false).Run(ctx, n.j, "c1", 1); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "pay") {
		t.Errorf("the redeployed flow: %v, want ErrConfig naming the step", err)
	}
	if looks != 0 {
		t.Errorf("look ran %d times, want the read-only prefix not to run", looks)
	}
	// The flow that started it still drives it.
	if _, err := mk(true).Run(ctx, n.j, "c1", 1); err != nil || looks != 1 {
		t.Errorf("the original flow: %v, look ran %d times; want it to finish", err, looks)
	}
}

// A run that started under a policy of its own, driven by a flow that lost the policy, is refused before any node
// runs when the flow has a node that can have a side effect, whether the run started at the top or inside a tainted
// run; a flow of read-only nodes resumes.
func TestFlowTaint_AFlowThatLostItsPolicyIsRefusedBeforeAnyNodeRuns(t *testing.T) {
	ctx := context.Background()
	looks := 0
	mk := func(pol *agent.TaintPolicy, payReadOnly bool) *Flow[int, int] {
		b := New[int, int]("lost")
		if pol != nil {
			b.WithTaintPolicy(*pol)
		}
		look := b.Step("look", func(_ context.Context, v int) (int, error) { looks++; return v, nil }, ReadOnly(), Approval(agent.SingleApproval()))
		var opts []NodeOption
		if payReadOnly {
			opts = append(opts, ReadOnly())
		}
		pay := b.Step("pay", func(_ context.Context, v int) (int, error) { return v, nil }, opts...)
		b.Edge(look, pay)
		f, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	// A run at the top.
	j := agenttest.MemJournal()
	if _, err := mk(single(), true).Run(ctx, j, "r1", 1); !isPendingAt(err, "node:look") {
		t.Fatalf("the first drive: %v", err)
	}
	if err := agent.Approve(ctx, j, "r1", "node:look", true); err != nil {
		t.Fatal(err)
	}
	if _, err := mk(nil, false).Run(ctx, j, "r1", 1); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "pay") {
		t.Errorf("the flow that lost its policy: %v, want ErrConfig naming the step", err)
	}
	if looks != 0 {
		t.Errorf("look ran %d times, want no node to run", looks)
	}
	// With read-only nodes only it resumes.
	if _, err := mk(nil, true).Run(ctx, j, "r1", 1); err != nil || looks != 1 {
		t.Errorf("a flow of read-only nodes without the policy: %v, look ran %d times; want it to finish", err, looks)
	}
	// A run started under a policy of its own inside a tainted node, driven directly by the flow without it.
	n := &nested{t: t, j: agenttest.MemJournal(), errs: map[string]error{}}
	looks = 0
	p := n.parent("inbound-email", single(), map[string]*Flow[int, int]{"c1": mk(single(), true)})
	if _, err := p.Run(ctx, n.j, "p1", 1); err != nil {
		t.Fatalf("parent: %v", err)
	}
	if err := n.errs["c1"]; !isPendingAt(err, "node:look") {
		t.Fatalf("the nested first drive: %v", err)
	}
	if err := agent.Approve(ctx, n.j, "c1", "node:look", true); err != nil {
		t.Fatal(err)
	}
	if _, err := mk(nil, false).Run(ctx, n.j, "c1", 1); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "pay") {
		t.Errorf("the nested run driven by the flow that lost its policy: %v, want ErrConfig naming the step", err)
	}
	if looks != 0 {
		t.Errorf("look ran %d times in the nested run, want no node to run", looks)
	}
}
