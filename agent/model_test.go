package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/schema"
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

// describedModel is a Model that describes itself.
type describedModel struct{ info ModelInfo }

func (describedModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }
func (m describedModel) Describe() ModelInfo                            { return m.info }

// plainModel is a Model that neither describes nor unwraps.
type plainModel struct{}

func (plainModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }

// unwrapModel wraps inner without describing itself.
type unwrapModel struct{ inner Model }

func (unwrapModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }
func (m unwrapModel) Unwrap() Model                                  { return m.inner }

// selfModel unwraps to itself.
type selfModel struct{}

func (selfModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }
func (m selfModel) Unwrap() Model                                  { return m }

// describingWrapper describes itself and also unwraps: its own description wins.
type describingWrapper struct {
	unwrapModel
	info ModelInfo
}

func (m describingWrapper) Describe() ModelInfo { return m.info }

func wrapN(m Model, n int) Model {
	for range n {
		m = unwrapModel{m}
	}
	return m
}

func TestModelInfoOf(t *testing.T) {
	inner := ModelInfo{Provider: "p", Model: "m", ResponseFormat: true}
	outer := ModelInfo{Provider: "outer", Model: "o"}
	for name, tc := range map[string]struct {
		m    Model
		want ModelInfo
		ok   bool
	}{
		"describer":                  {describedModel{inner}, inner, true},
		"one wrapper":                {wrapN(describedModel{inner}, 1), inner, true},
		"three wrappers":             {wrapN(describedModel{inner}, 3), inner, true},
		"the outermost describer":    {describingWrapper{unwrapModel{describedModel{inner}}, outer}, outer, true},
		"no describer":               {plainModel{}, ModelInfo{}, false},
		"wrapper of a plain model":   {wrapN(plainModel{}, 2), ModelInfo{}, false},
		"nil":                        {nil, ModelInfo{}, false},
		"unwraps to nil":             {unwrapModel{nil}, ModelInfo{}, false},
		"cycle":                      {selfModel{}, ModelInfo{}, false},
		"describer at the last link": {wrapN(describedModel{inner}, maxUnwrap-1), inner, true},
		"describer past the bound":   {wrapN(describedModel{inner}, maxUnwrap), ModelInfo{}, false},
	} {
		got, ok := ModelInfoOf(tc.m)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: ModelInfoOf = %+v, %v; want %+v, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}

// errModel fails every call with err.
type errModel struct{ err error }

func (m errModel) Stream(context.Context, Request) (*Stream, error) { return nil, m.err }

// A failed model call is an ErrModel, and its message says so once: an adapter's error that
// already wraps ErrModel (every provider adapter's does) is not wrapped in it again, which printed
// "(model) (model)".
func TestRun_ModelErrorSaysModelOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"adapter error wrapping ErrModel", fmt.Errorf("openai (%w)", ErrModel)},
		{"plain error", errors.New("connection refused")},
	} {
		_, err := mustNew(errModel{c.err}, memJournal()).Run(context.Background(), "r", UserText("hi"))
		if !errors.Is(err, ErrModel) {
			t.Errorf("%s: err = %v, want ErrModel", c.name, err)
			continue
		}
		if n := strings.Count(err.Error(), "(model)"); n != 1 {
			t.Errorf("%s: %q says (model) %d times, want once", c.name, err, n)
		}
	}
}

// lastToolResultModel records the tool results each request carries, then delegates to a script.
type lastToolResultModel struct {
	script *ScriptedModel
	mu     sync.Mutex
	seen   []json.RawMessage
}

func (m *lastToolResultModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	m.mu.Lock()
	for _, msg := range req.Messages {
		for _, p := range msg.Parts {
			if tr, ok := p.(ToolResult); ok {
				m.seen = append(m.seen, tr.Result)
			}
		}
	}
	m.mu.Unlock()
	return m.script.Stream(ctx, req)
}

func (m *lastToolResultModel) results() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.seen))
	for i, r := range m.seen {
		out[i] = string(r)
	}
	return out
}

