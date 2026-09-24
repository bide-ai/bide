# Competitive Intelligence

Research on where existing Go agent frameworks fall short, to sharpen positioning.
Metrics pulled 2026-09-23. Four research passes: LangChainGo+small (done), Eino (pending),
Genkit+ADK (pending), cross-cutting design gaps (pending).

---

## LangChainGo (github.com/tmc/langchaingo) — the fading incumbent

**Maintenance: dying.** 9,695 stars, created 2023-02, but ~1 commit in the trailing 90 days;
last real commit 2026-01-11. Latest release v0.1.14 (2025-10-20). **173 open PRs, 243 open
issues** (416 total). Oldest open PR #145 from 2023-06-25 (3+ years unmerged). README actively
**seeks maintainers** ("momentum for moving to a more community effort"). Effectively one
maintainer (tmc). Never left v0.1.x in 3.5 years — no stable API, no 1.0 (contrast Python
LangChain, which shipped 1.0 GA Oct 2025 with an 18-month no-break promise).

**Specific rot (all confirmed):**
- **Dead Google SDK dep** — pins `google/generative-ai-go v0.15.1`, which EOL'd 2025-11-30.
- **Broken Gemini streaming** — `invalid character ']'` across current Gemini models;
  `DefaultOptions()` hardcodes a retired `gemini-2.0-flash`, so a fresh client fails on first
  call (the quickstart 404).
- **`temperature` omitempty bug → 400s on Claude 4.7+** — the `Temperature` field lacks
  `omitempty`, so `"temperature":0` is sent every request; newer Claude rejects it. Traced via
  **dapr/components-contrib #4533 / #4535 / #4587**. When Dapr checked `main` to fix it, they
  found `tool_choice` support had been *deleted*. **Dapr ripped out langchaingo entirely and
  now calls `anthropic-sdk-go` directly** — a flagship user fleeing.
- **No middleware / interception** — `callbacks/callbacks.go` Handler methods all return
  nothing; you can observe a call, not modify/intercept it. No place for retry/caching/rewrite.
- **OTel is transitive-only** (indirect deps); and enabling callbacks is what *triggers* the
  Gemini streaming break.

**Tool interface — string in, string out.** Core interface is literally:
```go
type Tool interface {
    Name() string
    Description() string
    Call(ctx context.Context, input string) (string, error)
}
```
No typed args, no schema in the contract. Schema is an optional `ToolWithSchema` bolt-on found
by runtime type assertion. This is exactly the gap our typed-tools wedge fills.

**Leaky Python ports:** imports a Jinja2 engine (`gonja`) + `sprig` for prompt templating;
output parser hard-requires Gemini-style code fences and breaks on OpenAI; legacy `logrus`.
62 modules / 38 MB binary vs Genkit Go's 41 / 28.7 MB.

## Smaller frameworks — all pre-1.0, mostly solo/stalled

| Framework | Stars | Last push | Release | Maintainer | State |
|---|---|---|---|---|---|
| agent-sdk-go (Ingenimax) | ~634 | 2026-09-21 active | v0.2.75 | ~2, company | active but v0.2.x churn, breaking weekly |
| agno-Go (rexleimo) | ~66 | 2026-08-11 | v1.2.9 | **1 solo** | stalled, tiny |
| openai-agents-go (nlpodyssey) | ~273 | 2026-03-26 | **v0.1.0 only** | ~10, lost momentum | drifting Swarm-lineage port |
| AgenticGoKit | ~179 | 2026-08-01 | v0.5.9 | **~1 solo** | self-declared beta, breaking v1 coming |
| SwarmGo (prathyushnallamothu) | ~361 | **2025-04-14 dormant** | v1.1.0 | **1 solo** | ports a design OpenAI deprecated |

