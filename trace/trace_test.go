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

type fakeTool struct{}

func (fakeTool) Name() string                      { return "lookup" }
func (fakeTool) Description() string                { return "" }
func (fakeTool) Safety() agent.Safety               { return agent.Safety{} }
func (fakeTool) ArgsSchema() json.RawMessage        { return nil }
func (fakeTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func TestTool_EmitsExecuteToolSpan(t *testing.T) {
	sr, tp := recorder()
	tracer := tp.Tracer("test")

	wrapped := Tool(tracer, fakeTool{})
	if _, err := wrapped.Call(context.Background(), nil); err != nil {
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
}
