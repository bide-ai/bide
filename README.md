# go-agents (working codename)

**Durable AI agents for Go that survive a crash — without firing the same side effect twice.**

The agent loop is ~40 lines. The hard part is what happens when the process dies
mid-run. Most frameworks either lose the run, or blindly re-run the step that already
charged the card. This one journals every step, resumes exactly where it left off, and
**refuses to re-run a write whose outcome it can't verify — it halts and asks instead of
double-charging.**

Status: **working v0**, live-verified end-to-end. Requires **Go 1.27**.

## Why it's different

Not "another durable agent framework." The differentiators are things you can check:

- **Side-effect-safe resume.** A crash between a payment and its journal entry does **not**
  re-run the payment. Read-only tools re-run freely; idempotent ones retry; a
  non-idempotent write whose outcome is unknown **halts for confirmation**. Nobody else
  does this — the common behavior is at-least-once (double-charge) or "your problem."
- **A lean core.** A hello-world imports the **standard library only**. No Temporal, no
  Weaviate, no gRPC dragged into your binary. Enforced by a test (`architecture_test.go`).
- **Plain Go, not a graph DSL.** You write `if`/`for`/functions; the graph is *derived*
  from what ran (`RenderMermaid`) for viewing — you never author or debug one.
- **Claude reasoning survives round-trips.** Extended-thinking signatures are preserved;
  most SDKs drop them, silently breaking thinking + tool use.
- **Provider-aware tool schemas.** One reflected schema, emitted per dialect (OpenAI strict
  mode, etc.) — not one generic schema that strict mode and Gemini reject.
- **Any model, one adapter.** Native Claude + any OpenAI-compatible endpoint (OpenAI,
  Ollama, DeepSeek, Groq, OpenRouter, vLLM, Azure, xAI…) via `WithBaseURL`.

### vs. the typical Go agent framework

| | **go-agents** | Typical framework |
|---|---|---|
| Crash mid-write | **Halts — never double-fires** | Blindly re-runs (double-charge), or loses the run |
| Hello-world deps | **stdlib only** (core) | Often Temporal + Weaviate + gRPC (hundreds of pkgs) |
| Orchestration | **Plain Go**; graph derived for viewing | A graph/DSL you author *and* debug |
| Claude reasoning across turns | **Preserved (signatures)** | Dropped → breaks extended thinking |
| Tool schema | **Per-provider dialects** | One schema → rejected by strict mode / Gemini |
| Multi-node failover | **Any node resumes any run** (Postgres) | Single-writer lock — no failover |

## The money shot: it won't double-charge

```go
// A tool that moves money is a write: not ReadOnly, not Idempotent.
charge := agent.Func("charge_card", "Charge the customer", agent.Safety{},
	func(ctx context.Context, in ChargeArgs) (Receipt, error) { /* ... */ })

// If the process crashes after the charge fires but before its result is journaled,
// resume does NOT run it again — it returns *ResumeHalt so you confirm, not double-charge:
_, err := a.Run(ctx, runID, input)
var halt *agent.ResumeHalt
if errors.As(err, &halt) {
	// halt.ToolName == "charge_card": outcome unknown — a human decides, no double side effect.
}
```

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"os"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/model/openai"
	"github.com/dayna/go-agents/store/sqlite"
)

type WeatherArgs struct {
	City string `json:"city" desc:"city name"`
}
type Weather struct {
	TempF int    `json:"temp_f"`
	Sky   string `json:"sky"`
}

