package agent

import (
	"strings"
	"testing"
)

// An approver the own gate counted is told apart from the instance's by the gate's rule for matching approvers,
// ASCII case ignored, as the requester exclusion is.
func TestTally_ASiblingMatchesIgnoringCase(t *testing.T) {
	inst := ApprovalSubject{RunID: "r1", ToolUseID: "c1#taint", ToolName: "pay"}
	pol := sepGate(1, "ops", "sec1")
	vf := fakeVerifiers("ops", "sec1")
	recs := []Record{ownTally("c1", []string{"OPS"}, nil), instanceDecision(inst, "ops", true)}
	got, _ := tallyApprovals(recs, recs, inst, *pol.separatedFrom("c1"), vf)
	if got.Passed() || strings.Join(got.ExcludedBySibling, ",") != "ops" {
		t.Errorf("OPS counted on the own gate, ops on the instance: %+v; want ops excluded", got)
	}
}

// An approver who fills no seat for another reason keeps that reason: the exclusion by the sibling does not take it
// over, and does not list them.
func TestTally_ASiblingDoesNotTakeOverAnotherExclusion(t *testing.T) {
	inst := ApprovalSubject{RunID: "r1", ToolUseID: "c1#taint", ToolName: "pay"}
	pol := sepGate(1, "a", "b")
	sameKey := func(id string) (ApproverVerifier, bool) { return fakeVerifier{id: "a"}, id == "a" || id == "b" } // one key for both
	recs := []Record{ownTally("c1", []string{"a"}, nil), instanceDecision(inst, "a", true)}
	got, checks := tallyApprovals(recs, recs, inst, *pol.separatedFrom("c1"), sameKey)
	if len(got.ExcludedBySibling) != 0 || len(checks) != 1 || checks[0].Reason != ReasonSharedKey {
		t.Errorf("tally %+v checks %+v; want a's decision ignored for the shared key, and nobody excluded by the sibling", got, checks)
	}
}

// The own gate's tally is read from the whole journal, not from the records the gate counts: a flow gate counts
// only the records after its request, and the own gate's tally is before it.
func TestTally_TheOwnTallyIsReadFromTheWholeJournal(t *testing.T) {
	inst := ApprovalSubject{RunID: "r1", ToolUseID: "node:pay:taint", ToolName: "pay"}
	pol := sepGate(1, "ops", "sec1")
	vf := fakeVerifiers("ops", "sec1")
	own := ownTally("node:pay", []string{"ops"}, nil)
	journal := []Record{own, instanceDecision(inst, "ops", true)}
	counted := journal[1:] // what the gate counts: the decisions after the instance's request
	got, _ := tallyApprovals(counted, journal, inst, *pol.separatedFrom("node:pay"), vf)
	if got.Passed() || strings.Join(got.ExcludedBySibling, ",") != "ops" {
		t.Errorf("tally %+v; want ops excluded by the own gate's tally in the journal", got)
	}
}

// A copy of a policy that is kept apart from its own gate stays so (the gate evaluates copies of it).
func TestApprovalPolicy_ACopyKeepsItsSibling(t *testing.T) {
	p := sepGate(1, "a").separatedFrom("c1")
	if c := p.Clone(); c.sibling != "c1" {
		t.Errorf("Clone dropped the sibling: %q", c.sibling)
	}
	if v := sepGate(1, "a").SeparatedFrom("c1"); v.SeparatedFromSubject() != "c1" {
		t.Errorf("SeparatedFrom: %q", v.SeparatedFromSubject())
	}
	if sepGate(1, "a").Clone().sibling != "" {
		t.Error("a plain policy has a sibling")
	}
	var nilp *ApprovalPolicy
	if nilp.separatedFrom("c1") != nil {
		t.Error("a nil policy separated from an own gate is not nil")
	}
}
