package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/identitygate"
)

// Enforcement of flow taint (docs/design/flow-taint.md, A2) through whole runs of a flow. The guard that
// refuses a flow with taint declarations is turned off for these tests (allowTaint); it is removed with the
// last part of the enforcement.

func allowTaint(t *testing.T) {
	t.Helper()
	taintGuardOff = true
	t.Cleanup(func() { taintGuardOff = false })
}

// taintRun is a flow of fetch (a source of untrusted content), then pay (a side effect), then notify (a read),
// with counters of how many times each body ran.
type taintRun struct {
	t     *testing.T
	f     *Flow[int, int]
	j     *agent.Journal
	runs  map[string]int
	built *Builder[int, int]
}

type flowOpts struct {
	policy    *agent.TaintPolicy
	fetch     []NodeOption // options of the source
	pay       []NodeOption
	notify    []NodeOption
	verifiers agent.ApproverVerifierFor
}

func newTaintRun(t *testing.T, o flowOpts) *taintRun {
	t.Helper()
	allowTaint(t)
	r := &taintRun{t: t, j: agenttest.MemJournal(), runs: map[string]int{}}
	r.f = r.build(o)
	return r
}

func (r *taintRun) build(o flowOpts) *Flow[int, int] {
	r.t.Helper()
	step := func(name string) func(context.Context, int) (int, error) {
		return func(_ context.Context, n int) (int, error) { r.runs[name]++; return n, nil }
	}
	b := New[int, int]("f")
	if o.policy != nil {
		b.WithTaintPolicy(*o.policy)
	}
	if o.verifiers != nil {
		b.WithApproverVerifiers(o.verifiers)
	}
	fetch := b.Step("fetch", step("fetch"), append([]NodeOption{Untrusted("inbound-email")}, o.fetch...)...)
	pay := b.Step("pay", step("pay"), o.pay...)
	notify := b.Step("notify", step("notify"), append([]NodeOption{ReadOnly()}, o.notify...)...)
	b.Edge(fetch, pay)
	b.Edge(pay, notify)
	f, err := b.Build()
	if err != nil {
		r.t.Fatal(err)
	}
	return f
}

func (r *taintRun) run(opts ...agent.RunOption) (int, error) {
	return r.f.Run(context.Background(), r.j, "r1", 1, opts...)
}

func pending(t *testing.T, err error) *agent.ApprovalPending {
	t.Helper()
	var p *agent.ApprovalPending
	if !errors.As(err, &p) {
		t.Fatalf("err = %v, want a pause at a gate", err)
	}
	return p
}

func single() *agent.TaintPolicy { return &agent.TaintPolicy{Gate: agent.SingleApproval()} }

func TestFlowTaint_AGatedSideEffectAfterASourceWaitsAtTheEscalationGate(t *testing.T) {
	r := newTaintRun(t, flowOpts{policy: single()})
	_, err := r.run()
	p := pending(t, err)
	if p.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q, want node:pay:taint", p.ToolUseID)
	}
	if r.runs["fetch"] != 1 || r.runs["pay"] != 0 {
		t.Fatalf("bodies %v: fetch should have run and pay not", r.runs)
	}
	recs, _ := r.j.History(context.Background(), "r1")
	var haveReason, haveNode bool
	for _, rec := range recs {
		haveReason = haveReason || rec.Name == "gate-reason:node%3Apay"
		haveNode = haveNode || rec.Name == "node:pay"
	}
	if !haveReason || haveNode {
		t.Errorf("gate-reason %v, node record %v; want the reason and no node record", haveReason, haveNode)
	}
	// Approving the instance lets the node run once; the flow completes.
	if err := agent.Approve(context.Background(), r.j, "r1", "node:pay:taint", true); err != nil {
		t.Fatal(err)
	}
	if out, err := r.run(); err != nil || out != 1 {
		t.Fatalf("after approval: %d, %v", out, err)
	}
	if r.runs["pay"] != 1 || r.runs["notify"] != 1 {
		t.Errorf("bodies %v, want pay and notify once", r.runs)
	}
	// A replay of the finished run runs nothing and asks nothing.
	if _, err := r.run(); err != nil || r.runs["pay"] != 1 {
		t.Errorf("replay: %v, bodies %v", err, r.runs)
	}
}