// JSON the library writes on a tool's behalf reaches the model as the text it encodes, without
// HTML escaping: an adapter forwards a tool result's JSON text to the model, and json.Marshal's
// default escapes would turn "<b>" into escape sequences the model then reads literally.
func TestModelVisibleJSON_NotHTMLEscaped(t *testing.T) {
	ctx := context.Background()
	script := func() *ScriptedModel { return NewScriptedModel(ToolTurn("c1", "t", `{}`), TextTurn("done")) }

	t.Run("typed tool result", func(t *testing.T) {
		type out struct {
			HTML string `json:"html"`
		}
		m := &lastToolResultModel{script: script()}
		tool := MustFunc("t", "", func(context.Context, struct{}) (out, error) {
			return out{HTML: "<b>a & b</b>"}, nil
		}, WithSafety(Safety{ReadOnly: true}))
		if _, err := mustNew(m, memJournal(), WithTools(tool)).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := m.results(); len(got) != 1 || got[0] != `{"html":"<b>a & b</b>"}` {
			t.Fatalf("model read tool results %q", got)
		}
	})

	t.Run("tool error text", func(t *testing.T) {
		m := &lastToolResultModel{script: script()}
		tool := MustFunc("t", "", func(context.Context, struct{}) (string, error) {
			return "", errors.New("bad <input> & more")
		}, WithSafety(Safety{ReadOnly: true}))
		if _, err := mustNew(m, memJournal(), WithTools(tool)).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := m.results(); len(got) != 1 || got[0] != `"bad <input> & more"` {
			t.Fatalf("model read tool results %q", got)
		}
	})

	t.Run("sub-agent answer", func(t *testing.T) {
		m := &lastToolResultModel{script: NewScriptedModel(ToolTurn("c1", "t", `{"task":"go"}`), TextTurn("done"))}
		child := mustNew(NewScriptedModel(TextTurn("<ok> & done")), memJournal())
		if _, err := mustNew(m, memJournal(), WithTools(MustSubAgent("t", "", child))).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := m.results(); len(got) != 1 || got[0] != `"<ok> & done"` {
			t.Fatalf("model read tool results %q", got)
		}
	})

	t.Run("resolved halt", func(t *testing.T) {
		store := memJournal()
		m := &lastToolResultModel{script: script()}
		tool := MustFunc("t", "", func(context.Context, struct{}) (string, error) { return "", nil })
		if _, err := store.do(ctx, "r", toolAttemptStep("c1"), func(context.Context) (Record, error) {
			return Record{Kind: StepAttempt, ToolUseID: "c1"}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.do(ctx, "r", "@llm/0", func(context.Context) (Record, error) {
			msg := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(`{}`)}}}
			return Record{Kind: StepModel, Message: &msg}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := ResolveHalt(ctx, store, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged <id=7> & sent", IsError: false}); err != nil {
			t.Fatal(err)
		}
		if _, err := mustNew(m, store, WithTools(tool)).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := m.results(); len(got) != 1 || got[0] != `"charged <id=7> & sent"` {
			t.Fatalf("model read tool results %q", got)
		}
	})
}

// Each thinking block becomes its own Reasoning part, in order: a signature ends a block, a
// redacted block stands alone and ends the one before it, and reasoning that never gets a
// signature (OpenAI-compatible reasoning_content) is kept as one part.
func TestMessage_ReasoningBlocks(t *testing.T) {
	for name, tc := range map[string]struct {
		events []Event
		want   []Part
	}{
		"unsigned": {
			[]Event{ReasoningDelta{Text: "a"}, ReasoningDelta{Text: "b"}, TextDelta{Text: "x"}},
			[]Part{Reasoning{Text: "ab"}, Text{Text: "x"}},
		},
		"signed blocks": {
			[]Event{ReasoningDelta{Text: "a"}, ReasoningDelta{Signature: "s1"}, ReasoningDelta{Text: "b"}, ReasoningDelta{Signature: "s2"}},
			[]Part{Reasoning{Text: "a", Signature: "s1"}, Reasoning{Text: "b", Signature: "s2"}},
		},
		"redacted between": {
			[]Event{ReasoningDelta{Text: "a"}, ReasoningDelta{Redacted: "R"}, ReasoningDelta{Text: "b"}},
			[]Part{Reasoning{Text: "a"}, Reasoning{Redacted: "R"}, Reasoning{Text: "b"}},
		},
	} {
		ch := make(chan Emit, len(tc.events)+1)
		for _, e := range tc.events {
			ch <- Emit{Event: e}
		}
		ch <- Emit{Event: Finish{}}
		close(ch)
		msg, _, err := NewStream(ch).Message()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(msg.Parts, tc.want) {
			t.Errorf("%s: parts = %+v, want %+v", name, msg.Parts, tc.want)
		}
	}
}

// Provider tokens a later turn must send back (a Gemini thoughtSignature on a call) are
// journaled with the part and survive a decode, so a resumed run sends them as the live one
// would have.
func TestEncodeRecord_KeepsProviderTokens(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{
		Reasoning{Text: "t", Signature: "s1"},
		Reasoning{Redacted: "ENCRYPTED"},
		ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(`{}`), Signature: "sig"},
	}}
	b, err := EncodeRecord(Record{Name: "@llm/0", Kind: StepModel, Message: &msg})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRecord(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got.Message, msg) {
		t.Fatalf("decoded %+v, want %+v", *got.Message, msg)
	}
}

