# A tour of bide

The API, section by section: graphs, ambient runs, the agent loop, streaming, typed output, sessions, the audit trail, human approval, errors, middleware and governance. Each section links to the guide that covers it in full. New to bide? Start with [Getting started](getting-started.md).

## Graphs

In a graph-first framework the graph is what you author and what executes, with nodes, edges, a
state object, and sometimes a visual builder on top. In bide the same authoring surfaces exist, up
to a visual builder, as optional layers over a plain-Go journal.

A graph adds no expressive power. Anything a graph computes, ordinary control flow computes: a
computation graph is a control-flow graph, and sequence, selection, and iteration suffice to express
any of them. There is no agent behavior you can build as a node-and-edge graph that you cannot write
with `if`, `for`, and functions. What a graph adds is a first-class representation of the flow
that you can inspect, visualize, validate statically, and author outside the code. That is a
tooling layer, and it is not required to build agents.

So the substrate here is plain Go, and the guarantees (durability, at-most-once, the verifiable
trail) come from the journal. The graph exists as a derived view:
`RenderMermaid` reconstructs it from what actually ran.

To author a graph, use the `plan` package: a constrained, type-checked flow builder that compiles
to this runtime and inherits at-most-once and the audit trail. You wire typed nodes (`Step`, `Tool`, `Model`, `Switch`, fan-in `Join`, bounded
`LoopBack`) into a `Flow`, or author the same topology as declarative config (`plan.Load`) that a
higher layer such as a visual builder can emit. A node can carry an m-of-n approval gate
(`plan.Approval`, or `approval: {need, approvers}` in config): the flow pauses before the node until
k approvers have signed its exact input, with the same journaled gate and offline evidence as an
agent's tool call.

An authored graph is a diagram; a derived graph is reconstructed from the journal, so it is
exactly what ran. The `plan` layer ties
the two together: `Topology()` and `RenderMermaid()` expose the declared shape, and `Conform()`
cryptographically checks that a run followed the topology it declared. The rule that keeps any such layer from forking the runtime is
that a new surface may add a way to author, never a way to execute: every layer lowers to the one
journal-backed runtime. See [docs/guides/plan-flows.md](guides/plan-flows.md).

### Three ways to author the same flow

The same order-triage flow, three ways. Plain Go is the default: write ordinary control flow, and name the steps the journal must make crash-safe.

<!-- docsnip: setup ctx context.Context; journal *agent.Journal; order Order; type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{}; func classify(Order) (Assessment, error); func reserve(Assessment) (Reservation, error); func finalize(Reservation) (Receipt, error); func decline(Assessment) (Receipt, error) -->
```go
// classify, then branch: rush orders reserve-then-finalize, the rest decline.
assess, _ := journal.Step(ctx, "order-42", "classify",
    func(ctx context.Context) (Assessment, error) { return classify(order) },
    agent.WithSafety(agent.Safety{ReadOnly: true})) // safe to re-run after a crash

var receipt Receipt
if assess.Rush {
    res, _ := journal.Step(ctx, "order-42", "reserve", // a side effect: at most once
        func(ctx context.Context) (Reservation, error) { return reserve(assess) })
    receipt, _ = journal.Step(ctx, "order-42", "finalize",
        func(ctx context.Context) (Receipt, error) { return finalize(res) })
} else {
    receipt, _ = journal.Step(ctx, "order-42", "decline",
        func(ctx context.Context) (Receipt, error) { return decline(assess) })
}
```

To make that flow an inspectable artifact, the `plan` builder wires typed nodes into a `Flow` that lowers to the same runtime:

<!-- docsnip: setup type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{} -->
```go
b := plan.New[Order, Receipt]("order-triage")
classify := b.Step("classify", func(ctx context.Context, o Order) (Assessment, error) { ... })
reserve  := b.Step("reserve",  func(ctx context.Context, a Assessment) (Reservation, error) { ... }) // non-idempotent
finalize := b.Step("finalize", func(ctx context.Context, r Reservation) (Receipt, error) { ... })
decline  := b.Step("decline",  func(ctx context.Context, a Assessment) (Receipt, error) { ... })

b.Switch(classify,
    plan.When(func(a Assessment) bool { return a.Rush }, reserve).Named("rush"),
    plan.Else(decline),
)
b.Edge(reserve, finalize)

flow, err := b.Build() // inherits at-most-once and the audit trail
```

Or author that same topology as declarative config a higher layer (a visual builder) can emit, loaded with `plan.Load`:

```json
{
  "version": 1,
  "flow": "order-triage",
  "entry": "classify",
  "nodes": [
    {"name": "classify", "block": "classify"}, {"name": "reserve", "block": "reserve"},
    {"name": "finalize", "block": "finalize"}, {"name": "decline", "block": "decline"}
  ],
  "wiring": [
    {"switch": "classify", "when": [{"pred": "rush", "to": "reserve"}], "else": "decline"},
    {"edge": ["reserve", "finalize"]}
  ]
}
```

<!-- docsnip: setup type Order struct{}; type Receipt struct{}; configBytes []byte; reg *plan.Registry -->
```go
flow, err := plan.Load[Order, Receipt](configBytes, reg) // same topology, same Digest()
```

