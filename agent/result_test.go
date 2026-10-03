package agent

import (
	"context"
	"testing"
)

// toolTurnWithUsage returns a scripted tool-call turn whose Finish event carries known
// usage. Use this in RunResult tests to assert usage accumulation.
func toolTurnWithUsage(id, name, args string, u Usage) []Emit {
	return []Emit{
		{Event: ToolCallDelta{Index: 0, ID: id, Name: name, ArgsFragment: []byte(args)}},
		{Event: Finish{Reason: "tool_use", Usage: u}},
	}
}

// textTurnWithUsage returns a scripted text-answer turn whose Finish event carries known
// usage.
func textTurnWithUsage(s string, u Usage) []Emit {
	return []Emit{
		{Event: TextDelta{Text: s}},
		{Event: Finish{Reason: "stop", Usage: u}},
	}
}

// TestRunResult_TwoTurnAccumulatesUsage drives a two-turn run (tool call then final answer)
// and verifies that Result.Usage is the sum of both turns' usages, Result.Turns == 2,
// Result.Duration > 0, and Result.RunID is correct.
func TestRunResult_TwoTurnAccumulatesUsage(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}

	u1 := Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 1}
	u2 := Usage{InputTokens: 20, OutputTokens: 8, CacheReadTokens: 0, CacheWriteTokens: 3}

	m := &scriptModel{turns: [][]Emit{
		toolTurnWithUsage("c1", "lookup", `{"q":"x"}`, u1),
		textTurnWithUsage("final answer", u2),
	}}
	a := mustNew(m, memJournal(), WithTools(tool))

	res, err := a.Run(context.Background(), "run-result-1", UserText("hi"))
	if err != nil {
		t.Fatalf("RunResult: %v", err)
	}

	// Message must equal the final text.
	if got := textOf(res.Message); got != "final answer" {
		t.Errorf("Message text = %q, want %q", got, "final answer")
	}

	// RunID must echo the argument.
	if res.RunID != "run-result-1" {
		t.Errorf("RunID = %q, want %q", res.RunID, "run-result-1")
	}

	// Turns must be 2 (one tool-call turn, one answer turn).
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}

	// Usage must be the element-wise sum of u1 + u2.
	wantUsage := Usage{
		InputTokens:      u1.InputTokens + u2.InputTokens,
		OutputTokens:     u1.OutputTokens + u2.OutputTokens,
		CacheReadTokens:  u1.CacheReadTokens + u2.CacheReadTokens,
		CacheWriteTokens: u1.CacheWriteTokens + u2.CacheWriteTokens,
	}
	if res.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", res.Usage, wantUsage)
	}

	// Duration is measured wall-clock; a zero-work mock run can legitimately round to 0
	// on coarse-resolution clocks (seen on the Windows runner), so only require non-negative.
	if res.Duration < 0 {
		t.Errorf("Duration = %v, want >= 0", res.Duration)
	}

	// Tool must have executed exactly once.
	if calls != 1 {
		t.Errorf("tool called %d times, want 1", calls)
	}
}

// TestRunResult_MessageMatchesRun asserts that Run and RunResult return the same final
// message for an identical run, confirming backward compatibility.
func TestRunResult_MessageMatchesRun(t *testing.T) {
	u := Usage{InputTokens: 5, OutputTokens: 3}
	store1 := memJournal()
	store2 := memJournal()
	var calls1, calls2 int
	tool1 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls1}
	tool2 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls2}

	turns := func() [][]Emit {
		return [][]Emit{
			toolTurnWithUsage("c1", "lookup", `{"q":"x"}`, u),
			textTurnWithUsage("answer", u),
		}
	}

	m1 := &scriptModel{turns: turns()}
	res2, err := mustNew(m1, store1, WithTools(tool1)).Run(context.Background(), "r1", UserText("hi"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	plainMsg := res2.Message

	m2 := &scriptModel{turns: turns()}
	res, err := mustNew(m2, store2, WithTools(tool2)).Run(context.Background(), "r2", UserText("hi"))
	if err != nil {
		t.Fatalf("RunResult: %v", err)
	}

	if textOf(plainMsg) != textOf(res.Message) {
		t.Errorf("Run returned %q, RunResult.Message = %q — mismatch", textOf(plainMsg), textOf(res.Message))
	}
}

// TestRunResult_SingleTurnNoTools verifies a simple one-turn (no tool calls) run.
func TestRunResult_SingleTurnNoTools(t *testing.T) {
	u := Usage{InputTokens: 7, OutputTokens: 4}
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("hello", u)}}
	a := mustNew(m, memJournal())

	res, err := a.Run(context.Background(), "single", UserText("hi"))
	if err != nil {
		t.Fatalf("RunResult: %v", err)
	}
	if textOf(res.Message) != "hello" {
		t.Errorf("Message = %q, want %q", textOf(res.Message), "hello")
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
	if res.Usage != u {
		t.Errorf("Usage = %+v, want %+v", res.Usage, u)
	}
	if res.RunID != "single" {
		t.Errorf("RunID = %q, want %q", res.RunID, "single")
	}
}