Notable specifics:
- **agent-sdk-go** — the most mature independent one; multi-LLM, MCP, A2A, guardrails, tracing,
  eval. But v0.2.x after ~18mo, breaking releases multiple times/week, shipped non-functional
  features (input guardrails "had no effect" with memory), open bugs on token-usage aggregation
  (#327 breaks cost tracking), unimplemented documented APIs (#333/#334), `max_tokens` not
  configurable (#347). Requires Go 1.26+. Single-company survival risk.
- **SwarmGo & openai-agents-go** both descend from **OpenAI Swarm, which OpenAI itself
  deprecated** ("experimental, educational... migrate to the Agents SDK"). SwarmGo never
  discloses this, is dormant, and its central handoff primitive is reported broken (#30).

## Strategic takeaway (validates our wedge)

The independent frameworks are not the threat — they're pre-1.0 and mostly solo/stalled. The
real threat is vendor Tier 1: **Eino** (ByteDance ~13k, releasing every few days), **Google ADK
Go** (~8k), **Microsoft Agent Framework Go** (preview 2026), **Genkit Go** (GA, working trace UI).

The opening is the **underserved middle**: provider-agnostic + genuinely maintained + stable API
+ **typed/schema'd tools** + **real middleware (intercept, not just observe)** + **durable
sessions/state** + **first-class tracing**. That is precisely our typed-tools + durable-resume
wedge. The LangChainGo autopsy confirms each of these is a real, load-bearing gap — not theory.

Key sources: GitHub API (tmc/langchaingo commits/pulls/releases/contents, 2026-09-23);
dapr/components-contrib #4533/#4535/#4587; google/generative-ai-go (EOL); xavidop.me LangChainGo
vs Genkit Go (2026-09-11) and Top Go GenAI Frameworks 2026 (2026-09-21); openai/swarm deprecation.

---

## Eino (ByteDance) — the real Tier-1 threat

13,145 stars, most-starred Go LLM framework, releasing every few days, ByteDance production
pedigree (Doubao, TikTok). Adoption is ByteDance + Chinese enterprise; no independent Western
case studies surfaced. **This is the framework to position against carefully — overclaiming
will get rebutted.**

**IMPORTANT correction to our thesis:** Eino **already has auto schema generation** —
`utils.InferTool[T,D]` reflects Go struct `jsonschema` tags (nested/pointers/slices),
Pydantic-comparable. So "nobody does typed tools" is FALSE against Eino. Our typed-tools wedge
is strongest vs LangChainGo + the small frameworks; **vs Eino our differentiation is elsewhere**
(see below). Don't claim schema-from-struct as unique.

**Defensible wedges vs Eino (verifiable, quotable):**

1. **"Type-safe" is qualified.** The flagship `Graph` API wires nodes with **string keys** and
   `error`-returning methods; only `NewGraph[I,O]`/`Compile` carry generics. Mistyped keys,
   missing edges, cycles, adjacent-node type mismatches surface at **`Compile()` (a runtime
   call)** — not `go build`. Some checks defer to execution (`runtime type check failed:
   expected string, got int`). Eino's own docs: the Graph API is "a bit verbose" and static
   typing "adds cognitive burden." → **Our edge: plain Go funcs + generics + channels, real
   `go build` errors, no string-keyed graph DSL.**
2. **Durability is DIY.** Checkpoint/interrupt/resume primitives exist (pluggable
   `CheckPointStore{Get;Set}`, cross-process resume is real at the API level — *don't say "no
   durability"*), **but zero persistent store ships.** Every official example uses
   `NewInMemoryStore()` → no crash survival; no crash detection, no auto-resume, no dedup guard;
   between-node granularity only; subsystem rewritten as recently as v0.7. Contrast LangGraph's
   batteries-included Sqlite/Postgres savers. → **Our edge: durable resume batteries-included,
   single binary + SQLite/Postgres, crash-survival by default.**
3. **Multi-agent is incoherent and self-deprecated.** Legacy one-hop host router (primary
   `ChatModel` field already `// Deprecated`) + alpha `adk/` whose transfer/handoff path is
   stamped **"NOT RECOMMENDED"** by ByteDance itself. Supervisor `SetSubAgents` can fail
   **silently** (unchecked type assertion → `transfer_to_agent` never generated). → **Our edge:
   one coherent CSP model, no silent wiring failures.**
4. **Pre-1.0 churn + a concurrency-bug cluster** (ironic for a safety pitch). `v0.9.21` stable
   running *alongside* `v0.10.0-alpha.35`; breaking changes at minor bumps (v0.3/0.7/0.8/0.9);
   open races/SIGSEGVs (#1177, #1181, #1279, eino-ext #1005 shared-schema `sort.Strings` →
   intermittent 400s); Windows broken (#1281, #1285). v1 gated on "50+ business lines," undated.
5. **Single-vendor, China-centric.** <10 ByteDance engineers; docs sync from internal Feishu
   (English lags, measured broken-link issues); support threads Chinese-first; integrations lack
   the Western stack — no Pinecone/Weaviate/pgvector/Chroma, no Bedrock/Azure/Vertex-native/
   Mistral/Cohere/Groq. → **Our edge: neutral, English-first, provider-agnostic, community-owned.**

**Genuinely good (don't overclaim against):** real auto-schema (InferTool); graph-wide streaming
auto-concat/merge across nodes (ahead of LangChainGo, hard to DIY); compile-time node-I/O typing
at graph boundaries does catch real mistakes; single ~5MB binary; rich prebuilt patterns
(Supervisor/Sequential/Parallel/Loop/Plan-Execute-Replan/DeepAgent); Mermaid export; very active.

Sources: GitHub API + source 2026-09-23; eino issues #1304/#1177/#1181/#1279/#1254/#1263/#957/
#1281/#1285/#626/#938; eino-ext #1005/#1004/#997/#964; discussions #710/#323; cloudwego.io
release-notes/checkpoint/orchestration docs; eino-examples react.go (269 lines).

### Eino DESIGN study — how we exceed it (from their actual API, pkg.go.dev, 2026-09-23)

**Message model — we're strictly cleaner + fix a real bug.** Eino's `Message` is a 12-field fat
struct: `Content string` AND `MultiContent` (**deprecated**) AND `UserInputMultiContent` AND
`AssistantGenMultiContent` — four content reps, input/output split. Reasoning appears TWICE
(`ReasoningContent string` field + `MessageOutputReasoning` part) and **neither carries a
signature** → likely can't round-trip Anthropic extended-thinking. Our `Message{Role, Parts}`
with `Reasoning{Text, Signature}` is one rep and preserves the signature. Keep our design as-is.

**Orchestration — we delete their entire compose/ package.** Eino ships THREE primitives (Graph
/ Chain / Workflow) + 4-method `Runnable[I,O]` + string-keyed `AddEdge("a","b")` (errors at
`Compile()`, not `go build`) + `MapFields` field-mapping + `ProcessState[T]`. We replace all of
it with plain Go (sequential calls / `if` / `errgroup`); compile-time = the Go compiler. **The
bet we must honor:** deliver checkpointing + stream stitching + observability WITHOUT the graph
(via Durable layer + middleware), or the graph's value returns.

**Durability — moat holds, but they have more than the flaw-list implied.** Eino's
`StatefulInterrupt` + `ResumeWithData` is a real HITL flow (external-interrupt-via-context too) —
we must MATCH it (not built yet). But: (1) cooperative interrupt, NOT crash recovery — a node
must call `Interrupt()`; a real crash re-executes the whole node incl. side effects; (2)
checkpoint-between-nodes only; (3) `CheckPointStore` is just an interface — **no persistent store
ships**, every example in-memory; (4) zero side-effect safety. We ship real SQLite +
auto crash recovery + `Safety`/`ResumeHalt`. That's the differentiated core.

**Streaming — cleaner for agents, parity-or-behind on fan-in.** Eino's `StreamReader[T]` has a
mandatory-`Close()` footgun and `Copy(n)` shares the underlying pointer (their concurrent-map
crash). Our `iter.Seq2` + normalized typed `Event`s win on ergonomics. But their
`MergeNamedStreamReaders` fan-in is more mature — we need an equivalent for multi-agent.

**Schema — our provider-aware emitter wins.** Eino's `InferTool` (typed-from-struct) is parity,
but `ParamsOneOf` emits ONE JSON Schema 2020-12 — exactly what OpenAI strict / Gemini reject.

**Steal from Eino:** `ToolChoice` (auto/required/none) → add to our `Request`; multimodal parts
(image/audio/video/file) → extend `Part`; `StatefulInterrupt`+`ResumeWithData` HITL → match via
the select loop; Mermaid topology export → a `trace/` feature.

Source: pkg.go.dev/github.com/cloudwego/eino/{schema,compose} (2026-09-23).

## Genkit Go + Google ADK Go — both punt durability (the key convergence)

Both are real, GA, Google-built. **Do NOT attack their maturity or their typed tools** — both
have generics-based schema-from-struct, and Genkit is production-GA with native Anthropic. Attack
durability, provider-neutrality, boilerplate, observability correctness, and OSS-RAG.

### Genkit Go (`github.com/firebase/genkit/go`)
**Respect:** best-in-class zero-config local **Dev UI** (trace explorer + playground + tool
testing — their real moat); broad first-party models incl. a **native Anthropic plugin**; ~21-line
hello-world; `GenerateData[T]()` typed structured output.
**Flaws to hit:**
- **Durable flows were REMOVED** (#1158: "durable flows are gone"). The Sept-2026 Agents API is
  explicitly **"not durable execution: a restart, crash, or scale-in orphans every pending
  snapshot."** Only multi-instance store is Firestore (GCP coupling). → durability wedge.
- **Observability correctness bugs in "1.0":** span status **inverted vs OTLP** (#6356, successful
  spans show as errors); **hijacks the global OTel TracerProvider** (#3709, breaks existing
  instrumentation); Go treats `GENKIT_OTEL_ENABLE_LOGS` as **opt-out (exports by default)** — data-
  egress footgun.
- **OSS vector stores actively DECLINED:** Chroma #2741, Qdrant #4292, Redis #4290, sqlite #4291 all
  closed not-planned; Milvus PR closed unmerged; pgvector is "a code template, not a plugin";
  `localvec` is a dev-only JSON-file brute-force scan.
- Agents API is **preview-gated** (panics without `WithExperimental()`); sub-agent interrupts
  unsupported. Ops path needs a **GCP project ID + creds**; **Dev UI/CLI requires Node.js 20+/npm**
  even for pure-Go shops. Go trailed JS ~14 months to 1.0; requires Go 1.25+.

### ADK Go (`google.golang.org/adk/v2`)
**Respect:** OTel-native vendor-neutral observability; typed tools via generics; first-class
multi-agent graph engine (Sequential/Parallel/Loop/transfer + v2 DynamicNode); real offline
container path exists.
**Flaws to hit:**
- **No native Anthropic 10+ months post-launch** (#225, most-commented; #1097 "not a viable
  technology choice without them"); native OpenAI is EXPERIMENTAL "may be removed." **No LiteLLM
  for Go** (#1358: only Gemini + OpenAI wire formats). **Google's `genai.Content`/`Part` types are
  threaded through the whole stack** — every non-Google adapter writes bidirectional translation.
- **~50-line hello-world; no "just generate" entry point** (agent → runner → session service
  required). Structured output makes you hand-build a `*genai.Schema` tree, then the runner strips
  the parsed value before you see it. Middleware is fixed `Before/AfterModelCallback` (no `next()`
  onion). Line-count teardown (xavidop, Aug 2026): hello-world 49 vs Genkit 21; one-tool 66 vs 29.
- **Durability: "no automatic recovery"** — caller must manually detect interruptions and
  re-invoke; **tools run at-least-once, may run more than once on resume** (idempotency is your
  problem). GORM is a forced hard dep (#236); state buggy across backends (#1611/#1344/#324).
- **Production RAG/memory is Vertex-only** — Go ships only `InMemoryService` (naive keyword,
  ≤10 entries) + `memory/vertexai` (needs GCP). No first-party pgvector/Pinecone/Weaviate/Qdrant.
- Unported vs Python/Kotlin: **Evaluations** (#240), Skills (#540), session compaction (#298).
  v2.0 GA (June 2026) was a breaking module-path + graph-engine rewrite within ~8mo of launch.

**Honesty caveats (don't get caught out):** Genkit's "long alpha" is OVER (it's GA, has the great
Dev UI + native Anthropic). ADK's Agent Runtime does **NOT** bill idle-between-turns — attack
lock-in + boilerplate, not idle cost.

### ADK v2 DESIGN study (pkg.go.dev/google.golang.org/adk/v2/workflow, 2026-09-23)

The most sophisticated design we've studied. It VALIDATES our direction and forces one correction.

- **Google's v2 rewrite ADDED imperative-Go orchestration = our Option B.** `DynamicNode` +
  `RunNode[OUT](ctx, child, input)` lets you orchestrate with plain `if`/`for`/sequential calls,
  where each `RunNode` is a durably-managed step (per-node state, retry, HITL, resume). **`RunNode`
  ≈ our `agent.Step(ctx, dur, runID, name, fn)`.** The market leader independently arrived at
  "plain Go control flow + named durable steps." Strong convergent validation of B + the Durable
  reshape. We're the LEANER expression: no `Node`/`NodeConfig`/schema objects, just functions.
- **CORRECTION — don't reuse the string-key critique against ADK.** ADK v2 wiring is TYPED
  (`Edge{From: nodeA, To: nodeB}`, real Node refs) and `New()` validates the graph at BUILD time
  (unreachable/cycles/dup-names). That was Eino's flaw, not ADK's. Differentiate elsewhere.
- **Moat holds, confirmed against their API.** Rich resume (`RunState`/`NodeState`
  {Status,Attempt,ResumedInputs}, `Resume`, `ReconstructRunState`, `RerunOnResume *bool`), MORE
  than Eino — BUT docs say "no automatic recovery" + "tools may run more than once on resume";
  `Attempt`+`RetryConfig` = they retry side-effecting nodes with no write-safety. `RerunOnResume`
  is a coarse dev-set bool, not a derived idempotency contract. We're more principled
  (`Safety{ReadOnly/Idempotent/IdempotencyKey}` + the halt) and ship the "don't double-fire" they punt.
- **Differentiation vs ADK:** (1) far lighter (no Node/NodeConfig/schema/graph ceremony —
  `DynamicNode` still lives inside the node machinery); (2) provider-neutral (ADK threads
  `genai.Content`, no native Anthropic); (3) side-effect-safe resume; (4) single-binary/no-GCP
  (ADK prod memory is Vertex-locked).
- **C-hedge data point:** ADK ships BOTH declarative graph (Edges) AND imperative (DynamicNode).
  A mature vendor offers both → mildly supports our "B core, C optional later" plan.
- **Must-borrow:** `RequestInput{ResponseSchema}` HITL with response validation → match in our
  select-loop HITL; `RetryConfig` → middleware; `JoinNode`/`ParallelWorker` → errgroup + stream merge.

### The convergence that makes our moat
**Durable execution is punted by ALL THREE Tier-1 frameworks:** Eino ships zero persistent store;
Genkit *removed* durable flows and its Agents API is "not durable execution"; ADK gives "no
automatic recovery" + at-least-once tools. Nobody does side-effect-safe, single-binary, crash-
recoverable mid-tool-call resume. **That is the single biggest, most universally-unowned gap** —
and it's our headline. Secondary unowned wedges confirmed here: provider-neutral message type
(vs Google's `genai.Content`), a clean "just generate" low-altitude API, correct/non-invasive
OTel, first-class OSS RAG, and a Node-free native-Go trace UI.

Sources: genkit-ai/genkit issues #1158/#6356/#3709/#2741/#4292/#4290/#4291/#6045/#3554;
google/adk-go issues #225/#1097/#1358/#236/#1611/#240/#540; adk.dev/runtime/resume;
genkit.dev/docs/go/agents/background; xavidop.me Genkit-Go-vs-ADK-Go teardown (Aug 2026).

## Cross-cutting design gaps — the coherent whitespace

Every high-value gap is currently **unowned**, and they reinforce each other. Eight angles:

1. **Typed tools leak at the schema boundary.** Eino/Genkit/ADK/trpc *do* hand you a typed
   struct, so typed-tools is table stakes now — but the JSON Schema reflection emits
   (`$ref`/`$defs`, `additionalProperties`) gets **rejected by OpenAI strict mode + Gemini**, so
   everyone reinvents a buggy sanitizer. `ExpandedStruct:true` dangles nested refs;
   `DoNotReference:true` stack-overflows on recursion; OpenAI strict *requires*
   `additionalProperties:false` + all fields in `required`. **Hard ceiling:** Go can't synthesize
   a struct type from a runtime (MCP) schema (Genkit #3554) → need a dynamic path too.
   **Own it:** a *provider-aware* schema emitter — reflect once, emit OpenAI-strict / Gemini /
   Anthropic dialects; recursion-safe; shared field-walk so schema & unmarshal can't drift.
2. **Durability ≠ persistence — the single biggest gap.** Substrate exists (DBOS Transact Go,
   Restate, Temporal, Hatchet) but **nobody fused it into a single-binary SQLite-backed kit
   doing side-effect-safe mid-tool-call durable resume + durable HITL out of the box.** Go devs
   find Temporal "boilerplatey, doesn't fit Go's concurrency model." **Own it:** in-process,
   embedded-SQLite durable resume of the agent loop with **two-layer side-effect safety**
   (journal completed vs unknown-outcome tool calls; hard-block re-execution of *write* tools;
   never block reads), `context`-native, no deterministic-replay handcuffs.
3. **Streaming is the most under-abstracted surface (unowned in any language for Go's provider
   set).** 4 wire formats, index-based tool-call delta reassembly, UTF-8 rune splits,
   `bufio.Scanner` 64KB limit (openai-go #368), timeout leaks. Eino's `StreamReader` is best but
   has no JSON-validity/UTF-8 guard (crashes on truncated Claude `input_json_delta`, #894).
   **Own it:** normalized `{Text|Reasoning|ToolCallStart|ToolCallDelta|Finish|Error}` across all
   4 providers, index-keyed accumulation with a `json.Valid` gate, **UTF-8-safe byte framing**
   (buffer raw bytes, frame on `\n`, carry partial runes — the universally-missing piece).
4. **Middleware: three incompatible idioms, none idiomatic at the semantic layer.** Vendor SDKs
   chain at HTTP transport (see bytes, not tokens); Eino's 5-method callback is "verbose"; ADK
   Before/After. No "LiteLLM for Go." **Own it:** standalone `func(Handler) Handler` at the
   semantic layer (handlers see messages, tool calls, *token usage*): retry honoring
   `Retry-After`, circuit breaker, RPM+TPM limits, cache, **token→dollar cost accounting, budget
   caps that abort at the limit** — with the SDK's own retry disabled so budgets compose.
5. **Observability: spec exists, no idiomatic Go emitter.** OTel GenAI `gen_ai.*` is still
   "Development" status, churny, Python-first; no Go framework emits the full taxonomy natively.
   **Own it:** default-on `gen_ai.*` spans (invoke_agent/chat/execute_tool/embeddings), honor
   `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`, token→cost on spans.
6. **Multi-agent DSL debate = guarantees vs tax.** Graph camp (Eino/ADK 2.0) gets guarantees at
   a "verbose" cost + "debugging a graph, not a function"; goroutine camp wants the stdlib.
   **Own it:** *the durability/resumability/observability of a graph engine, expressed as
   ordinary idiomatic Go control flow — no DSL, no graph to debug.* Guarantees as a wrapper
   around plain typed functions.
7. **DX: Genkit's `genkit start` local Dev UI is the benchmark to steal** (zero-config trace
   viewer + per-component playground + HITL stepping). Eino Dev is an invasive IDE plugin; ADK
   Web needs Node. **Own it:** Genkit-grade local trace + playground, *decoupled & vendor-neutral*
   (OTLP-native, `go install`, `go:embed` UI, no Node/IDE plugin), 40-line-to-first-agent,
   English-first docs.
8. **Why switch from Python: sell the runtime layer, not "Go beats Python."** Python owns
   research/RAG/embeddings; Go owns runtime/serving (single binary, no GIL/async-coloring,
   compile-time schema safety, ~50k concurrent LLM-awaiting conns <1GB RAM). Accelerant: "LLMs
   write better Go." Blockers are all *ecosystem* (no Instructor/Outlines equiv; needs
   `jsonrepair` for messy JSON; RAG rudimentary). **Don't try to own RAG/embeddings/math** —
   make a clean gRPC/HTTP boundary to a Python sidecar first-class.

**Synthesized positioning line (sources support it):**
> *"The durability, observability, typed-tool safety, and middleware of a heavyweight framework —
> delivered as a single static binary with idiomatic Go control flow, no DSL, and a zero-config
> local trace UI."*

That one line answers Eino-verbose, ADK-agent-only, LangGraph-debug-the-graph, the no-framework
camp's minimalism, AND the durable-execution crowd's guarantees — simultaneously.

Sources: xavidop Top Go GenAI Frameworks 2026; Zep "Agents in Go Without a Framework"; Honchar
"Why LangGraph Overcomplicates"; DBOS Transact Go; Diagrid "checkpoints ≠ durable execution";
agent-native durable-resume; Anhaia buffer-pattern; llm-sse (TS, no Go equiv); invopop/jsonschema;
Genkit #3554; Greptime OTel GenAI; ADK #1634/#732; dspy-go interceptors; HN 47222270/44179889.

---

## jetify-com/ai (`go.jetify.com/ai`) — reviewed 2026, v0.5.1

**It's a Vercel-AI-SDK port, not an agent framework.** Despite the "agents" tagline, there
is **no `Agent` type, no agentic loop, no automatic tool execution** — the top-level API is
`GenerateText`/`StreamText` (one model call each); the caller must execute tool calls and
feed results back. So it competes with our *provider-adapter layer*, not our agent kit.
Company-backed (Jetify/Devbox), Apache-2.0, ~262★, effectively single-maintainer, pre-1.0
"Public Alpha" with unresolved-rename TODOs in the public API.

**Where it overlaps us (table stakes both meet — don't lead with these):** clean
`LanguageModel` interface + `iter.Seq` streaming; **typed content parts with reasoning +
Anthropic signature/redacted-thinking preservation** (matches our (c) for Anthropic); native
OpenAI (Responses API) + native Anthropic; OpenAI **strict-schema** (per-provider codec).
Streaming fragment accumulation is real (incl. parallel tool calls).

**Where WE are differentiated (all confirmed ABSENT in their code):** durable crash-resume +
journal/halt-on-unknown-write; SAGA compensation incl. across sub-agent trees; agent/sub-agent
orchestration at all; MCP (aspirational TODOs only); OTel; middleware; a genuine any-model
OpenAI-compatible adapter (they expose no Ollama/base-URL adapter; OpenRouter is `internal/`).

**Honest gaps of theirs to note (not attack unfairly):** manual JSON-schema tools (no
reflect-based auto-schema, no typed handler); **README overclaims** "production-ready…
retries, rate limiting, failover" that are NOT implemented in code (only an `IsRetryable`
classifier); no Gemini.

**Positioning takeaway:** typed content parts + OpenAI-strict are now table stakes (jetify has
them too). Lead with **durability / sagas / MCP / multi-agent / middleware / OTel** — the moat
jetify (and everyone else surveyed) doesn't touch. Source: github.com/jetify-com/ai @ 19847c8.
