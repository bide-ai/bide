package runview

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
)

// TestFlowNodeGate views a plan flow paused at a gated node (waiting, the gate on the node, no
// decision yet), then after two signed approvals (approved, the node ran, the signatures verified
// from a signed head).
func TestFlowNodeGate(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	b := plan.New[int, string]("refunds").WithApproverVerifiers(approverKeys(nil))
	check := b.Step("check", func(_ context.Context, n int) (int, error) { return n, nil }, plan.ReadOnly())
	pay := b.Step("refund", func(_ context.Context, n int) (string, error) { return "refunded", nil }, plan.Approval(&policy))
	b.Edge(check, pay)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Run(ctx, j, "f1", 120)
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("Run = %v; want a pause", err)
	}

	v := viewOf(t, Handler(Options{Source: JournalSource{J: j}}), "f1")
	if v.State != "waiting" || len(v.Gates) != 1 {
		t.Fatalf("state %q, gates %+v; want waiting at one gate", v.State, v.Gates)
	}
	g := v.Gates[0]
	if g.ToolUseID != "node:refund" || g.ToolName != "refund" || !g.Waiting || g.Need != 2 || len(g.Approvers) != 3 {
		t.Fatalf("gate %+v; want node:refund waiting on 2 of 3", g)
	}
	if n := node(v, "refund"); n.Status != "waiting" || n.Gate != "2 of 3" || !strings.HasPrefix(n.GateStatus, "pending") || n.GateLine != "waiting, 0 of 2 approvals" {
		t.Fatalf("graph node %+v; want refund waiting at its 2 of 3 gate", n)
	}
	if !hasEvent(v.Events, "approval requested: refund (node:refund)") {
		t.Fatalf("no request event in %+v", v.Events)
	}

	for _, who := range []string{"ops", "risk"} {
		sig := ed25519.Sign(key(who), agent.ApprovalDecisionBytes(pend.Subject(), who, true, testDecidedAt))
		if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "f1", ToolUseID: pend.ToolUseID, ApproverID: who, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Run(ctx, j, "f1", 120); err != nil {
		t.Fatalf("Run at quorum: %v", err)
	}
	th, err := audit.NewTreeHead(ctx, j, "f1", time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: key("log")})
	if err != nil {
		t.Fatal(err)
	}
	logPub := audit.Ed25519Verifier{Pub: key("log").Public().(ed25519.PublicKey)}
	v = viewOf(t, Handler(Options{Source: JournalSource{J: j}, Provers: []Prover{HeadProver(sth, logPub, Approvals{Keys: approverKeys(nil)})}}), "f1")
	g = v.Gates[0]
	if g.Waiting || !g.Tallied || !g.SignaturesOK || !strings.HasPrefix(g.Status, "approved") {
		t.Fatalf("gate after quorum %+v; want approved, tallied, signatures verified", g)
	}
	_, page := get(t, Handler(Options{Source: JournalSource{J: j}, Provers: []Prover{HeadProver(sth, logPub, Approvals{Keys: approverKeys(nil)})}}), "/run?id=f1")
	for _, want := range []string{`class="ngate good">approved 2 of 3</text>`, `class="ngate good">signatures verified</text>`, `class="gatehead good"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not show %s", want)
		}
	}
	if n := node(v, "refund"); n.Status != "done" || !strings.HasPrefix(n.GateStatus, "approved") || n.GateLine != "approved 2 of 3" || n.SigLine != "signatures verified" {
		t.Fatalf("graph node %+v; want refund done, its gate approved", n)
	}
}

// hasEvent reports whether an event has title.
func hasEvent(es []Event, title string) bool {
	for _, e := range es {
		if e.Title == title {
			return true
		}
	}
	return false
}
