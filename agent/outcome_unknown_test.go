package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A tool that cannot tell whether its side effect happened (its connection dropped after the
// request went out) reports ErrToolOutcomeUnknown. For a side effect the loop must record no
// result, stop the run, and halt the resume, as it does for a cancelled call; a journaled
// failure would tell the model the charge did not happen.
func TestToolOutcomeUnknown_SideEffectIsNotRecorded(t *testing.T) {
	store := memJournal()
	var charged int
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		charged++
		return "", fmt.Errorf("gateway connection reset (%w)", ErrToolOutcomeUnknown)
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), toolTurn("c2", "charge", `{}`), textTurn("done")}}
	_, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	if !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("run err = %v, want ErrToolOutcomeUnknown", err)
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); ok {
		t.Fatalf("the call's unknown outcome was journaled as %s (is_error=%v)", rec.Result, rec.IsError)
	}

	m2 := &greedyModel{script: [][]Emit{toolTurn("c2", "charge", `{}`), textTurn("done")}}
	_, err = mustNew(m2, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v, want OutcomeUnknown for c1", err)
	}
	if halt.Cause != HaltCrashed || halt.Op != (OpRef{Kind: OpTool, ID: "c1", ToolName: "charge"}) {
		t.Fatalf("halt = %+v, want an OpTool halt on charge c1 with Cause %q (a marker found on resume)", halt, HaltCrashed)
	}
	if charged != 1 {
		t.Fatalf("charged %d times, want 1", charged)
	}
}

// A retry-safe tool may simply run again, so its unknown outcome is an ordinary failure the
// model sees, and the run carries on.
func TestToolOutcomeUnknown_RetrySafeToolIsAFailure(t *testing.T) {
	store := memJournal()
	lookup := MustFunc("lookup", "read a balance", func(context.Context, struct{}) (string, error) {
		return "", fmt.Errorf("connection reset (%w)", ErrToolOutcomeUnknown)
	}, WithSafety(Safety{ReadOnly: true}))
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	if _, err := mustNew(m, store, WithTools(lookup)).Run(context.Background(), "r1", UserText("balance?")); err != nil {
		t.Fatalf("run err = %v, want the failure passed to the model", err)
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); !ok || !rec.IsError {
		t.Fatalf("recorded %+v (found=%v), want a failed result", rec, ok)
	}
}
