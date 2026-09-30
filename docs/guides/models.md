# Model adapters: configuration and multimodal input

The `Model` port is a single method, `Stream(ctx, Request) (*Stream, error)`
([extension points](../reference/extension-points.md)). Three reference adapters ship, all in the zero-dep
core module (net/http + stdlib, no provider SDK, enforced by `architecture_test.go`):

| Adapter | Package | Wire API | Covers |
|---|---|---|---|
| Anthropic (native) | `model/anthropic` | Messages API (`/v1/messages`) | Claude, extended thinking with signatures preserved |
| OpenAI-compatible | `model/openai` | Chat Completions (`/chat/completions`) | OpenAI, and any compatible endpoint via `WithBaseURL` (Ollama, DeepSeek, Groq, OpenRouter, vLLM, Azure, xAI, ...) |
| Gemini (native) | `model/gemini` | `streamGenerateContent` (SSE) | Google AI Studio, and Vertex AI / a proxy via `WithBaseURL` |

## Construction and options

Every adapter is `New(apiKey string, opts ...Option) *Model`. **No adapter reads an environment
variable**: the API key is always passed explicitly to `New`, so key management is the caller's
(pull it from your secret store, not from `os.Getenv` inside the SDK). The runnable
[`examples/smoke`](../../examples/smoke/main.go) shows the pattern (`os.Getenv("OPENROUTER_API_KEY")`
in the caller, passed to `openai.New`).

Shared options (all three adapters): `WithModel(id)`, `WithMaxTokens(n)`, `WithBaseURL(u)`,
`WithHTTPClient(c)`. Defaults if an option is not set:

| Adapter | Default model | Default max tokens | Default base URL |
|---|---|---|---|
| `anthropic` | `claude-sonnet-4-6` | `4096` | `https://api.anthropic.com` |
| `openai` | `gpt-4o` | provider default (unset) | `https://api.openai.com/v1` |
| `gemini` | `gemini-2.0-flash` | provider default (unset) | `https://generativelanguage.googleapis.com` |

Adapter-specific options:

- **`anthropic.WithPromptCache()`** enables Anthropic prompt caching (see below).
- **`openai.WithStrictSchema()`** turns on OpenAI structured-output strict mode for **tool argument**
  schemas (`"strict": true` on each function). Use it when the endpoint is genuine OpenAI, which
  many "OpenAI-compatible" endpoints do not fully implement. It is off by default for compatibility.
  Note this flag governs only tool schemas: `RunTypedNative[T]` always emits a strict
  `response_format` JSON schema regardless, so the native typed-output path does not need it.
  In strict mode an optional field (a pointer or `omitempty`) is required but nullable, so the
  model sends `null` for it. A type strict mode cannot express (a map, a recursive type, or an
  untyped field such as `any` or `json.RawMessage`) fails the request with an `ErrConfig` error
  wrapping `schema.ErrStrictUnsupported`, rather than being sent as a schema that changes what the
  model may answer.

The Gemini adapter translates tool argument schemas and the `RunTypedNative` response schema to
the OpenAPI subset Gemini reads (`schema.Gemini`): an optional field becomes `nullable`, a closed
object drops `additionalProperties: false`, and keywords Gemini lacks that only annotate or
further constrain a value are dropped. A tool with no arguments declares no `parameters` (Gemini
rejects an object with no properties, so a tool whose schema is a bare `{"type":"object"}` is
declared the same way). A schema the subset cannot express (a map, an `interface{}` or
`json.RawMessage` field, a recursive type, `$ref`, `oneOf`, ...) fails the request with an
`ErrConfig` error wrapping `schema.ErrGeminiUnsupported` that names the tool and the location.

Whatever the adapter, a `Func` tool decodes the arguments the model sends strictly, against the
same fields its schema describes: a missing required field (or `null` for one whose schema does
not admit null), an unknown or case-variant name, a
duplicate name, trailing data, invalid UTF-8, or an escaped lone surrogate is an `ErrToolArgs`
tool error the model reads and corrects. This matters most where the provider does not enforce the
schema (Anthropic, and OpenAI without strict mode). `RunTyped`'s `final_answer` and the JSON that
`RunTypedNative` decodes are read the same way.

