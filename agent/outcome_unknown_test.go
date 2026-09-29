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
	store := NewMemStore()
	var charged int
	charge := Func("charge", "charge the card", Safety{}, func(context.Context, struct{}) (string, error) {
		charged++
		return "", fmt.Errorf("gateway connection reset (%w)", ErrToolOutcomeUnknown)
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), toolTurn("c2", "charge", `{}`), textTurn("done")}}
	_, err := New(m, store, charge).Run(context.Background(), "r1", "pay")
	if !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("run err = %v, want ErrToolOutcomeUnknown", err)
	}
	if rec, ok := hasStep(t, store, "r1", "c1"); ok {
		t.Fatalf("the call's unknown outcome was journaled as %s (is_error=%v)", rec.Result, rec.IsError)
	}

	m2 := &greedyModel{script: [][]Emit{toolTurn("c2", "charge", `{}`), textTurn("done")}}
	_, err = New(m2, store, charge).Run(context.Background(), "r1", "pay")
	var halt *ResumeHalt
	if !errors.As(err, &halt) || halt.ToolUseID != "c1" {
		t.Fatalf("resume err = %v, want ResumeHalt for c1", err)
	}
	if charged != 1 {
		t.Fatalf("charged %d times, want 1", charged)
	}
}

// A retry-safe tool may simply run again, so its unknown outcome is an ordinary failure the
// model sees, and the run carries on.
func TestToolOutcomeUnknown_RetrySafeToolIsAFailure(t *testing.T) {
	store := NewMemStore()
	lookup := Func("lookup", "read a balance", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) {
		return "", fmt.Errorf("connection reset (%w)", ErrToolOutcomeUnknown)
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	if _, err := New(m, store, lookup).Run(context.Background(), "r1", "balance?"); err != nil {
		t.Fatalf("run err = %v, want the failure passed to the model", err)
	}
	if rec, ok := hasStep(t, store, "r1", "c1"); !ok || !rec.IsError {
		t.Fatalf("recorded %+v (found=%v), want a failed result", rec, ok)
	}
}
