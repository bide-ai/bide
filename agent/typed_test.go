package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// ExampleAgent_RunTyped shows structured output: RunTyped returns a typed value, decoded from
// a schema-guided final_answer tool call (a real adapter streams the call the same way).
func ExampleAgent_RunTyped() {
	type Weather struct {
		City  string `json:"city"`
		TempF int    `json:"temp_f"`
	}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "final_answer", `{"city":"SF","temp_f":68}`),
		textTurn("done"),
	}}
	a := mustNew(m, memJournal())

	w, _, err := a.RunTyped[Weather](context.Background(), "run-1", UserText("weather in SF?"))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s is %d°F\n", w.City, w.TempF)
	// Output: SF is 68°F
}

type answer struct {
	Answer string `json:"answer"`
	Score  int    `json:"score"`
}

// The model calls final_answer with structured args; RunTyped decodes them into T.
func TestRunTyped_ExtractsFromToolCall(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "final_answer", `{"answer":"42","score":9}`),
		textTurn("done"),
	}}
	a := mustNew(m, memJournal())

	got, _, err := a.RunTyped[answer](context.Background(), "r", UserText("what is the meaning?"))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Answer != "42" || got.Score != 9 {
		t.Fatalf("got %+v, want {42 9}", got)
	}
}

// If the model answers in plain JSON text instead of calling the tool, RunTyped falls
// back to parsing that text.
func TestRunTyped_FallbackToText(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{textTurn(`{"answer":"7","score":5}`)}}
	a := mustNew(m, memJournal())

	got, _, err := a.RunTyped[answer](context.Background(), "r", UserText("q"))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Answer != "7" || got.Score != 5 {
		t.Fatalf("got %+v, want {7 5}", got)
	}
}

// A real tool runs first (doing work), then final_answer carries the typed result.
func TestRunTyped_WithWorkTool(t *testing.T) {
	var calls int
	work := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "lookup", `{"q":"x"}`),
		toolTurn("c2", "final_answer", `{"answer":"ok","score":1}`),
		textTurn("done"),
	}}
	a := mustNew(m, memJournal(), WithTools(work))

	got, _, err := a.RunTyped[answer](context.Background(), "r", UserText("q"))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if calls != 1 {
		t.Fatalf("work tool ran %d times, want 1", calls)
	}
	if got.Answer != "ok" || got.Score != 1 {
		t.Fatalf("got %+v", got)
	}
}

// The typed value is recovered from the journal on resume — a crash after the
// final_answer call is recorded still yields the value.
func TestRunTyped_ResumeSafe(t *testing.T) {
	// The accepted final_answer ends the run, so the crash comes as the run is marked complete.
	store := mustJournal(&failOnceStore{MemStore: NewMemStore(), name: runCompleteStep})

	crashy := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "final_answer", `{"answer":"42","score":9}`),
	}}
	if _, _, err := mustNew(crashy, store).RunTyped[answer](context.Background(), "r", UserText("q")); err == nil {
		t.Fatal("expected crash on first attempt")
	}

	recovered := &scriptModel{turns: [][]Emit{textTurn("done")}}
	got, _, err := mustNew(recovered, store).RunTyped[answer](context.Background(), "r", UserText("q"))
	if err != nil {
		t.Fatalf("resume RunTyped: %v", err)
	}
	if got.Answer != "42" || got.Score != 9 {
		t.Fatalf("got %+v, want {42 9} from journal", got)
	}
}

// RunTyped reserves the tool name final_answer for its answer, so New refuses a tool of that name.
func TestRunTyped_RejectsNameCollision(t *testing.T) {
	var c int
	clash := &countingTool{name: "final_answer", safety: Safety{ReadOnly: true}, calls: &c}
	if a, err := New(&scriptModel{turns: [][]Emit{textTurn("x")}}, memJournal(), WithTools(clash)); !errors.Is(err, ErrConfig) || a != nil {
		t.Fatalf("New = %v, %v; want nil and ErrConfig for a tool named final_answer", a, err)
	}
}

