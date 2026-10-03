package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// loopModel always asks for a tool call, under a new ID each turn (a runaway model), so
// the loop only ends if a turn cap stops it.
type loopModel struct{ n int }

func (m *loopModel) Stream(context.Context, Request) (*Stream, error) {
	m.n++
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: fmt.Sprintf("c%d", m.n), Name: "spin", ArgsFragment: []byte(`{}`)}}
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)
	return NewStream(ch), nil
}

// WithMaxTurns stops a runaway tool loop with ErrMaxTurns (category ErrBudget).
func TestMaxTurns_StopsRunawayLoop(t *testing.T) {
	var calls int
	tool := &countingTool{name: "spin", safety: Safety{ReadOnly: true}, calls: &calls}
	a := mustNew(&loopModel{}, memJournal(), WithTools(tool), WithMaxTurns(3))

	_, err := a.Run(context.Background(), "r", "go")
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("err = %v, want errors.Is ErrMaxTurns", err)
	}
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want errors.Is ErrBudget (category)", err)
	}
	// The cap is on model turns: 3 turns generated, so at most 3 tool executions.
	if calls > 3 {
		t.Fatalf("tool ran %d times, want <= 3 (turn cap)", calls)
	}
}

// A run that finishes within the cap is unaffected.
func TestMaxTurns_UnderLimitCompletes(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	a := mustNew(m, memJournal(), WithTools(tool), WithMaxTurns(5))

	out, err := a.Run(context.Background(), "r", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if textOf(out) != "done" {
		t.Fatalf("answer = %q", textOf(out))
	}
}

// Default (no cap) is unbounded — WithMaxTurns(0) does not limit.
func TestMaxTurns_ZeroIsUnbounded(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{textTurn("ok")}}
	a := mustNew(m, memJournal(), WithMaxTurns(0))
	if _, err := a.Run(context.Background(), "r", "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
