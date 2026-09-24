package trace

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	agent "github.com/dayna/go-agents"
)

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

	base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
		return agent.Message{}, agent.Usage{InputTokens: 5, OutputTokens: 7}, nil
	})
	h := Model(tracer, WithModel("claude-sonnet-4-6"), WithSystem("anthropic"))(base)
	if _, _, err := h(context.Background(), agent.Request{}); err != nil {
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
	if a["gen_ai.system"].AsString() != "anthropic" {
		t.Errorf("system = %v", a["gen_ai.system"])
	}
	if a["gen_ai.usage.input_tokens"].AsInt64() != 5 || a["gen_ai.usage.output_tokens"].AsInt64() != 7 {
		t.Errorf("usage attrs wrong: %v / %v", a["gen_ai.usage.input_tokens"], a["gen_ai.usage.output_tokens"])
	}
}

func TestTool_EmitsExecuteToolSpan(t *testing.T) {
	sr, tp := recorder()
	tracer := tp.Tracer("test")

	base := agent.ToolHandler(func(context.Context, agent.ToolUse) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})
	h := Tool(tracer)(base)
	if _, err := h(context.Background(), agent.ToolUse{ID: "c1", Name: "lookup"}); err != nil {
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
	inner := mw(agent.ToolHandler(func(context.Context, agent.ToolUse) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}))
	outer := mw(agent.ToolHandler(func(ctx context.Context, _ agent.ToolUse) (json.RawMessage, error) {
		return inner(ctx, agent.ToolUse{ID: "c2", Name: "inner"})
	}))
	if _, err := outer(context.Background(), agent.ToolUse{ID: "c1", Name: "outer"}); err != nil {
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