func main() {
	// Any OpenAI-compatible endpoint — here OpenRouter; swap the base URL for Ollama, etc.
	model := openai.New(os.Getenv("OPENROUTER_API_KEY"),
		openai.WithBaseURL("https://openrouter.ai/api/v1"),
		openai.WithModel("openai/gpt-4o-mini"))

	// A tool is a typed Go function; its schema is derived automatically.
	weather := agent.Func("get_weather", "Current weather for a city",
		agent.Safety{ReadOnly: true},
		func(_ context.Context, in WeatherArgs) (Weather, error) {
			return Weather{TempF: 68, Sky: "sunny"}, nil
		})

	// Durable on-disk store — a crash mid-run resumes from here.
	store, _ := sqlite.Open("agent.db")
	defer store.Close()

	a := agent.New(model, store, weather).
		WithSystemPrompt("You are a concise weather assistant.")
	out, _ := a.Run(context.Background(), "run-1", "Weather in SF? Use the tool.")
	for _, p := range out.Parts {
		if t, ok := p.(agent.Text); ok {
			fmt.Println(t.Text)
		}
	}
}
```

Run the live smoke example: `OPENROUTER_API_KEY=sk-... go run ./examples/smoke`

`Run` returns just the final message. For a run summary — token usage (summed across turns,
including cache), model-turn count, wall-clock duration — use `RunResult` (and `RunSagaResult`):

```go
res, err := a.RunResult(ctx, runID, input)
// res.Message, res.Usage, res.Turns, res.Duration, res.RunID
```

## Streaming

`Run` blocks and returns the final answer. To watch the agent work — token deltas, turn
boundaries, tool start/finish — use `Stream`. It drives the **same loop** (`Run` is literally
`Stream(...).Final()`), so durability, resume, and side-effect safety are identical:

```go
stream := a.Stream(ctx, runID, input)
for ev := range stream.Events() {
	switch e := ev.(type) {
	case agent.ModelEvent: // live token/reasoning/tool-call deltas
		if d, ok := e.Event.(agent.TextDelta); ok {
			fmt.Print(d.Text)
		}
	case agent.ToolStarted:
		fmt.Printf("\n[calling %s]\n", e.Name)
	case agent.ToolCompleted:
		fmt.Printf("[%s done]\n", e.Name)
	}
}
answer, err := stream.Final() // terminal message + error (incl. *PendingApproval / *ResumeHalt)
```

Events: `TurnStarted`, `ModelEvent` (the token feed), `AssistantTurn`, `ToolStarted` /
`ToolCompleted`, `ApprovalRequired`, `Finished`. Range `Events()` for a UI then call `Final()`,
or call `Final()` alone to behave exactly like `Run` (it drains events for you).

Two things worth knowing, both consequences of durability:
- **Token deltas arrive below the middleware chain** (Retry / TokenBudget still see whole
  assembled messages), and **only on a fresh model call**.
- **On resume, the journaled transcript is re-emitted** as `AssistantTurn{Replayed: true}` +
  `ToolCompleted` before live progress — so a fresh UI reconstructs the whole story after a
  crash, and a replayed turn produces no token deltas (it was already decided).

`StreamSaga` is the streaming counterpart of `RunSaga`.

## Typed output

`RunTyped[T]` returns a typed `T` instead of a free-form message. It injects a synthetic
`final_answer` tool whose JSON schema is derived from `T` (via the `schema` package) and steers
the model to call it once its work is done — so a tool-using agent can do real work and *then*
answer typed. Provider-agnostic (built on native tool calling, not a provider's JSON mode).

```go
type Weather struct {
	City  string `json:"city"`
	TempF int    `json:"temp_f"`
}

w, err := agent.RunTyped[Weather](ctx, a, runID, "weather in SF?")
// w.City == "SF", w.TempF == 68
```

It's a package function, not a method (Go methods can't add type parameters). The value is
decoded from the *journaled* tool call, so it's **resume-safe** — a crash mid-run recovers the
typed answer from the log on resume. If the model replies in plain JSON text instead of calling
the tool, `RunTyped` falls back to parsing that text. `T` is meant to be a struct.

On OpenAI-compatible providers with strict structured outputs, `RunTypedNative[T]` uses the
provider's native JSON-schema response format instead of the tool (schema enforced provider-side,
no tool round-trip); Anthropic ignores it, so use `RunTyped` there for provider-agnostic output.

## Sampling

Generation controls are provider-neutral and set once — each adapter maps them onto its wire
format (and drops what it can't do, e.g. Anthropic has no `seed`):

```go
a := agent.New(model, store, tools...).
	WithSampling(agent.Temperature(0), agent.MaxTokens(500), agent.TopP(0.9), agent.Seed(42))
