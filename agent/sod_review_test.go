package agent

import (
	"context"
	"testing"
)

// REVIEW PROOF 3: a recorded tally of a policy that did not exclude the requester stands when the
// call is evaluated under a policy that does (a redeploy that adds the exclusion).
func TestReview_SoD_RecordedWeakTallyStandsUnderExcludingPolicy(t *testing.T) {
	store := memJournal()
	vf := fakeVerifiers(abc...)
	var charged int
	weak := &ApprovalPolicy{Need: 2, Approvers: abc}
	strong := sodPolicy(2)
	_, err := sodRun(store, "r1", true, weak, vf, &charged, Identity{OnBehalfOf: "alice"})
	if err == nil {
		t.Fatal("did not pause")
	}
	approveAs(t, store, "r1", "c1", "alice", true)
	approveAs(t, store, "r1", "c1", "bob", true)
	s := ApprovalSubject{RunID: "r1", ToolUseID: "c1", ToolName: "charge", Args: []byte(`{}`)}
	if _, final, err := tallyGate(context.Background(), store, "r1", s, weak, vf, ""); err != nil || !final {
		t.Fatalf("weak gate: final=%v err=%v", final, err)
	}
	// The deployment now excludes the requester; the call has not run yet.
	tally, final, err := tallyGate(context.Background(), store, "r1", s, strong, vf, "")
	if err == nil && final && tally.Passed() {
		t.Fatalf("an excluding gate passed on a recorded tally that counted the requester alice: %+v", tally)
	}
}

// REVIEW PROOF 4: the lenient stored-record reader (storedApproval) drops exclude_principal, so the
// policy a journaled tool result records reads back without the exclusion.
func TestReview_SoD_StoredApprovalKeepsExclusion(t *testing.T) {
	store := memJournal()
	vf := fakeVerifiers(abc...)
	var charged int
	pol := sodPolicy(2)
	_, err := sodRun(store, "r1", true, pol, vf, &charged, Identity{OnBehalfOf: "alice"})
	if err == nil {
		t.Fatal("did not pause")
	}
	approveAs(t, store, "r1", "c1", "bob", true)
	approveAs(t, store, "r1", "c1", "carol", true)
	if _, err := sodRun(store, "r1", false, pol, vf, &charged, Identity{}); err != nil {
		t.Fatal(err)
	}
	for _, r := range mustHistory(t, store, "r1") {
		if r.Kind == StepToolResult && r.ToolUseID == "c1" {
			if r.Approval == nil || !r.Approval.ExcludePrincipal {
				t.Fatalf("tool result's stored approval = %+v, want ExcludePrincipal", r.Approval)
			}
			return
		}
	}
	t.Fatal("no tool result")
}
