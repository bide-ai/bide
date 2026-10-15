package approvecmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
	"github.com/bide-ai/bide/internal/runview"
)

// A policy change after the first pause: the gate first paused under 2 of 3 without separation of
// duties; ops (the requester) and finance approved, and the old deployment's drive recorded the
// passing tally and crashed before the refund ran. Redeployed with ExcludePrincipal, the re-drive
// asks again and records a re-ask note. bide view and bide approve, which have only the journal,
// then show the gate waiting for the separation-of-duties re-approval, and risk's approval through
// bide approve counts: the gate runs the refund on finance and risk.
func TestSoDReask_ViewAndApproveAfterAPolicyChange(t *testing.T) {
	weak := &agent.ApprovalPolicy{Need: 2, Approvers: approvers}
	strong := &agent.ApprovalPolicy{Need: 2, Approvers: approvers, ExcludePrincipal: true}
	f := newFixtureID(t, weak, refundArgs, audit.AlgEd25519, "ops")
	for _, who := range []string{"ops", "finance"} {
		if code, out, errs := f.run_("", f.as(who, "-yes")...); code != 0 {
			t.Fatalf("%s = %d\n%s%s", who, code, out, errs)
		}
	}

	// The old deployment's drive records its tally, then crashes before the call.
	j, closeJ := f.store()
	recs, err := j.History(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	_, call, _ := agent.FindToolCall(recs, f.call)
	verify := func(id string) (agent.ApproverVerifier, bool) { v, ok := f.pubs[id]; return v, ok }
	tally, _ := agent.TallyApprovals(recs, agent.ApprovalSubject{RunID: f.run, ToolUseID: f.call, ToolName: call.Name, Args: call.Args}, *weak, verify)
	if !tally.Passed() {
		t.Fatalf("the old count did not pass: %+v", tally)
	}
	tally.DecisionFormat = agent.DecisionFormat
	b, _ := json.Marshal(tally)
	_, err = journaltest.Put(context.Background(), j, f.run, agent.ApprovalTallyStep(f.call), agent.Record{Kind: agent.StepValue, Result: b})
	closeJ()
	if err != nil {
		t.Fatal(err)
	}

	// Redeployed with the exclusion, the re-drive asks again.
	_, err = f.drive(strong, false)
	pend, ok := errors.AsType[*agent.ApprovalPending](err)
	if !ok || pend.Reason != agent.PendingSoDReapproval || f.charges.Load() != 0 {
		t.Fatalf("re-drive = %v (charges %d); want the separation-of-duties re-approval pause", err, f.charges.Load())
	}

	// bide view: waiting for the re-approval, ops excluded.
	j, closeJ = f.store()
	recs, err = j.History(context.Background(), f.run)
	closeJ()
	if err != nil {
		t.Fatal(err)
	}
	v := runview.Build(context.Background(), f.run, recs, nil)
	if len(v.Gates) != 1 {
		t.Fatalf("gates = %+v", v.Gates)
	}
	g := v.Gates[0]
	if !g.Waiting || g.Tallied || !g.ReapprovalSoD || !g.ExcludePrincipal || !strings.Contains(strings.Join(g.Excluded, ","), "ops") {
		t.Fatalf("bide view's gate = %+v; want waiting for the separation-of-duties re-approval, ops excluded", g)
	}

	// bide approve -list: the call waits, and risk is offered a command.
	code, out, errs := f.run_("", "-list", "-approver-keys", f.keysJSON)
	if code != 0 || !strings.Contains(out, "-as risk") || strings.Contains(out, "approvals verified") {
		t.Fatalf("-list = %d:\n%s%s", code, out, errs)
	}
	// ops is refused; risk's approval counts.
	if code, out, errs := f.run_("", f.as("ops", "-yes")...); code == 0 {
		t.Fatalf("ops (the requester) = 0, want a refusal\n%s%s", out, errs)
	}
	code, out, errs = f.run_("", f.as("risk", "-yes")...)
	if code != 0 {
		t.Fatalf("risk = %d\n%s%s", code, out, errs)
	}
	contains(t, "risk", out, "verified tally: 2 of 2 needed approved by finance, risk", "the gate has its approvals")
	if _, err := f.drive(strong, false); err != nil || f.charges.Load() != 1 {
		t.Fatalf("drive at quorum = %v (charges %d)", err, f.charges.Load())
	}
}
