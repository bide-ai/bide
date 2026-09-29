package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// chargeModel calls the "charge" tool until a tool result is in the conversation, then
// answers. It is stateless, so a resumed run (which replays the journaled tool turn) needs no
// per-run script.
type chargeModel struct{}

func (chargeModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	if hasToolResult(req.Messages) {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "charged"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	} else {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "charge", ArgsFragment: json.RawMessage(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func hasToolResult(msgs []agent.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if _, ok := p.(agent.ToolResult); ok {
				return true
			}
		}
	}
	return false
}

// kofnRun is a run whose 2-of-3 gated "charge" call proceeded, plus the keys an offline
// auditor holds: each approver's decision key and the log operator's STH key.
type kofnRun struct {
	store     agent.Durable
	policy    *agent.ApprovalPolicy
	approvers map[string]ed25519.PublicKey // every registered decision key, eligible or not
	logPub    ed25519.PublicKey
	logPriv   ed25519.PrivateKey
	sth       audit.SignedTreeHead
	charged   int
}

// buildKofnRun drives a real agent through a Need=2 of {alice, bob, carol} gate. Before the
// gate can pass, mallory (a registered key, but not in the eligible set) records a validly
// signed approval and carol records an approval whose signature is tampered; neither counts.
// alice then approves (still paused at 1 of 2), bob approves, and the resumed run executes the
// tool. One STH is signed over the finished journal.
func buildKofnRun(t *testing.T) *kofnRun {
	t.Helper()
	ctx := context.Background()
	const runID = "run-kofn"
	k := &kofnRun{
		store:     agent.NewMemStore(),
		policy:    &agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob", "carol"}},
		approvers: map[string]ed25519.PublicKey{},
	}
	signers := map[string]audit.Ed25519Signer{}
	for _, id := range []string{"alice", "bob", "carol", "mallory"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		k.approvers[id] = pub
		signers[id] = audit.Ed25519Signer{Priv: priv}
	}
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		pub, ok := k.approvers[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	}
	decide := func(id string, tamper bool) {
		t.Helper()
		sig, err := signers[id].Sign(agent.ApprovalDecisionBytes(runID, "c1", id, true))
		if err != nil {
			t.Fatalf("sign %s: %v", id, err)
		}
		if tamper {
			sig[0] ^= 0xff
		}
		if err := agent.ApproveAs(ctx, k.store, runID, "c1", id, true, sig); err != nil {
			t.Fatalf("ApproveAs %s: %v", id, err)
		}
	}
	run := func() error {
		charge := agent.Func("charge", "charge the card", agent.Safety{Approval: k.policy},
			func(context.Context, struct{}) (string, error) { k.charged++; return "ok", nil })
		_, err := agent.New(chargeModel{}, k.store, charge).WithApproverVerifiers(verifiers).Run(ctx, runID, "pay")
		return err
	}
	wantPaused := func(err error, approved int) {
		t.Helper()
		var pend *agent.PendingApproval
		if !errors.As(err, &pend) || pend.Quorum == nil {
			t.Fatalf("err = %v, want an m-of-n *PendingApproval", err)
		}
		if pend.Quorum.Approved != approved || k.charged != 0 {
			t.Fatalf("paused with %d approved (charged %d), want %d approved and no charge", pend.Quorum.Approved, k.charged, approved)
		}
	}

	wantPaused(run(), 0)
	decide("mallory", false) // valid signature, ineligible approver
	decide("carol", true)    // eligible approver, tampered signature
	wantPaused(run(), 0)
	decide("alice", false)
	wantPaused(run(), 1)
	decide("bob", false)
	if err := run(); err != nil {
		t.Fatalf("run at quorum: %v", err)
	}
	if k.charged != 1 {
		t.Fatalf("charge ran %d times, want 1", k.charged)
	}

	k.logPub, k.logPriv, _ = ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, k.store, runID, 1700000000)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	k.sth = audit.SignTreeHead(th, k.logPriv)
	return k
}

