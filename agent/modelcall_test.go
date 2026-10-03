package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// Two calls that add a hook to the same parent call (two hedged targets through the same
// middleware) each keep their own hook: adding one must not overwrite the other's, even when the
// parent's hook slice has spare capacity.
func TestAddHook_SiblingsKeepTheirOwnHooks(t *testing.T) {
	var got []string
	mark := func(s string) ModelCallHook {
		return ModelCallHook{After: func(context.Context, ModelCall, ModelAttempt) { got = append(got, s) }}
	}
	parent := ModelCall{hooks: make([]ModelCallHook, 3, 8)} // spare capacity
	a := parent.AddHook(mark("a"))
	b := parent.AddHook(mark("b"))
	for _, c := range []ModelCall{a, b} {
		for _, h := range c.hooks {
			if h.After != nil {
				h.After(context.Background(), c, ModelAttempt{})
			}
		}
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("hooks ran %v, want [a b]", got)
	}
	if len(parent.hooks) != 3 {
		t.Fatalf("the parent has %d hooks after its children added theirs, want 3", len(parent.hooks))
	}
}

// finishTurn is a text turn ending with reason and raw, reporting usage u.
func finishTurn(text string, reason FinishReason, raw string, u Usage) []Emit {
	return []Emit{{Event: TextDelta{Text: text}}, {Event: Finish{Reason: reason, Raw: raw, Usage: u}}}
}

// describedScript is a scriptModel that describes itself.
type describedScript struct{ *scriptModel }

func (describedScript) Describe() ModelInfo {
	return ModelInfo{Provider: "acme", Model: "acme-large", ResponseFormat: true}
}

// modelRecords returns the StepModel records of run runID.
func modelRecords(t *testing.T, store *Journal, runID string) []Record {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []Record
	for _, r := range recs {
		if r.Kind == StepModel {
			out = append(out, r)
		}
	}
	return out
}

// A middleware that builds its own ModelCall, instead of passing on a copy of the one it
// received, would drop the hooks outer middleware added and hide its requests from the run's
// spend meter. The model handler refuses such a call with ErrConfig, and sends nothing.
func TestModelCall_ForeignCallIsRefused(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	fresh := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			return next(ctx, ModelCall{Request: call.Request, Model: call.Model, RunID: call.RunID, Turn: call.Turn})
		}
	}
	_, err := mustNew(m, memJournal(), WithMiddleware(fresh)).Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig", err)
	}
	if m.i != 0 {
		t.Fatalf("the model was sent %d requests, want none", m.i)
	}
	nilModel := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.Model = nil
			return next(ctx, call)
		}
	}
	if _, err := mustNew(m, memJournal(), WithMiddleware(nilModel)).Run(context.Background(), "r", UserText("go")); !errors.Is(err, ErrConfig) {
		t.Fatalf("a call retargeted to a nil Model: err = %v, want ErrConfig", err)
	}
}

// The run's spend meter is not a hook: whatever a middleware does with the responses it gets, and
// whatever hooks it adds, every request the turn sends counts in Result.Spend and the budget.
// Here a middleware sends the call twice and keeps only the second response, and adds a hook of
// its own; the first request's usage is still spend.
func TestModelCall_MiddlewareCannotHideSpend(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("first", billed), textTurnWithUsage("second", billed)}}
	twiceMW := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call = call.AddHook(ModelCallHook{After: func(context.Context, ModelCall, ModelAttempt) {}})
			if _, err := next(ctx, call); err != nil {
				return ModelResponse{}, err
			}
			return next(ctx, call)
		}
	}
	res, err := mustNew(m, memJournal(), WithMiddleware(twiceMW)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Text() != "second" || res.Usage != billed || res.Spend != twice(billed) {
		t.Fatalf("answer %q, Usage %+v, Spend %+v; want second, %+v, %+v", res.Message.Text(), res.Usage, res.Spend, billed, twice(billed))
	}
}

