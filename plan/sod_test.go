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

// Separation of duties on a plan flow node's gate: the requester is the flow run's principal,
// journaled in its run:principal and run:start from the identity its context carries
// (agent.ContextWithIdentity).

var opsFinRisk = []string{"ops", "finance", "risk"}

func sodGate(need int) *agent.ApprovalPolicy {
	return &agent.ApprovalPolicy{Need: need, Approvers: opsFinRisk, ExcludePrincipal: true}
}

func asRequester(id string) context.Context {
	return agent.ContextWithIdentity(context.Background(), agent.Identity{OnBehalfOf: id})
}

// The requester's approval never counts on a flow node's gate; the node runs on two approvals
// from the others, and the journaled tally names the requester excluded.
func TestSoDFlow_RequesterExcluded(t *testing.T) {
	as, vf := approvers(opsFinRisk...)
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, sodGate(2), vf, &refunds)
	ctx := asRequester("ops")

	_, err := f.Run(ctx, j, "r1", "order-42")
	pend := asPending(t, err)
	as["ops"].decide(t, j, pend, true)
	as["finance"].decide(t, j, pend, true)

	_, err = f.Run(ctx, j, "r1", "order-42")
	pend = asPending(t, err)
	if refunds != 0 || pend.Quorum == nil || pend.Quorum.Approved != 1 || !slices.Equal(pend.Quorum.Excluded, []string{"ops"}) {
		t.Fatalf("refunds = %d, quorum = %+v; want the requester's approval not counted", refunds, pend.Quorum)
	}

	as["risk"].decide(t, j, pend, true)
	// A resume with no identity runs under the journaled principal.
	if _, err := f.Run(context.Background(), j, "r1", "order-42"); err != nil {
		t.Fatalf("Run at quorum without the requester: %v", err)
	}
	if refunds != 1 {
		t.Fatalf("refunds = %d, want 1", refunds)
	}
	rec, ok, err := j.Get(context.Background(), "r1", agent.ApprovalTallyStep("node:refund"))
	if err != nil || !ok {
		t.Fatalf("tally: %v %v", ok, err)
	}
	var tally agent.ApprovalTally
	if err := json.Unmarshal(rec.Result, &tally); err != nil {
		t.Fatal(err)
	}
	if !tally.ExcludePrincipal || !slices.Equal(tally.Requesters, []string{"ops"}) || !slices.Equal(tally.ApprovedBy, []string{"finance", "risk"}) {
		t.Fatalf("tally = %+v; want finance and risk counted, ops excluded", tally)
	}
}

// A flow run records the principal of its context's identity in run:start, and a later drive
// under another principal is refused (ErrConfig), as an agent run's is.
func TestSoDFlow_PrincipalJournaledAndHeld(t *testing.T) {
	_, vf := approvers(opsFinRisk...)
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, sodGate(2), vf, &refunds)
	_, err := f.Run(asRequester("ops"), j, "r1", "order-42")
	asPending(t, err)
	start, ok, err := agent.RecordedStart(context.Background(), j, "r1")
	if err != nil || !ok || start.Principal == nil || start.Principal.OnBehalfOf != "ops" {
		t.Fatalf("run:start = %+v (%v, %v); want principal ops", start, ok, err)
	}
	_, err = f.Run(asRequester("finance"), j, "r1", "order-42")
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "principal") {
		t.Fatalf("Run under another principal = %v; want ErrConfig naming the principal", err)
	}
}

// Fail closed: a flow run with no principal cannot pass a gate that excludes the requester.
func TestSoDFlow_NoPrincipalIsConfigError(t *testing.T) {
	_, vf := approvers(opsFinRisk...)
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, sodGate(2), vf, &refunds)
	_, err := f.Run(context.Background(), j, "r1", "order-42")
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "requester") {
		t.Fatalf("Run = %v; want ErrConfig naming the requester", err)
	}
	if refunds != 0 {
		t.Fatal("the gated node ran")
	}
}