func TestFlowTaint_ADenialOfTheEscalationGateStopsTheNode(t *testing.T) {
	r := newTaintRun(t, flowOpts{policy: single()})
	_, err := r.run()
	pending(t, err)
	if err := agent.Approve(context.Background(), r.j, "r1", "node:pay:taint", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.run(); !errors.Is(err, ErrApprovalDenied) {
			t.Fatalf("drive %d: %v, want ErrApprovalDenied", i, err)
		}
	}
	if r.runs["pay"] != 0 {
		t.Errorf("pay ran %d times after a denial", r.runs["pay"])
	}
}

func TestFlowTaint_TheNodesOwnGateComesFirstThenTheEscalationGate(t *testing.T) {
	own := agent.SingleApproval()
	r := newTaintRun(t, flowOpts{policy: single(), pay: []NodeOption{Approval(own)}})
	ctx := context.Background()
	_, err := r.run()
	if p := pending(t, err); p.ToolUseID != "node:pay" {
		t.Fatalf("first pause at %q, want the node's own gate", p.ToolUseID)
	}
	if err := agent.Approve(ctx, r.j, "r1", "node:pay", true); err != nil {
		t.Fatal(err)
	}
	_, err = r.run()
	if p := pending(t, err); p.ToolUseID != "node:pay:taint" {
		t.Fatalf("second pause at %q, want the escalation gate", p.ToolUseID)
	}
	if r.runs["pay"] != 0 {
		t.Fatal("pay ran with only its own gate passed")
	}
	if err := agent.Approve(ctx, r.j, "r1", "node:pay:taint", true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(); err != nil || r.runs["pay"] != 1 {
		t.Fatalf("after both: %v, pay ran %d", err, r.runs["pay"])
	}
}

func TestFlowTaint_ReadOnlyExemptAndUntaintedNodesAreNotGated(t *testing.T) {
	// A read-only node after a source, an exempt side effect, and a side effect before any source: none waits.
	r := newTaintRun(t, flowOpts{policy: single(), pay: []NodeOption{TaintExempt("reviewed")}})
	if out, err := r.run(); err != nil || out != 1 {
		t.Fatalf("exempt side effect: %d, %v", out, err)
	}
	if r.runs["pay"] != 1 || r.runs["notify"] != 1 {
		t.Errorf("bodies %v", r.runs)
	}
	recs, _ := r.j.History(context.Background(), "r1")
	for _, rec := range recs {
		if strings.HasPrefix(rec.Name, "approval-pending:") {
			t.Errorf("a gate was asked for: %q", rec.Name)
		}
	}
	// The exemption is recorded for the audit, with the taint reason.
	var found bool
	for _, rec := range recs {
		found = found || rec.Name == "gate-reason:node%3Apay"
	}
	if !found {
		t.Error("no gate-reason record for the exempt node")
	}
	// An Idempotent node is a side effect for taint: it is gated, though it writes no attempt claim.
	r = newTaintRun(t, flowOpts{policy: single(), pay: []NodeOption{Idempotent()}})
	_, err := r.run()
	if p := pending(t, err); p.ToolUseID != "node:pay:taint" || r.runs["pay"] != 0 {
		t.Errorf("an Idempotent node: paused at %q, pay ran %d", p.ToolUseID, r.runs["pay"])
	}
}

func TestFlowTaint_NoPolicyGatesNothing(t *testing.T) {
	// Declarations alone mean nothing without a policy: the run does not start under taint.
	r := newTaintRun(t, flowOpts{})
	if out, err := r.run(); err != nil || out != 1 || r.runs["pay"] != 1 {
		t.Fatalf("%d, %v, bodies %v", out, err, r.runs)
	}
	recs, _ := r.j.History(context.Background(), "r1")
	for _, rec := range recs {
		if strings.HasPrefix(rec.Name, "gate-reason:") || strings.HasPrefix(rec.Name, "taint-mark:") {
			t.Errorf("a taint record in a run with no policy: %q", rec.Name)
		}
	}
}

func TestFlowTaint_RefuseStopsTheNodeAndLeavesItNoRecord(t *testing.T) {
	r := newTaintRun(t, flowOpts{policy: &agent.TaintPolicy{Refuse: true}})
	for i := 0; i < 2; i++ {
		_, err := r.run()
		if !errors.Is(err, ErrTaintRefused) || !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "inbound-email") || !strings.Contains(err.Error(), "pay") {
			t.Fatalf("drive %d: %v, want ErrTaintRefused naming the node and the source", i, err)
		}
	}
	if r.runs["pay"] != 0 {
		t.Errorf("pay ran %d times", r.runs["pay"])
	}
	recs, _ := r.j.History(context.Background(), "r1")
	for _, rec := range recs {
		switch {
		case rec.Name == "node:pay" || strings.HasPrefix(rec.Name, "approval") || rec.Name == "attempt:step:node:pay":
			t.Errorf("a refused node left %q", rec.Name)
		case rec.Name == "run:complete":
			t.Error("the run completed")
		}
	}
	// The run stays unfinished until it is cancelled.
	if err := agent.Cancel(context.Background(), r.j, "r1", "test"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestFlowTaint_TheRunsUntrustedInputTaintsTheFirstNode(t *testing.T) {
	// pay is the entry here, so only the run's input can taint it.
	allowTaint(t)
	ran := 0
	b := New[int, int]("f").WithTaintPolicy(*single())
	b.Step("pay", func(_ context.Context, n int) (int, error) { ran++; return n, nil })
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Without a statement the input is not declared untrusted: the node runs ungated.
	if _, err := f.Run(ctx, agenttest.MemJournal(), "r0", 1); err != nil || ran != 1 {
		t.Fatalf("no statement: %v, ran %d", err, ran)
	}
	j := agenttest.MemJournal()
	_, err = f.Run(ctx, j, "r1", 1, agent.WithUntrustedInput("mail"))
	if p := pending(t, err); p.ToolUseID != "node:pay:taint" || ran != 1 {
		t.Fatalf("paused at %q, ran %d", p.ToolUseID, ran)
	}
	// A drive that states another source is refused; one that states none runs under the recorded one.
	if _, err := f.Run(ctx, j, "r1", 1, agent.WithUntrustedInput("web")); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("another source: %v, want ErrConfig", err)
	}
	if _, err := f.Run(ctx, j, "r1", 1); pending(t, err).ToolUseID != "node:pay:taint" {
		t.Error("a resume stating nothing did not stay at the gate")
	}
	// A trusted input taints nothing.
	if _, err := f.Run(ctx, agenttest.MemJournal(), "r2", 1, agent.WithTrustedInput("ours")); err != nil || ran != 2 {
		t.Errorf("trusted input: %v, ran %d", err, ran)
	}
}

func TestFlowTaint_ResumeUnderOtherDeclarationsRunsNoNode(t *testing.T) {
	ctx := context.Background()
	r := newTaintRun(t, flowOpts{policy: single()})
	_, err := r.run()
	pending(t, err)
	before := map[string]int{}
	for k, v := range r.runs {
		before[k] = v
	}
	other := map[string]flowOpts{
		"a different gate":    {policy: &agent.TaintPolicy{Gate: &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}}, verifiers: func(string) (agent.ApproverVerifier, bool) { return nil, false }},
		"Refuse for the gate": {policy: &agent.TaintPolicy{Refuse: true}},
		"a new exemption":     {policy: single(), pay: []NodeOption{TaintExempt("trust me")}},
		"a dropped source":    {policy: single(), fetch: nil},
	}
	for what, o := range other {
		if what == "a dropped source" {
			// The source is declared by the helper; build a flow whose fetch declares nothing instead.
			allowTaint(t)
			b := New[int, int]("f").WithTaintPolicy(*single())
			fetch := b.Step("fetch", func(_ context.Context, n int) (int, error) { r.runs["fetch"]++; return n, nil })
			pay := b.Step("pay", func(_ context.Context, n int) (int, error) { r.runs["pay"]++; return n, nil })
			notify := b.Step("notify", func(_ context.Context, n int) (int, error) { r.runs["notify"]++; return n, nil }, ReadOnly())
			b.Edge(fetch, pay)
			b.Edge(pay, notify)
			f, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Run(ctx, r.j, "r1", 1); !errors.Is(err, agent.ErrConfig) {
				t.Errorf("%s: %v, want ErrConfig", what, err)
			}
			continue
		}
		f := r.build(o)
		if _, err := f.Run(ctx, r.j, "r1", 1); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", what, err)
		}
	}
	for k, v := range r.runs {
		if v != before[k] {
			t.Errorf("a body ran under other declarations: %s %d -> %d", k, before[k], v)
		}
	}
	// The flow it started with still resumes.
	if err := agent.Approve(ctx, r.j, "r1", "node:pay:taint", true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(); err != nil {
		t.Fatalf("the original flow: %v", err)
	}
}

