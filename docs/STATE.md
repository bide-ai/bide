# Project state snapshot

Working codename **go-agents**. A durability-first, hexagonal Go agent devkit.
Module `github.com/dayna/go-agents`, **go 1.27**. All packages green, core race-clean and
dependency-pure (arch guard enforces it). Name still deferred.

## Repos, versions, remotes (read this first after a context reset)

Three repos under `/Users/dayna/code/`, all remotes on the `github-blackwell` SSH host:
- **go-agents** (this repo) — the agent devkit. Private: `blackwell-systems/go-agents`.
  No license yet (proprietary by default). Committed history, on `main`.
  **Multi-module** (see docs/MODULE-STRUCTURE.md): core module (agent/schema/middleware/model/
  govern/examples — deps gsm + x/sync only) plus one module per heavy adapter (mcp, trace,
  store/sqlite, store/postgres, govern/redislog, govern/sqlitelog). A core-only consumer's
  external-module surface dropped 54 → 2. Dev via a root `go.work`; adapter go.mods `replace` the
  core locally (pre-publish); CI iterates modules (`MODULES` in ci.yml). At v1.0/naming: rename
  paths + tag core + swap replaces for version pins.
- **gsm** — the convergence engine (founder's IP). Public: `blackwell-systems/gsm`.
  **Apache-2.0** (relicensed from MIT; copyright "Dayna Blackwell, Blackwell Systems").
  Released through **v0.6.0** (compensation synthesis that scales — backtracking + forward-checking —
  plus preference-guided (`SynthesizeWith`/`Prefer`), provably minimum-cost (`Optimal`), and an
  impossibility witness; CI green: lint + ubuntu/macos/windows × Go 1.22/1.23). go-agents depends on
  it via a normal versioned require (**pinned `gsm@v0.6.0`**). The old `replace ../gsm` is gone.
- **normalization-confluence** — the papers. Public: `blackwell-systems/normalization-confluence`.
  Two papers (single-registry `normalization_confluence_2026.tex`; federated
  `normalization_confluence_in_federated_registry_networks.tex`), CC-BY-4.0.
  **PDFs are NOT recompiled** since a large batch of theory additions (see Open).

## The pitch / moat
"Durable AI agents that survive a crash — without firing the same side effect twice."
Differentiators (all shipped + tested; confirmed absent in every competitor surveyed):
- **Durable, side-effect-safe resume** (journal + attempt-markers + halt-on-unknown-write).
- **Transactional SAGA agents** — reverse-order compensation across sub-agent trees.
- **Tier-2 convergent governance** (gsm-backed): concurrent agents on shared state converge
  provably; gsm proves it at build time and *refuses divergent models*.
- Typed content Parts w/ Anthropic reasoning-signature preservation; provider-aware schema;
  MCP; opt-in OTel; middleware; parallel tool calls; any-model.

## Agent capabilities — Tier-2 governance (CURRENT; the recent work)

The `govern` tier now supports the full federation ladder, all driven through
`FederatedGovernor` (durable, event-sourced) which just drives a `gsm.FedMachine` — so every
gsm federation capability reaches the agent tier for free on a version bump:
- **Single governor** — `Governor` / `PersistentGovernor` over one registry (event-sourced).
- **Tree federation** — hierarchical cross-agent constraints via directed morphisms
  (authority argument: source deterministically fixes a target's shared component).
- **Multi-source (DAG)** — a target with several sources declares a `Resolver` merge
  (priority / AND-OR / most-restrictive). Tested: `TestFederatedGovernor_MultiSource`
  (AND-gate access control). *Beyond the tree case.*
- **Monotone mesh (cycles)** — mutual/cyclic constraints between peers via
  `Federation.AllowMonotoneCycles()`; converges coordination-free by Kleene iteration.
  Tested: `TestFederatedGovernor_MonotoneMesh` (max-propagation ring). *The newest capability
  — hierarchical governance couldn't express mutual constraints before.*
- **Compositional subsystems** — `Federation.Embed`: verify a subsystem once, reuse as a unit.
- **Agent boundary** — `FederatedEventTool` turns an agent's tool call into a governed event.
- **Runnable demos**: `go run ./examples/mesh` (safety-interlock mesh: conflict reconciliation,
  order-independence, crash recovery, through `FederatedEventTool`) and
  `go run ./examples/compose` (a fulfillment subsystem verified once, reused in two systems).

## Package layout
- `agent` (root, gsm/adapter-free core): loop, Message/Part, Tool/Safety, Durable, Model
  (Stream-first), middleware types, sub-agents, saga, replay, RenderMermaid. Ports: Model,
  Durable, Tool, Middleware. `architecture_test.go` fails the build if core imports an adapter.
  **Caller-facing streaming** (`Agent.Stream`/`StreamSaga` → `AgentStream`): one loop shared with
  Run (Run is `Stream(...).Final()`); emits `TurnStarted`, live `ModelEvent` token deltas,
  `AssistantTurn` (with `Replayed` on resume), `ToolStarted`/`ToolCompleted`, `ApprovalRequired`,
  `Finished`. Token deltas forward below the middleware chain (still sees assembled messages) and
  only on a FRESH model call — replayed turns re-emit the journaled transcript instead.
- **Standardized errors** (`errors.go`): sentinel-based classification via `errors.Is` (no
  custom framework). Category sentinels — `ErrConfig`, `ErrModel`, `ErrTool`, `ErrStorage`,
  `ErrProtocol`, `ErrBudget`; condition sentinels wrap their category (`ErrUnknownTool` →
  `ErrTool`, etc.). Every error across core + adapters (model, mcp, store, govern, middleware)
  wraps a category via a second `%w`. Control-flow stays typed (`errors.As`): `*PendingApproval`,
  `*ResumeHalt`, `*SagaAborted`. (Chose sentinels over a `Kind` enum — `Model`/`Tool` collide
  with the interface type names, and `errors.Is` is the idiomatic fit.)
- **Richer HITL** (`hitl.go`): imperative `Interrupt[T](ctx, key, prompt)` inside a tool pauses
  the run durably and resumes with a TYPED value (generalizes approve/deny's bool). Returns
  `*Interrupted` out of Run; `Resume[T](ctx, d, runID, key, value)` records the answer (reuses
  `StepValue`, no new journal kind); re-run continues. Run-context (store+runID) is injected into
  the tool ctx (`withRunContext`). Must be a retry-safe tool (guarded with ErrConfig otherwise, so
  a non-retriable Interrupt fails loudly instead of ResumeHalt-ing). = trpc/LangGraph `interrupt()`,
  but on our durable at-most-once substrate. Existing declarative `PendingApproval`/`Approve` stays.
- **Typed output** (`RunTyped[T]`, `typed.go`): returns a typed `T` from a full agent run.
  Injects a synthetic `final_answer` tool with `T`'s schema (via `schema.For[T]`), steers the
  model to it with an injected system message (`injectSystem` middleware + `cloneWith`), and
  decodes from the JOURNALED call args (resume-safe), falling back to parsing final text. Package
  function, not a method (Go methods can't add type params). This is AgenticGoKit's #151 idea; we
  had the `schema` package but lacked the ergonomic. (Also still open from that audit: `Capabilities()`
  provider discovery #142, and the single-module vs submodule dep-hygiene question #144.)
- **Sampling params** (`Sampling` on `Request`, `Agent.WithSampling`): provider-neutral generation
  controls (Temperature/TopP/MaxTokens/Stop/Seed) as pointer fields (nil = provider default, so an
  explicit 0 is distinct from unset). Set once at the agent level via option helpers
  (`agent.Temperature(0)`, `agent.MaxTokens(500)`, …); both adapters translate to wire format
  (OpenAI `stop`/`seed`; Anthropic `stop_sequences`, no seed). Closed our own `model.go` TODO =
  AgenticGoKit #143.
- **Dynamic system prompt** (`Agent.WithSystemPromptFunc(func(ctx) string)`): computes the system
  message per run (dynamic context: time/tenant/retrieved state); takes precedence over the static
  `WithSystemPrompt`. `clone` carries it.
- **Native structured output** (`RunTypedNative[T]`): uses the provider's JSON-schema response
  format (`Request.ResponseFormat`; OpenAI emits strict `json_schema`) instead of the final_answer
  tool — provider-enforced schema, no tool round-trip. Anthropic ignores it (use `RunTyped` there).
  `cloneWith` refactored into `clone` + `cloneWith`.
- **ToolRetry** (`middleware.ToolRetry(n, WithBackoff(...))`): tool-side analogue of model `Retry`
  (backoff+jitter, honors `RateLimited`, ctx-aware); reuses retry.go's shared helpers.
- **Max-turns safety** (`Agent.WithMaxTurns(n)`): caps model turns per run so a model that keeps
  calling tools can't loop forever; hitting it returns `ErrMaxTurns` (wraps `ErrBudget`). Per run
  (per `Session.Send` turn). Counts replayed turns too (a resumed run past the cap stops at once).
  = trpc's "Call Count Limits" safety mechanism; a real hole in our loop (TokenBudget was the only,
  indirect guard). `cloneWith` carries it.
- **Deterministic Simulation Testing** (`dst_test.go`, moat #1 — the crown jewel): adversarially
  PROVES the at-most-once side-effect guarantee. `crashStore` fails the Kth persist (simulating a
  crash); a resume-safe `dstModel` (deterministic on the conversation) + a non-idempotent `chargeTool`
  (global counter). CrashSweep hits every write point; Randomized throws 500 multi-crash schedules;
  both assert `count ≤ 1` and terminal ∈ {completed, *ResumeHalt}. The K=result-write case fires then
  crashes → resume must HALT (haltSeen asserts this path runs, so it's non-vacuous). Turns "tested" into
  "adversarially verified" — the gsm exhaustive-verification ethos applied to the runtime. Foundation
  laid by the synctest adoption. **Chaos benchmark** (`chaos/`, moat #2 — the exported weapon,
  docs/CHAOS-BENCHMARK.md): `chaos.Verify(name, System, seeds)` sweeps crash points + randomized
  schedules and reports whether a non-idempotent side effect ever double-fires. `GoAgents()` reference
  PASSES (maxFired=1); `NaiveReference()` at-least-once baseline FAILS (maxFired=5, ~240 double-fires) —
  proving the harness non-vacuous. Runnable: `go run ./examples/chaosbench`. Point at any SDK via the
  `System`/`Run` interface; competitor adapters go in a SEPARATE module (keep their deps out of core);
  adapters must be FAIR (represent the SDK's best-effort durability, not a strawman).
  **First competitor wired: trpc-agent-go** (`benchmarks/` — SEPARATE module, own go.mod, trpc's ~80
  deps isolated from core; run `cd benchmarks && GOWORK=off go test -run Comparison -v`). RESULT:
  go-agents maxFired=1 PASS; **trpc-agent-go maxFired=5 FAIL (70 double-fires)**; naive maxFired=5 FAIL.
  The trpc finding is FAIR — `fairness_test.go` proves resuming a COMPLETE run is a no-op (its
  checkpoint/resume genuinely works); the double-fires are the documented LangGraph "nodes must be
  idempotent" window (crash between the side-effect node and its checkpoint persisting → resume
  re-runs it), which go-agents' attempt-marker/halt closes. ADK-Go finding: no checkpoint layer at all
  (weaker fit; models "re-invoke re-runs everything"). **Second competitor wired: langchaingo**
  (`benchmarks/langchaingo.go`) — NO durable resume; crash = ctx-cancel after the tool fires,
  "resume" = fresh invocation → re-runs everything. RESULT maxFired=64 FAIL (unbounded; fair per
  `lcg_fairness_test.go`). Full table: go-agents 1 PASS, trpc 5, langchaingo 64, EINO 64, naive 5. **eino** (`eino.go`) also
  wired: its checkpoint is HITL-interrupt-only (NOT automatic crash-resume; verified checkpoint.set
  fires only on interrupt), so a crash re-runs → maxFired=64 FAIL (fair per eino_fairness_test.go).
  **ADK-Go still PENDING** (needs mock genai model + tool + runner + crash-injecting session.Service;
  expected no-checkpoint → re-invoke re-runs). benchmarks not in go.work/CI (heavy deps).
  **Saga DST** (`saga_dst_test.go`): extends the proof to reverse-order compensation. Honest split —
  forward non-idempotent effect is at-most-once (halt on unknown); compensators are at-LEAST-once
  (memoized → once if they complete, but a crash mid-compensation re-runs them, the documented
  idempotency contract). Crash-sweep + 300 randomized schedules assert: charge never double-fires,
  rollback always completes (no charged-but-uncompensated), terminal ∈ {SagaAborted, ResumeHalt};
  both paths exercised. (`failTool` name taken → `boomTool`.)
- **Tamper-evident audit** (`audit/`, moat #4): `audit.Head(ctx, store, runID)` = SHA-256 hash-chain
  commitment over the journal in persisted order (any modify/insert/delete/reorder changes the head);
  `Sign`/`VerifySignature` (ed25519) to anchor it. Stdlib-only. **Merkle upgrade** (`audit/merkle.go`):
  RFC 6962 (Certificate Transparency) `Root`/`Prove`/`VerifyInclusion` for SELECTIVE DISCLOSURE — prove
  one record is in a committed run via an O(log n) inclusion proof without revealing the others (the
  compliance superpower: show an auditor one charge, expose nothing else). VERIFIED against the
  published RFC 6962 test vectors (9 CT reference roots, sizes 0..8, all match) + inclusion round-trips
  (sizes 1..33) + journal selective-disclosure. Same anchoring caveat as Head. **Consistency proofs**
  (`audit/consistency.go`, RFC 6962 §2.1.2): `ProveConsistency`/`VerifyConsistency` prove an earlier
  root is an append-only PREFIX of a later one (history only appended, never rewritten/reordered — the
  transparency-log guarantee). Generation from the RFC SUBPROOF recursion; verification is the canonical
  CT algorithm (decompInclProof + chainInner/Right/Border). Validated: round-trips reconstruct the
  spec-verified MTH roots (all m,n≤24), a hand-derived 1→2 vector, rewrite-detection, journal
  append-only + directionality. The full CT transparency-log triad now: Head, inclusion, consistency.
  **Signed Tree Head** (`audit/sth.go`): the CT-style anchoring artifact — `TreeHead{Size, Root,
  Timestamp}` + `SignTreeHead`/`Verify` (ed25519) binds the Merkle root to WHICH tree (size) and WHEN,
  so a signature can't be replayed across sizes. `NewTreeHead` builds it from the store. End-to-end
  compliance flow tested: sign STH → disclose one record + inclusion proof → auditor verifies against
  the signed root → consistency between two STHs proves append-only growth. #4 is a complete product. HONEST model (in the package doc):
  integrity always; tamper-evidence only if the head is anchored out-of-band (a chain in the same DB
  an attacker controls can be rewritten+rehashed). Compliance/enterprise axis (fintech/health). Read
  over persisted order (correct under parallel tools), no core Record change. Next: Merkle root +
  inclusion proofs (selective disclosure) leveraging merkle-strata.
- **RAG/memory = bring-your-own** (`retrieval.go`, decision in docs/RAG-MEMORY.md): ship NO vector
  store/embedder. Core seam: `Retriever` port (`Retrieve(ctx, query, k) []Doc`), `RetrievalTool`
  (agentic — model searches on demand), `WithRetrieval` middleware (classic — top-k injected as a
  system message on user turns, skipped mid-loop). `Message.Text()` helper added. Conversational
  memory = Sessions; dynamic context = WithSystemPromptFunc; semantic memory = this seam + your
  store. Concrete stores would be separate modules if demand appears. Deliberate scope boundary
  (keeps the zero-dep core + focus on the durability moat); positioning win vs bundled-vector-DB.
- **Sessions / multi-turn** (`session.go`): `Agent.Session(ctx, id) *Session`; `Session.Send(ctx,
  input)` is one durable turn seeded with the transcript so far (agent remembers prior turns).
  Transcript journaled turn-by-turn under `<id>` (StepValue `turn/N` = {input, answer}); turn N runs
  under `<id>/tN` (own journal, in-turn crash resume). Reload from the same store rebuilds the
  transcript. Tool calls don't leak across turns (memory = Q&A). A paused turn (approval/Interrupt)
  returns the error; re-Send same input to resume. Core change: `run` now seeds from a `[]Message`
  instead of `input string` (Run/RunSaga/RunResult/Stream wrap `[]Message{UserText(input)}`).
- **System prompt** (`Agent.WithSystemPrompt(s)`): seeds a `SystemText` at the head of the
  conversation on every model turn (re-seeded each Run, so resume-consistent); `cloneWith` carries
  it so `RunTyped` preserves it. First-class alternative to injecting a system message via
  middleware. (AgentFlow puts `system_prompt` in TOML config; we keep it a typed Go option — the
  values-not-structure line. We deliberately did NOT add their declarative config/orchestration DSL.)
- **Run result envelope** (`result.go`): `RunResult`/`RunSagaResult` return `*Result{Message,
  Usage (summed across turns, incl. cache), Turns, Duration, RunID}` — additive, `Run` still
  returns just `Message`. (Internal `run` now returns `(Message, Usage, int, error)`.) `AgentStream.Result()`
  deferred (would touch the stream goroutine). = competitors' rich Result/RunOptions (partial: no per-call overrides yet).
- **Retry hardening** (`middleware/retry.go`, `cost.go`): `Retry(n, WithBackoff(base,max))` — exponential
  backoff + jitter, honors `Retry-After` via a typed `agent.RateLimited` (adapters return it on HTTP 429).
  `Cost(&CostMeter, Rates)` accumulates USD from token usage (incl. cache read/write). = the "production
  hardening" competitors market; retry-after/cost were our gaps.
- **Two middleware chains** (`func(Handler) Handler`, mutating + short-circuiting): `Use` wraps
  the model call (`Middleware`/`ModelHandler`); `UseTool` wraps every tool call
  (`ToolMiddleware`/`ToolHandler`). Tool middleware runs INSIDE the durable memoized step, so a
  short-circuit/transformed result is journaled and resume-safe. This is the mutate/short-circuit
  answer to agent-sdk-go's 8 lifecycle hooks — two real boundaries, not hook-soup; run/turn
  observation is the `Agent.Stream` event feed.
- **Go 1.2x modernization**: `testing/synctest` for the retry/tool-retry timing tests (fake clock →
  deterministic + instant, kills the real-timer flake class); `math/rand/v2` (dropped the seeded
  source + gosec nolint); `slices.Sorted(maps.Keys)` / `slices.Sort` in schema; `min` builtin +
  `for range n` in retry backoff; removed the dead `c := c` loop-var shadow (loop vars per-iteration
  since 1.22). concurrency_test sleeps left as-is (race-window widening / timeout guard, not backoff).
- `middleware`: model batteries Retry, TokenBudget; tool batteries `ToolLog`, `ToolCache`.
  `trace`: opt-in OTel gen_ai.* — `trace.Model` (`.Use`) + `trace.Tool` (`.UseTool`, execute_tool
  span per call) + `trace.Invoke`. `trace.Tool` runs inside the loop so its span nests across the
  sub-agent boundary (a gap in ADK / AgenticGoKit / trpc-agent-go). Note: `trace.Tool` is now a
  ToolMiddleware, `Tool(tracer)` — was a per-tool decorator `Tool(tracer, t)` (breaking, pre-1.0).
- `model/anthropic` (native), `model/openai` (any OpenAI-compatible via WithBaseURL).
- **Prompt caching**: `anthropic.WithPromptCache()` places cache_control breakpoints on the
  system block + tool defs (the constant per-turn prefix); OpenAI caches prefixes automatically.
  Cache hits/writes surface cross-provider in `agent.Usage.CacheReadTokens`/`CacheWriteTokens`
  (Anthropic cache_read/creation; OpenAI prompt_tokens_details.cached_tokens). = trpc's caching idea.
- `schema`: reflect→inline JSON schema + OpenAIStrict. `mcp`: official go-sdk; annotations→Safety.
- `store/sqlite`, `store/postgres` (Durable adapters, single-flight).
- `govern` (Tier-2, gsm edge — core never imports it): `Governor`, `PersistentGovernor`,
  `FederatedGovernor`, `FederatedApplier`, `EventTool`, `FederatedEventTool`, `EventLog` port.
  Log adapters: `govern/sqlitelog`, `govern/redislog` (Redis Streams).
- `examples/`: `smoke` (live OpenRouter), `mesh` (monotone mesh), `compose` (Embed).
- Every adapter has a `var _ Port = (*Adapter)(nil)` compile-time contract.

## gsm / the theory (what's proven — see the papers)
- **Single registry**: WFC + CC ⇒ unique normal forms (Newman). Verification calculus.
- **Federation §8**: tree convergence via the authority argument (morphism M1).
- **Multi-source**: resolution operators (R1 source-determinacy, R2 validity) ⇒ DAG convergence.
- **Monotone cycles**: monotone repair on a lattice ⇒ convergence on ANY topology
  (Knaster–Tarski + chaotic iteration); CRDTs are the compensation-free special case.
- **Compositionality**: convex sub-federations collapse to effective registries (limits compose).
- **Infinite domains**: convergence is domain-independent (WFC+CC, not finiteness); finiteness
  only buys UBC + enumerative verification + O(1) tabulated runtime; monotone infinite lattices
  compute via Kleene + widening. gsm verifies all preconditions exhaustively at build time.
- **Compensation synthesis** (gsm v0.6.0, `Registry.Synthesize`): generate a convergent
  compensation from invariants + events, or prove none exists. Convergent ≠ desirable (human
  vets the repair). Brute-force, bounded; SAT/SMT scaling is the next step.

## Key decisions
- **Orchestration = plain Go control flow + named durable steps**; NO graph DSL (graph is a
  derived output, RenderMermaid).
- Hexagonal: ports in the consumer, adapters at edges, deps point inward.
- gsm is the guardrail/convergence engine, not the store.

## Docs index
- `docs/DESIGN.md`, `docs/COMPETITIVE*.md` — thesis + competitive teardowns.
- `docs/TIER2-DISTRIBUTED.md` — original distributed-saga plan.
- `docs/TIER2-FEDERATION.md` — the federation plan; **M0–M4 all DONE**, monotone cycles +
  compositionality done; consumed by go-agents.
- `docs/KNOWN-LIMITATIONS.md` — deep-tree recursion memory wall; saga atomicity; etc.

## Open / next candidates
- **Scale compensation synthesis** via SAT/SMT (brute force is bounded; shares machinery with
  symbolic verification). Also: surface `Synthesize` at the agent tier (suggest/repair a
  governed registry) if useful.
- **Compile the paper PDFs** — big unreleased batch (multi-source, monotone cycles,
  compositionality, regime table, cross-paper refs, infinite domains). Needs `colima start`
  + `./compile.sh` in the normalization-confluence repo; then re-publish to Zenodo.
- Symbolic verification (SAT/SMT) to break the ~1M-state enumeration ceiling; infinite-domain
  engine (overlaps with symbolic). Self-stabilization reframing (cheap positioning win).
- v1 agent gaps: sessions/multi-turn, production retry (backoff). (`Agent.Stream` — DONE.)
- Housekeeping: pick a real name; godoc examples; per-file SPDX headers (optional).
  (go-agents CI — DONE: `.github/workflows/ci.yml`, lint [gofmt + vet] + test matrix
  ubuntu/macos/windows on Go 1.27, `-race` off Windows; repo gofmt-clean.)

## Notes
- gsm module path is `github.com/blackwell-systems/gsm`; go-agents module path is the
  placeholder `github.com/dayna/go-agents` (rename on naming).
- The multi-instance-in-one-system limit of `Embed`: no registry namespacing yet (reuse is
  across separate systems; two instances of one subsystem in one federation would collide on
  registry names). Documented in `examples/compose`.
