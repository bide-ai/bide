package runview

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
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

// Probe: a tally on the instance that predates the signed decision format would make its gate ask again,
// unless the call has run: the instance's result is its call's, so a call that ran leaves no re-ask. The
// journal is edited to carry such a tally, as one written by an older binary would.
func TestBuild_EscalationInstanceOfACallThatRanAsksNothingAgain(t *testing.T) {
	ctx := context.Background()
	f := taintfixture.New(t, nil)
	_ = f.Drive()
	for _, id := range []string{"esc1", "esc2"} {
		if err := f.Submit("c2#taint", "c2#taint", id, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Drive(); err != nil || f.Wrote != 1 {
		t.Fatalf("drive = %v, writes %d", err, f.Wrote)
	}
	ran := editedJournal(t, ctx, f, agent.ApprovalTallyStep("c2#taint"))
	for _, g := range Build(ctx, taintfixture.Run, ran, nil).Gates {
		if g.Reapproval {
			t.Errorf("gate %s asks again although its call ran", g.ToolUseID)
		}
	}
	// Without the call's result the same tally does make the instance ask again: the edit is effective.
	var before []agent.Record
	for _, r := range ran {
		if r.Kind == agent.StepToolResult && r.ToolUseID == "c2" {
			break
		}
		before = append(before, r)
	}
	asks := false
	for _, g := range Build(ctx, taintfixture.Run, before, nil).Gates {
		asks = asks || g.ToolUseID == "c2#taint" && g.Reapproval
	}
	if !asks {
		t.Error("the edited tally does not make the instance ask again before its call ran: the fixture proves nothing")
	}
}

// editedJournal is the fixture's history with the record named name stripped of its decision format.
func editedJournal(t *testing.T, ctx context.Context, f *taintfixture.F, name string) []agent.Record {
	t.Helper()
	ms := agent.NewMemStore()
	for e, err := range f.Journal.Store().Load(ctx, taintfixture.Run, -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		if e.Name == name {
			var rec map[string]any
			if err := json.Unmarshal(data, &rec); err != nil {
				t.Fatal(err)
			}
			delete(rec["result"].(map[string]any), "decision_format")
			data, _ = json.Marshal(rec)
		}
		if _, _, err := ms.Insert(ctx, taintfixture.Run, e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	j, err := agent.NewJournal(ms)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := j.History(ctx, taintfixture.Run)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// Probe: a plan flow node named foo#taint is a gated node of its own: the viewer lists that one gate, waiting
// on its own policy, and no escalation instance of a node foo.
func TestBuild_AFlowNodeNamedLikeAnInstanceIsANode(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "risk"}}
	b := plan.New[int, string]("pay").WithApproverVerifiers(approverKeys(nil))
	b.Step("foo#taint", func(_ context.Context, n int) (string, error) { return "ok", nil }, plan.Approval(&policy))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	var pend *agent.ApprovalPending
	if _, err := f.Run(ctx, j, "f1", 1); !errors.As(err, &pend) {
		t.Fatalf("Run = %v, want a pause", err)
	}
	recs, err := j.History(ctx, "f1")
	if err != nil {
		t.Fatal(err)
	}
	gs := Build(ctx, "f1", recs, nil).Gates
	if len(gs) != 1 || gs[0].ToolUseID != "node:foo#taint" || !gs[0].Waiting || gs[0].Need != 2 || !slices.Equal(gs[0].Approvers, []string{"ops", "risk"}) {
		t.Fatalf("gates = %+v, want the one waiting node gate node:foo#taint, 2 of [ops risk]", gs)
	}
}

// Probe: bide view (the handler) serves the escalation instance as a gate of its own, in the JSON view and
// on the page, with the escalation gate's approvers.
func TestHandler_ServesTheEscalationInstance(t *testing.T) {
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	_ = f.Drive()
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	_ = f.Drive()
	h := Handler(Options{Source: JournalSource{J: f.Journal}})
	v := viewOf(t, h, taintfixture.Run)
	var esc *Gate
	for i := range v.Gates {
		if v.Gates[i].ToolUseID == "c2#taint" {
			esc = &v.Gates[i]
		}
	}
	if esc == nil || !esc.Waiting || esc.Need != 2 || !slices.Equal(esc.Approvers, []string{"esc1", "esc2"}) {
		t.Fatalf("gates = %+v, want c2#taint waiting on 2 of [esc1 esc2]", v.Gates)
	}
	if len(esc.Why) != 1 || esc.Why[0].Kind != "taint" || esc.Why[0].Source != "crm" {
		t.Errorf("the instance's reasons = %+v, want the taint reason from crm", esc.Why)
	}
	for _, g := range v.Gates {
		if g.ToolUseID == "c2" && len(g.Why) != 0 {
			t.Errorf("the call's own gate carries reasons %+v", g.Why)
		}
	}
	_, page := get(t, h, "/run?id="+taintfixture.Run)
	for _, want := range []string{"why this gate", "untrusted content from crm", "not part of what an approver signs"} {
		if !strings.Contains(page, want) {
			t.Errorf("the run page does not show %q", want)
		}
	}
	for _, want := range []string{"c2#taint", "esc1", "esc2"} {
		if !strings.Contains(page, want) {
			t.Errorf("the run page does not show %q", want)
		}
	}
}

// Probe: a flow with nodes foo and foo#taint, both gated, lists each gate once: the pause note of node
// foo#taint is not the note of an escalation instance of node foo.
func TestBuild_NodesFooAndFooTaintAreEachListedOnce(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	policy := agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops", "risk"}}
	b := plan.New[int, int]("pay").WithApproverVerifiers(approverKeys(nil))
	step := func(n int) func(context.Context, int) (int, error) {
		return func(_ context.Context, v int) (int, error) { return v + n, nil }
	}
	foo := b.Step("foo", step(1), plan.Approval(&policy))
	bar := b.Step("foo#taint", step(2), plan.Approval(&policy))
	b.Edge(foo, bar)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	var pend *agent.ApprovalPending
	if _, err := f.Run(ctx, j, "f1", 1); !errors.As(err, &pend) || pend.ToolUseID != "node:foo" {
		t.Fatalf("Run = %v, want a pause at node:foo", err)
	}
	sig := ed25519.Sign(key("ops"), agent.ApprovalDecisionBytes(pend.Subject(), "ops", true, testDecidedAt))
	if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "f1", ToolUseID: pend.ToolUseID, ApproverID: "ops", Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "f1", 1); !errors.As(err, &pend) || pend.ToolUseID != "node:foo#taint" {
		t.Fatalf("second Run = %v, want a pause at node:foo#taint", err)
	}
	recs, err := j.History(ctx, "f1")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, g := range Build(ctx, "f1", recs, nil).Gates {
		ids = append(ids, g.ToolUseID)
	}
	if !slices.Equal(ids, []string{"node:foo", "node:foo#taint"}) {
		t.Fatalf("gates = %v, want node:foo and node:foo#taint once each", ids)
	}
}

// Probe: a cap reason in the gate-reason record shows on the call's own gate (and the taint reason stays on
// the instance), with this call's figures labelled as of the ask, on the JSON view and on the page.
func TestHandler_ShowsACapReasonOnTheCallsOwnGate(t *testing.T) {
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	_ = f.Drive()
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	_ = f.Drive()
	j := f.WithCapReason()
	h := Handler(Options{Source: JournalSource{J: j}})
	v := viewOf(t, h, taintfixture.Run)
	var own, esc Gate
	for _, g := range v.Gates {
		switch g.ToolUseID {
		case "c2":
			own = g
		case "c2#taint":
			esc = g
		}
	}
	if len(own.Why) != 1 || own.Why[0].Kind != "cap" || own.Why[0].Rule != "refund_one" || own.Why[0].Amount == nil || *own.Why[0].Amount != 600000 || own.Why[0].Limit != 500000 {
		t.Fatalf("the own gate's reasons = %+v, want the cap reason with its figures", own.Why)
	}
	if len(esc.Why) != 1 || esc.Why[0].Kind != "taint" {
		t.Fatalf("the instance's reasons = %+v, want the taint reason alone", esc.Why)
	}
	_, page := get(t, h, "/run?id="+taintfixture.Run)
	if !strings.Contains(page, "cap refund_one: this call 6000.00 USD, limit 5000.00 USD (over, gate)") {
		t.Errorf("the page lacks the cap line:\n%s", page)
	}
}

// The escalation instance of a gated plan flow node ("node:refund:taint", docs/design/flow-taint.md) is a gate
// of its own in the viewer, as a tool call's is: it has its policy and decisions, it waits until decided, and
// it is done once the node it guards has run. The engine writes no instance yet, so the test drives the
// instance's gate through the flow hook.
func TestBuild_EscalationInstanceOfAFlowNodeIsAGateOfItsOwn(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	own := agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	b := plan.New[int, string]("refunds").WithApproverVerifiers(approverKeys(nil))
	b.Step("refund", func(_ context.Context, n int) (string, error) { return "refunded", nil }, plan.Approval(&own))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	var pend *agent.ApprovalPending
	if _, err := f.Run(ctx, j, "f1", 120); !errors.As(err, &pend) {
		t.Fatalf("Run = %v, want a pause", err)
	}
	nodePend := pend
	const inst = "node:refund:taint"
	esc := &agent.ApprovalPolicy{Need: 1, Approvers: []string{"risk"}}
	if _, err := journalhook.Gate(ctx, j, "f1", inst, "refund", json.RawMessage(`120`), esc, approverKeys(nil)); !errors.As(err, &pend) {
		t.Fatalf("the instance's first evaluation = %v, want a pause", err)
	}
	gates := func() map[string]Gate {
		t.Helper()
		recs, err := j.History(ctx, "f1")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]Gate{}
		for _, g := range Build(ctx, "f1", recs, nil).Gates {
			m[g.ToolUseID] = g
		}
		return m
	}
	m := gates()
	ownG, escG := m["node:refund"], m[inst]
	if ownG.ToolUseID == "" || escG.ToolUseID == "" {
		t.Fatalf("gates = %+v, want both node:refund and %s", m, inst)
	}
	if ownG.ToolName != "refund" || ownG.Need != 1 || !slices.Equal(ownG.Approvers, []string{"ops"}) || !ownG.Waiting {
		t.Errorf("the node's gate = %+v", ownG)
	}
	if escG.ToolName != "refund" || escG.Need != 1 || !slices.Equal(escG.Approvers, []string{"risk"}) || !escG.Waiting {
		t.Errorf("the instance's gate = %+v, want refund, 1 of [risk], waiting", escG)
	}
	// The instance's result is its node's: once the node has a record, the instance no longer waits, though
	// it has no decision of its own.
	recs, err := j.History(ctx, "f1")
	if err != nil {
		t.Fatal(err)
	}
	ran := append(recs[:len(recs):len(recs)], agent.Record{Name: "node:refund", Kind: agent.StepValue, Result: json.RawMessage(`"refunded"`)})
	for _, g := range Build(ctx, "f1", ran, nil).Gates {
		if g.ToolUseID == inst && g.Waiting {
			t.Errorf("the instance still waits after its node's record: %+v", g)
		}
	}
	// Decide both, run the flow: neither gate waits, and the instance's result is the node's.
	for _, d := range []struct {
		p        *agent.ApprovalPending
		key, who string
	}{{pend, inst, "risk"}, {nodePend, "node:refund", "ops"}} {
		sig := ed25519.Sign(key(d.who), agent.ApprovalDecisionBytes(d.p.Subject(), d.who, true, testDecidedAt))
		if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "f1", ToolUseID: d.key, ApproverID: d.who, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := journalhook.Gate(ctx, j, "f1", inst, "refund", json.RawMessage(`120`), esc, approverKeys(nil)); err != nil || !ok {
		t.Fatalf("the instance's gate after its decision = %v, %v", ok, err)
	}
	if _, err := f.Run(ctx, j, "f1", 120); err != nil {
		t.Fatal(err)
	}
	for id, g := range gates() {
		if g.Waiting {
			t.Errorf("gate %s still waits after the node ran: %+v", id, g)
		}
	}
	// Each gate lists the decisions recorded under its own subject only.
	m = gates()
	for _, d := range m[inst].Decisions {
		if d.Approver == "ops" {
			t.Errorf("the node's approver is listed on the instance: %+v", d)
		}
	}
	for _, d := range m["node:refund"].Decisions {
		if d.Approver == "risk" {
			t.Errorf("the instance's approver is listed on the node: %+v", d)
		}
	}
}

// A node with no gate of its own, whose only gate is the escalation instance, is listed as a gate in the viewer:
// the instance has its own request, and its decisions and tally.
func TestBuild_EscalationInstanceOfAnUngatedNodeIsAGate(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	const inst = "node:refund:taint"
	esc := &agent.ApprovalPolicy{Need: 1, Approvers: []string{"risk"}}
	var pend *agent.ApprovalPending
	if _, err := journalhook.Gate(ctx, j, "f1", inst, "refund", json.RawMessage(`120`), esc, approverKeys(nil)); !errors.As(err, &pend) {
		t.Fatalf("the instance's first evaluation = %v, want a pause", err)
	}
	gate := func() (Gate, bool) {
		t.Helper()
		recs, err := j.History(ctx, "f1")
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range Build(ctx, "f1", recs, nil).Gates {
			if g.ToolUseID == inst {
				return g, true
			}
		}
		return Gate{}, false
	}
	g, ok := gate()
	if !ok || g.ToolName != "refund" || g.Need != 1 || !slices.Equal(g.Approvers, []string{"risk"}) || !g.Waiting {
		t.Fatalf("the instance's gate = %+v, %v; want refund, 1 of [risk], waiting", g, ok)
	}
	sig := ed25519.Sign(key("risk"), agent.ApprovalDecisionBytes(pend.Subject(), "risk", true, testDecidedAt))
	if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "f1", ToolUseID: inst, ApproverID: "risk", Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
		t.Fatal(err)
	}
	if ok, err := journalhook.Gate(ctx, j, "f1", inst, "refund", json.RawMessage(`120`), esc, approverKeys(nil)); err != nil || !ok {
		t.Fatalf("the gate after its decision = %v, %v", ok, err)
	}
	g, ok = gate()
	if !ok || g.Waiting || !g.Tallied || len(g.Decisions) != 1 || g.Decisions[0].Approver != "risk" {
		t.Errorf("the instance's gate after its decision = %+v, %v; want tallied with risk's decision", g, ok)
	}
}