// A hook whose Before returned nil gets exactly one After, even when a later hook's Before fails
// and the request is not sent; the hooks after the failing one do not run at all.
func TestModelCallHook_BeforeFailure(t *testing.T) {
	var got []string
	hook := func(name string, fail bool) ModelCallHook {
		return ModelCallHook{
			Before: func(context.Context, ModelCall) error {
				got = append(got, name+".before")
				if fail {
					return errors.New(name + " refused")
				}
				return nil
			},
			After: func(_ context.Context, _ ModelCall, a ModelAttempt) {
				got = append(got, fmt.Sprintf("%s.after(%v)", name, a.Err))
			},
		}
	}
	mw := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			return next(ctx, call.AddHook(hook("a", false)).AddHook(hook("b", true)).AddHook(hook("c", false)))
		}
	}
	m := &scriptModel{turns: [][]Emit{textTurn("unsent")}}
	_, err := CallModel(context.Background(), m, Request{}, mw)
	if err == nil || err.Error() != "b refused" {
		t.Fatalf("err = %v, want b's refusal", err)
	}
	want := []string{"a.before", "b.before", "a.after(b refused)"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("hooks ran %q, want %q", got, want)
	}
	if m.i != 0 {
		t.Fatalf("the model was sent %d requests, want none", m.i)
	}
}

// CallModel sends a call outside an agent through the same model handler: hooks run, requests are
// numbered, the response carries the finish reason, and a nil Model is ErrConfig.
func TestCallModel(t *testing.T) {
	if _, err := CallModel(context.Background(), nil, Request{}); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil Model: err = %v, want ErrConfig", err)
	}
	m := &scriptModel{turns: [][]Emit{errTurn(errors.New("reset")), finishTurn("ok", FinishStop, "end_turn", billed)}}
	var attempts []int
	var spent Usage
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			if call.RunID != "" || call.Turn != 0 || call.Attempt() != 0 {
				t.Errorf("call = run %q turn %d attempt %d, want all zero outside an agent", call.RunID, call.Turn, call.Attempt())
			}
			return next(ctx, call.AddHook(ModelCallHook{After: func(_ context.Context, c ModelCall, a ModelAttempt) {
				attempts = append(attempts, c.Attempt())
				addUsage(&spent, a.Response.Usage)
			}}))
		}
	}
	resp, err := CallModel(context.Background(), m, Request{Messages: []Message{UserText("q")}}, count, retryOnceMW)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "ok" || resp.Finish != FinishStop || resp.RawFinish != "end_turn" || resp.Usage != billed {
		t.Fatalf("response = %+v", resp)
	}
	if fmt.Sprint(attempts) != "[1 2]" || spent != billed {
		t.Fatalf("hooks saw attempts %v and spend %+v; want [1 2] and %+v", attempts, spent, billed)
	}
}

