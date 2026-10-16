package plan

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/identitygate"
	"github.com/bide-ai/bide/internal/journaltest"
)

// A gated plan flow node passes the same pre-execution check as a tool call, immediately before its body
// would run (agent.preExecute through journalhook.PreExec), and not when its recorded result replays.

// certStub is a certificate-bound approver for the flow tests: it accepts the bare signature the test
// approvers make (the count is not under test here) and answers the check before execution with exec.
type certStub struct {
	edVerifier
	id   string
	pub  ed25519.PublicKey
	exec *func(agent.CertDecision) agent.CertCheck
	n    *int
	bad  *bool // the status a gate holds is one thing: once set, the count fails too
}

func (certStub) Alg() agent.Alg              { return agent.AlgCertificate }
func (certStub) Verify(msg, sig []byte) bool { return false }
func (c certStub) KeyIDs() []string          { return []string{"ident:" + c.id} }
func (c certStub) VerifyCertDecision(cd agent.CertDecision) agent.CertCheck {
	if *c.bad {
		return agent.CertCheck{Reason: agent.ReasonRevoked, Detail: "d"}
	}
	at, err := agent.ParseTimestamp(cd.Record.DecidedAt())
	if err != nil || !ed25519.Verify(c.pub, agent.ApprovalDecisionBytes(cd.Subject, cd.Record.Approver(), cd.Record.Approved, at), cd.Record.Signature()) {
		return agent.CertCheck{Reason: agent.ReasonBadSig}
	}
	return agent.CertCheck{Identity: c.id, KeyHash: sha256.Sum256(c.pub), Tier: "final"}
}
func (c certStub) VerifyCertExec(cd agent.CertDecision) agent.CertCheck {
	*c.n++
	if *c.exec != nil {
		return (*c.exec)(cd)
	}
	return c.VerifyCertDecision(cd)
}

func certApprovers(ids ...string) (map[string]edApprover, agent.ApproverVerifierFor, *func(agent.CertDecision) agent.CertCheck, *int, *bool) {
	as, _ := approvers(ids...)
	var exec func(agent.CertDecision) agent.CertCheck
	var n int
	var bad bool
	vs := map[string]agent.ApproverVerifier{}
	for id, a := range as {
		vs[id] = certStub{id: id, pub: a.priv.Public().(ed25519.PublicKey), exec: &exec, n: &n, bad: &bad}
	}
	return as, func(id string) (agent.ApproverVerifier, bool) { v, ok := vs[id]; return v, ok }, &exec, &n, &bad
}

func TestFlowNode_PreExecuteIsOnTheWayToTheBody(t *testing.T) {
	was := identitygate.Enabled.Load()
	identitygate.Enabled.Store(true)
	defer identitygate.Enabled.Store(was)
	ctx := context.Background()
	as, vf, exec, n, bad := certApprovers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	_, err := f.Run(ctx, j, "r1", "order-42")
	pend := asPending(t, err)
	as["ops"].decide(t, j, pend, true)

	// A revocation lands between the count and the body: the node does not run, the gate holds and asks again.
	*exec = func(cd agent.CertDecision) agent.CertCheck {
		*bad = true
		return agent.CertCheck{Reason: agent.ReasonRevoked, Detail: "d"}
	}
	_, err = f.Run(ctx, j, "r1", "order-42")
	held := asPending(t, err)
	if held.Reason != agent.PendingExecHoldReapproval || refunds != 0 {
		t.Fatalf("reason %q refunds %d", held.Reason, refunds)
	}
	if _, ok, _ := j.Get(ctx, "r1", agent.ApprovalExecHoldStep("node:refund", 0)); !ok {
		t.Fatal("no exec hold")
	}

	// Stale status waits without a hold; current status runs the node on the same tally.
	// (Fresh gate: generation 1 needs a decision first, so use a new run.)
	refunds2 := 0
	f2 := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds2)
	*exec, *bad = nil, false
	_, err = f2.Run(ctx, j, "r2", "order-43")
	pend2 := asPending(t, err)
	as["ops"].decide(t, j, pend2, true)
	*exec = func(agent.CertDecision) agent.CertCheck { return agent.CertCheck{Reason: agent.ReasonStatusStale} }
	_, err = f2.Run(ctx, j, "r2", "order-43")
	if p := asPending(t, err); p.Reason != agent.PendingIdentityStatus || refunds2 != 0 {
		t.Fatalf("reason %q refunds %d", p.Reason, refunds2)
	}
	if _, ok, _ := j.Get(ctx, "r2", agent.ApprovalExecHoldStep("node:refund", 0)); ok {
		t.Fatal("a stale wait wrote a hold")
	}
	*exec = nil
	if _, err := f2.Run(ctx, j, "r2", "order-43"); err != nil || refunds2 != 1 {
		t.Fatalf("%v refunds %d", err, refunds2)
	}

	// The node's result is recorded: asking the gate again does not run the check (the node replays).
	before := *n
	if err := runGate(ctx, j, "r2", f2.core, f2.core.byName["refund"], "node:refund", refund{Order: "order-43", Amount: 120}); err != nil {
		t.Fatalf("runGate on a node that ran: %v", err)
	}
	if *n != before {
		t.Fatalf("the check ran for a node whose result is recorded (%d -> %d)", before, *n)
	}
	_ = journaltest.Put
	_ = errors.New
}