`bide view -config` draws a config and marks its problems before you load it (see [Check a flow config](guides/run-viewer.md#check-a-flow-config)).

All three lower to the one journal-backed runtime, so at-most-once, HA resume and the verifiable trail apply whichever you pick.

## Ambient runs: durable sleep, wake, and interrupt

An ambient run does not sit in a synchronous chat loop. It sleeps, wakes on a trigger, and pauses for a human, and every
one of those transitions is a durable, at-most-once step on the journal, so the run survives crashes
and node handoffs between them.

- **Sleep until a deadline.** `Sleep`/`WaitUntil` pause a run and journal its wake time, so the pause
  outlives a restart. Re-invoking at the wake time resumes exactly once.
- **Wake on time or event.** A pluggable `Waker` (in-process `MemWaker` by default) re-invokes a due
  run; the trigger source is yours (an in-process loop, a cron, a queue, an inbound webhook), so the
  same substrate drives both scheduled and event-driven agents. A wake that cannot be scheduled
  fails the run (wrapping `ErrStorage`) rather than leave it asleep with nothing to wake it, and
  `RecoverLoop` retries it.
- **Interrupt for a human, durably.** `Interrupt[T]`/`AnswerInterrupt` pause a run at any point to request a
  typed decision and resume with the human's answer as a journaled step (see
  [Human-in-the-loop](#human-in-the-loop)). Approve/deny is the boolean special case.

You supply the trigger source and the oversight surface; the runtime keeps the run correct across
every sleep, wake, interrupt, crash, and handoff. Runnable in `examples/signals` (deliver an event
into a waiting run), `examples/interrupt` (human-in-the-loop pause/resume), and `examples/recover`
(durable resume). See the [signals and ambient guide](guides/signals.md).

## At most once, in code

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; type ChargeArgs struct{}; type Receipt struct{} -->
```go
// A tool that moves money is a write: not ReadOnly, not Idempotent.
charge := agent.MustFunc("charge_card", "Charge the customer", func(ctx context.Context, in ChargeArgs) (Receipt, error) { /* ... */ })

// If the process crashes after the charge fires but before its result is journaled,
// resume does NOT run it again: it returns *OutcomeUnknown so you confirm, not double-charge:
_, err := a.Run(ctx, runID, agent.UserText(input))
if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
	// halt.Op.ToolName == "charge_card": outcome unknown, a human decides, no double side effect.
}
```

<p align="center"><img src="../assets/divider-line.png" width="100%" alt=""></p>

## Quickstart

Requires Go 1.27 (the core uses generic methods). If `go version` is older, upgrade or set
`GOTOOLCHAIN=go1.27.0`.

The core package is `agent`, imported from `github.com/bide-ai/bide/agent`
(as the block below shows). In a module of your own, install it and the SQLite store with
`go get github.com/bide-ai/bide/agent@latest github.com/bide-ai/bide/store/sqlite@latest` (or write the code and run `go mod tidy`).
No API key? The [getting started guide](getting-started.md#no-api-key-use-agenttest) runs the
same agent offline with a scripted model.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/openai"
	"github.com/bide-ai/bide/store/sqlite"
)

type WeatherArgs struct {
	City string `json:"city" desc:"city name"`
}
type Weather struct {
	TempF int    `json:"temp_f"`
	Sky   string `json:"sky"`
}

func main() {
	// Any OpenAI-compatible endpoint (here OpenRouter); swap the base URL for Ollama, etc.
	model := openai.New(os.Getenv("OPENROUTER_API_KEY"),
		openai.WithBaseURL("https://openrouter.ai/api/v1"),
		openai.WithModel("openai/gpt-4o-mini"))

	// A tool is a typed Go function; its schema is derived automatically.
	weather := agent.MustFunc("get_weather", "Current weather for a city",
		func(_ context.Context, in WeatherArgs) (Weather, error) {
			return Weather{TempF: 68, Sky: "sunny"}, nil
		}, agent.WithSafety(agent.Safety{ReadOnly: true}))

	// Durable on-disk store: a crash mid-run resumes from here.
	store, err := sqlite.Open("agent.db")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		log.Fatal(err)
	}

	a, err := agent.New(
		model,
		j,
		agent.WithTools(weather),
		agent.WithSystemPrompt("You are a concise weather assistant."),
	)
	if err != nil {
		log.Fatal(err)
	}
	out, err := a.Run(context.Background(), "run-1", agent.UserText("Weather in SF? Use the tool."))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Message.Text())
}
```

Run the live smoke example: `OPENROUTER_API_KEY=sk-... go run github.com/bide-ai/bide/examples/smoke@latest`

A `Func` tool decodes its arguments **strictly**, so the tool reads exactly what the model sent.
A call that is missing a required field (one the schema lists as required: not a pointer, and no
`omitempty` or `omitzero` in its json tag) or sends it as `null` (unless its schema admits any
value, as for `any` or `json.RawMessage`), uses a name that is not a field (an unknown name, or a
case variant such as `"CITY"` for `city`), repeats a name, has data after the object, or holds
invalid UTF-8 or an escaped lone surrogate is an `ErrToolArgs` error. That error goes back to the
model as the call's result, so the model can correct the call. `encoding/json` would fill in zero
values, match names case-insensitively, drop unknown names, and keep the last duplicate. Give each
argument field the json tag the model sees in the schema.

`Run` takes a `Message` input (text, or text and images) and per-run options, and returns a
`Result`: the final message, token usage for the whole run (including cache and sub-agents), the
model-turn count, and the wall-clock duration. `Turns` counts only the model turns this invocation
made, so a resumed run, or a finished one driven again, can show fewer turns than the run made
overall (`Usage` and `Spend` are the whole run's). The `Result` is non-nil on every error once the run
ID is valid: a pause, a halt, a failure, a saga's abort, a cancellation:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string -->
```go
res, err := a.Run(ctx, runID, agent.UserText(input),
	agent.WithTokenBudget(50_000), agent.WithSystemPrompt("You are terse."))
// res.Message, res.Usage, res.Spend, res.Turns, res.Duration, res.RunID
_ = err
```

A run's first drive journals its input and the options its caller passed in `run:start`, and
every later drive runs under them, a recovery drive included: `Resume(ctx, runID)` needs
neither, a later drive may raise or lower a limit (`WithMaxTurns`, `WithTokenBudget`: journaled
as an amendment), and any other different setting is `ErrConfig`. `WithSaga()` runs the run as a
saga, and `WithToolFilter(names...)` restricts it to some of the agent's tools, enforced when each
call is dispatched. `agent.Cancel(ctx, j, runID, reason)` cancels a run (a saga is rolled back
first), and `agent.Status(ctx, j, runID)` reads its state from the journal.

## Streaming

`Run` blocks and returns the final answer. To watch the agent work (token deltas, turn
boundaries, tool start/finish), use `Stream`. It drives the **same loop** as `Run`
(`Stream(...).Result()` returns what `Run` would), so durability, resume, and side-effect safety are identical:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string -->
```go
stream := a.Stream(ctx, runID, agent.UserText(input))
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
res, err := stream.Result() // what Run returns: the Result and an error (incl. a pause: *ApprovalPending, *OutcomeUnknown, ...)
```

Events: `TurnStarted`, `ModelEvent` (the token feed), `TurnRestarted`, `AssistantTurn`, `ToolStarted` /
`ToolCompleted`, `ApprovalRequired`, `Finished`. Range `Events()` for a UI then call `Result()`,
or call `Result()` alone to behave exactly like `Run` (it drains events for you). `ToolStarted`
fires immediately before a tool is called, so a call cancelled before it starts (and recorded as
not started) emits neither `ToolStarted` nor `ToolCompleted`.

Two things worth knowing, both consequences of durability:
- **Token deltas arrive below the middleware chain** (Retry / Cost still see whole
  assembled messages), and **only on a fresh model call**. One request of a turn streams at a
  time. When a middleware such as Retry calls the model again after an attempt that already
  streamed deltas, or the turn's response is not the one that streamed (a Hedge backup won, a
  fallback or a cache answered), `TurnRestarted` marks those deltas as discarded, and the
  response the turn records follows: clear the turn's text when it arrives. Middleware has no
  streaming code for this; the agent does it.
- **On resume, the journaled transcript is re-emitted** as `AssistantTurn{Replayed: true}` +
  `ToolCompleted` before live progress, so a fresh UI reconstructs the whole story after a
  crash, and a replayed turn produces no token deltas (it was already decided).

`Stream` takes the same options as `Run` (`WithSaga()` for a saga), and its `RunStream.Result()`
returns what `Run` would.

## Typed output

`RunTyped[T]` returns a typed `T` instead of a free-form message. It injects a synthetic
`final_answer` tool whose JSON schema is derived from `T` (via the `schema` package) and steers
the model to call it once its work is done, so a tool-using agent can do its work and then
answer typed. It is provider-agnostic: it uses native tool calling, not a provider's JSON mode.

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string -->
```go
type Weather struct {
	City  string `json:"city"`
	TempF int    `json:"temp_f"`
}

w, _, err := a.RunTyped[Weather](ctx, runID, agent.UserText("weather in SF?"))
// w.City == "SF", w.TempF == 68
```

`a.RunTyped[T](ctx, runID, input, opts...)` is a Go 1.27 generic method; it returns the run's
`Result` too (its `Output` is the answer as journaled). A typed run journals
its output mode and `T`'s schema in `run:start`, so resuming it through an untyped entry point, or
with another `T`, is `ErrConfig`; recovery resumes it with `agent.ResumeTyped[T](a)`. The answer is the
arguments `final_answer` accepted, decoded strictly like any `Func` tool's (a loose call goes back
to the model to correct), as the tool received them after any tool middleware. The tool journals
them as its result, so the answer is **resume-safe**: a crash mid-run recovers the typed answer
from the log on resume. The first `final_answer` call the tool accepts ends the run.
Only if the model never makes one (it replies in plain JSON text instead) does `RunTyped` parse
the text of the run's final turn. `T` must be a JSON object (a struct, a pointer to one, or a map),
since providers take tool arguments only as an object; any other `T` is `ErrConfig`.

On OpenAI-compatible providers with strict structured outputs, `RunTyped[T]` with
`agent.WithOutputMode(agent.OutputNative)` uses the provider's native
JSON-schema response format instead of the tool (schema enforced provider-side, no tool
round-trip); the Anthropic adapter does not support it and returns `ErrConfig`, so use the default
tool mode there for provider-agnostic output.

## Sampling

Generation controls are provider-neutral and set once; each adapter maps them onto its wire
format (and drops what it can't do, e.g. Anthropic has no `seed`):

<!-- docsnip: setup model agent.Model; journal *agent.Journal; tools []agent.Tool -->
```go
a, err := agent.New(
	model,
	journal,
	agent.WithTools(tools...),
	agent.WithSampling(agent.Temperature(0), agent.MaxTokens(500), agent.TopP(0.9), agent.Seed(42)),
)
if err != nil {
	panic(err)
}
```

Fields are optional: an unset field uses the provider default, so an explicit
`Temperature(0)` is distinct from "not specified." Request-level `MaxTokens` overrides an
adapter's construction-time default.

## Prompt caching

An agent loop resends a large constant prefix (system prompt + tool schemas) every turn.
Anthropic prompt caching bills those repeats at the cache-read rate:

<!-- docsnip: setup key string -->
```go
model := anthropic.New(key, anthropic.WithPromptCache())
```

This places `cache_control` breakpoints on the system block and the tool definitions. OpenAI
caches prefixes automatically (no flag needed). Either way, cache effectiveness surfaces in
`agent.Usage` (`CacheReadTokens`, served from cache, and `CacheWriteTokens`, written to it),
so cost accounting, tracing, and the run's token budget see the real numbers.

## Sessions (multi-turn)

`Run` is one turn. A `Session` is a durable multi-turn conversation: each `Send` is a full agent
run (tools, resume, side-effect safety) seeded with the transcript so far, so the agent remembers
earlier turns.

<!-- docsnip: setup ctx context.Context; a *agent.Agent -->
```go
s, _ := a.Session(ctx, "user-42")   // reopens + rebuilds the transcript from the store
r1, err := s.Send(ctx, agent.UserText("what's the capital of France?"))
if err != nil {
	panic(err)
}
r2, err := s.Send(ctx, agent.UserText("and its population?")) // sees turn 1 in context
if err != nil {
	panic(err)
}
fmt.Println(r1.Message.Text(), r2.Message.Text())
```

The transcript is journaled turn-by-turn under the session id, so a restarted process
`a.Session(ctx, "user-42")` rebuilds it and continues. The id must not contain `>`. Turn N runs under `"<id>>@turn/N"` (its own
durable journal handles crash-resume *within* a turn); conversational memory is the question/answer
transcript: a turn's intermediate tool calls stay in that turn and don't leak into later ones. If
a turn pauses (approval / `Interrupt`), `Send` returns that error; resolve it and call `Send` again
with the same input to resume. Until then, `Send` with a different message returns `ErrConfig`: the
open turn belongs to its message. For inbound messages that may be redelivered, `SendOnce(ctx, id,
input)` answers each message id once. Several handles on one session never lose a turn, record one
twice, or answer one message with another's reply. Over a store with leases (`MemStore`, SQLite,
Postgres), a turn is driven by one worker at a time under its run's lease, so its token budget
holds across workers; a second worker sent the same message meanwhile gets `ErrTurnContended` and
sends it again later. A turn resumed after a crash is seeded with the same transcript it started
with, even if other messages were answered in between. `Send` and `SendOnce` take a `Message` and
run options and return a `Result`. A turn whose run was cancelled (`agent.Cancel`) is closed: `Send` of its message
returns `ErrRunCancelled`, and the next message's `Send` records the turn closed, with no answer and
outside the transcript, and runs its own turn.

## Auditability (tamper-evident journal)

The durable journal already records every step of a run. The `audit` package commits to that
history with a hash chain, so a run's execution is verifiable:

<!-- docsnip: setup ctx context.Context; journal *agent.Journal; runID string; priv ed25519.PrivateKey -->
```go
head, _ := audit.Head(ctx, journal, runID)                     // SHA-256 chain over the stored journal bytes
sig, _ := audit.Sign(head, audit.Ed25519Signer{Priv: priv}) // anchor it: sign / publish out-of-band
```

Any modify / insert / delete / reorder of a record changes the head. **Security model:** this
gives integrity unconditionally, and tamper-evidence *when you anchor the head out-of-band* (a chain
in the same DB an attacker controls can be rewritten and rehashed); see the package doc.

For **selective disclosure**, `audit.Root` / `Prove` / `VerifyInclusion` build an **RFC 6962**
(Certificate Transparency) Merkle tree, so you can prove one record is part of a committed run
via an O(log n) inclusion proof, without revealing the other records (e.g. show an auditor a
single charge happened, exposing no other customers or prompts). And `ProveConsistency` /
`VerifyConsistency` prove an earlier root is an **append-only prefix** of a later one: that history
was only appended, never rewritten or reordered (the transparency-log guarantee). The implementation
is checked against the published RFC 6962 test vectors.

`SignTreeHead` produces the CT-style **Signed Tree Head** (format `bide.audit.sth.v5`), `{Kind, RunID,
Size, Root, TimestampNanos}` signed together with its scheme under any `audit.Signer`: Ed25519,
ML-DSA-65 (post-quantum), or a hybrid of both. That is the artifact you publish. Leaves commit to the
bytes the journal stores for each record, verbatim, so a proof carries those bytes (`record_bytes`) and
a record written by a newer release with fields this one does not know still verifies. The full flow: sign an STH, later disclose a single record with an
inclusion proof an auditor checks against the signed root, and prove append-only growth between two STHs.
See [docs/guides/audit.md](guides/audit.md) for the model, the API, and the end-to-end compliance flow.

### Run viewer

`bide view` opens a read-only page for one run, served on `127.0.0.1` from a SQLite or Postgres
journal or an exported one. It shows the run's timeline (model turns, tool calls and their `Safety`,
attempts, approvals), a halted operation with the `ResolveHalt` call that records its outcome, each
approval gate's k-of-n status (a tool call's or a flow node's) and whether its signatures verify, a
plan flow's path over its declared topology, and whether the journal verifies against a signed tree
head or an evidence package. It never writes to the store. `bide view -config flow.yaml` shows a flow
definition the same way, with its problems marked on the graph, and `bide check flow.yaml` runs that check in CI.
Install it with `go install github.com/bide-ai/bide/cmd/bide@latest` (or `brew install bide-ai/tap/bide`); see the
[run viewer guide](guides/run-viewer.md).

<table>
<tr>
<td width="50%"><picture><source media="(prefers-color-scheme: dark)" srcset="./guides/img/refund-before-dark.png"><img src="./guides/img/refund-before-light.png" width="100%" alt="bide view: run refund-42 halted, outcome unknown, with its ResolveHalt call"></picture></td>
<td width="50%"><picture><source media="(prefers-color-scheme: dark)" srcset="./guides/img/refund-after-dark.png"><img src="./guides/img/refund-after-light.png" width="100%" alt="bide view: run refund-42 completed, approved 2 of 3, audit proof verified"></picture></td>
</tr>
<tr>
<td>Before: the refund crashed after it fired, so the run is halted with its outcome unknown, and the page gives the <code>ResolveHalt</code> call.</td>
<td>After: <code>ResolveHalt</code> recorded the outcome and the run completed; the gate shows 2 of 3 approvers, and the evidence package verifies.</td>
</tr>
</table>

## RAG & memory (bring your own)

Bide ships no vector store, embedder or memory backend: it gives you the interface, and you plug in
the store you already run. Implement one interface against your infra:

<!-- docsnip: api agent -->
```go
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]agent.Doc, error)
}
```

Then wire it in one of two ways:

<!-- docsnip: setup model agent.Model; journal *agent.Journal; myStore agent.Retriever -->
```go
// Agentic RAG: the model searches on demand:
a, err := agent.New(model, journal,
	agent.WithTools(agent.MustRetrievalTool("search_kb", "Search the knowledge base.", myStore, 5)))