// A config "approval" block's exclude_principal lowers to the gate, shows in the topology, and
// changes the flow's digest (a paused run cannot resume under a flow whose gate dropped it).
func TestSoDFlow_ConfigExcludePrincipal(t *testing.T) {
	_, vf := approvers(opsFinRisk...)
	load := func(block string) *Flow[int, int] {
		t.Helper()
		cfg := strings.Replace(approvalNodeConfig, "APPROVAL", block, 1)
		f, err := Load[int, int]([]byte(cfg), approvalRegistry(t), WithLoadedApproverVerifiers(vf))
		if err != nil {
			t.Fatalf("Load %s: %v", block, err)
		}
		return f
	}
	with := load(`{"need": 2, "approvers": ["ops", "finance", "risk"], "exclude_principal": true}`)
	without := load(`{"need": 2, "approvers": ["ops", "finance", "risk"]}`)
	if g := with.Topology().Nodes[0].Gate; g == nil || !g.ExcludePrincipal {
		t.Fatalf("gate = %+v; want ExcludePrincipal", g)
	}
	if g := without.Topology().Nodes[0].Gate; g == nil || g.ExcludePrincipal {
		t.Fatalf("gate = %+v; want no exclusion", g)
	}
	if with.Digest() == without.Digest() {
		t.Fatal("the digest does not commit to exclude_principal")
	}
	if p := with.core.nodes[0].approval; p == nil || !p.ExcludePrincipal {
		t.Fatalf("lowered policy = %+v; want ExcludePrincipal", p)
	}
	// exclude_principal must be a boolean.
	cfg := strings.Replace(approvalNodeConfig, "APPROVAL", `{"need": 2, "approvers": ["ops", "finance"], "exclude_principal": "yes"}`, 1)
	if _, err := Load[int, int]([]byte(cfg), approvalRegistry(t), WithLoadedApproverVerifiers(vf)); err == nil {
		t.Fatal("Load accepted a non-boolean exclude_principal")
	}
}

// "excluding the requester" holds only when every gate on the path excludes the requester.
func TestSoDFlow_AssertExcludingRequester(t *testing.T) {
	gated := strings.Replace(payConfig, `"else": "charge"`, `"else": "review"`, 1)
	sod := strings.Replace(gated, `"approvers": ["ops", "finance", "risk"]}`, `"approvers": ["ops", "finance", "risk"], "exclude_principal": true}`, 1)
	line := "every path to charge passes approval >= 2 of 3 excluding the requester"
	if res, _ := verdict(t, withAsserts(t, sod, line), nil); !res.Holds {
		t.Fatalf("with exclusion: %+v; want it to hold", res)
	}
	res, _ := verdict(t, withAsserts(t, gated, line), nil)
	if res.Holds || res.Invalid || !slices.Equal(res.Path, []string{"intake", "review", "charge"}) {
		t.Fatalf("without exclusion: %+v; want violated through review", res)
	}
	if !strings.Contains(res.Message, "excluding the requester") {
		t.Fatalf("message %q does not say what the gate lacks", res.Message)
	}
	// The plain form still holds for a gate that excludes the requester.
	if res, _ := verdict(t, withAsserts(t, sod, "every path to charge passes approval >= 2 of 3"), nil); !res.Holds {
		t.Fatalf("plain form on an excluding gate: %+v", res)
	}
}

// A flow run recorded with no principal (as every flow run before this version was) resumes under a
// context that now carries an identity: the drive is not refused, and the run keeps no principal, so
// a gate that excludes the requester still refuses it.
func TestSoDFlow_NoPrincipalRunResumesUnderOne(t *testing.T) {
	_, vf := approvers(opsFinRisk...)
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 2, Approvers: opsFinRisk}, vf, &refunds)
	_, err := f.Run(context.Background(), j, "r1", "order-42")
	asPending(t, err)
	_, err = f.Run(asRequester("ops"), j, "r1", "order-42")
	asPending(t, err)
	start, ok, err := agent.RecordedStart(context.Background(), j, "r1")
	if err != nil || !ok || start.Principal != nil {
		t.Fatalf("run:start = %+v (%v, %v); want it to keep no principal", start, ok, err)
	}
}

// A flow run binds its principal chain for the runs its nodes start: an agent run started inside a
// node records the flow run's principal as its ancestor, so a gate in it excludes the flow's
// requester too.
func TestSoDFlow_NodeRunsRecordTheFlowAsAncestor(t *testing.T) {
	j := agenttest.MemJournal()
	inner := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("ok")), j)
	b := New[int, int]("outer")
	b.Step("work", func(ctx context.Context, n int) (int, error) {
		_, err := inner.Run(agent.ContextWithIdentity(ctx, agent.Identity{OnBehalfOf: "desk"}), "inner", agent.UserText("go"))
		return n, err
	}, ReadOnly())
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(asRequester("ops"), j, "flow-1", 1); err != nil {
		t.Fatal(err)
	}
	recs, err := j.History(context.Background(), "inner")
	if err != nil {
		t.Fatal(err)
	}
	rp, ok := agent.RecordedPrincipal(recs)
	if !ok || !slices.Equal(rp.Requesters(), []string{"ops", "desk"}) {
		t.Fatalf("inner run's run:principal = %+v (%v); want desk under ops", rp, ok)
	}
}