func TestFlowTaint_AFlowWithNoPolicyRefusesASideEffectOfARunThatStartedUnderTaint(t *testing.T) {
	ctx := context.Background()
	r := newTaintRun(t, flowOpts{policy: single()})
	_, err := r.run()
	pending(t, err)
	if err := agent.Approve(ctx, r.j, "r1", "node:pay:taint", true); err != nil {
		t.Fatal(err)
	}
	policyless := r.build(flowOpts{})
	_, err = policyless.Run(ctx, r.j, "r1", 1)
	if !errors.Is(err, agent.ErrConfig) || errors.Is(err, ErrTaintRefused) || r.runs["pay"] != 0 {
		t.Fatalf("a policy-less drive: %v, pay ran %d; want ErrConfig and no body", err, r.runs["pay"])
	}
}

func TestFlowTaint_RequireProvenance(t *testing.T) {
	allowTaint(t)
	pol := &agent.TaintPolicy{Gate: agent.SingleApproval(), RequireProvenance: true}
	// A node with no declaration is a Build error naming it.
	b := New[int, int]("f").WithTaintPolicy(*pol)
	a := b.Step("a", func(_ context.Context, n int) (int, error) { return n, nil }, Trusted("ours"))
	bb := b.Step("b", func(_ context.Context, n int) (int, error) { return n, nil })
	b.Edge(a, bb)
	if _, err := b.Build(); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("an undeclared node: %v, want ErrConfig naming b", err)
	}
	// With every node declared, a new run must state its input's provenance, and nothing is written when it does not.
	b = New[int, int]("f").WithTaintPolicy(*pol)
	b.Step("a", func(_ context.Context, n int) (int, error) { return n, nil }, Trusted("ours"))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	j := agenttest.MemJournal()
	if _, err := f.Run(ctx, j, "r1", 1); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "WithUntrustedInput") {
		t.Fatalf("a run stating nothing: %v", err)
	}
	if recs, _ := j.History(ctx, "r1"); len(recs) != 0 {
		t.Errorf("the refused run wrote %d records", len(recs))
	}
	if _, err := f.Run(ctx, j, "r1", 1, agent.WithTrustedInput("the host's own")); err != nil {
		t.Fatalf("a run stating its input: %v", err)
	}
}

