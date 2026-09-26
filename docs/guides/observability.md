# OpenTelemetry observability

The `trace` package adds OpenTelemetry GenAI instrumentation to an agent, emitting spans that
carry the OTel GenAI semantic-convention attributes (`gen_ai.*`). It is opt-in: importing the
package is the single switch, and the core `agent` package carries no OpenTelemetry dependency,
so a binary that does not import `trace` pays nothing.

## The one-liner

`Instrument` wires the whole span taxonomy onto an agent in one call:

```go
a := trace.Instrument(agent.New(model, store, tools...), tracer, trace.WithRates(rates))
```

`tracer` is any `go.opentelemetry.io/otel/trace.Tracer`. Under the hood `Instrument` attaches
two middlewares through the existing hooks: a `Model` middleware (via `Agent.Use`) that wraps
each model call in a `chat` span, and a `Tool` middleware (via `Agent.UseTool`) that wraps each
tool call in an `execute_tool` span. You can attach `trace.Model(...)` and `trace.Tool(...)`
by hand if you want only one of them, but `Instrument` is the low-friction path.

## Span taxonomy

Three span kinds, matching the GenAI convention's `gen_ai.operation.name`:

| Span | `gen_ai.operation.name` | Source | Covers |
|---|---|---|---|
| `invoke_agent <name>` | `invoke_agent` | `trace.Invoke` (call site) | the whole run |
| `chat` / `chat <model>` | `chat` | `trace.Model` middleware | one model call, with token usage |
| `execute_tool <tool>` | `execute_tool` | `trace.Tool` middleware | one tool call |

The top-level `invoke_agent` span lives at the call site rather than on the agent, because it
wraps the `Run` call. Start it with `Invoke` and close it with the returned `end(err)`:

```go
ctx, end := trace.Invoke(ctx, tracer, "support-agent")
msg, err := a.Run(ctx, runID, input)
end(err)
```

`end(err)` records the error and sets the span status on a failed run.

## Sub-agent span nesting

Because the `execute_tool` span lives in the context passed to the tool, a tool that is itself
a sub-agent nests naturally: the sub-agent's own run and its `chat` / `execute_tool` spans
appear as children of the parent's `execute_tool` span. The trace crosses the sub-agent boundary
automatically, with no extra wiring, so a multi-agent run reads as one connected tree.

## Cost on the chat span

Pass `WithRates` to record USD cost on each `chat` span as the `gen_ai.usage.cost` attribute,
computed from the call's token usage:

```go
a := trace.Instrument(agent.New(model, store, tools...), tracer, trace.WithRates(rates))
```

`rates` is a `middleware.Rates`, the same rate table used to meter runs with a `CostMeter`, so
one table drives both. `gen_ai.usage.cost` is a custom extension (it is not part of the gen_ai
semantic convention), namespaced clearly. The standard `gen_ai.usage.input_tokens` and
`gen_ai.usage.output_tokens` are always recorded on the chat span regardless of `WithRates`.

`WithSystem` and `WithModel` label the chat span with the provider and model name; they also
apply to the chat span only.

## Content capture is off by default

Message content and tool arguments/results are NOT captured unless you opt in, honoring the
OTel GenAI convention. Set the environment variable to enable it:

```
OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true   # or 1
```

With it unset, spans carry structure (operation, model, token counts, tool names, cost, status)
but no prompt, completion, tool-argument, or tool-result payloads. This is privacy-safe by
default: content is only recorded when an operator deliberately turns it on. When enabled, the
`chat` span gains `gen_ai.input.messages` / `gen_ai.output.messages` and the `execute_tool`
span gains `gen_ai.tool.call.arguments` / `gen_ai.tool.call.result`.

## Attribute keys

The package hardcodes the stable `gen_ai.*` attribute keys rather than importing the semconv
module (which churns every release), so instrumentation output does not shift under a toolchain
bump. The emitted keys include `gen_ai.system`, `gen_ai.operation.name`,
`gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`,
`gen_ai.tool.name`, and `gen_ai.tool.call.id`.

## See also

- `trace/trace.go`: the full instrumentation surface.
- `trace/trace_test.go`: end-to-end wiring of `Instrument`, `Invoke`, and content capture.