`WithBaseURL` is how one adapter reaches many providers. For OpenAI-compatible endpoints, set the
base URL and the model, e.g. `openai.New("", openai.WithBaseURL("http://localhost:11434/v1"),
openai.WithModel("llama3"))` for Ollama; the OpenAI adapter only sets the auth header when the API
key is non-empty, so a keyless local endpoint works with an empty key. For Gemini on Vertex AI or
behind a proxy, override the host with `WithBaseURL`; the request path
(`/v1beta/models/{model}:streamGenerateContent`) is appended to it.

## Sampling parameters, per provider

Generation controls are provider-neutral and set once on the agent (`WithSampling`, see the README).
Each adapter maps what it supports onto its wire format and silently drops the rest, so a field one
provider lacks is not an error:

| `agent.Sampling` field | anthropic | openai | gemini |
|---|---|---|---|
| `MaxTokens` | `max_tokens` | `max_completion_tokens` or `max_tokens` (below) | `maxOutputTokens` |
| `Temperature` | `temperature` | `temperature` | `temperature` |
| `TopP` | `top_p` | `top_p` | `topP` |
| `Stop` | `stop_sequences` | `stop` | `stopSequences` |
| `Seed` | dropped (no seed param) | `seed` | dropped (no seed param) |

The OpenAI adapter sends the token limit as `max_completion_tokens` on OpenAI's own endpoint
(`api.openai.com`, where `max_tokens` is deprecated and the o-series and gpt-5 reasoning models
reject it) and for an OpenAI reasoning model id (`o1`, `o3-mini`, `gpt-5`, ..., also behind a
`vendor/` prefix) on any endpoint; other OpenAI-compatible servers get `max_tokens`, which is what
they implement. `openai.WithMaxCompletionTokens(bool)` forces the choice, for example for Azure
OpenAI serving a reasoning model under a deployment name.

A request-level `MaxTokens` overrides the adapter's construction-time default. Because the fields
are pointers, an explicit `Temperature(0)` is distinct from unset (which uses the provider default).

## Prompt caching and usage accounting

An agent loop resends a large constant prefix (system prompt + tool schemas) every turn.

- **Anthropic** (`WithPromptCache()`): places `cache_control: {type: ephemeral}` breakpoints on the
  last tool definition (caching the whole constant tool prefix) and the system block, so repeat
  turns bill the prefix at the cache-read rate.
- **OpenAI**: caches prefixes automatically, no flag needed.
- **Gemini**: no explicit prompt-cache flag in this adapter.

Cache effectiveness surfaces cross-provider in `agent.Usage`, so the run's token budget and cost accounting
see the real numbers:

- `CacheReadTokens`: Anthropic `cache_read_input_tokens`; OpenAI `prompt_tokens_details.cached_tokens`;
  Gemini's cached-content tokens.
- `CacheWriteTokens`: Anthropic `cache_creation_input_tokens`. OpenAI and Gemini do not report a
  separate cache-write count, so this stays zero for them.
- `InputTokens`: the input tokens that were neither read from nor written to the cache. Anthropic
  reports these directly as `input_tokens`. OpenAI's `prompt_tokens` and Gemini's
  `promptTokenCount` include the cached tokens, so those adapters subtract them.

The counts are disjoint, so each token is billed once at its own rate and the same call costs the
same on every provider. `Usage.TotalInputTokens()` is the whole prompt and `Usage.TotalTokens()`
adds the output.

## Error surfacing (shared across all three adapters)

