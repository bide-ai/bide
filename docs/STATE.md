# Project state snapshot

Working codename **go-agents**. A durability-first, hexagonal Go agent devkit.
Module `github.com/dayna/go-agents`, **go 1.27**. All packages green, core race-clean and
dependency-pure (arch guard enforces it). Name still deferred.

## Repos, versions, remotes (read this first after a context reset)

Three repos under `/Users/dayna/code/`, all remotes on the `github-blackwell` SSH host:
- **go-agents** (this repo) — the agent devkit. Private: `blackwell-systems/go-agents`.
  No license yet (proprietary by default). Committed history, on `main`.
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
- `middleware`: Retry, TokenBudget. `trace`: opt-in OTel gen_ai.* (API only).
- `model/anthropic` (native), `model/openai` (any OpenAI-compatible via WithBaseURL).
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