// A response a middleware builds is checked like a model's: an empty finish reason is recorded
// as FinishStop, a reason that is not an answer is the error the model's own would be, and the
// loop runs the calls a response carries whatever its reason says.
func TestModelResponse_BuiltByMiddleware(t *testing.T) {
	build := func(resp ModelResponse) Middleware {
		return func(ModelHandler) ModelHandler {
			return func(context.Context, ModelCall) (ModelResponse, error) { return resp, nil }
		}
	}
	text := Message{Role: RoleAssistant, Parts: []Part{Text{Text: "built"}}}
	store := memJournal()
	if _, err := mustNew(&scriptModel{}, store, WithMiddleware(build(ModelResponse{Message: text}))).Run(context.Background(), "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	recs := modelRecords(t, store, "r")
	if len(recs) != 1 || recs[0].Finish() != FinishStop || recs[0].Model() != nil {
		t.Fatalf("records = %+v, want one with Finish stop and no Model", recs)
	}
	for reason, want := range map[FinishReason]error{
		FinishLength:   ErrOutputTruncated,
		FinishFiltered: ErrOutputFiltered,
		FinishToolUse:  ErrStreamProtocol, // no tool call
		"invented":     ErrStreamProtocol,
	} {
		_, err := mustNew(
			&scriptModel{},
			memJournal(),
			WithMiddleware(build(ModelResponse{Message: text, Finish: reason})),
		).Run(context.Background(), "r", UserText("go"))
		if !errors.Is(err, want) {
			t.Errorf("Finish %q: err = %v, want %v", reason, err, want)
		}
	}

	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	n := 0
	stopWithCall := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			if n++; n == 1 {
				return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`)}}}, Finish: FinishStop}, nil
			}
			return next(ctx, call)
		}
	}
	m := &scriptModel{turns: [][]Emit{textTurn("done")}}
	if _, err := mustNew(m, memJournal(), WithTools(tool), WithMiddleware(stopWithCall)).Run(context.Background(), "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("the tool ran %d times, want 1: the loop must run a turn's calls whatever its Finish says", calls)
	}
}

// Each model record journals what its own turn was sent: the digests of the system prompt and
// tool set of the request that produced the response, and the model that answered it. A
// middleware that changes the system prompt on the second turn changes that turn's digest only.
func TestModelRecord_JournalsPerTurnDigests(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := describedScript{&scriptModel{turns: [][]Emit{
		{{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{}`)}}, {Event: Finish{Reason: FinishToolUse, Raw: "tool_calls"}}},
		finishTurn("done", FinishStop, "end_turn", Usage{}),
	}}}
	turn := 0
	amend := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			if turn++; turn == 2 {
				call.Request.Messages = append([]Message{SystemText("AMENDED")}, call.Request.Messages...)
			}
			return next(ctx, call)
		}
	}
	store := memJournal()
	if _, err := must(mustNew(m, store, WithTools(tool), WithSystemPrompt("OPERATOR")).With(WithMiddleware(amend))).Run(context.Background(), "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	recs := modelRecords(t, store, "r")
	if len(recs) != 2 {
		t.Fatalf("%d model records, want 2", len(recs))
	}
	tools := ToolsDigest([]ToolSpec{tool.Spec()})
	want := []struct {
		prompt       string
		finish       FinishReason
		raw          string
		promptDigest string
	}{
		{"OPERATOR", FinishToolUse, "tool_calls", PromptDigest([]Message{SystemText("OPERATOR")})},
		{"AMENDED+OPERATOR", FinishStop, "end_turn", PromptDigest([]Message{SystemText("AMENDED"), SystemText("OPERATOR")})},
	}
	for i, r := range recs {
		w := want[i]
		if r.PromptDigest() != w.promptDigest || r.ToolsDigest() != tools || r.Finish() != w.finish || r.RawFinish() != w.raw {
			t.Errorf("turn %d: prompt %s tools %s finish %q/%q; want the digest of %s, %s, %q/%q", i, r.PromptDigest(), r.ToolsDigest(), r.Finish(), r.RawFinish(), w.prompt, tools, w.finish, w.raw)
		}
		if r.Model() == nil || *r.Model() != (ModelInfo{Provider: "acme", Model: "acme-large", ResponseFormat: true}) {
			t.Errorf("turn %d: Model = %+v, want the describing model's info", i, r.Model())
		}
	}
	if recs[0].PromptDigest() == recs[1].PromptDigest() {
		t.Error("the two turns were sent different prompts but journal one digest")
	}
	back, err := DecodeRecord(recs[1].Raw())
	if err != nil || back.PromptDigest() != recs[1].PromptDigest() || back.Model() == nil || back.Finish() != FinishStop {
		t.Fatalf("the journal encoding does not round-trip the new fields: %+v, %v", back, err)
	}
}

// The digests are canonical: the tool set's does not depend on the order the tools are listed in,
// no two different prompts share one, and nothing sent is "".
func TestDigests(t *testing.T) {
	a := MustFunc("a", "first", func(context.Context, struct{}) (string, error) { return "", nil }, WithSafety(Safety{ReadOnly: true}))
	b := MustFunc("b", "second", func(context.Context, struct{}) (string, error) { return "", nil }, WithSafety(Safety{ReadOnly: true}))
	if ToolsDigest([]ToolSpec{a.Spec(), b.Spec()}) != ToolsDigest([]ToolSpec{b.Spec(), a.Spec()}) {
		t.Error("ToolsDigest depends on the order of the tools")
	}
	if ToolsDigest([]ToolSpec{a.Spec()}) == ToolsDigest([]ToolSpec{a.Spec(), b.Spec()}) || ToolsDigest(nil) != "" {
		t.Error("ToolsDigest does not tell tool sets apart")
	}
	if PromptDigest([]Message{UserText("q")}) != "" {
		t.Error("PromptDigest of a request with no system message is not empty")
	}
	ab := PromptDigest([]Message{SystemText("a"), SystemText("b")})
	if ab == PromptDigest([]Message{SystemText("ab")}) || ab == PromptDigest([]Message{SystemText("b"), SystemText("a")}) {
		t.Error("PromptDigest gives two different prompts one digest")
	}
}