func TestFlowTaint_AnM_of_nEscalationGateNeedsVerifiers(t *testing.T) {
	allowTaint(t)
	b := New[int, int]("f").WithTaintPolicy(agent.TaintPolicy{Gate: &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}})
	b.Step("a", func(_ context.Context, n int) (int, error) { return n, nil })
	if _, err := b.Build(); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "approver verifiers") {
		t.Fatalf("Build = %v, want ErrConfig naming the verifiers", err)
	}
}

func TestFlowTaint_ALoopBodyAsksOncePerIteration(t *testing.T) {
	allowTaint(t)
	b := New[int, int]("loop").WithTaintPolicy(*single())
	seed := b.Step("seed", func(_ context.Context, n int) (int, error) { return n, nil }, Untrusted("feed"))
	refine := b.Step("refine", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	check := b.Step("check", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	done := b.Step("done", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	b.Edge(seed, refine)
	b.Edge(refine, check)
	b.Switch(check, LoopBack(3, func(n int) bool { return n < 3 }, refine), Else(done))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	j := agenttest.MemJournal()
	var asked []string
	for i := 0; i < 8; i++ {
		_, err := f.Run(ctx, j, "r1", 0)
		var p *agent.ApprovalPending
		if !errors.As(err, &p) {
			if err != nil {
				t.Fatalf("drive %d: %v", i, err)
			}
			break
		}
		asked = append(asked, p.ToolUseID)
		if err := agent.Approve(ctx, j, "r1", p.ToolUseID, true); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"node:refine:taint"}
	// The head of the loop is refine: its first iteration runs under the iteration key.
	if len(asked) < 2 || !strings.HasSuffix(asked[0], "refine:taint") || asked[0] == asked[1] {
		t.Fatalf("asked at %v (e.g. %v), want one instance per iteration", asked, want)
	}
	for _, id := range asked {
		if !strings.HasPrefix(id, "node:iter:") {
			t.Errorf("a loop body node asked at %q, want an iteration key", id)
		}
	}
}

// The escalation gate has the pre-execution status check of a gate with certificate-bound approvers, on the
// way to the node's body, as the node's own gate has (TestFlowNode_PreExecuteIsOnTheWayToTheBody).
func TestFlowTaint_PreExecuteOfTheInstanceIsOnTheWayToTheBody(t *testing.T) {
	was := identitygate.Enabled.Load()
	identitygate.Enabled.Store(true)
	defer identitygate.Enabled.Store(was)
	ctx := context.Background()
	as, vf, exec, _, bad := certApprovers("ops")
	r := newTaintRun(t, flowOpts{policy: &agent.TaintPolicy{Gate: &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}}, verifiers: vf})
	_, err := r.run()
	pend := asPending(t, err)
	if pend.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q", pend.ToolUseID)
	}
	as["ops"].decide(t, r.j, pend, true)
	// A revocation lands between the count and the body: the node does not run, and the instance's gate holds
	// and asks again.
	*exec = func(agent.CertDecision) agent.CertCheck {
		*bad = true
		return agent.CertCheck{Reason: agent.ReasonRevoked, Detail: "d"}
	}
	_, err = r.run()
	held := asPending(t, err)
	if held.ToolUseID != "node:pay:taint" || held.Reason != agent.PendingExecHoldReapproval || r.runs["pay"] != 0 {
		t.Fatalf("held at %q reason %q, pay ran %d", held.ToolUseID, held.Reason, r.runs["pay"])
	}
	if _, ok, _ := r.j.Get(ctx, "r1", agent.ApprovalExecHoldStep("node:pay:taint", 0)); !ok {
		t.Fatal("no exec hold on the instance")
	}
}

// A refusal comes before any gate: no approval is asked for a node that will not run.
func TestFlowTaint_RefusalComesBeforeTheNodesOwnGate(t *testing.T) {
	r := newTaintRun(t, flowOpts{policy: &agent.TaintPolicy{Refuse: true}, pay: []NodeOption{Approval(agent.SingleApproval())}})
	_, err := r.run()
	if !errors.Is(err, ErrTaintRefused) {
		t.Fatalf("%v, want ErrTaintRefused, not a pause at the node's own gate", err)
	}
	recs, _ := r.j.History(context.Background(), "r1")
	for _, rec := range recs {
		if strings.HasPrefix(rec.Name, "approval-") {
			t.Errorf("a gate was asked for before the refusal: %q", rec.Name)
		}
	}
}

// A node whose result is recorded replays and runs no pre-execution check, the escalation gate's included: a
// certificate revoked after the flow ran does not hold a node that already ran.
func TestFlowTaint_ReplayOfARecordedNodeRunsNoPreExecuteOfTheInstance(t *testing.T) {
	was := identitygate.Enabled.Load()
	identitygate.Enabled.Store(true)
	defer identitygate.Enabled.Store(was)
	ctx := context.Background()
	as, vf, exec, n, bad := certApprovers("ops")
	r := newTaintRun(t, flowOpts{policy: &agent.TaintPolicy{Gate: &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}}, verifiers: vf})
	_, err := r.run()
	pend := asPending(t, err)
	as["ops"].decide(t, r.j, pend, true)
	if _, err := r.run(); err != nil || r.runs["pay"] != 1 {
		t.Fatalf("the flow did not run to completion: %v, pay ran %d", err, r.runs["pay"])
	}
	// The certificate is revoked afterwards. The node's record is there: asking its gates again checks nothing.
	*exec = func(agent.CertDecision) agent.CertCheck {
		*bad = true
		return agent.CertCheck{Reason: agent.ReasonRevoked, Detail: "d"}
	}
	before := *n
	ts := &taintState{flow: r.f.core.flowTaint(), standing: 2}
	node := r.f.core.byName["pay"]
	if err := runGate(ctx, r.j, "r1", r.f.core, ts, node, "node:pay", 1); err != nil {
		t.Fatalf("runGate on a node that ran: %v", err)
	}
	if *n != before {
		t.Fatalf("the pre-execution check ran for a node whose result is recorded (%d -> %d)", before, *n)
	}
	if _, ok, _ := r.j.Get(ctx, "r1", agent.ApprovalExecHoldStep("node:pay:taint", 0)); ok {
		t.Error("a hold was written for the instance of a node that ran")
	}
}