// With no accepted final_answer call, the answer is the text of the run's final turn, the same
// message Run returns and RunTyped (OutputNative) decodes. RunTyped used to take the last text of ANY turn,
// so a draft the model wrote beside a tool call stood in for a final turn that carried no text.
func TestRunTyped_TextFallbackIsTheFinalTurn(t *testing.T) {
	work := MustFunc("work", "does work", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithSafety(Safety{ReadOnly: true}))
	draft := []Event{
		TextDelta{Text: `{"name":"draft"}`},
		ToolCallDelta{Index: 0, ID: "w1", Name: "work", ArgsFragment: []byte(`{}`)},
		Finish{Reason: "tool_use"},
	}

	// The final turn has no text: there is no answer, not the draft.
	m := eventTurnsModel{draft, {Finish{Reason: "stop"}}}
	got, _, err := mustNew(m, memJournal(), WithTools(work)).RunTyped[typedAnswer](context.Background(), "r1", UserText("go"))
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("RunTyped = %+v, %v; want ErrProtocol (the final turn has no answer)", got, err)
	}

	// The final turn's text is the answer.
	m = eventTurnsModel{draft, {TextDelta{Text: `{"name":"final"}`}, Finish{Reason: "stop"}}}
	got, _, err = mustNew(m, memJournal(), WithTools(work)).RunTyped[typedAnswer](context.Background(), "r2", UserText("go"))
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
		if err := run(mustNew(m, memJournal())); !errors.Is(err, ErrConfig) {
			t.Errorf("RunTyped[%s] = %v, want ErrConfig", name, err)
		}
		if n := m.calls.Load(); n != 0 {
			t.Errorf("RunTyped[%s] called the model %d times, want 0", name, n)
		}
	}
	ctx := context.Background()
	check("[]string", func(a *Agent) error { _, _, err := a.RunTyped[[]string](ctx, "r", UserText("go")); return err })
	check("string", func(a *Agent) error { _, _, err := a.RunTyped[string](ctx, "r", UserText("go")); return err })
	check("any", func(a *Agent) error { _, _, err := a.RunTyped[any](ctx, "r", UserText("go")); return err })

	// A pointer to a struct, or a map, is an object.
	m := NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"name":"p"}`))
	if got, _, err := mustNew(m, memJournal()).RunTyped[*typedAnswer](ctx, "p", UserText("go")); err != nil || got == nil || got.Name != "p" {
		t.Errorf("RunTyped[*typedAnswer] = %+v, %v", got, err)
	}
	m = NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"k":1}`))
	if got, _, err := mustNew(m, memJournal()).RunTyped[map[string]int](ctx, "m", UserText("go")); err != nil || got["k"] != 1 {
		t.Errorf("RunTyped[map[string]int] = %v, %v", got, err)
	}
}

// RunTyped (OutputNative) sets a JSON-schema ResponseFormat on the request and decodes the model's
// direct JSON output (no final_answer tool).
func TestRunTyped_NativeResponseFormatAndDecode(t *testing.T) {
	type R struct {
		Answer string `json:"answer"`
		Score  int    `json:"score"`
	}
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn(`{"answer":"42","score":7}`)}}, got: &got}
	a := mustNew(m, memJournal())

	out, _, err := a.RunTyped[R](context.Background(), "r", UserText("q"), WithOutputMode(OutputNative))
	if err != nil {
		t.Fatalf("RunTyped (OutputNative): %v", err)
	}
	if out.Answer != "42" || out.Score != 7 {
		t.Fatalf("out = %+v, want {42 7}", out)
	}
	// The request carried a native response-format constraint, and no final_answer tool
	// was injected (native mode, not the tool-based path).
	if got.ResponseFormat == nil || got.ResponseFormat.Name != "response" || len(got.ResponseFormat.Schema) == 0 {
		t.Fatalf("ResponseFormat = %+v, want set with a schema", got.ResponseFormat)
	}
	for _, tl := range got.Tools {
		if tl.Name == finalAnswerTool {
			t.Fatal("native mode must not inject the final_answer tool")
		}
	}
}

// The caller's agent is untouched (RunTyped (OutputNative) works on a clone).
func TestRunTyped_NativeDoesNotMutateAgent(t *testing.T) {
	type R struct {
		X int `json:"x"`
	}
	m := &scriptModel{turns: [][]Emit{textTurn(`{"x":1}`)}}
	a := mustNew(m, memJournal())
	if _, _, err := a.RunTyped[R](context.Background(), "r", UserText("q"), WithOutputMode(OutputNative)); err != nil {
		t.Fatal(err)
	}
	if a.responseFormat != nil {
		t.Fatal("RunTyped (OutputNative) must not set responseFormat on the caller's agent")
	}
}

type typedAnswer struct {
	Name string `json:"name"`
}

// countModel counts model calls and delegates to another model.
type countModel struct {
	inner Model
	calls atomic.Int32
}

func (m *countModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	m.calls.Add(1)
	return m.inner.Stream(ctx, req)
}

