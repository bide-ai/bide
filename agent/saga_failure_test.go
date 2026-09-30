package agent

import (
	"encoding/json"
	"testing"
)

// sagaJournal is a saga run's journal in which the model called c1, followed by recs.
func sagaJournal(recs ...Record) []Record {
	asst := &Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}}}
	return append([]Record{{Name: "@llm/0", Kind: StepModel, Message: asst}}, recs...)
}

func tallyRecord(t ApprovalTally) Record {
	b, _ := json.Marshal(t)
	return Record{Name: ApprovalTallyStep("c1"), Kind: StepValue, Result: b}
}

// What aborts a saga: a recorded step failure, and a failure an operator recorded for a step a
// crash cut off (ResolveHalt with isError). Not a human's denial, 1-of-1 or m-of-n, which the
// model reacts to; not a success.
func TestSagaFailure(t *testing.T) {
	failed := Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: json.RawMessage(`"card declined"`)}
	quorum := ApprovalTally{Need: 2, Approvers: []string{"ops", "finance"}}
	deniedQuorum, passedQuorum := quorum, quorum
	deniedQuorum.Denied = 1
	passedQuorum.Approved = 2
	for _, tc := range []struct {
		name      string
		recs      []Record
		wantCause string
		want      bool
	}{
		{"recorded failure", sagaJournal(Record{Name: "c1", Kind: StepSagaFail, ToolUseID: "c1", Result: json.RawMessage(`"boom"`)}), "boom", true},
		{"reconciled failure", sagaJournal(Record{Name: "attempt:c1", Kind: StepAttempt, ToolUseID: "c1"}, failed), "card declined", true},
		{"reconciled failure recorded as JSON", sagaJournal(Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: json.RawMessage(`{"code":402}`)}), `{"code":402}`, true},
		{"reconciled failure after an approval", sagaJournal(Record{Name: "approval:c1", Kind: StepApproval, ToolUseID: "c1", Approved: true}, failed), "card declined", true},
		{"reconciled failure after a passed quorum", sagaJournal(tallyRecord(passedQuorum), failed), "card declined", true},
		{"reconciled failure after a quorum passed over a denial", sagaJournal(
			Record{Name: "approval:c1:risk", Kind: StepApproval, ToolUseID: "c1", Approver: "risk"}, tallyRecord(passedQuorum), failed), "card declined", true},
		{"denial", sagaJournal(Record{Name: "approval:c1", Kind: StepApproval, ToolUseID: "c1"}, failed), "", false},
		{"quorum denial", sagaJournal(tallyRecord(deniedQuorum), failed), "", false},
		{"success", sagaJournal(Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{}`)}), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause, got := sagaFailure(tc.recs)
			if got != tc.want || cause != tc.wantCause {
				t.Fatalf("sagaFailure = %q, %v; want %q, %v", cause, got, tc.wantCause, tc.want)
			}
		})
	}
}
