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
  Released through **v0.8.0** (through v0.6.0: scalable compensation synthesis with preference-guided,
  provably minimum-cost, and impossibility-witness modes; v0.7.0: the combinator rule vocabulary plus
  sugar, the two differential oracles, footprint-local `BuildCompositional`, and `Registry.PolicyDigest`;
  v0.8.0: `State.Digest` for state attestation). go-agents depends on it via a normal versioned require
  (**pinned `gsm@v0.8.0`**). The old `replace ../gsm` is gone.
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
- **Reliability wrapper hardening** (`middleware`, commit f01ef81): `WithTimeout` (per-attempt
  deadline for both `Retry` and `ToolRetry`); `WithRetryIf` plus a ready-made `Retryable` classifier
  (retries 5xx / 408 / `RateLimited` / per-attempt timeout, fails fast on 4xx auth/validation, with
  adapters now returning a typed `agent.APIError{StatusCode}`); and a dependency-free `RateLimiter`
  with `RateLimit` / `ToolRateLimit` (token bucket, proactive throttling). Backoff and Retry-After
  already existed.
- **Scale/concurrency benchmark** (`cmd/bench` + `cmd/bench/README.md`, commit 03b0d46): a load
  harness with measured numbers, about 128k runs/s framework-overhead-only, and 5,000 runs each
  blocking about 100ms on the model overlapping into about 448ms wall-clock on about 5,500 goroutines
  and about 35 MB. It backs Pillar 2 as throughput and operational simplicity (one process, no fleet),
  not lower latency than the model (the provider owns per-call latency). NOTE: these numbers use the
  in-memory store (the floor); at high fan-out the durable store's write throughput, not goroutines,
  is the production ceiling. STRATEGY.md carries the Go-dividend scale section, the capital-markets
  wedge, and HFT as a non-goal.
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
  re-runs it), which go-agents' attempt-marker/halt closes. **Second competitor wired: langchaingo**
  (`benchmarks/langchaingo.go`) — NO durable resume; crash = ctx-cancel after the tool fires,
  "resume" = fresh invocation → re-runs everything. RESULT maxFired=64 FAIL (unbounded; fair per
  `lcg_fairness_test.go`). **eino** (`eino.go`) also
  wired: its checkpoint is HITL-interrupt-only (NOT automatic crash-resume; verified checkpoint.set
  fires only on interrupt), so a crash re-runs → maxFired=64 FAIL (fair per eino_fairness_test.go).
  **ADK-Go now wired** (`adk.go`): Google's ADK has REAL event persistence — every event
  `AppendEvent`s to a `session.Service` and re-invoking replays history, so a history-aware model
  de-dupes recorded work. Injected the crash through ADK's real machinery (a `session.Service` that
  fails the Nth AppendEvent; the session survives across steps; resume = fresh `runner.Run` on it).
  Measured (not assumed): **maxFired=4 FAIL** — trpc-like, NOT eino-like. FAIR per
  `adk_fairness_test.go` (resume of a COMPLETE run is a no-op: recorded charge → replay says "already
  charged" → no re-fire). The double-fires are the execute→persist window ADK has no attempt-marker to
  close. Full table: **go-agents 1 PASS, trpc 5, adk-go 4, langchaingo 64, eino 64, naive 5.**
  benchmarks not in go.work/CI (heavy deps).
  **Saga DST** (`saga_dst_test.go`): extends the proof to reverse-order compensation. The split:
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
  the signed root → consistency between two STHs proves append-only growth. #4 is a complete product. Model (in the package doc):
  integrity always; tamper-evidence only if the head is anchored out-of-band (a chain in the same DB
  an attacker controls can be rewritten+rehashed). Compliance/enterprise axis (fintech/health). Read
  over persisted order (correct under parallel tools), no core Record change.
  **Event→audit sink** (`audit/eventsink.go`, from the "durable+auditable as one spine" review): an
  `EventLog` applies the SAME RFC 6962 machinery (merkleRoot/auditPath/verifyPath) to `Agent.Stream`'s
  semantic AgentEvents — `Add`/`Root`/`Head`/`Prove`/`VerifyEventInclusion`/`TreeHead`/`ProveConsistency`,
  all reusing the journal's `Inclusion`/`TreeHead`/`Consistency` types and `Sign`/`Verify` (one auditor
  flow for journal + events). `Record(log, stream, onEvent)` drains a live stream in one pass (UI feed +
  committed trail). DURABILITY: a live `EventLog` is in-memory (lost on crash; Root even shifts fresh-vs-
  replay), so the durable artifact is `audit.EventLogFromJournal` = a projection of the crash-safe journal
  via `agent.ReplayEvents` (StepModel→AssistantTurn{Replayed}, StepToolResult→ToolCompleted; deterministic,
  resume-stable, append-only-across-crash — proven in `replay_test.go` + `audit/eventsink_test.go`).
  Token deltas aren't journaled → not in the durable projection (correct for a compliance log).
  **BYO EventStore port** (`audit/eventstore.go`): `EventStore` interface (Append idempotent+append-only
  on (runID,seq), Load) + `MemEventStore` default (Postgres UNIQUE(run_id,seq)/WORM in prod).
  `PersistJournal(evStore, jStore, runID)` idempotently mirrors the journal PROJECTION (not the live
  stream — projection is resume-stable so re-mirror never forks) into a backend with its OWN retention;
  `LoadEventLog(evStore, runID)` rebuilds Root/STH/proofs from the store ALONE (journal can be GC'd).
  Tested: append-only/fork rejection, journal round-trip + idempotent re-mirror + store-only anchor,
  prefix→full consistency.
  **Continuous anchoring** (`audit/anchor.go` + `audit/audited_store.go`): `AuditedStore` wraps any
  `Durable` (drop-in) and on each journal growth signs an STH (reusing merkleRoot+SignTreeHead) and
  publishes via the BYO `Anchor` port. Memoized replay does NOT re-anchor (per-run lastSize dedup →
  each record anchored exactly once, even across a crash). Anchor failure is a SIDE CHANNEL: never
  fails the durable step (would risk retrying a non-idempotent side effect) → optional `OnError` hook.
  `MemAnchorLog` = reference external transparency log: append-only, keeps its OWN RFC 6962 tree over
  published STHs → `Prove`/`VerifyAnchorInclusion` (an STH was anchored) + `ProveConsistency` (the
  anchor log only grew). End-to-end chain proven: journal record → inclusion proof → STH → provably
  anchored in an independent append-only log. Tested: per-step anchoring, monotonic sizes, final STH
  == journal Root, no-reanchor-across-crash+resume, publish-error-doesn't-fail-step, anchor-log
  inclusion+consistency. Next: real external-log `Anchor` adapters (Trillian/CT, ledger, notary);
  batched/periodic anchoring for high throughput.
- **Governed-action attestation** (`govern.AttestedEventTool` + `audit.RecordPolicy`/`ProvePolicy`/
  `PolicyContent`, on gsm v0.8.0 `Registry.PolicyDigest`/`PolicyBytes` + `State.Digest`): a governed
  tool embeds the policy digest and the resulting `state_digest` (both opaque to the SDK) in its
  journaled result, so one committed leaf binds the action, the policy that admitted it, and the state
  it produced, and the STH and `ProofBundle` commit to all three. The policy itself is anchored as a dedicated in-log leaf
  (`StepValue` keyed by digest, idempotent), covered by the same tree; an action bundle and a policy
  bundle cross-link in one tree via matching digests. `goagents-audit verify-governance` recomputes
  the digest from the published bytes (no gsm import) and runs the external oracle (`astchecker`) to
  certify convergence; `verify-governed-action` does the whole cross-link end to end. Two independent
  roots of trust (tamper-evident log + external axiom-free-proof oracle) over one artifact, verifiable
  by someone who trusts neither the producer nor gsm. The **policy-level negative** is shipped too:
  governed-action leaves are keyed by policy digest (`audit.PolicyUsedKey`), so `audit.PoliciesUsed`
  recomputes the set of policies a run used and `audit.ProveAbsentBundle` yields an anchorable,
  offline-verifiable proof that no governed action ran under a disallowed digest (the negative has
  teeth: absence of a policy that was actually used cannot be proven). Combined with the approved
  policies being oracle-certified convergent, that supports "no violation was admitted" as a policy-
  level claim. A verifier holding the policy and the run's governed events replays them and reproduces
  every committed `state_digest`, a per-run differential check of the actual execution against the
  verified reference. Shipped and tested (`govern/attested_test.go`, `govern/attested_e2e_test.go`,
  `govern/attested_replay_test.go`, `audit/policy_test.go`, `audit/governance_absence_test.go`). No
  proof feature remains on the roadmap; what is left is operational (a hosted anchor service). NOTE:
  the negative is policy-level, not a per-action state-validity proof, and the replay checks the events
  this run took, not all inputs, so the runtime refinement gap stays open.
- **Absence proofs from the CLI** (`goagents-audit prove-absent`/`verify-absent`,
  `audit.SignAbsenceRoot`, `audit.ToolUseKeyFor`/`PolicyUsedKeyFor`): the auditor persona produces and
  checks "this never happened" proofs from the command line, like inclusion. `SignAbsenceRoot` signs
  the separate absence commitment in one call; keys are `tool:<id>` or `policy:<digest>`. Shipped.
- **Signature-scheme agility + post-quantum option** (`audit/signing.go`; `SignTreeHeadWith` /
  `VerifyWith`, `ProofBundle`/`AbsenceBundle.VerifyWith`; `SignedTreeHead.Alg` is `omitempty`, so
  existing ed25519 bundles are unchanged and the legacy `SignTreeHead`/`Verify` path is untouched):
  pluggable Signer/Verifier with three schemes, all stdlib as of Go 1.27 (no new dependency): ed25519
  (default), ML-DSA-65 (FIPS 204 post-quantum), and hybrid ed25519+ML-DSA-65 (accepted only if both
  verify). Rationale: audit anchors are long-lived (harvest-now, forge-later), and the signature is
  the quantum-vulnerable part while the SHA-256 Merkle hashing is unaffected and unchanged. Constraint:
  `crypto/mldsa` is unavailable under the FIPS 140-3 module, so FIPS mode and ML-DSA are mutually
  exclusive in this toolchain (pick per buyer). Shipped.
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
- `model/anthropic` (native), `model/gemini` (native), `model/openai` (any OpenAI-compatible via WithBaseURL).
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
- **Verification infrastructure** (as of 2026-09-24): rules can be expressed as a fixed
  **combinator vocabulary** (invariants/events as inspectable data, footprints derived from the
  expression tree), with an ergonomic sugar layer that lowers to the same primitives. gsm's
  per-machine verdict is **differentially checked by two oracles extracted from the axiom-free
  Coq proof**: a *table oracle* (over emitted step tables) and a *rules oracle* (recomputing
  convergence from the combinator declarations). **Footprint-local verification**
  (`Registry.BuildCompositional`) certifies each footprint-connected component over its own
  subspace, scaling past the global enumeration ceiling.

## Key decisions
- **Orchestration = plain Go control flow + named durable steps**; NO graph DSL (graph is a
  derived output, RenderMermaid).
- Hexagonal: ports in the consumer, adapters at edges, deps point inward.
- gsm is the guardrail/convergence engine, not the store.

## Docs index
- `docs/DESIGN.md`, `docs/COMPETITIVE*.md`: thesis + competitive teardowns.
- `docs/STRATEGY.md`, `docs/POSITIONING.md`: go-to-market wedge and product positioning.
- `docs/GUARANTEE.md`: the one-line guarantee and its precise scope.
- `docs/AUDIT.md`: the tamper-evident audit spine (RFC 6962 inclusion/consistency/STH,
  continuous anchoring, signed grants and delegation chains, governed-action attestation,
  earned authority, absence proofs, proof-carrying runs via `RunCertificate` / `verify-run`,
  and the `goagents-audit` CLI verbs).
- `docs/GOVERNANCE.md`: the governance/convergence tier as shipped, including federation
  (tree / multi-source DAG / monotone cycles), the identity/authority model, and quorum.
- `docs/QUORUM.md`: governed k-of-n model agreement (`govern.Quorum` + `verify-quorum`).
- `docs/RELIABILITY.md`: the reliability middleware (timeouts, classified retry, hedged
  model calls via `Hedge`, rate limiting, cost).
- `docs/DURABLE-STEPS.md`: `Step` / `Parallel` / `Task`, sagas, and durable timers
  (`Sleep` / `WaitUntil` + the `Waker`).
- `docs/MODELS.md`, `docs/MESSAGING.md`, `docs/MCP.md`: the model adapters; driving an
  agent from an inbound messenger webhook; MCP tool sourcing.
- `docs/COMPACTION.md`: journal compaction with proof continuity (design note).
- `docs/DEBUGGING.md`: replay, semantic-event reconstruction, Mermaid diagrams, and crash
  recovery (`Lister` / `Recover` / `IsComplete`).
- `docs/TESTING.md`, `docs/CHAOS-BENCHMARK.md`: what is tested and how; the exported chaos harness.
- `docs/EXTENSION-POINTS.md`, `docs/MODULE-STRUCTURE.md`, `docs/RAG-MEMORY.md`: ports/adapters;
  the multi-module layout; bring-your-own retrieval.
- `docs/KNOWN-LIMITATIONS.md`: deep-tree recursion memory wall; saga atomicity; etc.

(The former `TIER2-DISTRIBUTED.md` / `TIER2-FEDERATION.md` were implementation plans whose
milestones are all done; federation is now documented as a feature in `GOVERNANCE.md`, so the
plans were removed rather than kept as history. Remaining forward work is below.)

## Shipped this cycle (was Open, now done)
- **Crash-recovery re-driver**: `agent.Lister` (optional store enumeration), `agent.Recover(ctx,
  store, resume)`, `agent.IsComplete`, and the `run:complete` terminal marker (store.go, recover.go,
  agent.go). Recover enumerates runs, skips finished ones, and re-drives the rest; a durable pause is
  a success, not a failure. See docs/DEBUGGING.md.
- **Idempotency-key retry**: a tool with `Safety.IdempotencyKey != nil` is now retry-safe on an
  unknown outcome (automatic retry) instead of `*ResumeHalt` (tool.go). Turns halt-for-a-human stops
  into automatic retries for autonomous/ambient agents whose tools carry idempotency keys.
- **Durable timers**: `agent.Sleep` / `agent.WaitUntil`, the `*Sleeping` durable pause,
  `agent.WithClock` for tests, and the pluggable `Waker` port (`MemWaker` + `agent.WithWaker`)
  (timer.go, waker.go). See docs/DURABLE-STEPS.md.
- **Hedged model calls**: `middleware.Hedge` (race a backup, take the first) for tail latency and
  provider failover. See docs/RELIABILITY.md, `examples/hedge`.
- **Governed quorum**: `govern.Quorum` (k-of-n model agreement over `agent.Parallel`) +
  `goagents-audit verify-quorum` + `examples/quorum`. See docs/QUORUM.md.
- **Signed grants + attenuating delegation**: `audit.Grant` / `SignGrant` /
  `VerifyDelegationChain` / `AttenuatingSubAgent` (authority narrows by default down a delegation
  tree) + `examples/authority`, `examples/delegation`. See docs/AUDIT.md / docs/GOVERNANCE.md.
- **Earned authority**: `audit.EarnedAuthority` drives a grant's scope from the agent's track
  record, bounded by proof, + `examples/earned-authority`. See docs/AUDIT.md.
- **Proof-carrying runs**: `audit.RunCertificate`, `CertifyRun`, `RecordRunCertificate` /
  `ProveRunCertificate`, `VerifyRun`, and the `goagents-audit verify-run` CLI verb (audit/runcert.go,
  cmd/goagents-audit). One portable certificate asserts behavioral-property compliance over a whole
  run, checkable offline against a single signed tree head. See docs/AUDIT.md.
- **Identity**: `agent.Identity` + `agent.WithIdentity(ctx, id)` bind the acting principal onto the
  run context, so it propagates to tool calls and sub-agents and lands in governed leaves. See
  docs/GOVERNANCE.md / docs/AUDIT.md / docs/POSITIONING.md.

## Open / next candidates
- **Distributed at scale / HA**: a networked `EventLog` ships (Redis Streams, so multi-process
  convergence works today via shared-log replay); still forward: a Postgres `EventLog` for HA and
  **run leasing** (so competing recoverers do not both re-drive the same run), cross-language
  replicas via `Export()`, and modeling ergonomics (author the event alphabet from tool schemas,
  quantization helpers, surface CC counterexamples). This is the "tier 3" frontier.
- **Snapshotting / journal compaction** (design note only, see docs/COMPACTION.md): a live journal
  grows unbounded; compaction with proof continuity is designed but not implemented.
- **Scale compensation synthesis** via SAT/SMT (brute force is bounded; shares machinery with
  symbolic verification). Also: surface `Synthesize` at the agent tier (suggest/repair a governed
  registry) if useful. Symbolic verification (SAT/SMT) to break the ~1M-state enumeration ceiling;
  infinite-domain engine (overlaps with symbolic). Self-stabilization reframing (cheap positioning
  win).
- **Cross-language canonicalization pinning**: `Export()` gives cross-language replicas, but the
  JSON canonicalization the replica must match is not yet pinned as a versioned spec.
- **Per-run token cost**: `RunResult.Usage` sums usage across turns, but there is no per-run USD
  cost on the envelope (the `Cost` middleware accumulates into a `CostMeter`, not the `Result`).
- **Compile the paper PDFs**: big unreleased batch (multi-source, monotone cycles,
  compositionality, regime table, cross-paper refs, infinite domains). Needs `colima start`
  + `./compile.sh` in the normalization-confluence repo; then re-publish to Zenodo.
- Housekeeping: pick a real name; godoc examples; per-file SPDX headers (optional).
  (go-agents CI is DONE: `.github/workflows/ci.yml`, lint [gofmt + vet] + test matrix
  ubuntu/macos/windows on Go 1.27, `-race` off Windows; repo gofmt-clean.)

## Notes
- gsm module path is `github.com/blackwell-systems/gsm`; go-agents module path is the
  placeholder `github.com/dayna/go-agents` (rename on naming).
- The multi-instance-in-one-system limit of `Embed`: no registry namespacing yet (reuse is
  across separate systems; two instances of one subsystem in one federation would collide on
  registry names). Documented in `examples/compose`.