Every adapter maps HTTP failures onto the framework's typed errors so a retry middleware can
classify them ([retry docs in the README](../../README.md#middleware--observability)):

The provider's error object (`{"error":{...}}`, the same envelope on all three) is parsed, so the
error carries the provider's own message.

- **Quota or credit used up** returns `*agent.APIError` wrapping `agent.ErrQuotaExhausted`, whatever
  the status: HTTP 402, an error type or code of `insufficient_quota`, `billing_hard_limit_reached`,
  or `billing_not_active` (OpenAI, which sends these as 429), `billing_error` (Anthropic), or a
  Gemini `RESOURCE_EXHAUSTED` whose quota violation is per day. No wait lifts it, so
  `middleware.Retryable` does not retry it.
- **Any other HTTP 429** returns `*agent.RateLimited{RetryAfter, Message}`. `RetryAfter` is the
  `Retry-After` header (seconds or an HTTP date) or, without one, Gemini's `RetryInfo.retryDelay`;
  `0` if neither. `middleware.Retry` honors the hint.
- **Any other non-2xx** returns `*agent.APIError{StatusCode, Message, Type, Code, Body}`, so
  `middleware.Retryable` can tell a transient failure (5xx, 408) from a terminal one (most 4xx:
  auth, validation).
- **An error sent partway through a stream** (an OpenAI or Gemini data line holding an error
  object, an Anthropic `error` event) ends the turn with an error carrying the provider's message:
  classified by its status when it has one (Gemini), as `ErrQuotaExhausted` or `*RateLimited` when
  its type or code says so, and otherwise as a plain `ErrModel` that `Retryable` retries.
- **A stream line over `agent.MaxSSELine` (32MB)** fails with `agent.ErrResponseTooLarge`, which
  the same request would hit again, so it is not retried. Any other failed stream read wraps
  `ErrModel`.
- All of these wrap `agent.ErrModel`, so `errors.Is(err, agent.ErrModel)` holds either way (see the
  errors section of the README).

Error text is bounded whatever the endpoint sends: at most 64KB of a failed response is read, and
the body, message, type, and code an error carries are each cut to 8KB (a cut body ends in
`...(truncated)`), so a broken endpoint, or one that echoes the prompt back, cannot put megabytes
into memory, logs, or traces.

## Tool names and tool choice

Each adapter checks a request's tools and tool choice against the provider's rules before sending
it, and refuses a setup the provider would answer with a 400 as an `ErrConfig` that says what is
wrong:

- A tool name must match `^[a-zA-Z0-9_-]{1,64}$` (OpenAI, Anthropic) or
  `^[a-zA-Z_][a-zA-Z0-9_.:-]{0,63}$` (Gemini), and no two tools may share a name.
- A `ToolChoice` mode must be `""`, `auto`, `none`, `required`, or `tool`. `required` and `tool`
  need declared tools, and `tool` must name one of them.
- `auto` or `none` with no tools declared is already met, so no tool choice is sent (the providers
  reject one with no tools).

## Tool-call IDs

The agent keys each tool call's result and journal step by its tool-use ID, so every call in a
run needs its own. Anthropic and OpenAI issue one per call. Gemini usually sends none: the Gemini
adapter keeps an `id` when Gemini sends one and otherwise makes up a random `call_<24 hex digits>`,
which carries nothing from the tool name or the call's position.

The agent checks every live model turn whatever the adapter: a tool call with no ID, an ID already
used earlier in the conversation, or one repeated within the turn fails the turn with
`agent.ErrToolUseIDReused` (wrapping `ErrProtocol` and `ErrModel`) instead of passing the call off
as already done. The check runs below middleware, so `middleware.Retry` retries it, and the rejected
turn is never journaled. Turns replayed from an existing journal are not re-checked.

## Multimodal input (images)

User messages carry mixed content parts, so an image can accompany text on a user turn. Build a
multimodal user message with `agent.UserParts`:

```go
img, _ := os.ReadFile("chart.png")
msg := agent.UserParts(
    agent.Text{Text: "What trend does this chart show?"},
    agent.ImageData("image/png", img), // raw bytes; the adapter base64-encodes it
)
// agent.ImageURL("https://...") references a hosted image instead.
```

`agent.Image` sets exactly one of `Data` (raw bytes, plus `Mime`) or `URL` (a hosted image). All
three adapters translate it to their native form (Anthropic base64 image source, OpenAI image-URL /
data-URI content, Gemini `inlineData` / `fileData`). Gemini's `fileData` requires a MIME type:
for an image by URL it is `Image.Mime`, or else the type the URL's file extension names; with
neither, the request fails with `ErrConfig`. This is **input-only**: models emit text,
reasoning, and tool calls, never images, so nothing produces an `Image` on the response path. Audio
and video input are not modeled (see [KNOWN-LIMITATIONS.md](../KNOWN-LIMITATIONS.md)).

## Extended thinking (Anthropic)

The Anthropic adapter preserves extended-thinking **signatures**: a `Reasoning` part carries the
provider's opaque `Signature`, which is echoed back on later turns. Dropping it corrupts thinking +
tool use, which is why message content is typed parts rather than a flat string. Each thinking
block is its own `Reasoning` part with its own signature, in the order the model produced them,
and a `redacted_thinking` block is kept as a `Reasoning` part whose `Redacted` field holds its
encrypted data, sent back unchanged. A `Reasoning` part with neither a signature nor redacted data
(reasoning from another provider) is not sent to Anthropic, which would reject it.

Other Anthropic request details: empty or whitespace-only text blocks are left out (Anthropic
rejects them), system text from several parts or turns is joined with a blank line, and
`ToolChoice{Mode: "none"}` is sent as `{"type": "none"}` with the tools still declared (Anthropic
requires the declarations whenever the history holds `tool_use` blocks).

The OpenAI and Gemini adapters take the opposite, provider-correct stance: they do not send a prior
`Reasoning` part back on an assistant-input turn (the providers reject it, and there is no stable
signature to echo), so request-side `Reasoning` parts are dropped rather than sent with an invalid
token. The OpenAI adapter still surfaces inbound reasoning it receives (`reasoning_content` from
DeepSeek / Ollama and similar) as a `ReasoningDelta` on the stream, and the Gemini adapter surfaces
a part Gemini flags `thought` the same way.

Gemini's thinking models attach a `thoughtSignature` to a `functionCall` part and reject the next
turn unless it comes back on that part. The adapter keeps it on the call (`agent.ToolUse.Signature`,
journaled with the call) and sends it back with the `functionCall`. Signatures Gemini puts on text
parts are not kept; Gemini does not require them.

## GCF tool-result encoding (opt-in)

Tool results are sent to the model as JSON by default. For structured output you can
switch the model-facing encoding to [GCF](https://gcformat.com), the Graph Compact
Format: a compact wire format for structured data that is more token-efficient and
better comprehended by frontier models than JSON. Switching cuts the prompt tokens spent
echoing tool output back each turn. It is opt-in per adapter and changes only what the
model reads: the journal and the audit trail keep the canonical JSON form, so durability
and proofs are unaffected.

Wire it in with any adapter's `WithToolResultCodec`:

```go
import (
	"github.com/bide-ai/bide/model/openai"
	"github.com/bide-ai/bide/codec/gcf"
)

model := openai.New(apiKey, openai.WithToolResultCodec(gcf.New()))
```

The same option exists on the Anthropic and Gemini adapters. GCF lives in its own module
(`github.com/bide-ai/bide/codec/gcf`), so the core takes no GCF dependency unless you opt
in. Best paired with current frontier models; weaker or older models may comprehend the
compact form less reliably, so treat it as a per-deployment tuning knob rather than a
default. If encoding a particular result ever fails, the adapter falls back to the JSON
form for that result.

## What is not built

- No **native Bedrock** adapter (reach Bedrock-hosted models through an OpenAI-compatible proxy).
- Settings are agent-level, not per-`Run` (see [KNOWN-LIMITATIONS.md](../KNOWN-LIMITATIONS.md)).