// callStream is a model turn that calls tool name with id and ends with the given reason.
func callStream(id, name string, reason FinishReason) *Stream {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: id, Name: name, ArgsFragment: json.RawMessage(`{}`)}}
	ch <- Emit{Event: Finish{Reason: reason}}
	close(ch)
	return NewStream(ch)
}

// A turn whose reason says the model stopped to call tools, but that carries no call, lost the
// calls it was for: it is not an answer, however complete its text looks.
func TestStream_ToolUseReasonWithoutCallsIsNotAnAnswer(t *testing.T) {
	msg, _, err := finishStream("Let me look that up.", FinishToolUse).Message()
	if !errors.Is(err, ErrStreamProtocol) {
		t.Fatalf("got %q, %v; want ErrStreamProtocol", msg.Text(), err)
	}
}

// stopCallModel calls "lookup" on its first turn but reports the natural-stop reason (as OpenAI
// does under a forced tool_choice), then answers.
type stopCallModel struct{ n atomic.Int32 }

func (m *stopCallModel) Stream(context.Context, Request) (*Stream, error) {
	if m.n.Add(1) == 1 {
		return callStream("c1", "lookup", FinishStop), nil
	}
	return finishStream("done", FinishStop), nil
}

// Whether the loop runs tools is decided by the turn's content, never by its finish reason: a
// turn that calls a tool and reports "stop" still runs the call.
func TestRun_ToolCallsRunWhateverTheReason(t *testing.T) {
	var calls atomic.Int32
	tool := MustFunc("lookup", "", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "found", nil
	}, WithSafety(Safety{ReadOnly: true}))
	res, err := mustNew(&stopCallModel{}, memJournal(), WithTools(tool)).Run(context.Background(), "r", UserText("go"))
	var final Message
	if res != nil {
		final = res.Message
	}
	if err != nil || final.Text() != "done" || calls.Load() != 1 {
		t.Fatalf("final %q, err %v, tool ran %d times; want done, nil, 1", final.Text(), err, calls.Load())
	}
}

// finishStream is a model turn of text ending with the given finish reason.
func finishStream(text string, reason FinishReason) *Stream {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: text}}
	ch <- Emit{Event: Finish{Reason: reason}}
	close(ch)
	return NewStream(ch)
}

// The finish reason says whether the turn is the model's whole answer. A turn cut off at the
// token limit or stopped by a filter is not, so it is an error rather than a message: recorded as
// a final answer, it would end the run with half an answer, for good.
func TestStream_FinishReasonDecidesTheTurn(t *testing.T) {
	for reason, wantErr := range map[FinishReason]error{
		"":           nil, // a Model that does not report a reason
		"stop":       nil,
		"tool_use":   ErrStreamProtocol, // no call arrived: the calls the model ended its turn for are lost
		"length":     ErrOutputTruncated,
		"filtered":   ErrOutputFiltered,
		"end_turn":   ErrStreamProtocol, // a provider's own word: adapters map it, the core does not guess
		"pause_turn": ErrStreamProtocol,
	} {
		msg, _, err := finishStream("The total is", reason).Message()
		switch {
		case wantErr == nil && err != nil:
			t.Errorf("reason %q: err = %v, want the message", reason, err)
		case wantErr != nil && !errors.Is(err, wantErr):
			t.Errorf("reason %q: got %q, %v; want an error wrapping %v", reason, msg.Text(), err, wantErr)
		}
	}
}