// countEligibleSigned is the offline auditor's tally: from the disclosed decision records it
// counts distinct approvers that are in the policy's eligible set, approved, decided before
// the action, and whose signature verifies under that approver's key over
// agent.ApprovalDecisionBytes. It uses only the evidence and out-of-band keys, never the store.
func countEligibleSigned(t *testing.T, acts []audit.EvidenceAction, policy *agent.ApprovalPolicy, keys map[string]ed25519.PublicKey, logPub ed25519.PublicKey) (counted []string) {
	t.Helper()
	action := acts[len(acts)-1]
	if action.Kind != "tool" {
		t.Fatalf("last evidence action kind %q, want tool", action.Kind)
	}
	eligible := map[string]bool{}
	for _, a := range policy.Approvers {
		eligible[a] = true
	}
	seen := map[string]bool{}
	for _, a := range acts[:len(acts)-1] {
		if ok, err := a.Bundle.Verify(logPub); err != nil || !ok {
			continue
		}
		r := a.Bundle.Record
		if a.Kind != "approval" || r.Kind != agent.StepApproval || r.ToolUseID != action.Bundle.Record.ToolUseID {
			continue
		}
		if !eligible[r.Approver] || seen[r.Approver] || !r.Approved {
			continue
		}
		if a.Bundle.Inclusion.Index >= action.Bundle.Inclusion.Index {
			continue
		}
		pub, ok := keys[r.Approver]
		if !ok || !ed25519.Verify(pub, agent.ApprovalDecisionBytes(a.Bundle.RunID, r.ToolUseID, r.Approver, r.Approved), r.Signature) {
			continue
		}
		seen[r.Approver] = true
		counted = append(counted, r.Approver)
	}
	return counted
}

// TestMofnEvidence_KofNVerifiesOffline: a 2-of-3 gate proceeds after two eligible signed
// approvals; under ONE STH, the tool action and every decision prove included, all bind to the
// STH size, the decisions precede the action in journal order, and an offline tally counts
// exactly alice and bob: carol's tampered signature and mallory's ineligible approval are
// rejected. Editing a disclosed signature breaks the inclusion proof itself.
func TestMofnEvidence_KofNVerifiesOffline(t *testing.T) {
	ctx := context.Background()
	k := buildKofnRun(t)

	candidates := []string{"alice", "bob", "carol", "mallory"}
	acts, err := audit.ApprovalEvidence(ctx, k.store, "run-kofn", "c1", candidates, k.sth)
	if err != nil {
		t.Fatalf("ApprovalEvidence: %v", err)
	}
	if len(acts) != len(candidates)+1 {
		t.Fatalf("got %d evidence actions, want %d decisions + the action", len(acts), len(candidates))
	}
	action := acts[len(acts)-1]
	if action.Kind != "tool" || action.Bundle.Record.Kind != agent.StepToolResult || action.Bundle.Record.ToolUseID != "c1" {
		t.Fatalf("last evidence action is not the c1 tool result: %+v", action)
	}
	for i, a := range acts {
		if ok, err := a.Bundle.Verify(k.logPub); err != nil || !ok {
			t.Fatalf("evidence %d (%s) failed to verify (ok=%v err=%v)", i, a.Label, ok, err)
		}
		if !reflect.DeepEqual(a.Bundle.STH, k.sth) || a.Bundle.Inclusion.Size != k.sth.Size {
			t.Fatalf("evidence %d (%s) not bound to the one STH (size %d)", i, a.Label, k.sth.Size)
		}
		if i < len(acts)-1 && a.Bundle.Inclusion.Index >= action.Bundle.Inclusion.Index {
			t.Fatalf("decision %s at index %d does not precede the action at %d", a.Label, a.Bundle.Inclusion.Index, action.Bundle.Inclusion.Index)
		}
	}

	// The same evidence also matches ProveApproval for each decision.
	for _, a := range acts[:len(acts)-1] {
		pb, err := audit.ProveApproval(ctx, k.store, "run-kofn", "c1", a.Label, k.sth)
		if err != nil {
			t.Fatalf("ProveApproval %s: %v", a.Label, err)
		}
		if pb.Inclusion.Index != a.Bundle.Inclusion.Index {
			t.Fatalf("ProveApproval %s index %d, ApprovalEvidence index %d", a.Label, pb.Inclusion.Index, a.Bundle.Inclusion.Index)
		}
	}

	counted := countEligibleSigned(t, acts, k.policy, k.approvers, k.logPub)
	if len(counted) < k.policy.Need {
		t.Fatalf("offline tally counted %v, want at least Need=%d", counted, k.policy.Need)
	}
	if len(counted) != 2 || counted[0] != "alice" || counted[1] != "bob" {
		t.Fatalf("offline tally counted %v, want exactly [alice bob]", counted)
	}

	// Tampering with a disclosed signature is caught twice: the record no longer matches its
	// committed leaf, and the forged signature does not verify under the approver's key.
	forged := acts[0]
	forged.Bundle.Record.Signature = append([]byte(nil), forged.Bundle.Record.Signature...)
	forged.Bundle.Record.Signature[0] ^= 0xff
	if ok, _ := forged.Bundle.Verify(k.logPub); ok {
		t.Fatal("bundle with a tampered approver signature still verified")
	}
	if n := countEligibleSigned(t, []audit.EvidenceAction{forged, action}, k.policy, k.approvers, k.logPub); len(n) != 0 {
		t.Fatalf("tampered decision counted: %v", n)
	}

	// Swapping an ineligible approver in for an eligible one does not reach Need.
	swapped := []audit.EvidenceAction{acts[0], acts[3], action} // alice + mallory
	if acts[3].Label != "mallory" {
		t.Fatalf("expected mallory at position 3, got %s", acts[3].Label)
	}
	if n := countEligibleSigned(t, swapped, k.policy, k.approvers, k.logPub); len(n) >= k.policy.Need {
		t.Fatalf("ineligible approver helped reach Need: counted %v", n)
	}

}