// A run that started with no taint declarations is resumed by a flow that now has a policy requiring provenance
// without being asked where its input came from: the requirement is for a new run only.
func TestFlowTaint_RequireProvenanceIsForNewRunsOnly(t *testing.T) {
	allowTaint(t)
	ctx := context.Background()
	j := agenttest.MemJournal()
	own := agent.SingleApproval()
	build := func(pol *agent.TaintPolicy) *Flow[int, int] {
		b := New[int, int]("f")
		if pol != nil {
			b.WithTaintPolicy(*pol)
		}
		b.Step("pay", func(_ context.Context, n int) (int, error) { return n, nil }, Approval(own), Trusted("ours"))
		f, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	// Started with no policy (as v0.25.x flows did), paused at the node's own gate.
	_, err := build(nil).Run(ctx, j, "r1", 1)
	pending(t, err)
	// Driven by the same flow with a policy that requires provenance: not asked.
	req := &agent.TaintPolicy{Gate: agent.SingleApproval(), RequireProvenance: true}
	_, err = build(req).Run(ctx, j, "r1", 1)
	if errors.Is(err, agent.ErrConfig) && strings.Contains(err.Error(), "WithUntrustedInput") {
		t.Fatalf("a resume of a run that started with no declarations was asked for its input's provenance: %v", err)
	}
	// A new run still is.
	if _, err := build(req).Run(ctx, agenttest.MemJournal(), "r2", 1); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "WithUntrustedInput") {
		t.Fatalf("a new run stating nothing: %v", err)
	}
}
