package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// runRecovering runs a and turns a panic into an error, so a test can report a crash as a
// failure instead of taking the whole test binary down.
func runRecovering(a *Agent, runID string) (out Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return a.Run(context.Background(), runID, "go")
}

// A tool-use ID is the journal key of its call's result, in the same namespace as the run's
// own steps. An ID that names one of them ("@llm/1", the next model turn) must be refused:
// before the check the call's result took the model turn's key, and the next turn replayed
// a tool result as if it were the model's answer.
func TestRun_ToolUseIDNamingAModelTurnIsRejected(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("@llm/1", "lookup", `{}`), textTurn("done")}}
	_, err := runRecovering(New(m, NewMemStore(), tool), "r")
	if !errors.Is(err, ErrToolUseIDReused) || !errors.Is(err, ErrProtocol) || !errors.Is(err, ErrModel) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDReused", err, calls)
	}
	if calls != 0 {
		t.Fatalf("tool ran %d times, want 0", calls)
	}
}

// An ID that names the completion marker ("run:complete") must be refused: before the check
// the call's result took the marker's key, so the finished run was never marked complete and
// a re-invocation asked the model again.
func TestRun_ToolUseIDNamingTheCompletionMarkerIsRejected(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	store := NewMemStore()
	m := &scriptModel{turns: [][]Emit{toolTurn(runCompleteStep, "lookup", `{}`), textTurn("done")}}
	_, err := runRecovering(New(m, store, tool), "r")
	if !errors.Is(err, ErrToolUseIDReused) {
		complete, _ := IsComplete(context.Background(), store, "r")
		t.Fatalf("err = %v (tool ran %d times, IsComplete %v), want ErrToolUseIDReused", err, calls, complete)
	}
	if calls != 0 {
		t.Fatalf("tool ran %d times, want 0", calls)
	}
}

// A sub-agent's run is "<parent run>/<tool-use ID>", so an ID with a '/' can name another
// call's sub-run in the same tree: the root's call "a/b" and call "b" made by the sub-agent
// behind the root's call "a" both ran as "r/a/b", and the second was handed the first's
// answer without its own agent running. Such an ID must be refused.
func TestRun_ToolUseIDWithSlashCannotNameAnotherSubRun(t *testing.T) {
	store := NewMemStore()
	var innerCalls int
	inner := &countingTool{name: "noop", safety: Safety{ReadOnly: true}, calls: &innerCalls}
	b := New(NewScriptedModel(TextTurn("from b")), store)
	a := New(NewScriptedModel(ToolTurn("b", "b", `{"task":"x"}`), TextTurn("a done")), store, SubAgent("b", "b", b))
	cModel := &countModel{inner: NewScriptedModel(TextTurn("from c"))}
	c := New(cModel, store, inner)
	root := New(NewScriptedModel(
		ToolTurn("a", "a", `{"task":"x"}`),
		ToolTurn("a/b", "c", `{"task":"y"}`),
		TextTurn("root done"),
	), store, SubAgent("a", "a", a), SubAgent("c", "c", c))
	_, err := runRecovering(root, "r")
	if !errors.Is(err, ErrToolUseIDReused) {
		t.Fatalf("err = %v (sub-agent c's model ran %d times), want ErrToolUseIDReused", err, cModel.calls.Load())
	}
}

// The IDs providers issue are accepted.
func TestRun_ProviderToolUseIDsAreAccepted(t *testing.T) {
	for _, id := range []string{
		"toolu_01A09q90qw90lq917835lq9", // Anthropic
		"call_abc123XYZ",                // OpenAI, and the Gemini adapter's own
		"chatcmpl-tool-4f1c2a9e",        // vLLM
		"9aB3kd7Qz",                     // Mistral
	} {
		var calls int
		tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
		m := &scriptModel{turns: [][]Emit{toolTurn(id, "lookup", `{}`), textTurn("done")}}
		out, err := runRecovering(New(m, NewMemStore(), tool), "r")
		if err != nil || out.Text() != "done" || calls != 1 {
			t.Fatalf("id %q: out = %q, err = %v, tool ran %d times; want done, nil, 1", id, out.Text(), err, calls)
		}
	}
}