// Classic RAG: top-k auto-injected as context on each user turn:
a, err = agent.New(model, journal, agent.WithRetrieval(myStore, 5))
```

Conversational memory is already built in (`Session`); dynamic context goes through
`WithSystemPromptFunc`; this seam covers semantic / long-term memory. Concrete store adapters (if
ever needed) would be separate modules, never in the core. See
[docs/guides/rag-memory.md](guides/rag-memory.md).

## Resume safety, in one table

```go
agent.Safety{ReadOnly: true}          // no side effects → always safe to re-run
agent.Safety{Idempotent: true}        // safe to retry (dedupes downstream)
agent.Safety{}                        // a write → HALT on unknown outcome, don't double-fire
agent.WithApproval(agent.SingleApproval()) // not Safety: a tool option that pauses for human approval before executing
```

Before a non-idempotent side effect the loop records a durable *attempt marker*, so
resume can tell "never ran" (safe to run) from "ran and crashed" (halt).

`dst_test.go` is a deterministic simulation test: a
fault-injecting store crashes at *every* write point (and across hundreds of randomized
multi-crash schedules), and the harness asserts a non-idempotent side effect fires **at most
once** every time, with the run always ending completed or halted, never double-firing.

The harness is exported (`chaos/`) and pointed at other SDKs in `benchmarks/`. The measured result:
**Bide `maxFired=1` (PASS); trpc-agent-go `maxFired=6`; langchaingo `maxFired=64` (both FAIL).**
trpc's checkpoint/resume works (resuming a completed run is a no-op); its double-fire is the
documented LangGraph "nodes must be idempotent" window. langchaingo has no durability, so retries
re-run everything. Bide's attempt marker closes that window.

`WithMaxTurns(n)` caps model turns per run so a model that keeps calling tools can't loop forever:
hitting it returns `ErrMaxTurns` (which is `errors.Is` `ErrBudget`). `WithTokenBudget(n)` caps the
tokens a run may use, cached input included: once the run has used `n`, it makes no further model
call and returns `ErrBudgetExceeded`. The budget covers the run's whole agent tree: its sub-agents'
model calls count against it, parallel ones included. Each call's usage is journaled with its turn,
so both limits are rebuilt from the journal and hold across a crash and resume.

## Human-in-the-loop

Three forms. **Approve/deny**: a tool marked `WithApproval(SingleApproval())` pauses *before* running; the
human decision is a bool:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; journal *agent.Journal; runID string; input string -->
```go
_, err := a.Run(ctx, runID, agent.UserText(input))
if pend, ok := errors.AsType[*agent.ApprovalPending](err); ok {
	// ... get a human decision ...
	agent.Approve(ctx, journal, pend.RunID, pend.ToolUseID, true)
	res, _ := a.Run(ctx, pend.RootRunID, agent.UserText(input))
	var out agent.Message
	if res != nil {
		out = res.Message
	} // resumes past the pause
}
```

**Interrupt/resume**: a tool pauses *at an arbitrary point* and resumes with a *typed* value
(generalizing the bool). Call `agent.Interrupt[T]` inside a retry-safe tool:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; journal *agent.Journal; runID string; input string; type Options struct{}; type Plan struct{}; chosenPlan Plan -->
```go
tool := agent.MustFunc("choose_plan", "pick a plan", func(ctx context.Context, in Options) (Plan, error) {
		pick, err := agent.Interrupt[Plan](ctx, "plan", in) // pauses the run; in is shown to the human
		if err != nil {
			return Plan{}, err // *InterruptPending propagates out of Run
		}
		return pick, nil // on resume, pick is the human's typed answer
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))