// Truncated tool-call arguments stay ErrTruncatedToolArgs whatever the reason says, so a caller
// that classifies that error keeps working.
func TestStream_TruncatedArgsWinOverTheReason(t *testing.T) {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "cut", Name: "lookup", ArgsFragment: []byte(`{"q":`)}}
	ch <- Emit{Event: Finish{Reason: "length"}}
	close(ch)
	if _, _, err := NewStream(ch).Message(); !errors.Is(err, ErrTruncatedToolArgs) {
		t.Fatalf("err = %v, want ErrTruncatedToolArgs", err)
	}
}

// reasonModel answers every call with text and the given finish reason.
type reasonModel struct{ reason FinishReason }

func (m reasonModel) Stream(context.Context, Request) (*Stream, error) {
	return finishStream("The total is", m.reason), nil
}

// A run whose final turn was cut off does not complete: nothing is journaled for the turn, the
// run is not marked complete, and a later run asks the model again.
func TestRun_CutOffFinalTurnDoesNotComplete(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []FinishReason{FinishLength, FinishFiltered} {
		store := memJournal()
		res, err := mustNew(reasonModel{reason}, store).Run(ctx, "r", UserText("sum it"))
		var msg Message
		if res != nil {
			msg = res.Message
		}
		if err == nil {
			t.Errorf("reason %q: Run returned %q with no error", reason, msg.Text())
		}
		if done, _ := IsComplete(ctx, store, "r"); done {
			t.Errorf("reason %q: the run was marked complete", reason)
		}
		// Only the journal header and the run's start record (its input, see RunStart) are
		// journaled: nothing for the turn.
		if recs, _ := store.History(ctx, "r"); len(recs) != 2 || recs[0].Name != headerStep || recs[1].Name != runStartStep {
			t.Errorf("reason %q: journal has %d records, want only the header and %s", reason, len(recs), runStartStep)
		}
	}
}

// cutModel's response stops partway through the turn, as when a connection closes cleanly
// mid-stream: some text arrives, then the stream ends with no Finish.
type cutModel struct{}

func (cutModel) Stream(context.Context, Request) (*Stream, error) {
	ch := make(chan Emit, 1)
	ch <- Emit{Event: TextDelta{Text: "Your refund is appr"}}
	close(ch)
	return NewStream(ch), nil
}

// A response cut off partway is not the model's answer. The run must fail with
// ErrIncompleteResponse, stay incomplete, and journal nothing for the turn, so a retry asks
// the model again instead of the truncated text becoming the run's recorded answer.
func TestIncompleteResponse_IsNotTheAnswer(t *testing.T) {
	store := memJournal()
	res, err := mustNew(cutModel{}, store).Run(context.Background(), "r1", UserText("status of my refund?"))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if !errors.Is(err, ErrIncompleteResponse) || !errors.Is(err, ErrModel) {
		t.Errorf("run = %q, %v; want ErrIncompleteResponse (an ErrModel)", msg.Text(), err)
	}
	if complete, _ := IsComplete(context.Background(), store, "r1"); complete {
		t.Errorf("the run was marked complete with a truncated answer")
	}
	recs, _ := store.History(context.Background(), "r1")
	for _, r := range recs {
		if r.Kind == StepModel {
			t.Errorf("the truncated turn was journaled: %q", r.Message.Text())
		}
	}
}

// An error ends the stream: a consumer that keeps ranging after it sees that one error and
// nothing more, not a second ErrIncompleteResponse for the Finish that never came.
func TestStreamError_IsTerminal(t *testing.T) {
	boom := errors.New("boom")
	ch := make(chan Emit, 3)
	ch <- Emit{Event: TextDelta{Text: "a"}}
	ch <- Emit{Err: boom}
	ch <- Emit{Event: TextDelta{Text: "after"}}
	close(ch)
	var errs []error
	var events int
	for ev, err := range NewStream(ch).Events() {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_ = ev
		events++
	}
	if len(errs) != 1 || !errors.Is(errs[0], boom) || events != 1 {
		t.Fatalf("got events=%d errs=%v; want 1 event, then only boom", events, errs)
	}
}

// finishedThen returns a stream that sends some text, a Finish, and then late.
func finishedThen(late Event) *Stream {
	ch := make(chan Emit, 3)
	ch <- Emit{Event: TextDelta{Text: "done"}}
	ch <- Emit{Event: Finish{Reason: "stop"}}
	ch <- Emit{Event: late}
	close(ch)
	return NewStream(ch)
}

