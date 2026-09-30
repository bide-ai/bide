package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type strictInner struct {
	ID string `json:"id"`
}

type strictEmbedded struct {
	Region string `json:"region"`
}

type strictArgs struct {
	strictEmbedded
	Name  string       `json:"name"`
	Note  string       `json:"note,omitempty"`
	Count *int         `json:"count"`
	Inner strictInner  `json:"inner"`
	Items []strictItem `json:"items,omitempty"`
}

type strictItem struct {
	Label string `json:"label"`
}

// Tool arguments decode strictly: what the model sent is exactly what the tool reads. A
// required field (as schema.For defines it: not a pointer, no omitempty or omitzero) that is
// missing, a name that is not a field (unknown, or a case variant encoding/json would match), a
// duplicate name, trailing data, invalid UTF-8, and an escaped lone surrogate are each
// ErrToolArgs, which goes back to the model as a tool error. encoding/json accepted all of them,
// filling in zero values, dropping names, or keeping the last duplicate.
func TestFunc_ArgumentsDecodeStrictly(t *testing.T) {
	var got strictArgs
	tool := Func("t", "strict", Safety{ReadOnly: true}, func(_ context.Context, in strictArgs) (string, error) {
		got = in
		return "ok", nil
	})
	ok := `{"region":"eu","name":"a","inner":{"id":"i"}}`
	for _, args := range []string{
		ok,
		`{"region":"eu","name":"a","note":"n","count":null,"inner":{"id":"i"},"items":[{"label":"x"}]}`,
		` {"region":"eu","name":"a","inner":{"id":"i"}} `,
	} {
		if _, err := tool.Call(context.Background(), json.RawMessage(args)); err != nil {
			t.Errorf("%s: Call = %v, want no error", args, err)
		}
	}
	if got.Region != "eu" || got.Name != "a" || got.Inner.ID != "i" {
		t.Fatalf("decoded %+v", got)
	}
	for name, args := range map[string]string{
		"no arguments":             ``,
		"missing required":         `{"region":"eu","inner":{"id":"i"}}`,
		"missing promoted":         `{"name":"a","inner":{"id":"i"}}`,
		"missing nested":           `{"region":"eu","name":"a","inner":{}}`,
		"missing in a slice item":  `{"region":"eu","name":"a","inner":{"id":"i"},"items":[{}]}`,
		"unknown name":             `{"region":"eu","name":"a","inner":{"id":"i"},"extra":1}`,
		"unknown nested name":      `{"region":"eu","name":"a","inner":{"id":"i","x":1}}`,
		"case variant":             `{"region":"eu","NAME":"a","inner":{"id":"i"}}`,
		"duplicate name":           `{"region":"eu","name":"a","name":"b","inner":{"id":"i"}}`,
		"trailing data":            `{"region":"eu","name":"a","inner":{"id":"i"}} {}`,
		"invalid UTF-8":            "{\"region\":\"eu\",\"name\":\"\xff\",\"inner\":{\"id\":\"i\"}}",
		"escaped lone surrogate":   `{"region":"eu","name":"\ud800","inner":{"id":"i"}}`,
		"a wrong type still fails": `{"region":"eu","name":5,"inner":{"id":"i"}}`,
	} {
		if _, err := tool.Call(context.Background(), json.RawMessage(args)); !errors.Is(err, ErrToolArgs) {
			t.Errorf("%s: Call(%s) = %v, want ErrToolArgs", name, args, err)
		}
	}
}

// A sub-agent's arguments decode strictly too, and a bad call is ErrToolArgs like any other.
func TestSubAgent_ArgumentsDecodeStrictly(t *testing.T) {
	sub := SubAgent("helper", "helps", New(NewScriptedModel(TextTurn("done")), NewMemStore()))
	for _, args := range []string{`{}`, `{"task":"x","extra":1}`, `{"Task":"x"}`, `{"task":"x","task":"y"}`} {
		if _, err := sub.Call(context.Background(), json.RawMessage(args)); !errors.Is(err, ErrToolArgs) {
			t.Errorf("SubAgent.Call(%s) = %v, want ErrToolArgs", args, err)
		}
	}
}

// A final_answer call the strict decoder rejects goes back to the model as a tool error, and the
// model's corrected call is the answer.
func TestRunTyped_LooseAnswerIsCorrected(t *testing.T) {
	for _, bad := range []string{`{}`, `{"NAME":"x"}`, `{"name":"x","nmae":"typo"}`, `{"name":"a","name":"b"}`} {
		m := &countModel{inner: NewScriptedModel(
			ToolTurn("f1", finalAnswerTool, bad),
			ToolTurn("f2", finalAnswerTool, `{"name":"ok"}`),
		)}
		store := NewMemStore()
		got, err := RunTyped[typedAnswer](context.Background(), New(m, store), "r", "go")
		if err != nil || got.Name != "ok" || m.calls.Load() != 2 {
			t.Errorf("%s: RunTyped = %+v, %v after %d model calls; want the corrected answer after 2", bad, got, err, m.calls.Load())
			continue
		}
		recs, _ := store.History(context.Background(), "r")
		rejected := false
		for _, r := range recs {
			if r.Kind == StepToolResult && r.ToolUseID == "f1" && r.IsError {
				rejected = true
			}
		}
		if !rejected {
			t.Errorf("%s: the first call has no error result in the journal", bad)
		}
	}
}