_, err := a.Run(ctx, runID, agent.UserText(input))
if intr, ok := errors.AsType[*agent.InterruptPending](err); ok {
	// ... show intr.Prompt, get a typed answer ...
	journal.AnswerInterrupt(ctx, intr.RunID, intr.Name, chosenPlan)
	res, _ := a.Run(ctx, intr.RootRunID, agent.UserText(input))
	var out agent.Message
	if res != nil {
		out = res.Message
	} // resumes; Interrupt now returns chosenPlan
}
```

Both are durable: the decision/value is a journaled step, so it survives a crash. Interrupt
must be in a retry-safe tool (`ReadOnly`/`Idempotent`): on resume the tool re-runs until the
interrupt resolves, so everything before the `Interrupt` call must be safe to repeat.

**m-of-n approval**: when one sign-off is not enough, require k signed decisions from a named set
of n approvers. Each approver signs the exact call (tool and arguments); the gate proceeds at k
approvals, denies once k is unreachable, and otherwise pauses with the running tally. A forged or
mistaken decision is ignored without locking its approver out:

<!-- docsnip: setup ctx context.Context; model agent.Model; journal *agent.Journal; pend *agent.ApprovalPending; type RefundArgs struct{}; doRefund func(context.Context, RefundArgs) (string, error); keysByApprover agent.ApproverVerifierFor; signer audit.Signer -->
```go
refund := agent.MustFunc("refund", "refund the order", doRefund,
	agent.WithApproval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}))
