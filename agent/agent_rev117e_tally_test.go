package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
)

// dupTally is a terminal tally with a duplicate member: a lenient reader takes the last
// "approved" (passed), audit.VerifyApprovals refuses it.
var dupTally = json.RawMessage(`{"need":1,"approvers":["a"],"approved":0,"denied":1,"approved":1}`)

// sagaFailure reads a recorded tally strictly, as quorumTally does: one that does not decode is
// ErrStorage, never read one way here and another by the audit.
func TestRev117e_SagaFailureReadsTallyStrictly(t *testing.T) {
	recs := sagaJournal(Record{Name: ApprovalTallyStep("c1"), Kind: StepValue, Result: dupTally},
		Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: json.RawMessage(`"denied"`)})
	if _, _, err := sagaFailure("r", recs); !errors.Is(err, ErrStorage) {
		t.Fatalf("sagaFailure = %v, want ErrStorage", err)
	}
}

// The loop's read of a recorded tally (a terminal decision for a call not yet run) is strict too.
func TestRev117e_LoopReadsTallyStrictly(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	var ran atomic.Int32
	tool := Func("t", "", Safety{}, func(context.Context, struct{}) (string, error) { ran.Add(1); return "ok", nil })
	m := NewScriptedModel(ToolTurn("c1", "t", `{}`), TextTurn("done"))
	a := mustNew(m, store, WithTools(tool))
	// A tally journaled for the call before it runs (as an m-of-n gate's terminal decision is).
	if _, err := store.do(ctx, "r", ApprovalTallyStep("c1"), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: dupTally}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := a.Run(ctx, "r", "go")
	if !errors.Is(err, ErrStorage) || ran.Load() != 0 {
		t.Fatalf("Run = %v, ran %d; want ErrStorage before the tool runs", err, ran.Load())
	}
}