// A typed answer that reaches RunTyped as text, or RunTypedNative as the provider's JSON, decodes
// strictly as well: a loose answer is ErrProtocol, not a value with zero or dropped fields.
func TestRunTyped_TextAnswersDecodeStrictly(t *testing.T) {
	for _, text := range []string{`{}`, `{"NAME":"x"}`, `{"name":"x","extra":1}`, `{"name":"a","name":"b"}`, `{"name":"x"} {}`} {
		m := NewScriptedModel(TextTurn(text))
		if got, err := RunTyped[typedAnswer](context.Background(), New(m, NewMemStore()), "r", "go"); !errors.Is(err, ErrProtocol) {
			t.Errorf("RunTyped with text %s = %+v, %v; want ErrProtocol", text, got, err)
		}
		m = NewScriptedModel(TextTurn(text))
		if got, err := RunTypedNative[typedAnswer](context.Background(), New(m, NewMemStore()), "r", "go"); !errors.Is(err, ErrProtocol) {
			t.Errorf("RunTypedNative with %s = %+v, %v; want ErrProtocol", text, got, err)
		}
	}
}

// RunTyped returns the arguments final_answer accepted: what the tool received, after tool
// middleware, as it does for any tool. It used to re-decode the model's original arguments, so a
// middleware that rewrote them (to fix a known quirk, or fill a default) completed the run and
// then failed it for good with ErrProtocol, on this call and every later one.
func TestRunTyped_AnswerIsWhatTheToolAccepted(t *testing.T) {
	m := &countModel{inner: NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"name":5}`))}
	store := NewMemStore()
	a := New(m, store).UseTool(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name == finalAnswerTool {
				call.Use.Args = json.RawMessage(`{"name":"5"}`)
			}
			return next(ctx, call)
		}
	})
	for i := range 2 { // the first call runs, the second reads the finished run's journal
		got, err := RunTyped[typedAnswer](context.Background(), a, "r", "go")
		if err != nil || got.Name != "5" {
			t.Fatalf("call %d: RunTyped = %+v, %v; want the rewritten answer \"5\"", i, got, err)
		}
	}
	if m.calls.Load() != 1 {
		t.Fatalf("model called %d times, want 1", m.calls.Load())
	}
}

// A run journaled before RunTyped recorded the accepted arguments has a final_answer result of {}
// (the old tool's empty acknowledgement). Its answer is still the model's arguments, decoded as
// the old tool decoded them (encoding/json, loosely), so a finished run reads back the answer it
// finished with.
func TestRunTyped_AnswerFromAnOlderJournal(t *testing.T) {
	store := NewMemStore()
	m := &countModel{inner: NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"name":"old","NOTE":"loose"}`))}
	// The agent as the older RunTyped built it: a final_answer tool that decodes with
	// encoding/json and acknowledges with {}.
	old := New(m, store).cloneWith(legacyAnswerTool{})
	old.terminalTool = finalAnswerTool
	if _, err := old.Run(context.Background(), "r", "go"); err != nil {
		t.Fatalf("old run: %v", err)
	}
	got, err := RunTyped[typedAnswer](context.Background(), New(m, store), "r", "go")
	if err != nil || got.Name != "old" || m.calls.Load() != 1 {
		t.Fatalf("RunTyped over an older journal = %+v, %v after %d model calls; want \"old\" after 1", got, err, m.calls.Load())
	}
}

// legacyAnswerTool is final_answer as the older RunTyped built it (a Func over typedAnswer when
// Func decoded with encoding/json): it accepts loose arguments and acknowledges with {}.
type legacyAnswerTool struct{}

