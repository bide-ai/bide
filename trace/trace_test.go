package trace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

const captureEnv = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"

func recorder() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	sr := tracetest.NewSpanRecorder()
	return sr, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func TestModel_EmitsGenAIChatSpan(t *testing.T) {
	sr, tp := recorder()
	tracer := tp.Tracer("test")

	base := agent.ModelHandler(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		return agent.ModelResponse{Usage: agent.Usage{InputTokens: 5, OutputTokens: 7}, Finish: agent.FinishToolUse}, nil
	})
	h := Model(tracer)(base)
	if _, err := h(context.Background(), agent.ModelCall{Model: describedModel{}}); err != nil {
		t.Fatal(err)
	}

	spans := sr.Ended()
	if len(spans) != 1 || spans[0].Name() != "chat claude-sonnet-4-6" {
		t.Fatalf("spans = %v", spans)
	}
	a := spanAttrs(spans[0])
	if a["gen_ai.operation.name"].AsString() != "chat" {
		t.Errorf("operation = %v", a["gen_ai.operation.name"])
	}
	if a["gen_ai.system"].AsString() != "anthropic" || a["gen_ai.request.model"].AsString() != "claude-sonnet-4-6" {
		t.Errorf("system = %v, model = %v; want the ones the call's Model describes", a["gen_ai.system"], a["gen_ai.request.model"])
	}
	if fr := a["gen_ai.response.finish_reasons"].AsStringSlice(); len(fr) != 1 || fr[0] != "tool_use" {
		t.Errorf("finish reasons = %v, want [tool_use]", fr)
	}
	if a["gen_ai.usage.input_tokens"].AsInt64() != 5 || a["gen_ai.usage.output_tokens"].AsInt64() != 7 {
		t.Errorf("usage attrs wrong: %v / %v", a["gen_ai.usage.input_tokens"], a["gen_ai.usage.output_tokens"])
	}
}

func TestTool_EmitsExecuteToolSpan(t *testing.T) {
	sr, tp := recorder()
	tracer := tp.Tracer("test")

	base := agent.ToolHandler(func(context.Context, agent.ToolCall) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})
	h := Tool(tracer)(base)
	if _, err := h(context.Background(), agent.ToolCall{Use: agent.ToolUse{ID: "c1", Name: "lookup"}}); err != nil {
		t.Fatal(err)
	}

	spans := sr.Ended()
	if len(spans) != 1 || spans[0].Name() != "execute_tool lookup" {
		t.Fatalf("spans = %v", spans)
	}
	a := spanAttrs(spans[0])
	if a["gen_ai.operation.name"].AsString() != "execute_tool" {
		t.Errorf("operation = %v", a["gen_ai.operation.name"])
	}
	if a["gen_ai.tool.name"].AsString() != "lookup" {
		t.Errorf("tool.name = %v", a["gen_ai.tool.name"])
	}
	if a["gen_ai.tool.call.id"].AsString() != "c1" {
		t.Errorf("tool.call.id = %v", a["gen_ai.tool.call.id"])
	}
}

// A nested tool call produces a child span: the inner span's parent is the outer span.
// This is what lets a sub-agent (which is just a tool) nest under its caller's trace.
func TestTool_NestsChildSpanAcrossBoundary(t *testing.T) {
	sr, tp := recorder()
	tracer := tp.Tracer("test")
	mw := Tool(tracer)

	// The "outer" tool, when run, itself invokes an "inner" traced tool with the same ctx —
	// exactly how a sub-agent tool re-enters the loop and runs its own tools.
	inner := mw(agent.ToolHandler(func(context.Context, agent.ToolCall) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}))
	outer := mw(agent.ToolHandler(func(ctx context.Context, _ agent.ToolCall) (json.RawMessage, error) {
		return inner(ctx, agent.ToolCall{Use: agent.ToolUse{ID: "c2", Name: "inner"}})
	}))
	if _, err := outer(context.Background(), agent.ToolCall{Use: agent.ToolUse{ID: "c1", Name: "outer"}}); err != nil {
		t.Fatal(err)
	}

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	// Spans end innermost-first: spans[0] is "inner", spans[1] is "outer".
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	in, out := byName["execute_tool inner"], byName["execute_tool outer"]
	if in == nil || out == nil {
		t.Fatalf("missing spans: %v", spans)
	}
	if in.Parent().SpanID() != out.SpanContext().SpanID() {
		t.Fatalf("inner span parent = %v, want outer span %v (trace must cross the boundary)",
			in.Parent().SpanID(), out.SpanContext().SpanID())
	}
}

