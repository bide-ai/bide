// Package trace adds OpenTelemetry GenAI instrumentation to an agent — opt-in, so the
// core agent package carries NO OTel dependency (a user who doesn't import trace pays
// nothing; contrast frameworks whose core drags the full OTel + Temporal stack into
// every binary). It plugs in through the existing middleware hook (Model) and a Tool
// decorator, emitting spans with the OTel GenAI semantic-convention attributes.
//
// We hardcode the stable gen_ai.* attribute keys rather than import the semconv module,
// which churns every release (v1.37 -> v1.41). Message/argument CONTENT is not captured
// by default (privacy-safe; unlike ADK #1634 which leaked tool args regardless).
package trace

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	agent "github.com/dayna/go-agents"
)

// GenAI semantic-convention attribute keys (stable subset).
const (
	attrSystem       = "gen_ai.system"
	attrOperation    = "gen_ai.operation.name"
	attrRequestModel = "gen_ai.request.model"
	attrInputTokens  = "gen_ai.usage.input_tokens"
	attrOutputTokens = "gen_ai.usage.output_tokens"
	attrToolName     = "gen_ai.tool.name"
)

type config struct{ system, model string }

// Option labels the model span with provider/model (Request doesn't carry them yet).
type Option func(*config)

func WithSystem(s string) Option { return func(c *config) { c.system = s } }
func WithModel(m string) Option  { return func(c *config) { c.model = m } }

// Model returns middleware that wraps each model call in a gen_ai "chat" span with
// token usage and status. Attach via agent.Agent.Use.
func Model(tracer oteltrace.Tracer, opts ...Option) agent.Middleware {
	var c config
	for _, o := range opts {
		o(&c)
	}
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

			msg, u, err := next(ctx, req)
			span.SetAttributes(
				attribute.Int(attrInputTokens, u.InputTokens),
				attribute.Int(attrOutputTokens, u.OutputTokens),
			)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return msg, u, err
		}
	}
}

// Tool wraps a tool so each execution is a gen_ai "execute_tool" span. Wrap the tools
// you pass to agent.New.
func Tool(tracer oteltrace.Tracer, t agent.Tool) agent.Tool {
	return &tracedTool{Tool: t, tracer: tracer}
}

type tracedTool struct {
	agent.Tool
	tracer oteltrace.Tracer
}

func (t *tracedTool) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	ctx, span := t.tracer.Start(ctx, "execute_tool "+t.Name(), oteltrace.WithAttributes(
		attribute.String(attrOperation, "execute_tool"),
		attribute.String(attrToolName, t.Name()),
	))
	defer span.End()
	res, err := t.Tool.Call(ctx, args)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
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