// eventTurnsModel emits turns[i] on the call that follows i assistant turns (the last one repeats).
type eventTurnsModel [][]Event

func (m eventTurnsModel) Stream(_ context.Context, req Request) (*Stream, error) {
	i := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleAssistant {
			i++
		}
	}
	turn := m[min(i, len(m)-1)]
	ch := make(chan Emit, len(turn))
	for _, e := range turn {
		ch <- Emit{Event: e}
	}
	close(ch)
	return NewStream(ch), nil
}

// A final_answer call completes the run: the answer exists, so RunTyped does not ask the model
// for another turn. Before, the run went on until the model chose to stop, so a model that kept
// answering ran into ErrMaxTurns with an answer in hand.
func TestRunTyped_EndsAtFinalAnswer(t *testing.T) {
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("f1", finalAnswerTool, `{"name":"first"}`),
		ToolTurn("f2", finalAnswerTool, `{"name":"second"}`),
		ToolTurn("f3", finalAnswerTool, `{"name":"third"}`),
	)}
	a := mustNew(m, memJournal(), WithMaxTurns(3))
	got, _, err := a.RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "first" || m.calls.Load() != 1 {
		t.Fatalf("RunTyped = %+v after %d model calls, want the first answer after 1", got, m.calls.Load())
	}
}

// The answer is the first final_answer call the tool accepted: a call whose arguments do not
// decode is an error the model corrects, not the answer.
func TestRunTyped_AnswerIsTheAcceptedCall(t *testing.T) {
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("f1", finalAnswerTool, `{"name":5}`),
		ToolTurn("f2", finalAnswerTool, `{"name":"ok"}`),
		TextTurn("done"),
	)}
	got, _, err := mustNew(m, memJournal()).RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "ok" || m.calls.Load() != 2 {
		t.Fatalf("RunTyped = %+v after %d model calls, want \"ok\" after 2", got, m.calls.Load())
	}
}

// optionalAnswer has no required field, so a final_answer call with no arguments is accepted.
type optionalAnswer struct {
	Name string `json:"name,omitempty"`
}

// A final_answer call with no arguments, for a T with no required field, is the answer the tool
// accepted, the zero value. It is not replaced by whatever prose the model wrote.
func TestRunTyped_EmptyFinalAnswerIsNotReplacedByProse(t *testing.T) {
	m := eventTurnsModel{
		{ToolCallDelta{Index: 0, ID: "f1", Name: finalAnswerTool}, Finish{Reason: "tool_use"}},
		{TextDelta{Text: `{"name":"from prose"}`}, Finish{Reason: "stop"}},
	}
	store := memJournal()
	got, _, err := mustNew(m, store).RunTyped[optionalAnswer](context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got != (optionalAnswer{}) {
		t.Fatalf("RunTyped = %+v, want the zero answer the final_answer call carried", got)
	}
	// The call's empty arguments are journaled as the empty object the tool decoded.
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == "f1" && string(r.Result) != `{"accepted":{}}` {
			t.Fatalf("final_answer result = %s, want {\"accepted\":{}}", r.Result)
		}
	}
}

// Two accepted final_answer calls in one turn: the first, in the model's order, is the answer.
func TestRunTyped_FirstAcceptedCallInATurnWins(t *testing.T) {
	m := eventTurnsModel{{
		ToolCallDelta{Index: 0, ID: "f1", Name: finalAnswerTool, ArgsFragment: []byte(`{"name":"a"}`)},
		ToolCallDelta{Index: 1, ID: "f2", Name: finalAnswerTool, ArgsFragment: []byte(`{"name":"b"}`)},
		Finish{Reason: "tool_use"},
	}}
	got, _, err := mustNew(m, memJournal()).RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
	if err != nil || got.Name != "a" {
		t.Fatalf("RunTyped = %+v, %v; want the first call's answer \"a\"", got, err)
	}
}

// failOnceStore fails the first write of one step name, standing in for a crash just before it.
type failOnceStore struct {
	*MemStore
	name   string
	failed atomic.Bool
}