// With OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT set, the chat span carries the
// input and output message content; without it (the default), it does not.
func TestModel_CapturesContentWhenEnabled(t *testing.T) {
	t.Setenv(captureEnv, "true")
	sr, tp := recorder()
	tracer := tp.Tracer("test")

	base := agent.ModelHandler(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		return agent.ModelResponse{}, nil
	})
	h := Model(tracer)(base) // captureContent() read here, so the env must be set first
	req := agent.Request{Messages: []agent.Message{agent.UserText("hello")}}
	if _, err := h(context.Background(), agent.ModelCall{Request: req}); err != nil {
		t.Fatal(err)
	}

	a := spanAttrs(sr.Ended()[0])
	if in, ok := a["gen_ai.input.messages"]; !ok || !strings.Contains(in.AsString(), "hello") {
		t.Errorf("input messages not captured: %v", a["gen_ai.input.messages"])
	}
	if _, ok := a["gen_ai.output.messages"]; !ok {
		t.Errorf("output messages not captured")
	}
}

func TestModel_NoContentByDefault(t *testing.T) {
	t.Setenv(captureEnv, "") // force off, deterministically
	sr, tp := recorder()
	tracer := tp.Tracer("test")

	base := agent.ModelHandler(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		return agent.ModelResponse{}, nil
	})
	h := Model(tracer)(base)
	req := agent.Request{Messages: []agent.Message{agent.UserText("secret")}}
	if _, err := h(context.Background(), agent.ModelCall{Request: req}); err != nil {
		t.Fatal(err)
	}

	a := spanAttrs(sr.Ended()[0])
	if _, ok := a["gen_ai.input.messages"]; ok {
		t.Error("input content captured without the opt-in env var (privacy leak)")
	}
	if _, ok := a["gen_ai.output.messages"]; ok {
		t.Error("output content captured without the opt-in env var (privacy leak)")
	}
}

// WithRates records USD cost on the chat span, computed from token usage.
func TestModel_RecordsCostWithRates(t *testing.T) {
	sr, tp := recorder()
	tracer := tp.Tracer("test")

	base := agent.ModelHandler(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		return agent.ModelResponse{Usage: agent.Usage{InputTokens: 1_000_000, OutputTokens: 2_000_000}}, nil
	})
	h := Model(tracer, WithRates(middleware.Rates{InputPer1M: 3, OutputPer1M: 15}))(base)
	if _, err := h(context.Background(), agent.ModelCall{Request: agent.Request{}}); err != nil {
		t.Fatal(err)
	}

	a := spanAttrs(sr.Ended()[0])
	got, ok := a["gen_ai.usage.cost"]
	if !ok {
		t.Fatal("no cost attribute recorded")
	}
	if want := 1*3.0 + 2*15.0; got.AsFloat64() != want { // 3 + 30 = 33 USD
		t.Errorf("cost = %v, want %v", got.AsFloat64(), want)
	}
}

