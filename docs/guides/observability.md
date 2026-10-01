# OpenTelemetry observability

The `trace` package adds OpenTelemetry GenAI instrumentation to an agent, emitting spans that
carry the OTel GenAI semantic-convention attributes (`gen_ai.*`). It is opt-in: importing the
package is the single switch, and the core `agent` package carries no OpenTelemetry dependency,
so a binary that does not import `trace` pays nothing.

## The one-liner

`Instrument` is the agent option that wires the whole span taxonomy onto an agent in one call:

<!-- docsnip: setup model agent.Model; journal *agent.Journal; tools []agent.Tool; import oteltrace "go.opentelemetry.io/otel/trace"; tracer oteltrace.Tracer; rates middleware.Rates -->
```go
a, err := agent.Build(model, journal, agent.WithTools(tools...), trace.Instrument(tracer, trace.WithRates(rates)))
```

`tracer` is any `go.opentelemetry.io/otel/trace.Tracer`. Under the hood `Instrument` attaches
two middlewares through the existing hooks: a `Model` middleware (`agent.WithMiddleware`) that
wraps each model call in a `chat` span, and a `Tool` middleware (`agent.WithToolMiddleware`) that
wraps each tool call in an `execute_tool` span, at the place the option appears among the agent's
options. You can attach `trace.Model(...)` and `trace.Tool(...)`
by hand if you want only one of them, but `Instrument` is the low-friction path.

## Span taxonomy

Three span kinds, matching the GenAI convention's `gen_ai.operation.name`:

| Span | `gen_ai.operation.name` | Source | Covers |
|---|---|---|---|
| `invoke_agent <name>` | `invoke_agent` | `trace.Invoke` (call site) | the whole run |
| `chat` / `chat <model>` | `chat` | `trace.Model` middleware | one model call, with token usage and finish reason |
| `execute_tool <tool>` | `execute_tool` | `trace.Tool` middleware | one tool call |

The top-level `invoke_agent` span lives at the call site rather than on the agent, because it
wraps the `Run` call. Start it with `Invoke` and close it with the returned `end(err)`:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; import oteltrace "go.opentelemetry.io/otel/trace"; tracer oteltrace.Tracer -->
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

<!-- docsnip: setup model agent.Model; journal *agent.Journal; tools []agent.Tool; import oteltrace "go.opentelemetry.io/otel/trace"; tracer oteltrace.Tracer; rates middleware.Rates -->
```go
a, err := agent.Build(model, journal, agent.WithTools(tools...), trace.Instrument(tracer, trace.WithRates(rates)))
```

`rates` is a `middleware.Rates`, the same rate table used to meter runs with a `CostMeter`, so
one table drives both. `gen_ai.usage.cost` is a custom extension (it is not part of the gen_ai
semantic convention), namespaced clearly. The standard `gen_ai.usage.input_tokens` and
`gen_ai.usage.output_tokens` are always recorded on the chat span regardless of `WithRates`.

The chat span names the provider (`gen_ai.system`) and model (`gen_ai.request.model`) the call is
sent to, as `agent.ModelInfoOf(call.Model)` reports them; the first-party adapters describe
themselves, and a custom `Model` can too by implementing `agent.Describer`. A model that does not
describe itself gives a span named `chat` with neither attribute. The span also records the
response's neutral finish reason as `gen_ai.response.finish_reasons`. `trace.Model` placed outside
a `Hedge` names the primary: backups are retargeted below it.

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

Error text follows the same rule, because it often carries content: a provider's error body can
echo the prompt, and a tool's error commonly embeds its arguments. With capture off, a failed
span (`chat`, `execute_tool`, or `invoke_agent`) gets status Error with a description from
`middleware.ErrorSummary`, which names only what failed: the control-flow signal, the provider's
HTTP status and error type or code, and the condition or category sentinel the error wraps (for
example `api error: status 400 (invalid_request_error): model`, or `unknown tool: tool`). No
exception event is recorded. With capture on, the span records an `exception` event and the
error's text as the status description, with every URL in the text redacted as the agent redacts
a tool error before journaling it (see the security model's tool-error redaction): capture is for
content, not credentials. An `execute_tool` span records exactly the text the agent journals for
the call, so an `Agent.WithToolErrorRedactor` applies there too. A tool middleware of your own
that records error text gets the same text from `call.ErrorText(err)` on the `agent.ToolCall` it
receives.

A call that panics ends its span with status Error and the description `panic`, whatever the
capture setting; the panic value, which can carry content, is not recorded, and the panic
continues.

`middleware.ToolLog` applies the same rule to its log line: a failed call is logged by its
`ErrorSummary`. Pass `middleware.LogErrorText()` to log the error text instead: the text the agent
journals for the call (`ToolCall.ErrorText`), so your `WithToolErrorRedactor` and URL redaction
apply to the log line too.

## Attribute keys

The package hardcodes the stable `gen_ai.*` attribute keys rather than importing the semconv
module (which churns every release), so instrumentation output does not shift under a toolchain
bump. The emitted keys include `gen_ai.system`, `gen_ai.operation.name`,
`gen_ai.request.model`, `gen_ai.response.finish_reasons`, `gen_ai.usage.input_tokens`,
`gen_ai.usage.output_tokens`, `gen_ai.tool.name`, and `gen_ai.tool.call.id`.

## See also

- `trace/trace.go`: the full instrumentation surface.
- `trace/trace_test.go`: end-to-end wiring of `Instrument`, `Invoke`, and content capture.
