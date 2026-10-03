package agent

import (
	"context"
	"strconv"
	"testing"
)

// WithSystemPromptFunc computes the system message per run (dynamic context).
func TestSystemPromptFunc_Dynamic(t *testing.T) {
	var got Request
	calls := 0
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok"), textTurn("ok")}}, got: &got}
	a := mustNew(
		m,
		memJournal(),
		WithSystemPromptFunc(func(_ context.Context, _ RunInfo) (string, error) {
			calls++
			return "turn " + strconv.Itoa(calls), nil
		}),
	)

	if _, err := a.Run(context.Background(), "r1", "hi"); err != nil {
		t.Fatal(err)
	}
	if textOf(got.Messages[0]) != "turn 1" || got.Messages[0].Role != RoleSystem {
		t.Fatalf("run 1 system = %+v, want 'turn 1'", got.Messages[0])
	}
	if _, err := a.Run(context.Background(), "r2", "hi"); err != nil {
		t.Fatal(err)
	}
	if textOf(got.Messages[0]) != "turn 2" {
		t.Fatalf("run 2 system = %q, want 'turn 2' (recomputed per run)", textOf(got.Messages[0]))
	}
}

// The dynamic func takes precedence over a static WithSystemPrompt.
func TestSystemPromptFunc_PrecedenceOverStatic(t *testing.T) {
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}, got: &got}
	a := must(mustNew(m, memJournal(), WithSystemPrompt("static")).With(WithSystemPromptFunc(func(_ context.Context, _ RunInfo) (string, error) { return "dynamic", nil })))

	if _, err := a.Run(context.Background(), "r", "hi"); err != nil {
		t.Fatal(err)
	}
	if textOf(got.Messages[0]) != "dynamic" {
		t.Fatalf("system = %q, want 'dynamic' (func wins)", textOf(got.Messages[0]))
	}
}
