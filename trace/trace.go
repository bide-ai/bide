// Package trace adds OpenTelemetry GenAI instrumentation to an agent — opt-in, so the
// core agent package carries NO OTel dependency (a user who doesn't import trace pays
// nothing; contrast frameworks whose core drags the full OTel + Temporal stack into
// every binary). It plugs in through the existing middleware hooks — Model (an
// agent.WithMiddleware middleware) and Tool (an agent.WithToolMiddleware middleware) — emitting spans with the OTel GenAI
// semantic-convention attributes. Instrument wires both onto an agent as one agent.Option.
//
// We hardcode the stable gen_ai.* attribute keys rather than import the semconv module,
// which churns every release (v1.37 -> v1.41). Message and tool-argument CONTENT is not
// captured unless OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT is set to "true" or "1"
// (privacy-safe by default; unlike ADK #1634 which leaked tool args regardless), honoring the
// OTel GenAI convention's opt-in. Neither is error text, which can carry that content: with
// capture off a failed span records middleware.ErrorSummary (category, condition, provider
// status) as its status, and with it on its text, with every URL in it redacted. Pass WithRates
// to also record USD cost on the chat span.
package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// GenAI semantic-convention attribute keys (stable subset). attrCost is a custom extension:
// USD cost is not part of the gen_ai semconv, so we namespace it clearly.
const (
	attrSystem         = "gen_ai.system"
	attrOperation      = "gen_ai.operation.name"
	attrRequestModel   = "gen_ai.request.model"
	attrFinishReasons  = "gen_ai.response.finish_reasons"
	attrInputTokens    = "gen_ai.usage.input_tokens"
	attrOutputTokens   = "gen_ai.usage.output_tokens"
	attrToolName       = "gen_ai.tool.name"
	attrToolCallID     = "gen_ai.tool.call.id"
	attrInputMessages  = "gen_ai.input.messages"
	attrOutputMessages = "gen_ai.output.messages"
	attrToolArguments  = "gen_ai.tool.call.arguments"
	attrToolResult     = "gen_ai.tool.call.result"
	attrCost           = "gen_ai.usage.cost" // custom, not semconv: USD
)

type config struct {
	rates *middleware.Rates
}

// Option configures the chat span.
type Option func(*config)

// WithRates records USD cost on the chat span (attribute gen_ai.usage.cost), computed from
// token usage at these rates. Reuses middleware.Rates so a caller pricing runs with a
// CostMeter uses one rate table for both.
func WithRates(r middleware.Rates) Option { return func(c *config) { c.rates = &r } }

// captureContent reports whether message and tool-argument content should be recorded,
// honoring the OTel GenAI convention's OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT.
func captureContent() bool {
	v := os.Getenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT")
	return v == "true" || v == "1"
}

// recordError marks span failed with err. An error's text can carry content (a provider error
// body that echoes the prompt, a tool error that embeds the call's arguments), so with content
// capture on the span records text(), the error's text as the agent would journal it (an
// exception event and the status description), and with it off the status description is
// middleware.ErrorSummary(err): the error's category, condition, and provider status, never its
// text. Capture is for content, not credentials: text() redacts every URL in the text and, for a
// tool call, applies the agent's WithToolErrorRedactor, so a span holds no more than the journal.
func recordError(span oteltrace.Span, err error, capture bool, text func() string) {
	if err == nil {
		return
	}
	if !capture {
		span.SetStatus(codes.Error, middleware.ErrorSummary(err))
		return
	}
	msg := text()
	span.AddEvent("exception", oteltrace.WithAttributes(
		attribute.String("exception.type", fmt.Sprintf("%T", err)),
		attribute.String("exception.message", msg),
	))
	span.SetStatus(codes.Error, msg)
}

// errorText is err's text with every URL in it redacted, for an error no tool redactor covers.
func errorText(err error) func() string {
	return func() string { return agent.RedactURLs(err.Error()) }
}

// end ends span, marking it failed if the call it covers panicked; deferred, it sees the panic
// before it propagates, and re-raises it. The status names the panic and not its value, which can
// carry content. It recovers the panic itself so that the SDK's span.End, which would otherwise
// record the value as an exception event whatever the capture setting, never sees it.
func end(span oteltrace.Span) {
	if r := recover(); r != nil {
		span.SetStatus(codes.Error, "panic")
		span.End()
		panic(r)
	}
	span.End()
}