func (s *failOnceStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == s.name && s.failed.CompareAndSwap(false, true) {
		return Entry{}, false, errors.New("crash")
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// A run that crashed after its final_answer was recorded, but before it was marked complete,
// completes on resume without asking the model again.
func TestRunTyped_ResumeAfterFinalAnswerDoesNotAskAgain(t *testing.T) {
	store := mustJournal(&failOnceStore{MemStore: NewMemStore(), name: runCompleteStep})
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("f1", finalAnswerTool, `{"name":"first"}`),
		ToolTurn("f2", finalAnswerTool, `{"name":"second"}`),
	)}
	a := mustNew(m, store, WithMaxTurns(3))
	if _, _, err := a.RunTyped[typedAnswer](context.Background(), "r", UserText("go")); err == nil {
		t.Fatal("the first attempt should fail writing the completion marker")
	}
	got, _, err := a.RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatalf("resumed RunTyped: %v", err)
	}
	if got.Name != "first" || m.calls.Load() != 1 {
		t.Fatalf("resumed RunTyped = %+v after %d model calls, want the first answer after 1", got, m.calls.Load())
	}
	if ok, _ := IsComplete(context.Background(), store, "r"); !ok {
		t.Fatal("the resumed run is not marked complete")
	}
}

// WithOutputMode on an untyped run is ErrConfig at its first drive, before anything is journaled
// or any model call is made, as it is at a later drive.
func TestWithOutputModeOnUntypedFirstDriveIsErrConfig(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	m := &scriptModel{turns: [][]Emit{errTurn(errors.New("down")), textTurn("ok")}}
	a := mustNew(m, j)
	if _, err := a.Run(ctx, "r", UserText("go"), WithOutputMode(OutputNative)); !errors.Is(err, ErrConfig) {
		t.Fatalf("first drive = %v; want ErrConfig", err)
	}
	if _, ok, err := j.Get(ctx, "r", runStartStep); err != nil || ok {
		t.Fatalf("the refused drive journaled run:start (%v)", err)
	}
	if m.i != 0 {
		t.Fatalf("the refused drive made %d model calls", m.i)
	}
	if _, err := a.Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("setup: the first model turn should fail, leaving the run open")
	}
	if _, err := a.Run(ctx, "r", UserText("go"), WithOutputMode(OutputNative)); !errors.Is(err, ErrConfig) {
		t.Fatalf("later drive = %v; want ErrConfig", err)
	}
}

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
	tool := MustFunc("t", "strict", func(_ context.Context, in strictArgs) (string, error) {
		got = in
		return "ok", nil
	}, WithSafety(Safety{ReadOnly: true}))
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
	sub := MustSubAgent("helper", "helps", mustNew(NewScriptedModel(TextTurn("done")), memJournal()))
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
		store := memJournal()
		got, _, err := mustNew(m, store).RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
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

// A typed answer that reaches RunTyped as text, or RunTyped (OutputNative) as the provider's JSON, decodes
// strictly as well: a loose answer is ErrProtocol, not a value with zero or dropped fields.
func TestRunTyped_TextAnswersDecodeStrictly(t *testing.T) {
	for _, text := range []string{`{}`, `{"NAME":"x"}`, `{"name":"x","extra":1}`, `{"name":"a","name":"b"}`, `{"name":"x"} {}`} {
		m := NewScriptedModel(TextTurn(text))
		if got, _, err := mustNew(m, memJournal()).RunTyped[typedAnswer](context.Background(), "r", UserText("go")); !errors.Is(err, ErrProtocol) {
			t.Errorf("RunTyped with text %s = %+v, %v; want ErrProtocol", text, got, err)
		}
		m = NewScriptedModel(TextTurn(text))
		if got, _, err := mustNew(m, memJournal()).RunTyped[typedAnswer](context.Background(), "r", UserText("go"), WithOutputMode(OutputNative)); !errors.Is(err, ErrProtocol) {
			t.Errorf("RunTyped (OutputNative) with %s = %+v, %v; want ErrProtocol", text, got, err)
		}
	}
}

// RunTyped returns the arguments final_answer accepted: what the tool received, after tool
// middleware, as it does for any tool. It used to re-decode the model's original arguments, so a
// middleware that rewrote them (to fix a known quirk, or fill a default) completed the run and
// then failed it for good with ErrProtocol, on this call and every later one.
func TestRunTyped_AnswerIsWhatTheToolAccepted(t *testing.T) {
	m := &countModel{inner: NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"name":5}`))}
	store := memJournal()
	a := mustNew(
		m,
		store,
		WithToolMiddleware(func(next ToolHandler) ToolHandler {
			return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
				if call.Use.Name == finalAnswerTool {
					call.Use.Args = json.RawMessage(`{"name":"5"}`)
				}
				return next(ctx, call)
			}
		}),
	)
	for i := range 2 { // the first call runs, the second reads the finished run's journal
		got, _, err := a.RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
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
	store := memJournal()
	m := &countModel{inner: NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"name":"old","NOTE":"loose"}`))}
	// The agent as the older RunTyped built it: a final_answer tool that decodes with
	// encoding/json and acknowledges with {}.
	old := mustNew(m, store).cloneWith(legacyAnswerTool{})
	old.terminalTool = finalAnswerTool
	if _, err := old.Run(context.Background(), "r", UserText("go")); err != nil {
		t.Fatalf("old run: %v", err)
	}
	got, _, err := mustNew(m, store).RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
	if err != nil || got.Name != "old" || m.calls.Load() != 1 {
		t.Fatalf("RunTyped over an older journal = %+v, %v after %d model calls; want \"old\" after 1", got, err, m.calls.Load())
	}
}