a, err := agent.New(model, journal, agent.WithTools(refund), agent.WithApproverVerifiers(keysByApprover))
if err != nil {
	panic(err)
}

// each approver, out of band, signs the paused call they were shown:
sig, _ := signer.Sign(agent.ApprovalDecisionBytes(pend.Subject(), "finance", true))
agent.SubmitDecision(ctx, journal, agent.Decision{RunID: pend.RunID, ToolUseID: pend.ToolUseID,
	ApproverID: "finance", Approved: true, Alg: signer.Alg(), Signature: sig})
```

`audit.ApprovalEvidence` and `audit.VerifyApprovals` (or `bide audit verify-approvals`) then prove
offline that k named approvers signed off on this exact call *before* it ran (each decision counts
only under the signature scheme journaled with it, which must be its approver key's), under the expected
policy, from evidence that cannot leave a decision out unnoticed. Each approver needs a key of their
own: a policy two of whose approvers resolve to one key is refused with `ErrConfig`, since whoever
holds that key could sign as both. A [plan flow node](guides/plan-flows.md#approval-gates-on-nodes)
takes the same gate. See the [approval guide](guides/hitl-approval.md); runnable across
separate processes in `examples/approval` (and `examples/approval/flow` for a gated flow node).

## Errors

Failures are classified with sentinel errors matched by `errors.Is`. Two tiers: a **category** (the coarse class) and a
**condition** (a specific cause) that wraps its category, so a match works at whichever level
you need:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; func backOffAndRetry(); func fixToolWiring(); func alertOps() -->
```go
_, err := a.Run(ctx, runID, agent.UserText(input))
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
Conditions (each wraps a category): `ErrUnknownTool`, `ErrToolArgs`, `ErrToolOutcomeUnknown` (wrap `ErrTool`),
`ErrToolReinvoked`, `ErrInvalidApproval`, `ErrAlreadyDecided` (wrap `ErrConfig`),
`ErrNoRecordedOutput`, `ErrIncompleteResponse` (wrap `ErrModel`), `ErrTruncatedToolArgs` (wraps
`ErrProtocol`), `ErrStreamProtocol` and `ErrNegativeUsage` (wrap `ErrProtocol` and `ErrModel`), `ErrBudgetExceeded`,
`ErrMaxTurns` (wrap `ErrBudget`). `ErrToolNotCalled` wraps no category: it marks a tool call known
never to have reached its tool (a tool middleware's denial wraps it). Provider adapters also return `*RateLimited` (HTTP 429, with a
`RetryAfter` hint) and `*APIError` (other non-2xx, with the `StatusCode`), both wrapping
`ErrModel`. Every error the
toolkit returns (including from the model, MCP, store, and governance adapters) carries a category,
so `errors.Is` is reliable across the whole surface.

