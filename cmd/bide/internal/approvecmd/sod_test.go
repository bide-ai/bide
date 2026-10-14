package approvecmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// Separation of duties from the terminal: ops requested the run, so bide approve refuses ops's
// decision (it would not count), lists no command for ops, counts finance alone as 1 of 2, and only
// with risk's approval reports the gate's approvals in; the real gate agrees.
func TestSoD_EndToEnd(t *testing.T) {
	pol := &agent.ApprovalPolicy{Need: 2, Approvers: approvers, ExcludePrincipal: true}
	f := newFixtureID(t, pol, refundArgs, audit.AlgEd25519, "ops")

	code, out, errs := f.run_("", "-list", "-approver-keys", f.keysJSON)
	if code != 0 {
		t.Fatalf("-list = %d: %s", code, errs)
	}
	contains(t, "-list", out, "excluding the requester (separation of duties): requested by ops; cannot approve: ops", "pending: finance, risk", "-as finance", "-as risk")
	if strings.Contains(out, "-as ops") {
		t.Errorf("-list offers ops, the requester, a command:\n%s", out)
	}

	code, out, errs = f.run_("", f.as("ops", "-yes")...)
	if code == 0 || !strings.Contains(errs+out, "requester") || !strings.Contains(errs+out, "would not count") {
		t.Fatalf("ops (the requester) = %d; want a refusal saying the decision would not count\n%s%s", code, out, errs)
	}
	if n := len(f.decisions()); n != 0 {
		t.Fatalf("%d decisions recorded after the refusal, want 0", n)
	}

	code, out, errs = f.run_("", f.as("finance", "-yes")...)
	if code != 0 {
		t.Fatalf("finance = %d\n%s%s", code, out, errs)
	}
	contains(t, "finance", out, "verified tally: 1 of 2 needed approved by finance", "still pending: risk")
	if strings.Contains(out, "the gate has its approvals") {
		t.Errorf("gate declared passed on finance alone:\n%s", out)
	}
	if _, err := f.drive(pol, false); err == nil {
		t.Fatal("the real gate passed on finance alone")
	}

	code, out, errs = f.run_("", f.as("risk", "-yes")...)
	if code != 0 {
		t.Fatalf("risk = %d\n%s%s", code, out, errs)
	}
	contains(t, "risk", out, "verified tally: 2 of 2 needed approved by finance, risk", "the gate has its approvals")
	if _, err := f.drive(pol, false); err != nil || f.charges.Load() != 1 {
		t.Fatalf("drive at quorum = %v, charges %d", err, f.charges.Load())
	}
}

// A decision the requester recorded another way (agent.SubmitDecision, validly signed with ops's
// key) is never counted by bide approve's tally: with finance's approval the gate is at 1 of 2,
// with or without the approvers' keys, and -list still waits on risk.
func TestSoD_RequesterDecisionNotCounted(t *testing.T) {
	pol := &agent.ApprovalPolicy{Need: 2, Approvers: approvers, ExcludePrincipal: true}
	f := newFixtureID(t, pol, refundArgs, audit.AlgEd25519, "ops")
	sg, err := LoadSigner(f.keys["ops"])
	if err != nil {
		t.Fatal(err)
	}
	j, closeJ := f.store()
	recs, err := j.History(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	_, call, _ := agent.FindToolCall(recs, f.call)
	at := time.Now()
	sig, err := sg.Sign(agent.ApprovalDecisionBytes(agent.ApprovalSubject{RunID: f.run, ToolUseID: f.call, ToolName: call.Name, Args: call.Args}, "ops", true, at))
	if err != nil {
		t.Fatal(err)
	}
	err = agent.SubmitDecision(context.Background(), j, agent.Decision{RunID: f.run, ToolUseID: f.call, ApproverID: "ops", Approved: true, DecidedAt: at, Alg: sg.Alg(), Signature: sig})
	closeJ()
	if err != nil {
		t.Fatal(err)
	}
	code, out, errs := f.run_("", f.as("finance", "-yes")...)
	if code != 0 {
		t.Fatalf("finance = %d\n%s%s", code, out, errs)
	}
	contains(t, "finance", out, "verified tally: 1 of 2 needed approved by finance", "still pending: risk")
	code, out, _ = f.run_("", "-list", "-approver-keys", f.keysJSON)
	if code != 0 || !strings.Contains(out, "pending: risk") || strings.Contains(out, "approvals verified") {
		t.Fatalf("-list = %d:\n%s", code, out)
	}
	code, out, _ = f.run_("", "-list")
	if code != 0 || !strings.Contains(out, "pending: risk") || strings.Contains(out, "approved by ops") {
		t.Fatalf("-list unverified = %d:\n%s", code, out)
	}
	if code, out, errs := f.run_("", f.asUnverified("ops", "-yes")...); code == 0 {
		t.Fatalf("ops unverified = 0, want a refusal\n%s%s", out, errs)
	}
}

// Without the approvers' keys, bide approve counts the requester's seat out of the gate: with ops
// excluded from 2 of 3, finance's denial alone settles it (one seat is left for two approvals).
func TestSoD_UnverifiedDenialCountsTheExclusion(t *testing.T) {
	pol := &agent.ApprovalPolicy{Need: 2, Approvers: approvers, ExcludePrincipal: true}
	f := newFixtureID(t, pol, refundArgs, audit.AlgEd25519, "ops")
	code, out, errs := f.run_("", f.asUnverified("finance", "-reject", "-yes")...)
	if code != 0 {
		t.Fatalf("finance -reject = %d\n%s%s", code, out, errs)
	}
	contains(t, "finance -reject", out, "would settle the gate")
}
