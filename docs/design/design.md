# Bide: Design Doc

## Thesis

Every existing Go agent framework (Eino, Google ADK Go, Genkit Go, LangChainGo) ported
Python's mental model into Go and inherited Python's assumptions. This kit does the opposite:
**model agents in Go's native idioms, not translated into them.**

Tagline candidate: *"Agents modeled in Go, not translated into it."*

The agent loop is ~40 lines. We don't hide it behind magic; we harden it. The value is in
the production layer Go is genuinely best at: typed tools, streaming done right, durable
resume, middleware, telemetry, and deep use of the concurrency model.

## The wedge (pick ONE identity, don't out-feature ByteDance)

Fuse two things the incumbents structurally can't retrofit:

1. **Typed tools**: compile-time tool contracts (change the struct, the handler won't compile).
2. **Durable resume**: crash-resilient agents that resume mid-tool-call, single binary + SQLite/Postgres, NO Temporal cluster.

Pitch: **"Type-safe agents that survive a crash. Single binary. No cluster."**

### Accuracy check vs the real threat (Eino): research-corrected

- **Typed tools alone is NOT a moat vs Eino.** Eino already auto-generates schema from struct
  tags (`InferTool[T,D]`). Our typed-tools story wins decisively vs LangChainGo (`Call(ctx,
  string) (string, error)`, no schema) and the small frameworks, but **not** vs Eino.
- **Where we actually beat Eino** (verified in the research passes below):
  - **No string-keyed graph DSL.** Eino wires nodes with string keys; type/wiring errors land
    at `Compile()` (runtime), some at execution. We use plain Go funcs + generics + channels →
    real `go build` errors. "Compile-time" for us means the Go compiler, not their `Compile()`.
  - **Durability batteries-included.** Eino ships checkpoint *primitives* but **zero persistent
    store**: every official example is in-memory, no crash survival. This is our sharpest,
    most defensible wedge. Durable resume by default, single binary + SQLite/Postgres.
  - **Coherent multi-agent.** Eino's handoff path is self-stamped "NOT RECOMMENDED" and can
    fail silently. One clean CSP model, no silent wiring failures.
  - **Neutral & maintained.** Eino is <10 ByteDance engineers, Chinese-first docs, pre-1.0
    churn (v0.10.0-alpha.35), a cluster of open races/SIGSEGVs, Western-stack integration gaps.
- **Net:** lead the pitch with **durable resume** (the true gap), typed tools + no-DSL +
  neutrality as the supporting case. Don't headline "type-safe tools" as if unique; Eino will
  rebut it.

## Strategic reality (eyes open)

The agent-framework lane is crowded and vendor-backed (two Google teams + ByteDance). We are
building here anyway because it's what we want to build. Winning ≠ more integrations than Eino;
winning = a violently opinionated take on the two wedge features + Go-idiomatic ergonomics.
(Go evals were the other empty lane. We now ship a statistical `eval` package, but as a
supporting layer subordinate to the provable governance/audit moats, not as the product wedge:
it measures the model statistically and says so; see [testing](../testing/testing.md) for the boundary.)

---

## Complaints about existing solutions (what people gripe about)

- **You hand-roll everything around the loop**: logging, tracing, retries, rate-limiting,
  cost tracking, config. The loop is easy; the scaffolding is all DIY.
- **No typed tools.** Nobody auto-generates a JSON schema from a typed Go function.
  LangChainGo's tool interface is literally `Call(ctx, string) (string, error)`: no schema,
  no typed args.
- **Streaming leaks.** SSE minefield exposed to the user: tool-call args accumulating by index,
  UTF-8 splits across chunks, providers framing differently, streams ending without `[DONE]`.
- **Messy-JSON pain.** Strict `encoding/json` rejects malformed LLM JSON; everyone writes
  their own repair helpers.
- **LangChainGo is rotting**: stuck at 0.1.x, broken Gemini streaming, `temperature` omitempty
  bug forcing 400s on newer Claude, no middleware/retry/OTel.
- **Eino is verbose + graph-DSL-heavy**: config structs everywhere, you learn a graph
  abstraction, Chinese-first docs, pre-1.0 churn.
- **Genkit/ADK are cloud-flavored**: Vertex/Firebase pull, thin vector stores, agent packages
  still experimental.

> DONE: 4 research passes complete (LangChainGo + small frameworks, Eino, Genkit + ADK,
> cross-cutting gaps). The cited teardowns were working notes and are not kept in this repo.
>
> **Convergence conclusion:** durable execution is punted by ALL THREE Tier-1 frameworks: Eino
> ships zero persistent store; Genkit *removed* durable flows ("not durable execution"); ADK gives
> "no automatic recovery" + at-least-once tools. Side-effect-safe, single-binary, crash-recoverable
> mid-tool-call resume is the single most universally-unowned gap. **This is our headline.**
>
> **Caveats (don't build positioning on stale claims):** (1) typed tools is table stakes,
> Eino/Genkit/ADK all have schema-from-struct; our differentiation is durability + no-DSL +
> neutrality + correct ops, not "typed tools." (2) Genkit is GA with a great Dev UI + native
> Anthropic: attack durability/observability/OSS-RAG, not maturity. (3) ADK's runtime does NOT
> bill idle: attack lock-in + boilerplate, not idle cost.
>
> **Extra confirmed wedges:** provider-neutral message type (vs Google's `genai.Content` threaded
> through ADK); a clean low-altitude "just generate" API that scales up to agents (ADK's ~50-line
> tax); correct/non-invasive OTel (Genkit inverts span status #6356, hijacks global provider
> #3709); Node-free native-Go trace UI (Genkit's needs npm).

## Gaps (unfilled, ownable)

1. Durable/resumable agents as a library (no cluster): **wide open**.
2. Compile-time-typed tool contracts: the Go superpower nobody exploits well.
3. Native OTel GenAI semconv: weak everywhere.
4. `net/http`-style composable middleware: must hand-roll; no one ships it.
5. Local dev UI / trace viewer: only Genkit has one, and it's its most-praised feature. Steal it.
6. HITL pause/resume: emerging need, poorly served.

---

## Orchestration decision (LOCKED): Option B, plain Go + derived graph

**Authoring is plain Go control flow (functions, `if`, `for`, goroutines/errgroup) + explicitly
named durable steps. NO graph-authoring DSL. The graph is a DERIVED OUTPUT** (rendered from the
trace/named-steps for the Dev UI + `RenderGraph(runID)`), never something the user writes.
Optional thin graph-builder ("Option C") only if user research shows buyers won't adopt without a
hand-authored graph; build speculatively = no.

Why B, tested against Eino's own strongest case (the research passes above + the two Eino
orchestration docs):
- **Even Eino treats the agent as a LOOP, not a graph.** Their "Graph or Agent" page: agent =
  autonomous/stateful/process-matters (a ReAct loop); graph = deterministic/stateless/final-result
  (a tool the loop calls). Their recommended integration is "encapsulate Graph as Agent's Tool."
  That's our architecture. We only drop the DSL for the deterministic-pipeline part, which their
  own chapter-8 "graph tool" collapses to a plain Go function + `errgroup` in our model.
- **Their headline "static type safety via type alignment" is an anti-Python argument that cuts
  against them.** `b := stepB(stepA(x))` IS type alignment, enforced by the Go compiler at
  `go build`, not a runtime `Compile()`. Eino erased the call structure into string-keyed edges,
  LOST compile-time safety, and built a graph to recover it at runtime. Plain Go wins their own goal.
- **Their "process clarity / don't guess how it runs" is exactly what B delivers**, as a derived
  view, without the authoring tax. They make you author a graph to read one; we render it.
- **A graph is a workaround for having erased your call structure into data.** Keep the call
  structure (plain Go) → get type-safety + control flow free at compile time → derive the graph
  for viewing. Strictly better; Eino's own docs are the evidence.
- **`FieldMapping` (their chapter 8) manufactures the data-plumbing problem it then solves**:
  routing data between non-adjacent *nodes*. In plain Go those are just variables in lexical
  scope; the problem doesn't exist.

The one legitimate graph-adjacent win we DO owe (a streaming-layer problem, not a graph problem):
value↔stream bridging + fan-in merge. See must-build below.

## Go-idiomatic design principles

**1. The agent loop IS a `select`, not a hidden while-loop.**
```go
for {
    select {
    case ev := <-modelEvents:   // next token / tool call
    case res := <-toolResults:  // a tool finished
    case <-approval:            // human-in-the-loop
    case <-ctx.Done():          // cancel / deadline
    }
}
```
HITL, cancellation, streaming, timeouts all fall out of ONE idiom. Design keystone.

**2. Multi-agent = CSP, not a graph.** "Share memory by communicating." Orchestrator sends
tasks on channels, agents reply on channels. No DAG builder. Replaces Eino's DSL with plain Go.

**3. `context.Context` is the spine**: durability + cancellation + budget carrier. A run's
context holds the whole-agent deadline, cancellation that propagates to sub-agents AND aborts
in-flight LLM HTTP, and request-scoped values (trace ID, token budget). `context.AfterFunc`
for teardown/tool-rollback on cancel.

**4. `errgroup` + `SetLimit` for fan-out.** Bounded parallel tool calls / sub-agent fan-out
with first-error cancellation. No custom scheduler.

**5. Workflows are Go pipelines, not graphs.** source → stage → stage → sink, goroutines wired
by channels, context threaded through. Pike's pipeline pattern = the agent-chain pattern.

**6. Structured concurrency: we guarantee no goroutine leaks.** A run doesn't return until every
sub-agent/tool goroutine finishes or is cancelled. errgroup gives us this.

**7. `select`-based races for free:** hedged requests (first of N replicas wins), speculative
fallback (primary OR cache OR cheaper model), per-tool timeouts. Trivial with `select`.

**8. Backpressure via bounded channels.** Flow control from channel capacity: no rate-limiter dep.

**9. Everything testable with `testing/synctest` (Go 1.25)**: deterministic fake-clock testing
of retry/timeout/coordination. Ship as a user-facing feature.

## Cutting-edge Go we build on (FLOOR: Go 1.27)

Decision: **require Go 1.27** (shipped ~Aug 2026). Verified against the official go1.27 release
notes. It uniquely strengthens this project's pillars, and requiring latest Go courts the
early-adopter crowd (enterprises pin old Go and aren't our first users).

- **`encoding/json/v2`, now DEFAULT-ON in 1.27** (opt-out `GOEXPERIMENT=nojsonv2`). Resolves the
  earlier "don't expose until un-gated" caveat. Faster Unmarshal, stricter (rejects invalid UTF-8
  + duplicate keys). Ships **`encoding/json/jsontext`** with a token/value-level `Decoder`: the
  exact primitive for **incrementally parsing streamed tool-call JSON deltas** (streaming pillar)
  and for lenient messy-LLM-JSON handling. Caveat: still `GOEXPERIMENT`-flagged, so isolate it
  behind our own interface in case the API shifts.
- **`runtime/pprof` goroutine-leak profile (1.27)**: `/debug/pprof/goroutineleak`, GC-based
  detection of goroutines blocked on unreachable primitives. Directly serves the
  structured-concurrency / no-leaks pillar: use in our own tests AND expose as a user feature.
- **`testing/synctest` + `synctest.Sleep()` + `httptest.NewTestServer` in-memory network (1.27)**:
  deterministically test the whole durable loop INCLUDING model HTTP calls, fake clock + fake
  network. Ship as a feature: "deterministically test your agent's retry/timeout/**resume**."
- **Range-over-func iterators (1.23)**: `for ev := range agent.Stream(ctx, msg)`. Streaming keystone.
  DONE: `Agent.Stream`/`StreamSaga` return an `AgentStream`; `for ev := range stream.Events()` yields
  the semantic lifecycle (token deltas, turn boundaries, tool start/finish), `Final()` the answer.
  Shares one loop with `Run` (`Run` == `Stream(...).Final()`); token deltas forward below middleware.
- **Generic methods (1.27) + generic type aliases (1.24)**: clean typed-tool registry ergonomics.
- Minor but handy: `bytes.CutLast`/`strings.CutLast`; `runtime/secret` (secret-mode goroutines,
  relevant to privacy-gating API keys / prompt content in the observability pillar).

> DONE: Go 1.27 release notes verified (go.dev/doc/go1.27). Floor set to 1.27.

## Typed tools: the ergonomic target

```go
type WeatherArgs struct {
    City  string `json:"city"  desc:"city name"`
    Units string `json:"units" desc:"celsius|fahrenheit"`
}
// schema auto-derived from the struct; handler gets a typed struct, not map[string]any.
a.Tool("get_weather", "current weather", func(ctx context.Context, in WeatherArgs) (Weather, error) {
    ...
})
```
Gotchas to handle: `invopop/jsonschema` needs `ExpandedStruct: true`; some LLM APIs reject
`$ref` output, so flatten it.

## Anti-patterns (idiomatic = refusing to do these)

- No graph DSL. No `map[string]any` tool args. Don't invent cancellation (use `context`).
- Don't leak goroutines. Don't lowest-common-denominator the model interface into leaky
  abstraction (LangChainGo's sin). Don't hide concurrency: EXPOSE the Go primitives.

---

## Product surface (research-validated) & v1 scope

The cross-cutting research converged all eight gaps into one coherent product. Refined tagline:

> **"The durability, observability, typed-tool safety, and middleware of a heavyweight framework,
> delivered as a single static binary with idiomatic Go control flow, no DSL, and a zero-config
> local trace UI."**

Eight gaps, all currently unowned. **Don't build all eight at once.** Sequence:

- **v1 (the moat):** durable resume (gap 2) + orchestration-as-plain-Go (gap 6) + typed tools
  with the provider-aware schema emitter (gap 1). Lead the pitch with durable resume.
- **v1.x:** unified streaming event core (gap 3: model-layer `Event` + caller-facing
  `Agent.Stream`, both DONE) + semantic middleware chain (gap 4, DONE).
- **v2:** native OTel `gen_ai.*` (gap 5) + decoupled local trace UI (gap 7).
- **Explicit non-goal:** RAG / embeddings / math (gap 8). Ship a clean gRPC/HTTP boundary to a
  Python sidecar instead. Do NOT try to replace NumPy/Instructor.

## Concrete design decisions (locked by research)

**Durable resume: two-layer side-effect safety** (the hard part everyone punts). On resume:
- Journal every tool call as `completed` (result stored) vs `unknown-outcome` (started, crash
  before result persisted).
- **Hard-block re-execution of WRITE tools** with unknown outcome (surface to HITL / require
  idempotency key). **Never block READ tools**: just re-run them.
- `context`-native, no deterministic-replay rules. Embedded SQLite default, Postgres for prod.
  Between-step AND mid-tool-call granularity (Eino only does between-node).

**Typed tools: provider-aware schema emitter.** Reflect the Go struct ONCE, emit per-dialect:
- OpenAI strict: `additionalProperties:false`, all fields in `required`, inline (no `$ref`).
- Gemini / Anthropic variants as needed. Recursion-safe. Optionality from Go pointer semantics;
  enums from typed Go constants. Shared field-walk so schema and unmarshal provably can't drift.
- **Dynamic path for MCP tools** (runtime schema → no Go struct): a `map`-backed typed-ish tool
  so runtime tools aren't second-class.

**Streaming: unified event core.** Normalize all 4 providers (OpenAI SSE / Anthropic typed /
Gemini NDJSON / Bedrock binary) to:
```go
type Event interface{ isEvent() }
// Text | Reasoning | ToolCallStart | ToolCallDelta | Finish | Error
```
Index-keyed tool-call accumulation gated by `json.Valid`; **UTF-8-safe byte framing** (buffer raw
bytes, frame on `\n`, carry partial runes across chunks), the universally-missing piece;
`bufio` 64KB-limit-proof reader; `context`-first lifecycle (no mandatory-`Close` footgun).

**Middleware: `func(Handler) Handler` at the SEMANTIC layer** (handlers see messages, tool
calls, token usage, not bytes). Batteries: retry honoring `Retry-After`, circuit breaker,
RPM+TPM limits, cache, token→dollar cost, **budget caps that abort at the limit**. Disable the
underlying SDK's own retry so budgets/retries compose predictably.

## Open questions / next steps

- [x] Fold in the 4 flaw-research agents' findings (summarized above).
- [ ] Confirm Go 1.26/1.27 relevant features.
- [ ] Prototype the `select`-driven loop + typed-tool registry to feel the ergonomics.
- [ ] Decide the durable-store interface (SQLite first? Postgres? pluggable `Store`).
- [ ] Name the project (deferred until it's real).
- [ ] Provider adapters: Anthropic + OpenAI + Ollama minimal `Model` interface.
- [x] json.Valid gate on concatenated tool-call args (`msgBuilder.finalize`, v1): DONE.
- [ ] At the `model/anthropic` + streaming build: adopt `encoding/json/v2` + `jsontext` behind a
      one-file decode boundary (`internal/enc`); swap `finalize`'s `json.Valid` to `jsontext`; add
      a `testing/synctest` stream-lifecycle test. NOTE: on Go 1.27 we already get the v2 *engine*
      via the stdlib v1 import; this is for v2 *semantics* + `jsontext`, still GOEXPERIMENT-gated,
      hence the one-file isolation.

### From the Eino design study (things to build / match)
- [ ] Add `ToolChoice` (auto/required/none) to `Request`: Eino has it, we don't.
- [ ] Extend `Part` to multimodal: `Image`/`Audio`/`Video`/`File` (Eino covers these).
- [x] Match Eino's HITL: `StatefulInterrupt` + `ResumeWithData` equivalent. DONE: durable typed
      `Interrupt[T]`/`Resume` (pause anywhere, resume with a typed value), plus declarative
      `RequiresApproval`/`Approve`. Both journaled (survive a crash), on the at-most-once substrate.
- [ ] **MUST-BUILD (stream layer, not graph):** value↔stream bridging (drain a stream to a value
      when a consumer wants a value; box a value into a one-chunk stream when it wants a stream) +
      **named fan-in merge** (Eino's `MergeNamedStreamReaders` equivalent). This is the ONE
      legitimate graph-adjacent win from Eino's design-principles doc; we solve it at the
      `Stream`/`stream` layer so it never justifies a graph. `Stream.Message()` already does the
      drain direction; owe the box + merge directions.
- [x] Deliver checkpointing + stream stitching + observability WITHOUT a graph: the core bet
      that justifies deleting Eino's compose/. DONE: durable resume (checkpointing), `Agent.Stream`
      (stitching), `trace.*` (observability), all plain Go, no graph. The bet held; the crash-safety
      side is now DST-proven and the audit trail is RFC 6962. (Still owed from the stream layer: the
      value→stream *box* + named fan-in *merge* directions.)
- [ ] `Durable.RenderGraph(runID)` + Dev-UI topology view: the "graph as derived output" (Option B)
      mechanism. Render from trace/named-steps; never authored.
- [ ] Keep `Reasoning.Signature`: confirmed win vs Eino (their reasoning parts drop it).

### gsm relationship (decided)
- Durability substrate = our `Durable`/SQLite (+ optional DBOS backend). gsm is NOT the substrate
  (no persistence, unbounded agent state, sequential-not-out-of-order).
- gsm = optional GUARDRAIL layer: model the finite business domain as a governed state machine,
  tool calls as events, build-time-proven invariant safety + multi-agent convergence. Distinct
  later feature ("agent that provably can't violate your business invariants"), not the foundation.