func (legacyAnswerTool) Name() string                { return finalAnswerTool }
func (legacyAnswerTool) Description() string         { return "answer" }
func (legacyAnswerTool) Safety() Safety              { return Safety{ReadOnly: true} }
func (legacyAnswerTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (legacyAnswerTool) Call(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var v typedAnswer
	if len(args) > 0 { // empty arguments were the zero value
		if err := json.Unmarshal(args, &v); err != nil {
			return nil, err
		}
	}
	return json.RawMessage(`{}`), nil
}

// An older journal whose final_answer call had no arguments answers the zero value, as the older
// tool accepted it.
func TestRunTyped_EmptyAnswerFromAnOlderJournal(t *testing.T) {
	store := NewMemStore()
	m := &countModel{inner: eventTurnsModel{{ToolCallDelta{Index: 0, ID: "f1", Name: finalAnswerTool}, Finish{Reason: "tool_use"}}}}
	old := New(m, store).cloneWith(legacyAnswerTool{})
	old.terminalTool = finalAnswerTool
	if _, err := old.Run(context.Background(), "r", "go"); err != nil {
		t.Fatalf("old run: %v", err)
	}
	got, err := RunTyped[typedAnswer](context.Background(), New(m, store), "r", "go")
	if err != nil || got != (typedAnswer{}) || m.calls.Load() != 1 {
		t.Fatalf("RunTyped over an older journal = %+v, %v after %d model calls; want the zero answer after 1", got, err, m.calls.Load())
	}
}

// The journaled final_answer result is read strictly too: a tool middleware that rewrites it into
// something other than one accepted answer makes the answer ErrProtocol, not a guess.
func TestRunTyped_RewrittenResultIsReadStrictly(t *testing.T) {
	for _, result := range []string{`{"accepted":{"name":"a"},"extra":1}`, `{"Accepted":{"name":"a"}}`, `{"accepted":{"name":"a","x":1}}`} {
		m := NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"name":"a"}`))
		a := New(m, NewMemStore()).UseTool(func(next ToolHandler) ToolHandler {
			return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
				if _, err := next(ctx, call); err != nil {
					return nil, err
				}
				return json.RawMessage(result), nil
			}
		})
		if got, err := RunTyped[typedAnswer](context.Background(), a, "r", "go"); !errors.Is(err, ErrProtocol) {
			t.Errorf("result %s: RunTyped = %+v, %v; want ErrProtocol", result, got, err)
		}
	}
}

type nullArgs struct {
	S    string            `json:"s"`
	L    []int             `json:"l"`
	M    map[string]int    `json:"m"`
	In   strictInner       `json:"in"`
	At   time.Time         `json:"at"`
	Any  any               `json:"any"`
	Raw  json.RawMessage   `json:"raw"`
	Opt  string            `json:"opt,omitempty"`
	Ptr  *string           `json:"ptr"`
	Tags map[string]string `json:"tags,omitzero"`
}

// null for a required field whose schema does not admit null (a string, slice, map, struct, or
// time.Time: For gives each a type without null) is rejected like a missing field: encoding/json
// read it as the zero value, a value the model never sent. A required field whose schema admits
// any value (any, json.RawMessage) takes null, and so does an optional one (a pointer, omitempty,
// omitzero), which OpenAI strict mode sends as null.
func TestFunc_NullForARequiredFieldIsRejected(t *testing.T) {
	tool := Func("t", "nulls", Safety{ReadOnly: true}, func(context.Context, nullArgs) (string, error) { return "ok", nil })
	base := map[string]string{
		"s": `"x"`, "l": `[1]`, "m": `{"a":1}`, "in": `{"id":"i"}`, "at": `"2026-01-02T03:04:05Z"`,
		"any": `1`, "raw": `{}`, "opt": `"o"`, "ptr": `"p"`, "tags": `{}`,
	}
	doc := func(null string) json.RawMessage {
		b := []byte("{")
		first := true
		for k, v := range base {
			if !first {
				b = append(b, ',')
			}
			first = false
			if k == null {
				v = "null"
			}
			b = append(b, `"`+k+`":`+v...)
		}
		return append(b, '}')
	}
	if _, err := tool.Call(context.Background(), doc("")); err != nil {
		t.Fatalf("Call with no null = %v", err)
	}
	for _, f := range []string{"s", "l", "m", "in", "at"} {
		if _, err := tool.Call(context.Background(), doc(f)); !errors.Is(err, ErrToolArgs) {
			t.Errorf("null for required %q: Call = %v, want ErrToolArgs", f, err)
		}
	}
	for _, f := range []string{"any", "raw", "opt", "ptr", "tags"} {
		if _, err := tool.Call(context.Background(), doc(f)); err != nil {
			t.Errorf("null for %q: Call = %v, want no error", f, err)
		}
	}
	// Nested: null for a required field of a nested struct.
	strict := Func("t2", "strict", Safety{ReadOnly: true}, func(context.Context, strictArgs) (string, error) { return "ok", nil })
	if _, err := strict.Call(context.Background(), json.RawMessage(`{"region":"eu","name":"a","inner":{"id":null}}`)); !errors.Is(err, ErrToolArgs) {
		t.Errorf("null for a nested required field: Call = %v, want ErrToolArgs", err)
	}
}

// SubAgent and final_answer reject null for a required field the same way.
func TestNullForARequiredField_SubAgentAndFinalAnswer(t *testing.T) {
	sub := SubAgent("helper", "helps", New(NewScriptedModel(TextTurn("done")), NewMemStore()))
	if _, err := sub.Call(context.Background(), json.RawMessage(`{"task":null}`)); !errors.Is(err, ErrToolArgs) {
		t.Errorf("SubAgent.Call({\"task\":null}) = %v, want ErrToolArgs", err)
	}
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("f1", finalAnswerTool, `{"name":null}`),
		ToolTurn("f2", finalAnswerTool, `{"name":"ok"}`),
	)}
	got, err := RunTyped[typedAnswer](context.Background(), New(m, NewMemStore()), "r", "go")
	if err != nil || got.Name != "ok" || m.calls.Load() != 2 {
		t.Errorf("RunTyped = %+v, %v after %d model calls; want the corrected answer after 2", got, err, m.calls.Load())
	}
}
