package runview

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
)

// The viewer shows a gate that excludes the requester: before its tally it does not count the
// requester's signed approval or list them as pending, and after it the tally's exclusion, the
// requester's card, and the signature check against the recorded policy (exclusion included).
func TestSoD_ViewerShowsRequesterExcluded(t *testing.T) {
	ctx := agent.ContextWithIdentity(context.Background(), agent.Identity{OnBehalfOf: "ops"})
	j := agenttest.MemJournal()
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true}
	b := plan.New[int, string]("refunds").WithApproverVerifiers(approverKeys(nil))
	b.Step("refund", func(_ context.Context, n int) (string, error) { return "refunded", nil }, plan.Approval(&policy))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Run(ctx, j, "f1", 120)
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("Run = %v; want a pause", err)
	}
	sign := func(who string) {
		sig := ed25519.Sign(key(who), agent.ApprovalDecisionBytes(pend.Subject(), who, true, testDecidedAt))
		if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "f1", ToolUseID: pend.ToolUseID, ApproverID: who, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	sign("ops")
	sign("finance")

	v := viewOf(t, Handler(Options{Source: JournalSource{J: j}}), "f1")
	g := v.Gates[0]
	if !g.ExcludePrincipal || g.Requester != "ops" || !slices.Equal(g.Excluded, []string{"ops"}) {
		t.Fatalf("gate %+v; want ops excluded as the requester", g)
	}
	if !slices.Equal(g.ApprovedBy, []string{"finance"}) || !slices.Equal(g.Pending, []string{"risk"}) || !strings.HasPrefix(g.Status, "pending") {
		t.Fatalf("gate before the tally: approved %v, pending %v, status %q; want finance approved, risk pending", g.ApprovedBy, g.Pending, g.Status)
	}

	sign("risk")
	if _, err := f.Run(context.Background(), j, "f1", 120); err != nil {
		t.Fatalf("Run at quorum: %v", err)
	}
	th, err := audit.NewTreeHead(context.Background(), j, "f1", time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: key("log")})
	if err != nil {
		t.Fatal(err)
	}
	logPub := audit.Ed25519Verifier{Pub: key("log").Public().(ed25519.PublicKey)}
	h := Handler(Options{Source: JournalSource{J: j}, Provers: []Prover{HeadProver(sth, logPub, Approvals{Keys: approverKeys(nil)})}})
	v = viewOf(t, h, "f1")
	g = v.Gates[0]
	if !g.Tallied || !g.SignaturesOK || !strings.HasPrefix(g.Status, "approved") || !slices.Equal(g.Excluded, []string{"ops"}) || g.Requester != "ops" {
		t.Fatalf("gate after quorum %+v; want approved, signatures verified, ops excluded", g)
	}
	_, page := get(t, h, "/run?id=f1")
	for _, want := range []string{`a-excluded`, `excluded: the run&#39;s requester`, `excluding the requester (ops)`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not show %s", want)
		}
	}
}

// A config diff calls dropping a gate's exclusion of the requester a weakening, and adding it a
// strengthening.
func TestSoD_DiffExclusionIsAGateChange(t *testing.T) {
	with := &plan.TopologyGate{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true}
	without := &plan.TopologyGate{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	if got := gateChange(with, without); got != "weakened" {
		t.Fatalf("dropping the exclusion: %q, want weakened", got)
	}
	if got := gateChange(without, with); got != "strengthened" {
		t.Fatalf("adding the exclusion: %q, want strengthened", got)
	}
	if got := gateMove(without, with); got != "approval 2 of 3 -> 2 of 3 excluding the requester" {
		t.Fatalf("gateMove = %q", got)
	}
}