The **control-flow signals** are richer than a category, so they stay concrete types. A paused or
halted run returns an `agent.Pause`, a sealed interface with exactly five kinds: `*ApprovalPending`
(approval needed), `*InterruptPending` (waiting for human input), `*TimerPending` (durable timer
pending), `*SignalPending` (waiting for an external signal or channel message), and
`*OutcomeUnknown` (unsafe to resume; its `Cause` is `HaltCrashed` or `HaltContended`; a `plan` flow's
node runs as a `Step` and halts as one, named by its node key `node:<name>`, and `Flow.ResolveHalt`
clears it). Test for one
with `agent.IsPause(err)`, read it with `agent.AsPause(err)` (its `Paused()` names the run to answer
and the run to re-invoke), or match a kind with `errors.As`. The others are `*SagaAborted` (rolled
back) and `*HaltTooYoung` (from `ResolveHalt`, when `WithMinHaltAge` has not elapsed yet). A
paused or halted run is not a "failure" category; inspect the struct for its run, operation, or
compensation details. A run cancelled with `agent.Cancel` returns `ErrRunCancelled` (with a
`Result`), which carries no category: it is a terminal status, not a fault. Context cancellation
surfaces as the usual `context.Canceled` /
`context.DeadlineExceeded`, and a drive cancelled because its run lease was lost (`agent.Lease`) as
`ErrLeaseLost`; like cancellation, it carries no category.