// A Finish ends the turn. An event after it (more content, or a second Finish) is a producer
// that broke its protocol, not more of the answer: the stream ends with ErrStreamProtocol and
// the consumer never sees the late event.
func TestStreamEventAfterFinish_IsProtocolError(t *testing.T) {
	for name, late := range map[string]Event{
		"text":          TextDelta{Text: " and more"},
		"tool call":     ToolCallDelta{Index: 0, ID: "c1", Name: "refund", ArgsFragment: json.RawMessage(`{}`)},
		"second finish": Finish{Reason: "stop"},
	} {
		var got []Event
		var errs []error
		for ev, err := range finishedThen(late).Events() {
			if err != nil {
				errs = append(errs, err)
				continue
			}
			got = append(got, ev)
		}
		if len(errs) != 1 || !errors.Is(errs[0], ErrStreamProtocol) || !errors.Is(errs[0], ErrModel) || len(got) != 2 {
			t.Errorf("%s after Finish: events %+v, errs %v; want the 2 events, then only ErrStreamProtocol", name, got, errs)
		}
		if msg, _, err := finishedThen(late).Message(); !errors.Is(err, ErrStreamProtocol) {
			t.Errorf("%s after Finish: Message = %q, %v; want ErrStreamProtocol", name, msg.Text(), err)
		}
	}
}

// A provider reporting negative token counts must not lower the run's totals: the response is
// rejected as a provider protocol fault.
func TestRun_NegativeUsageIsRejected(t *testing.T) {
	neg := Usage{InputTokens: -1000, OutputTokens: 5}
	for name, turn := range map[string][]Emit{
		"answer":   textTurnWithUsage("done", neg),
		"cache":    textTurnWithUsage("done", Usage{CacheReadTokens: -1}),
		"truncate": truncatedTurn(neg),
	} {
		m := &scriptModel{turns: [][]Emit{turn}}
		store := memJournal()
		_, err := mustNew(m, store).Run(context.Background(), "r", UserText("go"))
		if !errors.Is(err, ErrProtocol) || !errors.Is(err, ErrModel) {
			t.Fatalf("%s: err = %v, want a model protocol error", name, err)
		}
		recs, _ := store.History(context.Background(), "r")
		for _, r := range recs {
			for _, u := range []*Usage{r.Usage, r.DiscardedUsage} {
				if u != nil && (u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0) {
					t.Fatalf("%s: journaled negative usage %+v", name, *u)
				}
			}
		}
	}
}

// A middleware returning negative usage is rejected the same way.
func TestRun_NegativeUsageFromMiddlewareIsRejected(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	neg := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call)
			resp.Usage = Usage{OutputTokens: -7}
			return resp, err
		}
	}
	_, err := mustNew(m, memJournal(), WithMiddleware(neg)).Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want a protocol error", err)
	}
}

// A stream that fails after reporting negative usage reports none of the negative part.
func TestStream_FailedStreamNeverReportsNegativeUsage(t *testing.T) {
	ch := make(chan Emit, 3)
	ch <- Emit{Event: TextDelta{Text: "x"}}
	ch <- Emit{Event: Finish{Reason: "stop", Usage: Usage{InputTokens: -3, OutputTokens: 2}}}
	ch <- Emit{Event: TextDelta{Text: "late"}}
	close(ch)
	_, u, err := NewStream(ch).Message()
	if !errors.Is(err, ErrStreamProtocol) {
		t.Fatalf("err = %v, want ErrStreamProtocol", err)
	}
	if u != (Usage{OutputTokens: 2}) {
		t.Fatalf("usage = %+v, want the non-negative part {OutputTokens: 2}", u)
	}
}

// captureModel records the last Request it was asked to stream, then delegates.
type captureModel struct {
	inner Model
	got   *Request
}

func (m *captureModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	*m.got = req
	return m.inner.Stream(ctx, req)
}