// legacyAnswerTool is final_answer as the older RunTyped built it (a Func over typedAnswer when
// Func decoded with encoding/json): it accepts loose arguments and acknowledges with {}.
type legacyAnswerTool struct{}

// Spec describes the tool to the agent (see Tool).
func (t legacyAnswerTool) Spec() ToolSpec {
	return ToolSpec{Name: finalAnswerTool, Description: "answer", Input: json.RawMessage(`{"type":"object"}`), Safety: Safety{ReadOnly: true}}
}

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
	store := memJournal()
	m := &countModel{inner: eventTurnsModel{{ToolCallDelta{Index: 0, ID: "f1", Name: finalAnswerTool}, Finish{Reason: "tool_use"}}}}
	old := mustNew(m, store).cloneWith(legacyAnswerTool{})
	old.terminalTool = finalAnswerTool
	if _, err := old.Run(context.Background(), "r", UserText("go")); err != nil {
		t.Fatalf("old run: %v", err)
	}
	got, _, err := mustNew(m, store).RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
	if err != nil || got != (typedAnswer{}) || m.calls.Load() != 1 {
		t.Fatalf("RunTyped over an older journal = %+v, %v after %d model calls; want the zero answer after 1", got, err, m.calls.Load())
	}
}

// The journaled final_answer result is read strictly too: a tool middleware that rewrites it into
// something other than one accepted answer makes the answer ErrProtocol, not a guess.
func TestRunTyped_RewrittenResultIsReadStrictly(t *testing.T) {
	for _, result := range []string{`{"accepted":{"name":"a"},"extra":1}`, `{"Accepted":{"name":"a"}}`, `{"accepted":{"name":"a","x":1}}`} {
		m := NewScriptedModel(ToolTurn("f1", finalAnswerTool, `{"name":"a"}`))
		a := mustNew(
			m,
			memJournal(),
			WithToolMiddleware(func(next ToolHandler) ToolHandler {
				return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
					if _, err := next(ctx, call); err != nil {
						return nil, err
					}
					return json.RawMessage(result), nil
				}
			}),
		)
		if got, _, err := a.RunTyped[typedAnswer](context.Background(), "r", UserText("go")); !errors.Is(err, ErrProtocol) {
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
	tool := MustFunc("t", "nulls", func(context.Context, nullArgs) (string, error) { return "ok", nil }, WithSafety(Safety{ReadOnly: true}))
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
	strict := MustFunc("t2", "strict", func(context.Context, strictArgs) (string, error) { return "ok", nil }, WithSafety(Safety{ReadOnly: true}))
	if _, err := strict.Call(context.Background(), json.RawMessage(`{"region":"eu","name":"a","inner":{"id":null}}`)); !errors.Is(err, ErrToolArgs) {
		t.Errorf("null for a nested required field: Call = %v, want ErrToolArgs", err)
	}
}

// SubAgent and final_answer reject null for a required field the same way.
func TestNullForARequiredField_SubAgentAndFinalAnswer(t *testing.T) {
	sub := MustSubAgent("helper", "helps", mustNew(NewScriptedModel(TextTurn("done")), memJournal()))
	if _, err := sub.Call(context.Background(), json.RawMessage(`{"task":null}`)); !errors.Is(err, ErrToolArgs) {
		t.Errorf("SubAgent.Call({\"task\":null}) = %v, want ErrToolArgs", err)
	}
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("f1", finalAnswerTool, `{"name":null}`),
		ToolTurn("f2", finalAnswerTool, `{"name":"ok"}`),
	)}
	got, _, err := mustNew(m, memJournal()).RunTyped[typedAnswer](context.Background(), "r", UserText("go"))
	if err != nil || got.Name != "ok" || m.calls.Load() != 2 {
		t.Errorf("RunTyped = %+v, %v after %d model calls; want the corrected answer after 2", got, err, m.calls.Load())
	}
}