// With the opt-in env var, the execute_tool span carries the tool arguments and result.
func TestTool_CapturesArgsAndResultWhenEnabled(t *testing.T) {
	t.Setenv(captureEnv, "1")
	sr, tp := recorder()
	tracer := tp.Tracer("test")

	base := agent.ToolHandler(func(context.Context, agent.ToolCall) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":1}`), nil
	})
	h := Tool(tracer)(base)
	if _, err := h(context.Background(), agent.ToolCall{Use: agent.ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`)}}); err != nil {
		t.Fatal(err)
	}

	a := spanAttrs(sr.Ended()[0])
	if a["gen_ai.tool.call.arguments"].AsString() != `{"q":"x"}` {
		t.Errorf("tool arguments = %v", a["gen_ai.tool.call.arguments"])
	}
	if a["gen_ai.tool.call.result"].AsString() != `{"ok":1}` {
		t.Errorf("tool result = %v", a["gen_ai.tool.call.result"])
	}
}

// instrModel calls the "ping" tool on the first turn, then answers, so a full run produces
// both a chat span and an execute_tool span.
type instrModel struct{}

func (instrModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	answered := false
	for _, msg := range req.Messages {
		if msg.Role == agent.RoleTool {
			answered = true
		}
	}
	var emits []agent.Emit
	if answered {
		emits = []agent.Emit{{Event: agent.TextDelta{Text: "done"}}, {Event: agent.Finish{Reason: "stop"}}}
	} else {
		emits = []agent.Emit{
			{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "ping", ArgsFragment: []byte(`{}`)}},
			{Event: agent.Finish{Reason: "tool_use"}},
		}
	}
	ch := make(chan agent.Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return agent.NewStream(ch), nil
}

type pingTool struct{}

func (pingTool) Name() string                { return "ping" }
func (pingTool) Description() string         { return "" }
func (pingTool) Safety() agent.Safety        { return agent.Safety{ReadOnly: true} }
func (pingTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (pingTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"pong":1}`), nil
}

// Instrument wires both the chat and execute_tool spans in one call: a full run emits both.
func TestInstrument_WiresChatAndToolSpans(t *testing.T) {
	sr, tp := recorder()
	tracer := tp.Tracer("test")

	a := buildAgent(t, agent.NewMemStore(), agent.WithTools(pingTool{}), Instrument(tracer))
	if _, err := a.Run(context.Background(), "r", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, s := range sr.Ended() {
		names[s.Name()] = true
	}
	if !names["chat"] {
		t.Errorf("no chat span emitted; got %v", names)
	}
	if !names["execute_tool ping"] {
		t.Errorf("no execute_tool span emitted; got %v", names)
	}
}

// gen_ai.usage.input_tokens is the whole prompt, cached or not. A call whose prompt was mostly
// served from the cache still processed every input token.
func TestModel_InputTokensIncludeCachedInput(t *testing.T) {
	sr, tp := recorder()
	base := agent.ModelHandler(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		return agent.ModelResponse{Usage: agent.Usage{InputTokens: 150, CacheReadTokens: 800, CacheWriteTokens: 50, OutputTokens: 7}}, nil
	})
	if _, err := Model(tp.Tracer("test"))(base)(context.Background(), agent.ModelCall{Request: agent.Request{}}); err != nil {
		t.Fatal(err)
	}
	if got := spanAttrs(sr.Ended()[0])["gen_ai.usage.input_tokens"].AsInt64(); got != 1000 {
		t.Errorf("gen_ai.usage.input_tokens = %d, want 1000 (150 uncached + 800 cache reads + 50 cache writes)", got)
	}
}

// describedModel is a Model that describes itself, as the first-party adapters do.
type describedModel struct{ agent.Model }

func (describedModel) Describe() agent.ModelInfo {
	return agent.ModelInfo{Provider: "anthropic", Model: "claude-sonnet-4-6"}
}

// A Model that does not describe itself gives an unnamed chat span with no provider or model.
func TestModel_UndescribedModel(t *testing.T) {
	sr, tp := recorder()
	base := agent.ModelHandler(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		return agent.ModelResponse{}, nil
	})
	if _, err := Model(tp.Tracer("test"))(base)(context.Background(), agent.ModelCall{Model: instrModel{}}); err != nil {
		t.Fatal(err)
	}
	s := sr.Ended()[0]
	a := spanAttrs(s)
	if _, ok := a["gen_ai.system"]; ok || s.Name() != "chat" {
		t.Errorf("span %q with attributes %v, want a span named chat with no gen_ai.system", s.Name(), a)
	}
}

// buildAgent builds an agent over instrModel and store's journal with opts, failing the test on
// an error.
func buildAgent(t *testing.T, store *agent.MemStore, opts ...agent.Option) *agent.Agent {
	t.Helper()
	j, err := agent.NewJournal(store)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(instrModel{}, j, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
