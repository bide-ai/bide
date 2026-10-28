package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
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
	if v := sepGate(1, "a").SeparatedFrom("c1"); v.sibling != "c1" || v.Need != 1 {
		t.Errorf("SeparatedFrom: %+v", v)
	}
	if sepGate(1, "a").Clone().sibling != "" {
		t.Error("a plain policy has a sibling")
	}
	var nilp *ApprovalPolicy
	if nilp.separatedFrom("c1") != nil {
		t.Error("a nil policy separated from an own gate is not nil")
	}
}

// The engine counts an instance from its policy, never from the gate-reason record a display reads: a record edited to
// drop the flag cannot un-separate the gate it counts. (A reader of the journal then shows the excluded approver as
// eligible; the engine still leaves their decision out.)
func TestTallyGate_ADroppedFlagCannotUnseparateTheEngine(t *testing.T) {
	ctx := context.Background()
	inst := ApprovalSubject{RunID: "r1", ToolUseID: "c1#taint", ToolName: "pay"}
	j := memJournal()
	b, _ := json.Marshal(gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c1"}) // the flag dropped
	for _, r := range []Record{{Name: gateReasonStep("c1"), Kind: StepValue, Result: b}, ownTally("c1", []string{"ops"}, nil), instanceDecision(inst, "ops", true), instanceDecision(inst, "sec1", true)} {
		if _, _, err := j.putNew(ctx, "r1", r.Name, r); err != nil {
			t.Fatal(err)
		}
	}
	tally, final, err := tallyGate(ctx, j, "r1", inst, sepGate(1, "ops", "sec1").separatedFrom("c1"), fakeVerifiers("ops", "sec1"), "")
	if err != nil || !final || strings.Join(tally.ApprovedBy, ",") != "sec1" || strings.Join(tally.ExcludedBySibling, ",") != "ops" {
		t.Errorf("tally %+v final %v err %v; want ops left out whatever the record says", tally, final, err)
	}
}

// Conform holds the gate-reason record's flag to the declarations the run started under.
func TestFlowTaintConform_TheGateReasonFlagAgreesWithTheDeclarations(t *testing.T) {
	sep := func(d *journalhook.FlowTaint) { d.SeparateApprovers = true }
	for _, c := range []struct {
		name       string
		declsSep   bool
		flag       bool
		ownDecided bool
		want       string
	}{
		{"a flag the declarations do not have", false, true, false, "do not separate approvers"},
		{"the flag dropped after the own gate decided", true, false, true, "does not say"},
		{"the flag dropped before the own gate decided", true, false, false, ""},
		{"agreeing", true, true, true, ""},
		{"no separation", false, false, true, ""},
	} {
		var mut func(*journalhook.FlowTaint)
		if c.declsSep {
			mut = sep
		}
		j, _ := beginNested(t, context.Background(), hookDecls(mut), nil)
		ctx := context.Background()
		b, _ := json.Marshal(gateReasonRecord{Format: gateReasonFormat, ToolUseID: "node:pay", SeparateApprovers: c.flag})
		for _, r := range []Record{{Name: gateReasonStep("node:pay"), Kind: StepValue, Result: b}} {
			if _, _, err := j.putNew(ctx, "r1", r.Name, r); err != nil {
				t.Fatal(err)
			}
		}
		if c.ownDecided {
			r := ownTally("node:pay", []string{"a"}, nil)
			if _, _, err := j.putNew(ctx, "r1", r.Name, r); err != nil {
				t.Fatal(err)
			}
		}
		recs, _ := j.History(ctx, "r1")
		diffs := flowTaintConform(recs, hookDecls(mut), nil, map[string]any{"pay": Safety{}})
		got := strings.Join(diffs, "|")
		if c.want == "" && strings.Contains(got, "gate-reason") && (strings.Contains(got, "kept apart") || strings.Contains(got, "separate")) {
			t.Errorf("%s: %q; want no divergence about the flag", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
}