```

Fields are optional by design: an unset field uses the provider default, so an explicit
`Temperature(0)` is distinct from "not specified." Request-level `MaxTokens` overrides an
adapter's construction-time default.

## Prompt caching

An agent loop resends a large constant prefix — system prompt + tool schemas — every turn.
Anthropic prompt caching bills those repeats at the cache-read rate:

```go
model := anthropic.New(key, anthropic.WithPromptCache())
```

This places `cache_control` breakpoints on the system block and the tool definitions. OpenAI
caches prefixes automatically (no flag needed). Either way, cache effectiveness surfaces in
`agent.Usage` — `CacheReadTokens` (served from cache) and `CacheWriteTokens` (written to it) —
so middleware like `TokenBudget` and cost accounting see the real numbers.

## Sessions (multi-turn)

`Run` is one turn. A `Session` is a durable multi-turn conversation: each `Send` is a full agent
run (tools, resume, side-effect safety) seeded with the transcript so far, so the agent remembers
earlier turns.

```go
s, _ := a.Session(ctx, "user-42")   // reopens + rebuilds the transcript from the store
a1, _ := s.Send(ctx, "what's the capital of France?")
a2, _ := s.Send(ctx, "and its population?")   // sees turn 1 in context
```

The transcript is journaled turn-by-turn under the session id, so a restarted process
`a.Session(ctx, "user-42")` rebuilds it and continues. Turn N runs under `"<id>/tN"` (its own
durable journal handles crash-resume *within* a turn); conversational memory is the question/answer
transcript — a turn's intermediate tool calls stay in that turn and don't leak into later ones. If
a turn pauses (approval / `Interrupt`), `Send` returns that error; resolve it and call `Send` again
with the same input to resume.

## Auditability (tamper-evident journal)

The durable journal already records every step of a run. The `audit` package commits to that
history with a hash chain, so a run's execution is verifiable:

```go
head, _ := audit.Head(ctx, store, runID)     // SHA-256 chain over the journal (persisted order)
sig := audit.Sign(head, priv)                // anchor it: sign / publish out-of-band
```

Any modify / insert / delete / reorder of a record changes the head. **Honest security model:** this
gives integrity unconditionally, and tamper-evidence *when you anchor the head out-of-band* (a chain
in the same DB an attacker controls can be rewritten and rehashed) — see the package doc. It's the
compliance/enterprise seam: provable at-most-once side effects *plus* a verifiable record of exactly
what the agent did.

For **selective disclosure**, `audit.Root` / `Prove` / `VerifyInclusion` build an **RFC 6962**
(Certificate Transparency) Merkle tree, so you can prove one record is part of a committed run —
via an O(log n) inclusion proof — *without revealing the other records* (e.g. show an auditor a
single charge happened, exposing no other customers or prompts). And `ProveConsistency` /
`VerifyConsistency` prove an earlier root is an **append-only prefix** of a later one — that history
was only appended, never rewritten or reordered (the transparency-log guarantee). The implementation
is checked against the published RFC 6962 test vectors.

`SignTreeHead` produces the CT-style **Signed Tree Head** — `{Size, Root, Timestamp}` signed with
Ed25519 — the artifact you publish. The full flow: sign an STH, later disclose a single record with an
inclusion proof an auditor checks against the signed root, and prove append-only growth between two STHs.
See [docs/AUDIT.md](docs/AUDIT.md) for the model, the API, and the end-to-end compliance flow.

## RAG & memory (bring your own)

go-agents ships **no vector store, embedder, or memory backend** — it gives you the *seam* and
you plug in the store you already run. Implement one interface against your infra:

```go
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]agent.Doc, error)
}
```

Then wire it in one of two ways:

```go
// Agentic RAG — the model searches on demand:
a := agent.New(model, store, agent.RetrievalTool(myStore, 5))