// WithSampling options flow into the Request the model receives.
func TestSampling_FlowsIntoRequest(t *testing.T) {
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}, got: &got}
	a := mustNew(
		m,
		memJournal(),
		WithSampling(Temperature(0), MaxTokens(500), TopP(0.9), Stop("END"), Seed(42)),
	)

	if _, err := a.Run(context.Background(), "r", UserText("hi")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	s := got.Sampling
	if s.Temperature == nil || *s.Temperature != 0 {
		t.Fatalf("Temperature = %v, want set to 0", s.Temperature)
	}
	if s.MaxTokens == nil || *s.MaxTokens != 500 {
		t.Fatalf("MaxTokens = %v, want 500", s.MaxTokens)
	}
	if s.TopP == nil || *s.TopP != 0.9 {
		t.Fatalf("TopP = %v, want 0.9", s.TopP)
	}
	if len(s.Stop) != 1 || s.Stop[0] != "END" {
		t.Fatalf("Stop = %v, want [END]", s.Stop)
	}
	if s.Seed == nil || *s.Seed != 42 {
		t.Fatalf("Seed = %v, want 42", s.Seed)
	}
}

// Unset sampling stays nil (provider defaults), so an explicit 0 is distinguishable.
func TestSampling_UnsetIsNil(t *testing.T) {
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}, got: &got}
	a := mustNew(m, memJournal())

	if _, err := a.Run(context.Background(), "r", UserText("hi")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Sampling.Temperature != nil || got.Sampling.MaxTokens != nil {
		t.Fatal("unset sampling fields should be nil")
	}
}

// slowPart is what marshalPart wrote before spliceType: the part's struct decoded into a map and
// the map encoded. spliceType must write exactly these bytes, since they are journaled and an
// audit leaf commits to them.
func slowPart(t *testing.T, p Part) []byte {
	t.Helper()
	kind, err := partKind(p)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := marshalJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tagPart(inner, kind)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func checkSplice(t *testing.T, p Part) {
	t.Helper()
	kind, err := partKind(p)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := marshalJournal(p)
	if err != nil {
		return // marshalPart fails the same way whichever path it would take
	}
	got, ok := spliceType(inner, kind)
	if !ok {
		t.Fatalf("spliceType(%q) declined the encoding of a %T", inner, p)
	}
	if want := slowPart(t, p); !bytes.Equal(got, want) {
		t.Fatalf("spliceType(%q) =\n%q\nthe map encoding writes\n%q", inner, got, want)
	}
	viaPart, err := marshalPart(p)
	if err != nil || !bytes.Equal(viaPart, got) {
		t.Fatalf("marshalPart = %q, %v; want %q", viaPart, err, got)
	}
}

// Every kind of part, with the strings and raw JSON the journal must keep as they are, is
// spliced to the bytes the map encoding writes.
func TestSpliceType_MatchesMapEncoding(t *testing.T) {
	odd := "a <b> & c\"d\\e}f,g]h{i[j:k" + lineSep + paraSep + "\x00\x1f bad\xffutf8 é 😀"
	raws := []string{
		`{}`, `[]`, `null`, `true`, `false`, `0`, `-1.50e+20`, `""`, `"x"`,
		`{ "q" : "a<b && c>d" ,  "n": 1.50 }`,
		`{"a":{"b":[1,{"c":"}]"}],"d":"\"}"},"e":[[],{}]}`,
		"\"" + lineSep + "\xff\\u0041\\/\"",
		`[1e20, -0.0, 9007199254740993]`,
		`{"type":"not the part's"}`,
	}
	parts := []Part{
		Text{}, Text{Text: odd},
		Reasoning{}, Reasoning{Text: odd, Signature: odd, Redacted: odd},
		ToolUse{}, ToolUse{ID: odd, Name: odd, Signature: odd},
		ToolResult{}, ToolResult{ToolUseID: odd, IsError: true},
		Image{}, Image{Mime: odd, Data: []byte{0, 1, 0xff, '<', '"'}, URL: odd},
	}
	for _, raw := range raws {
		parts = append(parts,
			ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(raw)},
			ToolResult{ToolUseID: "c1", Result: json.RawMessage(raw), IsError: true})
	}
	for _, p := range parts {
		checkSplice(t, p)
	}
}

