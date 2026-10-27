package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// instancePassed: the escalation gate had passed in the journal before the node's record.
func TestInstancePassed(t *testing.T) {
	ctx := context.Background()
	const inst = "node:pay:taint"
	args := json.RawMessage(`{"amount":7}`)
	history := func(j *Journal) []Record {
		recs, err := j.History(ctx, "r1")
		if err != nil {
			t.Fatal(err)
		}
		return recs
	}

	// A one-decision gate: not asked, asked, denied, approved.
	single := SingleApproval()
	j := memJournal()
	if instancePassed(history(j), inst, "pay", single) {
		t.Error("a gate nobody asked passed")
	}
	var pend *ApprovalPending
	if _, err := flowGate(ctx, j, "r1", inst, "pay", args, single, nil); !errors.As(err, &pend) {
		t.Fatal(err)
	}
	if instancePassed(history(j), inst, "pay", single) {
		t.Error("a gate still waiting passed")
	}
	if err := Approve(ctx, j, "r1", inst, true); err != nil {
		t.Fatal(err)
	}
	if !instancePassed(history(j), inst, "pay", single) {
		t.Error("an approved one-decision gate did not pass")
	}
	jd := memJournal()
	_, _ = flowGate(ctx, jd, "r1", inst, "pay", args, single, nil)
	if err := Approve(ctx, jd, "r1", inst, false); err != nil {
		t.Fatal(err)
	}
	if instancePassed(history(jd), inst, "pay", single) {
		t.Error("a denied one-decision gate passed")
	}
	// A decision recorded before the gate's request is no decision of the gate's.
	jb := memJournal()
	writeRaw(t, jb, "r1", approvalStep(inst), Record{Kind: StepApproval, ToolUseID: inst, Approved: true})
	_, _ = flowGate(ctx, jb, "r1", inst, "pay", args, single, nil)
	if instancePassed(history(jb), inst, "pay", single) {
		t.Error("a decision recorded before the request passed the gate")
	}

	// An m-of-n gate passes on a recorded terminal tally that passed, and on nothing else.
	vf := fakeVerifiers("a", "b")
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
	subj := ApprovalSubject{RunID: "r1", ToolUseID: inst, ToolName: "pay", Args: args}
	run := func(votes map[string]bool) *Journal {
		jm := memJournal()
		if _, err := flowGate(ctx, jm, "r1", inst, "pay", args, pol, vf); !errors.As(err, &pend) {
			t.Fatal(err)
		}
		for id, v := range votes {
			if err := decideAs(ctx, jm, "r1", inst, id, v, fakeSign(id, ApprovalDecisionBytes(subj, id, v, testDecidedAt))); err != nil {
				t.Fatal(err)
			}
		}
		_, _ = flowGate(ctx, jm, "r1", inst, "pay", args, pol, vf)
		return jm
	}
	if instancePassed(history(run(map[string]bool{"a": true})), inst, "pay", pol) {
		t.Error("one of two passed")
	}
	if !instancePassed(history(run(map[string]bool{"a": true, "b": true})), inst, "pay", pol) {
		t.Error("two of two did not pass")
	}
	if instancePassed(history(run(map[string]bool{"a": false})), inst, "pay", pol) {
		t.Error("a denied m-of-n gate passed")
	}
	// The same decisions with the terminal tally left out: no outcome was recorded.
	recs := history(run(map[string]bool{"a": true, "b": true}))
	var noTally []Record
	for _, r := range recs {
		if r.Name != ApprovalTallyStep(inst) {
			noTally = append(noTally, r)
		}
	}
	if instancePassed(noTally, inst, "pay", pol) {
		t.Error("an m-of-n gate with no recorded tally passed")
	}
}
