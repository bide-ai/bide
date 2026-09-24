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

	a := agent.New(model, store, weather)
	out, _ := a.Run(context.Background(), "run-1", "Weather in SF? Use the tool.")
	for _, p := range out.Parts {
		if t, ok := p.(agent.Text); ok {
			fmt.Println(t.Text)
		}
	}
}
```

Run the live smoke example: `OPENROUTER_API_KEY=sk-... go run ./examples/smoke`

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

## Human-in-the-loop

```go
_, err := a.Run(ctx, runID, input)
var pend *agent.PendingApproval
if errors.As(err, &pend) {
	// ... get a human decision ...
	agent.Approve(ctx, store, runID, pend.ToolUseID, true)
	out, _ := a.Run(ctx, runID, input) // resumes past the pause
}
```

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
a := agent.New(model, store, tools...).
	Use(middleware.Retry(3), middleware.TokenBudget(100_000)).       // wraps the model call
	UseTool(middleware.ToolLog(log.Printf), middleware.ToolCache())  // wraps every tool call

// opt-in OTel gen_ai.* spans — the core has no OTel dependency:
a.Use(trace.Model(tracer, trace.WithSystem("openai"), trace.WithModel("gpt-4o-mini")))
```

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