// spliceType declines what it cannot splice verbatim, and marshalPart then takes the map
// encoding: names that need unescaping, are not ASCII, repeat, or are "type" already, and
// anything that is not one compact object.
func TestSpliceType_Declines(t *testing.T) {
	for _, in := range []string{
		``, `{`, `[]`, `"x"`, `{"a":1,}`, `{"a":,"b":1}`, `{"a":1,"b"}`, `{"a"}`, `{"a":}`, `{,}`, `{"a":1"b":2}`,
		`{"a\"b":1}`, `{"a` + jsonEscape + `0062":1}`, `{"é":1}`, `{"a":1,"a":2}`, `{"type":"x"}`, `{"a":"unterminated}`,
		`{"a":[1}`, `{"a":[1}]}`, `{"a":{"b":1]}`, `{"a":[1],"b":}`, `{"a":1]`, `{ "a":1}`,
	} {
		if b, ok := spliceType([]byte(in), "text"); ok {
			t.Errorf("spliceType(%q) = %q, true; want it declined", in, b)
		}
	}
	if b, ok := spliceType([]byte(`{}`), "text"); !ok || string(b) != `{"type":"text"}` {
		t.Errorf(`spliceType("{}") = %q, %v`, b, ok)
	}
}

func FuzzSpliceType(f *testing.F) {
	f.Add("a <b> & c", []byte(`{ "h" : "<b>" }`), []byte{0xff})
	f.Add("x\xffy"+lineSep+"\x00", []byte("\"a\xffb"+paraSep+"\""), []byte(nil))
	f.Add("}],\"", []byte(`[1.50, 1e20, -0.0, {"a":"}"}]`), []byte("<>"))
	f.Fuzz(func(t *testing.T, s string, raw, data []byte) {
		parts := []Part{Text{Text: s}, Reasoning{Text: s, Signature: s, Redacted: s},
			ToolUse{ID: s, Name: s, Signature: s}, ToolResult{ToolUseID: s},
			Image{Mime: s, Data: data, URL: s}}
		if json.Valid(raw) {
			parts = append(parts, ToolUse{ID: s, Name: s, Args: raw}, ToolResult{ToolUseID: s, Result: raw, IsError: len(data)%2 == 0})
		}
		for _, p := range parts {
			checkSplice(t, p)
		}
	})
}

// TestSystemPrompt_SeededAsFirstMessage verifies that WithSystemPrompt causes
// the first message in the model Request to be a RoleSystem message with the
// configured text, followed by the RoleUser input.
func TestSystemPrompt_SeededAsFirstMessage(t *testing.T) {
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}, got: &got}
	a := mustNew(m, memJournal(), WithSystemPrompt("you are terse"))

	if _, err := a.Run(context.Background(), "r1", UserText("hi")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(got.Messages) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(got.Messages))
	}
	sys := got.Messages[0]
	if sys.Role != RoleSystem {
		t.Fatalf("Messages[0].Role = %q, want %q", sys.Role, RoleSystem)
	}
	if len(sys.Parts) == 0 {
		t.Fatal("Messages[0] has no parts")
	}
	txt, ok := sys.Parts[0].(Text)
	if !ok {
		t.Fatalf("Messages[0].Parts[0] type = %T, want Text", sys.Parts[0])
	}
	if txt.Text != "you are terse" {
		t.Fatalf("system text = %q, want %q", txt.Text, "you are terse")
	}

	user := got.Messages[1]
	if user.Role != RoleUser {
		t.Fatalf("Messages[1].Role = %q, want %q", user.Role, RoleUser)
	}
}

// TestSystemPrompt_AbsentByDefault verifies that without WithSystemPrompt the
// first message is the RoleUser input (no system message prepended).
func TestSystemPrompt_AbsentByDefault(t *testing.T) {
	var got Request
	m := &captureModel{inner: &scriptModel{turns: [][]Emit{textTurn("ok")}}, got: &got}
	a := mustNew(m, memJournal())

	if _, err := a.Run(context.Background(), "r2", UserText("hello")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(got.Messages) == 0 {
		t.Fatal("expected at least 1 message, got 0")
	}
	if got.Messages[0].Role != RoleUser {
		t.Fatalf("Messages[0].Role = %q, want %q (no system prompt)", got.Messages[0].Role, RoleUser)
	}
}

// TestSystemPrompt_PersistsAcrossToolTurns verifies that the system message
// appears at position 0 on the second model call (after a tool turn), confirming
// it is re-seeded for every call.
func TestSystemPrompt_PersistsAcrossToolTurns(t *testing.T) {
	var requests []Request
	inner := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "noop", `{}`),
		textTurn("done"),
	}}
	capture := &multiCaptureModel{inner: inner, requests: &requests}

	noopTool := MustFunc("noop",
		"does nothing",
		func(_ context.Context, _ struct{}) (struct{}, error) { return struct{}{}, nil }, WithSafety(Safety{ReadOnly: true}))

	a := mustNew(capture, memJournal(), WithTools(noopTool), WithSystemPrompt("be brief"))

	if _, err := a.Run(context.Background(), "r3", UserText("go")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(requests) < 2 {
		t.Fatalf("expected at least 2 model calls, got %d", len(requests))
	}
	for i, req := range requests {
		if len(req.Messages) == 0 {
			t.Fatalf("turn %d: no messages", i)
		}
		if req.Messages[0].Role != RoleSystem {
			t.Fatalf("turn %d: Messages[0].Role = %q, want system", i, req.Messages[0].Role)
		}
		txt, ok := req.Messages[0].Parts[0].(Text)
		if !ok || txt.Text != "be brief" {
			t.Fatalf("turn %d: system text = %q, want %q", i, txt.Text, "be brief")
		}
	}
}

