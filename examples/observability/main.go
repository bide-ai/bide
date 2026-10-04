// Command observability wires OpenTelemetry GenAI instrumentation onto an agent with one
// call, trace.Instrument, and exports the resulting spans to stdout so they print with no
// tracing backend. trace.WithRates records USD cost on the chat span (attribute
// gen_ai.usage.cost) from token usage, so the cost attribute appears alongside the
// gen_ai.usage.* token counts.
//
// It runs with NO API key: the model is a small inline scripted Model that calls one tool,
// then answers in text, reporting token usage on Finish so the cost attribute is non-zero.
// The stdout exporter is a minimal in-code SpanExporter (no extra dependency beyond the
// OTel SDK the trace package already requires); swap in an OTLP exporter to ship to a
// real backend.
//
//	go run .   # from this directory (examples/observability)
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
	"github.com/bide-ai/bide/trace"
)

// scriptModel calls the weather tool once (reporting token usage on Finish so cost is
// non-zero), then answers in text on the next turn.
type scriptModel struct{ turn int }

// Describe names the model on the chat span (gen_ai.system and gen_ai.request.model), as the
// first-party adapters' Describe does.
func (*scriptModel) Describe() agent.ModelInfo {
	return agent.ModelInfo{Provider: "inline", Model: "inline-demo"}
}

func (m *scriptModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 4)
	if m.turn == 0 {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "get_weather", ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use", Usage: agent.Usage{InputTokens: 120, OutputTokens: 18}}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "It is sunny."}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop", Usage: agent.Usage{InputTokens: 140, OutputTokens: 12}}}
	}
	m.turn++
	close(ch)
	return agent.NewStream(ch), nil
}

// stdoutExporter is a minimal SpanExporter that prints each finished span's name and its
// attributes. It stands in for an OTLP/stdouttrace exporter so the example needs no backend.
type stdoutExporter struct{}

func (stdoutExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		fmt.Fprintf(os.Stdout, "span %q\n", s.Name())
		attrs := s.Attributes()
		sort.Slice(attrs, func(i, j int) bool { return attrs[i].Key < attrs[j].Key })
		for _, kv := range attrs {
			fmt.Fprintf(os.Stdout, "    %s = %s\n", kv.Key, valueString(kv.Value))
		}
	}
	return nil
}

func (stdoutExporter) Shutdown(context.Context) error { return nil }

func valueString(v attribute.Value) string {
	if v.Type() == attribute.FLOAT64 {
		return fmt.Sprintf("%.6f", v.AsFloat64())
	}
	return v.String()
}

func main() {
	ctx := context.Background()

	// A SyncSpanProcessor makes the exporter run synchronously on span end, so all spans are
	// printed before the program exits without an explicit flush ordering concern.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(stdoutExporter{}))
	defer func() { _ = tp.Shutdown(ctx) }()
	tracer := tp.Tracer("observability-example")

	weather := agent.MustFunc("get_weather", "Get the weather", func(_ context.Context, _ struct{}) (string, error) { return "sunny", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))

	// Instrument wires the chat span (with cost) and the execute_tool span in one call.
	// Rates turn token usage into a USD cost recorded as gen_ai.usage.cost on the chat span.
	rates := middleware.Rates{InputPer1M: 0.15, OutputPer1M: 0.60}
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	a, err := agent.New(&scriptModel{}, j,
		agent.WithTools(weather),
		trace.Instrument(tracer, trace.WithRates(rates)),
	)
	if err != nil {
		log.Fatal(err)
	}

	// Invoke starts the top-level invoke_agent span around the run.
	ctx, end := trace.Invoke(ctx, tracer, "weather-agent")
	res, err := a.Run(ctx, "obs-1", agent.UserText("What's the weather?"))
	var out agent.Message
	if res != nil {
		out = res.Message
	}
	end(err)
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	fmt.Printf("\nfinal answer: %s\n", out.Text())
}