// Classic RAG — top-k auto-injected as context on each user turn:
a.Use(agent.WithRetrieval(myStore, 5))
```

Conversational memory is already built in (`Session`); dynamic context goes through
`WithSystemPromptFunc`; this seam covers semantic / long-term memory. Concrete store adapters (if
ever needed) would be separate modules, never in the core. See
[docs/RAG-MEMORY.md](docs/RAG-MEMORY.md).

## Resume safety, in one table

```go
agent.Safety{ReadOnly: true}          // no side effects → always safe to re-run
agent.Safety{Idempotent: true}        // safe to retry (dedupes downstream)
agent.Safety{}                        // a write → HALT on unknown outcome, don't double-fire
agent.Safety{RequiresApproval: true}  // pause for human approval before executing
```

Before a non-idempotent side effect the loop records a durable *attempt marker*, so
resume can tell "never ran" (safe to run) from "ran and crashed" (halt) — precisely, not
conservatively.

This is **proven, not asserted.** `dst_test.go` is a deterministic simulation test: a
fault-injecting store crashes at *every* write point (and across hundreds of randomized
multi-crash schedules), and the harness asserts a non-idempotent side effect fires **at most
once** every time, with the run always ending completed or halted — never double-firing.

The harness is exported (`chaos/`) and pointed at other SDKs in `benchmarks/`. The measured result:
**go-agents `maxFired=1` (PASS); trpc-agent-go `maxFired=5` (FAIL, 70 double-fires).** trpc's
checkpoint/resume genuinely works (verified — resuming a completed run is a no-op); its double-fire
is the documented LangGraph "nodes must be idempotent" window, which go-agents' attempt-marker closes.

`WithMaxTurns(n)` caps model turns per run so a model that keeps calling tools can't loop forever
— hitting it returns `ErrMaxTurns` (which is `errors.Is` `ErrBudget`).

## Human-in-the-loop

Two flavors. **Approve/deny** — a tool marked `RequiresApproval` pauses *before* running; the
human decision is a bool:

```go
_, err := a.Run(ctx, runID, input)
var pend *agent.PendingApproval
if errors.As(err, &pend) {
	// ... get a human decision ...
	agent.Approve(ctx, store, runID, pend.ToolUseID, true)
	out, _ := a.Run(ctx, runID, input) // resumes past the pause
}
```

**Interrupt/resume** — a tool pauses *at an arbitrary point* and resumes with a *typed* value
(generalizing the bool). Call `agent.Interrupt[T]` inside a retry-safe tool:

```go
tool := agent.Func("choose_plan", "pick a plan", agent.Safety{ReadOnly: true},
	func(ctx context.Context, in Options) (Plan, error) {
		pick, err := agent.Interrupt[Plan](ctx, "plan", in) // pauses the run; in is shown to the human
		if err != nil {
			return Plan{}, err // *Interrupted propagates out of Run
		}
		return pick, nil // on resume, pick is the human's typed answer
	})

_, err := a.Run(ctx, runID, input)
var intr *agent.Interrupted
if errors.As(err, &intr) {
	// ... show intr.Prompt, get a typed answer ...
	agent.Resume(ctx, store, runID, intr.Key, chosenPlan)
	out, _ := a.Run(ctx, runID, input) // resumes; Interrupt now returns chosenPlan
}
```

Both are durable — the decision/value is a journaled step, so it survives a crash. Interrupt
must be in a retry-safe tool (`ReadOnly`/`Idempotent`): on resume the tool re-runs until the
interrupt resolves, so everything before the `Interrupt` call must be safe to repeat.

## Errors

Failures are classified with sentinel errors matched by `errors.Is` — the standard-library
idiom, no custom error framework. Two tiers: a **category** (the coarse class) and a
**condition** (a specific cause) that wraps its category, so a match works at whichever level
you need:

```go
_, err := a.Run(ctx, runID, input)
switch {
case errors.Is(err, agent.ErrModel):       // any provider fault (HTTP status, decode, stream)
	backOffAndRetry()
case errors.Is(err, agent.ErrUnknownTool):  // a specific condition (implies agent.ErrTool)
	fixToolWiring()
case errors.Is(err, agent.ErrStorage):      // durable-store I/O
	alertOps()
}
```

Categories: `ErrConfig`, `ErrModel`, `ErrTool`, `ErrStorage`, `ErrProtocol`, `ErrBudget`.
Conditions (each wraps a category): `ErrUnknownTool`, `ErrToolArgs`, `ErrNoRecordedOutput`,
`ErrTruncatedToolArgs`, `ErrBudgetExceeded`. Every error the toolkit returns — including from
the model, MCP, store, and governance adapters — carries a category, so `errors.Is` is reliable
across the whole surface.

The **control-flow signals** are richer than a category, so they stay concrete types matched
with `errors.As`: `*PendingApproval` (approval needed), `*ResumeHalt` (unsafe to resume),
`*SagaAborted` (rolled back). A paused or halted run is not a "failure" category — inspect the
struct for `RunID` / `ToolUseID` / compensation details. Cancellation surfaces as the usual
`context.Canceled` / `context.DeadlineExceeded`.

## Middleware & observability

Two independent `func(Handler) Handler` chains at the two boundaries that matter — the model
call (`Use`) and each tool call (`UseTool`). First added = outermost. Both are *mutating and
short-circuiting*: rewrite what goes in, transform what comes out, or return without calling
`next`.

```go
var cost middleware.CostMeter
a := agent.New(model, store, tools...).
	Use(
		middleware.Retry(3, middleware.WithBackoff(200*time.Millisecond, 10*time.Second)),
		middleware.TokenBudget(100_000),
		middleware.Cost(&cost, middleware.Rates{InputPer1M: 3, OutputPer1M: 15}),
	).
	UseTool(middleware.ToolLog(log.Printf), middleware.ToolCache(), middleware.ToolRetry(3))