// TestMofnEvidence_PackageAndTallyStep: the k-of-n evidence appends directly to an
// EvidencePackage built over the same STH, the package verifies as a whole, and the gate's
// final "approval-tally:c1" step proves as a single record reporting Approved >= Need.
func TestMofnEvidence_PackageAndTallyStep(t *testing.T) {
	ctx := context.Background()
	k := buildKofnRun(t)

	tallyName := "approval-tally:c1"
	pkg, err := audit.Evidence(ctx, k.store, "run-kofn", k.logPriv, 1700000000,
		audit.WithLabel("2-of-3 charge approval"),
		audit.WithToolCall("c1"),
		audit.WithStep(tallyName),
	)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if !reflect.DeepEqual(pkg.STH, k.sth) {
		t.Fatal("Evidence minted a different STH than the audited one; approval proofs would not share its root")
	}
	decisions, err := audit.ApprovalEvidence(ctx, k.store, "run-kofn", "c1", k.policy.Approvers, pkg.STH)
	if err != nil {
		t.Fatalf("ApprovalEvidence: %v", err)
	}
	pkg.Actions = append(pkg.Actions, decisions[:len(decisions)-1]...) // the action is already packaged

	// Round-trip through JSON, as an auditor receiving the file would.
	raw, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got audit.EvidencePackage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	report, err := got.Verify(k.logPub)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK {
		t.Fatalf("package did not verify: %+v", report.Items)
	}
	kinds := map[string]int{}
	for _, it := range report.Items {
		kinds[it.Kind]++
	}
	if kinds["tool"] != 1 || kinds["step"] != 1 || kinds["approval"] != 3 {
		t.Fatalf("report kinds = %v, want 1 tool, 1 step, 3 approval", kinds)
	}

	// The final tally is itself one provable record.
	pb, err := audit.ProveStep(ctx, k.store, "run-kofn", tallyName, k.sth)
	if err != nil {
		t.Fatalf("ProveStep %s: %v", tallyName, err)
	}
	if ok, err := pb.Verify(k.logPub); err != nil || !ok {
		t.Fatalf("tally bundle failed to verify (ok=%v err=%v)", ok, err)
	}
	var tally agent.ApprovalTally
	if err := json.Unmarshal(pb.Record.Result, &tally); err != nil {
		t.Fatalf("decode tally: %v", err)
	}
	if tally.Need != 2 || tally.Approved != 2 || tally.Denied != 0 {
		t.Fatalf("journaled tally = %+v, want Need=2 Approved=2 Denied=0", tally)
	}
	if len(tally.Pending) != 1 || tally.Pending[0] != "carol" {
		t.Fatalf("journaled tally Pending = %v, want [carol] (her tampered decision did not count)", tally.Pending)
	}
}
