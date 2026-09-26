// Package trace adds OpenTelemetry GenAI instrumentation to an agent — opt-in, so the
// core agent package carries NO OTel dependency (a user who doesn't import trace pays
// nothing; contrast frameworks whose core drags the full OTel + Temporal stack into
// every binary). It plugs in through the existing middleware hooks — Model (a .Use
// middleware) and Tool (a .UseTool middleware) — emitting spans with the OTel GenAI
// semantic-convention attributes.
//
// We hardcode the stable gen_ai.* attribute keys rather than import the semconv module,
// which churns every release (v1.37 -> v1.41). Message and tool-argument CONTENT is not
// captured unless OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT is set to "true" or "1"
// (privacy-safe by default; unlike ADK #1634 which leaked tool args regardless), honoring the
// OTel GenAI convention's opt-in. Pass WithRates to also record USD cost on the chat span.
package trace

import (
	"context"
	"encoding/json"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/middleware"
)

// GenAI semantic-convention attribute keys (stable subset). attrCost is a custom extension:
// USD cost is not part of the gen_ai semconv, so we namespace it clearly.
const (
	attrSystem         = "gen_ai.system"
	attrOperation      = "gen_ai.operation.name"
	attrRequestModel   = "gen_ai.request.model"
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
	system, model string
	rates         *middleware.Rates
}

// Option labels the model span with provider/model (Request doesn't carry them yet).
type Option func(*config)

func WithSystem(s string) Option { return func(c *config) { c.system = s } }
func WithModel(m string) Option  { return func(c *config) { c.model = m } }

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

// Model returns middleware that wraps each model call in a gen_ai "chat" span with
// token usage and status. Attach via agent.Agent.Use.
func Model(tracer oteltrace.Tracer, opts ...Option) agent.Middleware {
	var c config
	for _, o := range opts {
		o(&c)
	}
	capture := captureContent()
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
			name := "chat"
			if c.model != "" {
				name = "chat " + c.model
			}
			ctx, span := tracer.Start(ctx, name, oteltrace.WithAttributes(
				attribute.String(attrOperation, "chat"),
			))
			defer span.End()
			if c.system != "" {
				span.SetAttributes(attribute.String(attrSystem, c.system))
			}
			if c.model != "" {
				span.SetAttributes(attribute.String(attrRequestModel, c.model))
			}
			if capture {
				if b, e := json.Marshal(req.Messages); e == nil {
					span.SetAttributes(attribute.String(attrInputMessages, string(b)))
				}
			}

			msg, u, err := next(ctx, req)
			span.SetAttributes(
				attribute.Int(attrInputTokens, u.InputTokens),
				attribute.Int(attrOutputTokens, u.OutputTokens),
			)
			if c.rates != nil {
				span.SetAttributes(attribute.Float64(attrCost, c.rates.Cost(u)))
			}
			if capture && err == nil {
				if b, e := json.Marshal(msg); e == nil {
					span.SetAttributes(attribute.String(attrOutputMessages, string(b)))
				}
			}
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return msg, u, err
		}
	}
}

// Tool returns tool middleware that wraps every tool call in a gen_ai "execute_tool"
// span. Attach via agent.Agent.UseTool. Because it runs inside the agent loop, the span
// lives in the context passed to the tool — so when a tool is itself a sub-agent, that
// sub-agent's run (and its own spans) nest as children of this span: the trace crosses
// the sub-agent boundary automatically, a gap in ADK / AgenticGoKit / trpc-agent-go.
func Tool(tracer oteltrace.Tracer) agent.ToolMiddleware {
	capture := captureContent()
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
			ctx, span := tracer.Start(ctx, "execute_tool "+tu.Name, oteltrace.WithAttributes(
				attribute.String(attrOperation, "execute_tool"),
				attribute.String(attrToolName, tu.Name),
			))
			defer span.End()
			if tu.ID != "" {
				span.SetAttributes(attribute.String(attrToolCallID, tu.ID))
			}
			if capture && len(tu.Args) > 0 {
				span.SetAttributes(attribute.String(attrToolArguments, string(tu.Args)))
			}
			res, err := next(ctx, tu)
			if capture && err == nil && len(res) > 0 {
				span.SetAttributes(attribute.String(attrToolResult, string(res)))
			}
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return res, err
		}
	}
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
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
