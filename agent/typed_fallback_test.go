package agent

import (
	"context"
	"errors"
	"testing"
)

// With no accepted final_answer call, the answer is the text of the run's final turn, the same
// message Run returns and RunTypedNative decodes. RunTyped used to take the last text of ANY turn,
// so a draft the model wrote beside a tool call stood in for a final turn that carried no text.
func TestRunTyped_TextFallbackIsTheFinalTurn(t *testing.T) {
	work := Func("work", "does work", Safety{ReadOnly: true},
		func(context.Context, struct{}) (string, error) { return "ok", nil })
	draft := []Event{
		TextDelta{Text: `{"name":"draft"}`},
		ToolCallDelta{Index: 0, ID: "w1", Name: "work", ArgsFragment: []byte(`{}`)},
		Finish{Reason: "tool_use"},
	}

	// The final turn has no text: there is no answer, not the draft.
	m := eventTurnsModel{draft, {Finish{Reason: "stop"}}}
	got, err := RunTyped[typedAnswer](context.Background(), New(m, NewMemStore(), work), "r1", "go")
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("RunTyped = %+v, %v; want ErrProtocol (the final turn has no answer)", got, err)
	}

	// The final turn's text is the answer.
	m = eventTurnsModel{draft, {TextDelta{Text: `{"name":"final"}`}, Finish{Reason: "stop"}}}
	got, err = RunTyped[typedAnswer](context.Background(), New(m, NewMemStore(), work), "r2", "go")
	if err != nil || got.Name != "final" {
		t.Fatalf("RunTyped = %+v, %v; want the final turn's answer", got, err)
	}
}

// RunTyped's answer is a tool call's arguments, and providers take only an object there (the
// Anthropic input_schema and the OpenAI function parameters must be type "object"). A T whose
// schema is not an object is a config error before the run starts, not a request the provider
// rejects on every turn.
func TestRunTyped_NonObjectTypeIsAConfigError(t *testing.T) {
	check := func(name string, run func(*Agent) error) {
		t.Helper()
		m := &countModel{inner: NewScriptedModel(TextTurn("{}"))}
		if err := run(New(m, NewMemStore())); !errors.Is(err, ErrConfig) {
			t.Errorf("RunTyped[%s] = %v, want ErrConfig", name, err)
		}
		if n := m.calls.Load(); n != 0 {
			t.Errorf("RunTyped[%s] called the model %d times, want 0", name, n)
		}
	}
	ctx := context.Background()
	check("[]string", func(a *Agent) error { _, err := RunTyped[[]string](ctx, a, "r", "go"); return err })
	check("string", func(a *Agent) error { _, err := RunTyped[string](ctx, a, "r", "go"); return err })
	check("any", func(a *Agent) error { _, err := RunTyped[any](ctx, a, "r", "go"); return err })

	// A pointer to a struct, or a map, is an object.
	m := NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"name":"p"}`))
	if got, err := RunTyped[*typedAnswer](ctx, New(m, NewMemStore()), "p", "go"); err != nil || got == nil || got.Name != "p" {
		t.Errorf("RunTyped[*typedAnswer] = %+v, %v", got, err)
	}
	m = NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"k":1}`))
	if got, err := RunTyped[map[string]int](ctx, New(m, NewMemStore()), "m", "go"); err != nil || got["k"] != 1 {
		t.Errorf("RunTyped[map[string]int] = %v, %v", got, err)
	}
}