// Model returns middleware that wraps each model call in a gen_ai "chat" span with token usage,
// the finish reason and status. The span names the provider (gen_ai.system) and model
// (gen_ai.request.model) the call is sent to, as agent.ModelInfoOf reports them for call.Model;
// a Model that does not describe itself leaves both out, and the span is named "chat". Attach via
// agent.Agent.Use; placed outside a Hedge, the span names the primary.
func Model(tracer oteltrace.Tracer, opts ...Option) agent.Middleware {
	var c config
	for _, o := range opts {
		o(&c)
	}
	capture := captureContent()
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			info, described := agent.ModelInfoOf(call.Model)
			name := "chat"
			if described && info.Model != "" {
				name = "chat " + info.Model
			}
			ctx, span := tracer.Start(ctx, name, oteltrace.WithAttributes(
				attribute.String(attrOperation, "chat"),
			))
			defer end(span)
			if described && info.Provider != "" {
				span.SetAttributes(attribute.String(attrSystem, info.Provider))
			}
			if described && info.Model != "" {
				span.SetAttributes(attribute.String(attrRequestModel, info.Model))
			}
			if capture {
				if b, e := json.Marshal(call.Request.Messages); e == nil {
					span.SetAttributes(attribute.String(attrInputMessages, string(b)))
				}
			}

			resp, err := next(ctx, call)
			u := resp.Usage
			span.SetAttributes(
				attribute.Int(attrInputTokens, u.TotalInputTokens()), // semconv: includes cached input
				attribute.Int(attrOutputTokens, u.OutputTokens),
			)
			if c.rates != nil {
				span.SetAttributes(attribute.Float64(attrCost, c.rates.Cost(u)))
			}
			if err == nil {
				// An empty reason is a natural stop, which the agent journals as FinishStop.
				fin := resp.Finish
				if fin == "" {
					fin = agent.FinishStop
				}
				span.SetAttributes(attribute.StringSlice(attrFinishReasons, []string{string(fin)}))
			}
			if capture && err == nil {
				if b, e := json.Marshal(resp.Message); e == nil {
					span.SetAttributes(attribute.String(attrOutputMessages, string(b)))
				}
			}
			recordError(span, err, capture, errorText(err))
			return resp, err
		}
	}
}

// Tool returns tool middleware that wraps every tool call in a gen_ai "execute_tool"
// span. Attach with agent.WithToolMiddleware. Because it runs inside the agent loop, the span
// lives in the context passed to the tool — so when a tool is itself a sub-agent, that
// sub-agent's run (and its own spans) nest as children of this span: the trace crosses
// the sub-agent boundary automatically, a gap in ADK / AgenticGoKit / trpc-agent-go.
func Tool(tracer oteltrace.Tracer) agent.ToolMiddleware {
	capture := captureContent()
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
			ctx, span := tracer.Start(ctx, "execute_tool "+call.Use.Name, oteltrace.WithAttributes(
				attribute.String(attrOperation, "execute_tool"),
				attribute.String(attrToolName, call.Use.Name),
			))
			defer end(span)
			if call.Use.ID != "" {
				span.SetAttributes(attribute.String(attrToolCallID, call.Use.ID))
			}
			if capture && len(call.Use.Args) > 0 {
				span.SetAttributes(attribute.String(attrToolArguments, string(call.Use.Args)))
			}
			res, err := next(ctx, call)
			if capture && err == nil && len(res) > 0 {
				span.SetAttributes(attribute.String(attrToolResult, string(res)))
			}
			recordError(span, err, capture, func() string { return call.ErrorText(err) })
			return res, err
		}
	}
}

// Instrument is the agent option that wires the gen_ai span taxonomy onto an agent in one call:
// the "chat" span (via Model) and the "execute_tool" span (via Tool), using tracer. It is the
// low-friction way to enable observability without hand-wiring each middleware, while the core
// agent package keeps no OpenTelemetry dependency (importing this package is the single opt-in).
// Options (WithRates) apply to the chat span. Its middleware is appended where the option
// appears among the agent's options. For the top-level "invoke_agent" span, wrap the run with
// Invoke, which lives at the call site rather than on the agent.
//
//	a, err := agent.New(model, journal, agent.WithTools(tools...), trace.Instrument(tracer, trace.WithRates(rates)))
func Instrument(tracer oteltrace.Tracer, opts ...Option) agent.Option {
	return agent.WithOptions(agent.WithMiddleware(Model(tracer, opts...)), agent.WithToolMiddleware(Tool(tracer)))
}

// Invoke starts a top-level gen_ai "invoke_agent" span; call the returned end(err) when
// the run finishes. Wrap agent.Run:
//
//	ctx, end := trace.Invoke(ctx, tracer, "support-agent")
//	msg, err := a.Run(ctx, runID, input)
//	end(err)
func Invoke(ctx context.Context, tracer oteltrace.Tracer, name string) (context.Context, func(error)) {
	ctx, span := tracer.Start(ctx, "invoke_agent "+name, oteltrace.WithAttributes(
		attribute.String(attrOperation, "invoke_agent"),
	))
	capture := captureContent()
	return ctx, func(err error) {
		recordError(span, err, capture, errorText(err))
		span.End()
	}
}
