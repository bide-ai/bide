package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
	"github.com/bide-ai/bide/internal/testca"
)

// The evidence of a certificate-bound gate that was held before execution and asked again: the
// superseded generation-0 tally and the exec hold travel with the generation-1 tally, in journal order,
// and the re-ask notes are disclosed.

func putValue(t *testing.T, c *certGate, name string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journaltest.Do(context.Background(), c.store, gateRun, name, func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Name: name, Result: b}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalEvidence_ExecHoldAndReask(t *testing.T) {
	c := newCertGate(t, 1, "alice", "bob")
	c.pause()
	c.decideCert("alice", nil)
	recs, _ := c.store.History(context.Background(), gateRun)
	var aliceStep string
	for _, r := range recs {
		if agent.IsApprovalDecision(r, "c1") {
			aliceStep = r.Name
		}
	}
	// What the gate recorded before the hold: the generation-0 count, then the hold of the pre-execution
	// check (written by hand here: the tool does not run in this journal until generation 1).
	put0 := agent.ApprovalTally{Need: 1, Approved: 1, Approvers: []string{"alice", "bob"}, ApprovedBy: []string{"alice"}, Records: []string{aliceStep}, DecisionFormat: agent.DecisionFormat}
	putValue(t, c, agent.ApprovalIdentTallyStep("c1", 0), put0)
	putValue(t, c, agent.ApprovalExecHoldStep("c1", 0), agent.ExecHold{Format: agent.ApprovalExecHoldFormat, ToolUseID: "c1", Reason: agent.ReasonRevoked, Detail: "d", ReasonCode: -1})
	// Alice's certificate is revoked back to before her decision became provable; bob decides.
	c.status = c.buildStatus(testca.Revoked(c.leaves["alice"].Cert, certA.Add(-time.Minute), 0))
	c.pause() // the gate asks again: it writes the generation-1 re-ask note
	c.decideCert("bob", nil)
	c.anchor(certA)
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	if c.charged != 1 {
		t.Fatalf("charged %d", c.charged)
	}
	acts, err := audit.ApprovalEvidence(context.Background(), c.store, gateRun, "c1", c.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	var tallies, steps []string
	for _, a := range acts {
		switch a.Kind {
		case audit.KindApprovalTally:
			tallies = append(tallies, a.Ref)
		case audit.KindStep:
			steps = append(steps, a.Ref)
		}
	}
	if want := []string{agent.ApprovalIdentTallyStep("c1", 0), agent.ApprovalIdentTallyStep("c1", 1)}; !slices.Equal(tallies, want) {
		t.Fatalf("tallies %v, want %v", tallies, want)
	}
	if !slices.Contains(steps, agent.ApprovalExecHoldStep("c1", 0)) {
		t.Fatalf("the exec hold is not disclosed: %v", steps)
	}
	if !slices.Contains(steps, agent.ApprovalIdentReaskStep("c1", 1)) {
		t.Fatalf("the re-ask note is not disclosed: %v", steps)
	}
	// The hold precedes the replacing tally; the superseded tally precedes the hold.
	pos := map[string]int{}
	for i, a := range acts {
		pos[a.Ref] = i
	}
	if !(pos[agent.ApprovalIdentTallyStep("c1", 0)] < pos[agent.ApprovalExecHoldStep("c1", 0)] && pos[agent.ApprovalExecHoldStep("c1", 0)] < pos[agent.ApprovalIdentTallyStep("c1", 1)]) {
		t.Fatalf("order %v", pos)
	}
	// Without the hold the generation-0 tally would read as the outcome: the evidence of the outcome is
	// the generation-1 tally.
	if got := tallies[len(tallies)-1]; got != agent.ApprovalIdentTallyStep("c1", 1) {
		t.Fatalf("outcome %s", got)
	}
}

func TestApprovalEvidence_BoundGateNamesItsTally(t *testing.T) {
	c := newCertGate(t, 1, "alice")
	c.pause()
	c.decideCert("alice", nil)
	c.anchor(certA)
	if err := c.run(); err != nil || c.charged != 1 {
		t.Fatalf("%v %d", err, c.charged)
	}
	acts, err := audit.ApprovalEvidence(context.Background(), c.store, gateRun, "c1", c.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range acts {
		if a.Kind == audit.KindApprovalTally {
			got = append(got, a.Ref)
		}
	}
	if !slices.Equal(got, []string{agent.ApprovalIdentTallyStep("c1", 0)}) {
		t.Fatalf("%v", got)
	}
}

// certLike makes a bare Ed25519 approver certificate-bound for the gate (it implements
// agent.CertDecisionVerifier, so the gate files its tally under the identity names), and checks the decision
// the way the bare-key verifier does: no chain, no status, the v3 signature over the call. It stands in for
// audit.CertApprover where a test needs only the gate's naming and the chain's evidence.
type certLike struct {
	id  string
	pub ed25519.PublicKey
}

func (certLike) Alg() agent.Alg                  { return agent.AlgCertificate }
func (certLike) Verify(message, sig []byte) bool { return false }
func (c certLike) KeyIDs() []string              { return []string{"ident:" + c.id} }
func (c certLike) VerifyCertExec(cd agent.CertDecision) agent.CertCheck { return c.VerifyCertDecision(cd) }
func (c certLike) VerifyCertDecision(cd agent.CertDecision) agent.CertCheck {
	decidedAt, err := agent.ParseTimestamp(cd.Record.DecidedAt())
	if err != nil || !ed25519.Verify(c.pub, agent.ApprovalDecisionBytes(cd.Subject, cd.Record.Approver(), cd.Record.Approved, decidedAt), cd.Record.Signature()) {
		return agent.CertCheck{Reason: agent.ReasonBadSig}
	}
	return agent.CertCheck{Identity: c.id, KeyHash: sha256.Sum256(c.pub), Tier: "final"}
}

// holdChainGate is a gate of bare-key approvers whose journal carries an identity tally chain written by
// hand (the verdict's recount is the bare-key one): generation 0 counted alice, an exec hold of
// generation 0 superseded it, bob's decision is counted at generation 1 and the tool ran on that.
// holdLate puts the hold after the generation-1 tally instead of before it.
func holdChainGate(t *testing.T, holdLate bool) *gate { return holdChainGateAt(t, holdLate, false) }

// holdChainGateAt also can put the hold before the tally it holds.
func holdChainGateAt(t *testing.T, holdLate, holdEarly bool) *gate {
	t.Helper()
	g := newGate(1, []string{"alice", "bob"}, []string{"alice", "bob"})
	g.custom = func(id string) (agent.ApproverVerifier, bool) {
		pub, ok := g.pubs[id]
		return certLike{id: id, pub: pub}, ok
	}
	wantPaused(t, g.run(), 0)
	g.decide(t, "alice", true, false)
	g.decide(t, "bob", true, false)
	recs, _ := g.store.History(context.Background(), gateRun)
	var steps []string
	for _, r := range recs {
		if agent.IsApprovalDecision(r, "c1") {
			steps = append(steps, r.Name)
		}
	}
	put := func(name string, v any) {
		b, _ := json.Marshal(v)
		if _, err := journaltest.Do(context.Background(), g.store, gateRun, name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Name: name, Result: b}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	hold := agent.ExecHold{Format: agent.ApprovalExecHoldFormat, ToolUseID: "c1", Reason: agent.ReasonRevoked, Detail: "d", ReasonCode: -1}
	t0 := agent.ApprovalTally{Need: 1, Approved: 1, Approvers: []string{"alice", "bob"}, ApprovedBy: []string{"alice"}, Pending: []string{"bob"}, Records: steps[:1], DecisionFormat: agent.DecisionFormat}
	t1 := agent.ApprovalTally{Need: 1, Approved: 2, Approvers: []string{"alice", "bob"}, ApprovedBy: []string{"alice", "bob"}, Records: steps, DecisionFormat: agent.DecisionFormat,
		Supersedes: agent.ApprovalIdentTallyStep("c1", 0)}
	if holdEarly {
		put(agent.ApprovalExecHoldStep("c1", 0), hold)
	}
	put(agent.ApprovalIdentTallyStep("c1", 0), t0)
	if !holdLate && !holdEarly {
		put(agent.ApprovalExecHoldStep("c1", 0), hold)
	}
	// The gate's re-ask note for generation 1, as the pause at the hold wrote it.
	put(agent.ApprovalIdentReaskStep("c1", 1), agent.ApprovalWait{ToolUseID: "c1", Need: 1, Approvers: []string{"alice", "bob"}, RecordedAt: "2026-10-16T04:26:58.000Z", Reask: agent.PendingExecHoldReapproval, Supersedes: agent.ApprovalIdentTallyStep("c1", 0)})
	put(agent.ApprovalIdentTallyStep("c1", 1), t1)
	// The identity record of generation 1, which the pre-execution check reads to find the counted decisions.
	history, _ := g.store.History(context.Background(), gateRun)
	for _, r := range history {
		if r.Name == agent.ApprovalIdentTallyStep("c1", 1) {
			sum := sha256.Sum256(r.Result)
			ir := agent.ApprovalIdentity{Format: agent.ApprovalIdentityFormat, ToolUseID: "c1", Generation: 1, Tally: r.Name, TallyDigest: hex.EncodeToString(sum[:])}
			for i, a := range []string{"alice", "bob"} {
				ir.Decisions = append(ir.Decisions, agent.IdentityDecision{Approver: a, Step: steps[i]})
			}
			put(agent.ApprovalIdentityStep("c1", 1), ir)
		}
	}
	if holdLate {
		put(agent.ApprovalExecHoldStep("c1", 0), hold)
	}
	if err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	return g
}

func TestVerifyApprovals_ExecHoldChain(t *testing.T) {
	g := holdChainGate(t, false)
	acts := evidenceFor(t, g)
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK {
		t.Fatalf("%+v %v", v, err)
	}
	if !slices.Equal(v.Counted, []string{"alice", "bob"}) {
		t.Fatalf("counted %v", v.Counted)
	}
	// The hold is part of the proof: dropping it from the evidence is not silently accepted as the
	// same outcome (the generation-0 tally then reads as the outcome, but the superseded tally and the
	// hold are proven only if disclosed), and a hold that is not proven under the head fails.
	noHold := without(acts, func(a audit.EvidenceAction) bool { return a.Ref == agent.ApprovalExecHoldStep("c1", 0) })
	v2, err := audit.VerifyApprovals(noHold, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err == nil && v2.OK && slices.Contains(v2.Counted, "bob") {
		t.Fatalf("without the hold the verdict still counts generation 1: %+v", v2)
	}
}

// A hold journaled after the tally that replaces it is out of order and is reported.
func TestVerifyApprovals_ExecHoldOutOfOrder(t *testing.T) {
	g := holdChainGate(t, true)
	v, err := audit.VerifyApprovals(evidenceFor(t, g), "c1", g.policy, g.resolver(), edV(g.logPub))
	if err := reportErr(v.OK, err); err != nil {
		t.Fatal(err)
	}
	if v.OK || !hasProblem(v, "exec hold") {
		t.Fatalf("%+v", v)
	}
}

// A hold journaled before the tally it holds is out of order too.
func TestVerifyApprovals_ExecHoldBeforeItsTally(t *testing.T) {
	g := holdChainGateAt(t, false, true)
	v, err := audit.VerifyApprovals(evidenceFor(t, g), "c1", g.policy, g.resolver(), edV(g.logPub))
	if err := reportErr(v.OK, err); err != nil {
		t.Fatal(err)
	}
	if v.OK || !hasProblem(v, "exec hold") {
		t.Fatalf("%+v", v)
	}
}

// The verdict reports the newest proven re-ask note, a certificate-bound gate's included.
func TestVerifyApprovals_ReportsTheIdentReask(t *testing.T) {
	g := holdChainGate(t, false)
	v, err := audit.VerifyApprovals(evidenceFor(t, g), "c1", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK {
		t.Fatalf("%+v %v", v, err)
	}
	if v.Reask != agent.PendingExecHoldReapproval || v.ReaskedAt != "2026-10-16T04:26:58.000Z" {
		t.Fatalf("reask %q at %q", v.Reask, v.ReaskedAt)
	}
}