// Replay reproduces what the original run journaled for each turn: its finish reason and the
// provider's raw reason, and its spend, including the requests the turn discarded, so the
// replayed run's Result.Spend and records match the original's.
func TestReplay_ReproducesFinishAndSpend(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		truncatedTurn(billed), // discarded by the retry
		{{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{}`)}}, {Event: Finish{Reason: FinishToolUse, Raw: "tool_calls", Usage: billed}}},
		finishTurn("done", FinishStop, "end_turn", billed),
	}}
	src := memJournal()
	orig, err := mustNew(m, src, WithTools(tool), WithMiddleware(retryOnceMW)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	rm, err := Replay(context.Background(), src, "r")
	if err != nil {
		t.Fatal(err)
	}
	dst := memJournal()
	again, err := mustNew(rm, dst, WithTools(tool)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Spend != orig.Spend || again.Usage != orig.Usage || orig.Spend == orig.Usage {
		t.Fatalf("replayed Spend %+v Usage %+v, original %+v %+v", again.Spend, again.Usage, orig.Spend, orig.Usage)
	}
	want, got := modelRecords(t, src, "r"), modelRecords(t, dst, "r")
	if len(got) != len(want) {
		t.Fatalf("%d replayed model records, want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if g.Finish() != w.Finish() || g.RawFinish() != w.RawFinish() || fmt.Sprint(g.DiscardedUsage) != fmt.Sprint(w.DiscardedUsage) || *g.Usage != *w.Usage {
			t.Errorf("turn %d: replayed %q/%q discarded %v usage %v, original %q/%q %v %v", i, g.Finish(), g.RawFinish(), g.DiscardedUsage, *g.Usage, w.Finish(), w.RawFinish(), w.DiscardedUsage, *w.Usage)
		}
	}
	if want[0].RawFinish() != "tool_calls" || want[0].DiscardedUsage == nil {
		t.Fatalf("setup: the original's first turn journaled %+v", want[0])
	}
}

// Messages and Tools reach every handler clipped: a middleware that appends to them cannot write
// into spare capacity its caller still holds.
func TestModelCall_RequestSlicesAreClipped(t *testing.T) {
	spare := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			msgs := make([]Message, len(call.Request.Messages), len(call.Request.Messages)+4)
			copy(msgs, call.Request.Messages)
			call.Request.Messages = msgs
			resp, err := next(ctx, call)
			if full := msgs[:cap(msgs)]; full[len(msgs)].Role != "" {
				t.Errorf("a handler below wrote %+v into the caller's spare capacity", full[len(msgs)])
			}
			return resp, err
		}
	}
	appendOne := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			if c := cap(call.Request.Messages); c != len(call.Request.Messages) {
				t.Errorf("Messages arrived with capacity %d for length %d", c, len(call.Request.Messages))
			}
			call.Request.Messages = append(call.Request.Messages, UserText("appended"))
			return next(ctx, call)
		}
	}
	m := &scriptModel{turns: [][]Emit{textTurn("ok")}}
	if _, err := CallModel(context.Background(), m, Request{Messages: []Message{UserText("q")}}, spare, appendOne); err != nil {
		t.Fatal(err)
	}
}

// The model call path carries no engine data in the context: generate.go and modelcall.go call no
// context.WithValue (and read no ctx.Value), so middleware cannot reach or replace the sink, the
// hooks or the meter through the context.
func TestModelCall_NoContextValues(t *testing.T) {
	for _, file := range []string{"generate.go", "modelcall.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "context" && sel.Sel.Name == "WithValue" {
				t.Errorf("%s calls context.WithValue", file)
			}
			if sel.Sel.Name == "Value" {
				t.Errorf("%s reads a context value (%s.Value)", file, types.ExprString(sel.X))
			}
			return true
		})
	}
}