## Middleware & observability

Two independent `func(Handler) Handler` chains: one around the model
call (`WithMiddleware`) and each tool call (`WithToolMiddleware`). First added = outermost. Both are *mutating and
short-circuiting*: rewrite what goes in, transform what comes out, or return without calling
`next`.

<!-- docsnip: setup model agent.Model; journal *agent.Journal; tools []agent.Tool; import oteltrace "go.opentelemetry.io/otel/trace"; tracer oteltrace.Tracer -->
```go
var cost middleware.CostMeter
a, err := agent.New(model, journal,
	agent.WithTools(tools...),
	agent.WithTokenBudget(100_000), // per run, rebuilt from the journal on resume
	agent.WithMiddleware(
		middleware.Retry(3, middleware.WithBackoff(200*time.Millisecond, 10*time.Second)),
		middleware.Cost(&cost, middleware.Rates{InputPer1M: 3, OutputPer1M: 15}),
	),
	agent.WithToolMiddleware(middleware.ToolLog(log.Printf), middleware.ToolCache(), middleware.ToolRetry(3)),
)
if err != nil {
	panic(err)
}

// opt-in OTel gen_ai.* spans (provider and model from agent.ModelInfoOf); the core has no OTel dependency:
a, err = a.With(
	agent.WithMiddleware(trace.Model(tracer)),
	agent.WithToolMiddleware(trace.Tool(tracer)), // execute_tool span per call; nests across the sub-agent boundary
)
// ... after the run: cost.Snapshot() (answer and spend, in tokens and USD)
```

`Retry` does exponential backoff with jitter and honors a `Retry-After` on a provider 429 (the
adapter returns a typed `*agent.RateLimited`); `Cost` accumulates USD from token usage (incl.
cache-read/write) into a `CostMeter` you read after the run.

A model middleware is a `func(next agent.ModelHandler) agent.ModelHandler`. It receives an
`agent.ModelCall` (the `Request`, the target `Model`, `RunID` and `Turn`) and returns an
`agent.ModelResponse` (the `Message`, its `Usage`, and the `Finish` reason). The call is a value:
change a copy and pass it on (`c := call; c.Model = backup` is all `Hedge` does). Work that belongs
to each request actually sent, rather than to the call, goes in a hook, which runs once around every
request of every retried attempt and hedged target below it:

<!-- docsnip: setup import "log" -->
```go
// Log every request a turn sends, with its number within the turn.
func LogRequests(next agent.ModelHandler) agent.ModelHandler {
	return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
		return next(ctx, call.AddHook(agent.ModelCallHook{
			After: func(_ context.Context, c agent.ModelCall, a agent.ModelAttempt) {
				log.Printf("run %s turn %d request %d: %d tokens, err %v", c.RunID, c.Turn, c.Attempt(), a.Response.Usage.TotalTokens(), a.Err)
			},
		}))
	}
}
```

Hooks are append-only, and the run's own spend accounting is not a hook, so no middleware can hide a
request from `WithTokenBudget` or `Result.Spend`. Outside an agent, `agent.CallModel(ctx, model,
req, mw...)` sends one call through the same chain.

Because `trace.Tool` runs inside the loop, its span sits in the context handed to the tool, so
when a tool is itself a sub-agent, the sub-agent's run and its own spans nest as children. The
trace crosses the sub-agent boundary.

Tool middleware runs *inside* the durable step, so a short-circuit result (a `ToolCache` hit) is
journaled like any tool result, and so is a policy denial that wraps `agent.ErrToolNotCalled`; resume
replays it and never re-runs the middleware or the tool. A denial must wrap `agent.ErrToolNotCalled`:
one that does not leaves a side effect's outcome unknown, records nothing, and the run halts. `ToolRetry` and `ToolCache` act only on tools whose `Safety` allows it
(retry-safe, and `ReadOnly`, respectively), and the agent runs a tool that is not retry-safe at
most once per call whatever the middleware does. Write your own with the `agent.ToolMiddleware` signature:

