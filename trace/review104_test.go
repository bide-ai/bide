package trace_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	btrace "github.com/bide-ai/bide/trace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type okModel struct{}

func (okModel) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: "x"}}
	ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishStop}}
	close(ch)
	return agent.NewStream(ch), nil
}

// T1: a middleware-built response with an empty finish: the journal records "stop", the chat
// span records no finish reason.
func TestT1_TraceFinishOfBuiltResponse(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	built := func(agent.ModelHandler) agent.ModelHandler {
		return func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
			return agent.ModelResponse{Message: agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "cached"}}}}, nil
		}
	}
	store := agenttest.MemJournal()
	a := agenttest.MustNew(okModel{}, store, agent.WithMiddleware(btrace.Model(tp.Tracer("t")), built))
	if _, err := a.Run(context.Background(), "r", agent.UserText("q")); err != nil {
		t.Fatal(err)
	}
	recs, _ := store.History(context.Background(), "r")
	var fin agent.FinishReason
	for _, r := range recs {
		if r.Kind == agent.StepModel {
			fin = r.Finish()
		}
	}
	var spanFin string
	for _, s := range sr.Ended() {
		for _, kv := range s.Attributes() {
			if kv.Key == "gen_ai.response.finish_reasons" {
				spanFin = strings.Join(kv.Value.AsStringSlice(), ",")
			}
		}
	}
	if string(fin) != spanFin {
		t.Fatalf("journal finish %q, span finish_reasons %q", fin, spanFin)
	}
}