// multiCaptureModel records every Request passed to Stream.
type multiCaptureModel struct {
	inner    Model
	requests *[]Request
}

func (m *multiCaptureModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	// deep-copy Messages slice so journal replay appends don't mutate our snapshot
	cp := make([]Message, len(req.Messages))
	copy(cp, req.Messages)
	*m.requests = append(*m.requests, Request{
		Messages: cp,
		Tools:    req.Tools,
		Sampling: req.Sampling,
	})
	return m.inner.Stream(ctx, req)
}

// Ensure multiCaptureModel satisfies the tool-args schema for the noop tool by
// providing a minimal ArgsSchema implementation via the Func helper. The test
// already uses Func, so this comment is for clarity only.
var _ json.RawMessage = nil // suppress unused-import lint

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

	if _, err := a.Run(context.Background(), "r1", UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if textOf(got.Messages[0]) != "turn 1" || got.Messages[0].Role != RoleSystem {
		t.Fatalf("run 1 system = %+v, want 'turn 1'", got.Messages[0])
	}
	if _, err := a.Run(context.Background(), "r2", UserText("hi")); err != nil {
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

	if _, err := a.Run(context.Background(), "r", UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if textOf(got.Messages[0]) != "dynamic" {
		t.Fatalf("system = %q, want 'dynamic' (func wins)", textOf(got.Messages[0]))
	}
}

type hiddenArgs struct {
	Q string `json:"q"`
}

// undecodableArgs reaches its only field through an embedded pointer to an unexported struct,
// which encoding/json cannot allocate: no call's arguments could ever decode.
type undecodableArgs struct {
	*hiddenArgs
}

// Func refuses an argument type no call could decode, at construction, rather than build a tool
// with no schema whose every call fails.
func TestFunc_UndescribableArgsPanics(t *testing.T) {
	defer func() {
		r := recover()
		err, _ := r.(error)
		if !errors.Is(err, schema.ErrUnsupportedType) || !strings.Contains(fmt.Sprint(r), `"q"`) {
			t.Fatalf("Func panicked with %v; want an error wrapping schema.ErrUnsupportedType naming the field", r)
		}
	}()
	MustFunc("t", "", func(context.Context, undecodableArgs) (string, error) { return "", nil }, WithSafety(Safety{ReadOnly: true}))
}

// RunTyped reports such a result type as a configuration error before running anything.
func TestRunTyped_UndescribableTypeIsConfigError(t *testing.T) {
	a := mustNew(NewScriptedModel(TextTurn(`{}`)), memJournal())
	if _, _, err := a.RunTyped[undecodableArgs](context.Background(), "r", UserText("go")); !errors.Is(err, ErrConfig) || !errors.Is(err, schema.ErrUnsupportedType) {
		t.Fatalf("RunTyped = %v; want ErrConfig wrapping schema.ErrUnsupportedType", err)
	}
	if _, _, err := a.RunTyped[undecodableArgs](context.Background(), "r2", UserText("go"), WithOutputMode(OutputNative)); !errors.Is(err, ErrConfig) || !errors.Is(err, schema.ErrUnsupportedType) {
		t.Fatalf("RunTyped (OutputNative) = %v; want ErrConfig wrapping schema.ErrUnsupportedType", err)
	}
}