<!-- docsnip: setup func authorized(context.Context, string) bool -->
```go
// Deny a tool by policy: the tool never executes; the model sees the error and reacts.
// A denial must wrap agent.ErrToolNotCalled: without it the agent cannot tell the tool did
// not run, so a side effect's outcome is unknown and the run halts.
func RequireTag(tag string) agent.ToolMiddleware {
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
			if !authorized(ctx, tag) {
				return nil, fmt.Errorf("tool %q denied: %w", call.Use.Name, agent.ErrToolNotCalled)
			}
			return next(ctx, call) // mutate call.Use.Args before, transform the result after
		}
	}
}
```

<p align="center"><img src="../assets/divider-wave-2.png" width="100%" alt=""></p>

## Modules

Bide is a multi-module repo: a dependency-light **core** (`github.com/bide-ai/bide`,
the loop, schema, middleware, model adapters, the `plan` flow builder, `audit`; its one dependency is `x/text`) plus one
module per heavy adapter (`mcptools`, `trace`, `store/sqlite`, `store/postgres`, `govern/redislog`,
`govern/sqlitelog`, `govern/postgreslog`, `codec/gcf`) and the `govern` module, which carries gsm and stays v0.x until gsm
is stable. Import an adapter and you pull its dependency tree; import only the core
and you don't. A core-only consumer's external-module surface is 1, not 54. See
[docs/reference/module-structure.md](reference/module-structure.md).

## Architecture

Hexagonal: the core defines the ports (`Model`, `Store`, `Tool`,
`Middleware`); adapters plug in at the edges. Dependencies point inward; the core imports
no adapter and no infrastructure, guarded by `architecture_test.go`.

```
agent (root)     durable loop · Journal/Store · Message/Part · Tool/Safety · middleware types · RenderMermaid
plan             plan flows: the optional builder + declarative config; lowers to the loop (Topology · Conform)
model/anthropic  native Claude (thinking + signatures)
model/openai     any OpenAI-compatible endpoint
model/gemini     native Gemini (generativelanguage / Vertex via WithBaseURL)
schema           reflect Go types → inline JSON Schema + OpenAIStrict
middleware       Retry, RateLimit, Cost, Hedge
trace            opt-in OTel gen_ai.* spans
store/sqlite     on-disk durable resume (single binary, no cluster)
store/postgres   HA durable resume (any node resumes any run)
govern           Tier-2: federated governed state + quorum for agents that must agree (gsm-backed; own module)
```

## Federated governance (Tier-2)

The durable core keeps one agent's work crash-safe. The `govern` tier is for many independently
run agents that have to agree, across process, team, or organizational
boundaries, with no coordinator beyond the shared, totally ordered event log, and no single writer. It gives two verifiable forms of
agreement; in both, a party checks the outcome from public artifacts without trusting anyone else's
agent.

**Agreement on shared state (convergence).** Describe the shared state as a registry (variables +
invariants + events); gsm checks *at build time* that every interleaving of agent actions reaches
the same valid state, or refuses to build and shows you a counterexample (see
[known limitations](KNOWN-LIMITATIONS.md#governed-state-gsm) for its scope). Runtime is O(1) table
lookups; state is event-sourced and crash-recoverable. It scales from a single shared registry up
through **federations** (cross-boundary constraints: trees, multi-source DAGs with resolvers,
monotone cyclic *meshes*), composes via `Embed`, and can synthesize the compensation for
you (declare the rules, get a convergent governor, or a proof that none exists). Agents plug in
through `FederatedEventTool`, so an LLM tool call becomes a governed event.

**Agreement on a decision (quorum).** k-of-n named voters (each a model, provider, or principal)
cast a normalized decision; every vote is a journaled, at-most-once step that records who voted how,
and the k-of-n gate is a gsm invariant over the vote count, so "k agreed" is machine-checked over
every possible tally. `bide audit verify-quorum` re-checks the tally and every vote from public
artifacts, reproducing the plurality rule without trusting the producer. The claim is precise: a
quorum proves *that k voters agreed* and lowers single-model risk; it does not certify the decision
is correct (correlated errors are not independence), and only normalized decisions can be quorumed,
not free-form prose.

<!-- docsnip: setup ctx context.Context; machine *gsm.Machine; import "github.com/blackwell-systems/gsm"; log govern.EventLog -->
```go
gov, _ := govern.NewPersistent(ctx, machine, log, "order-42", machine.NewState())
tool := govern.EventTool(gov, govern.EventToolConfig{Name: "pay", Description: "mark the order paid", Event: "pay"})
// hand `tool` to the agent: concurrent agents sharing `gov` converge, durably.
```

> Full guide, capability ladder, and the runnable demos (`examples/govern/mesh`, `examples/govern/compose`,
> `examples/govern/quorum`) in **[the governance guide](guides/governance.md)**.

<p align="center"><img src="../assets/divider-line.png" width="100%" alt=""></p>