// opt-in OTel gen_ai.* spans — the core has no OTel dependency:
a.Use(trace.Model(tracer, trace.WithSystem("openai"), trace.WithModel("gpt-4o-mini")))
a.UseTool(trace.Tool(tracer)) // execute_tool span per call; nests across the sub-agent boundary
// ... after the run: cost.Total() (USD), cost.Usage()
```

`Retry` does exponential backoff with jitter and honors a `Retry-After` on a provider 429 (the
adapter returns a typed `*agent.RateLimited`); `Cost` accumulates USD from token usage (incl.
cache-read/write) into a `CostMeter` you read after the run.

Because `trace.Tool` runs inside the loop, its span sits in the context handed to the tool — so
when a tool is itself a sub-agent, the sub-agent's run and its own spans nest as children. The
trace crosses the sub-agent boundary automatically (a gap in ADK / AgenticGoKit / trpc-agent-go).

Tool middleware runs *inside* the durable step, so a short-circuit (a `ToolCache` hit) or a
policy denial is journaled like any tool result — resume replays it and never re-runs the
middleware or the tool. Write your own with the `agent.ToolMiddleware` signature:

```go
// Deny a tool by policy — the tool never executes; the model sees the error and reacts.
func RequireTag(tag string) agent.ToolMiddleware {
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
			if !authorized(ctx, tag) {
				return nil, fmt.Errorf("tool %q denied: %w", tu.Name, agent.ErrTool)
			}
			return next(ctx, tu) // mutate tu.Args before, transform the result after
		}
	}
}
```

## Modules

go-agents is a multi-module repo: a dependency-light **core** (`github.com/dayna/go-agents` —
the loop, schema, middleware, model adapters, govern; deps are just gsm + `x/sync`) plus one
module per heavy adapter (`mcp`, `trace`, `store/sqlite`, `store/postgres`, `govern/redislog`,
`govern/sqlitelog`). Import an adapter and you pull its dependency tree; import only the core
and you don't. A core-only consumer's external-module surface is 2, not 54. See
[docs/MODULE-STRUCTURE.md](docs/MODULE-STRUCTURE.md).

## Architecture

Hexagonal by construction: the core defines the ports (`Model`, `Durable`, `Tool`,
`Middleware`); adapters plug in at the edges. Dependencies point inward — the core imports
no adapter and no infrastructure, guarded by `architecture_test.go`.

```
agent (root)     durable loop · Message/Part · Tool/Safety · Durable · middleware types · RenderMermaid
model/anthropic  native Claude (thinking + signatures)
model/openai     any OpenAI-compatible endpoint
schema           reflect Go types → inline JSON Schema + OpenAIStrict
middleware       Retry, TokenBudget
trace            opt-in OTel gen_ai.* spans
store/sqlite     on-disk durable resume (single binary, no cluster)
store/postgres   HA durable resume (any node resumes any run)
govern           Tier-2: convergent shared state for concurrent agents (gsm-backed)
```

## Convergent governance (Tier-2)

The durable core keeps *one* agent's work crash-safe. The `govern` tier handles the other
hard case: **many agents mutating shared state, converging without coordination.** You
describe the shared state as a registry (variables + invariants + events); gsm proves *at
build time* that every interleaving of agent actions reaches the same valid state — or refuses
to build and shows you a counterexample. Runtime is O(1) table lookups; state is event-sourced
and crash-recoverable.

It scales from a single shared registry up through **federations** (cross-agent constraints:
trees → multi-source DAGs with resolvers → monotone cyclic *meshes*), composes via `Embed`,
and can even **synthesize** the compensation for you (declare the rules, get a convergent
governor — or a proof that none exists). Agents plug in through `FederatedEventTool`, so an
LLM tool call becomes a governed event.

```go
gov, _ := govern.NewPersistent(ctx, machine, log, "order-42", machine.NewState())
tool := govern.EventTool(gov, "pay", "mark the order paid", "pay", agent.Safety{})
// hand `tool` to the agent — concurrent agents sharing `gov` converge, durably.
```

> Full guide, capability ladder, and the two runnable demos (`examples/mesh`, `examples/compose`)
> in **[docs/GOVERNANCE.md](docs/GOVERNANCE.md)**.

> Name is deliberately deferred — this is a codename. Design + competitive analysis in
> `docs/DESIGN.md` and `docs/COMPETITIVE*.md`; the governance tier in `docs/GOVERNANCE.md`.
