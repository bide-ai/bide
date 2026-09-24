# Project state snapshot

Working codename **go-agents**. A durability-first, hexagonal Go agent devkit.
Module `github.com/dayna/go-agents`, **go 1.27**. ~12 test packages, ~51 tests, all green,
core race-clean and dependency-pure (arch guard enforces it). Name still deferred.

## The pitch / moat
"Durable AI agents that survive a crash — without firing the same side effect twice."
Differentiators (all shipped + tested; confirmed absent in every competitor surveyed):
- **Durable, side-effect-safe resume** (journal + attempt-markers + halt-on-unknown-write).
- **Transactional SAGA agents** with automatic reverse-order compensation, incl. across
  sub-agent trees (bidirectional: parent abort → recurse; sub failure → whole-tree reverse).
- **Tier-2 distributed/convergent compensating agents** (gsm-backed): concurrent agents on
  shared state converge provably; gsm proves at build time and *refuses divergent models*.
- Typed content Parts w/ Anthropic reasoning-signature preservation; provider-aware schema
  (OpenAIStrict); MCP via official SDK w/ annotations→Safety; opt-in OTel; middleware;
  parallel tool calls; any-model (one OpenAI-compatible adapter + native Anthropic).

## Package layout
- `agent` (root, gsm/adapter-free core): loop, Message/Part, Tool/Safety, Durable, Model
  (Stream-first), middleware types, sub-agents, saga (`saga.go`), replay, RenderMermaid,
  parallel tool exec, single-flight-safe. Ports: Model, Durable, Tool, Middleware.
- `middleware`: Retry, TokenBudget.
- `trace`: opt-in OTel gen_ai.* (API only, no SDK).
- `model/anthropic` (native), `model/openai` (any OpenAI-compatible via WithBaseURL).
- `schema`: reflect→inline JSON schema + OpenAIStrict dialect.
- `store/sqlite` (default on-disk), `store/postgres` (HA). Both single-flight.
- `mcp`: official modelcontextprotocol/go-sdk; annotations→Safety.
- `govern` (Tier-2, gsm edge — core never imports it): Governor + PersistentGovernor
  (event-sourced), EventTool (governor middleware), EventLog port. Adapters:
  `govern/sqlitelog` (on-disk), `govern/redislog` (Redis Streams). PoC-complete.
- `examples/smoke`: live OpenRouter end-to-end (verified working).
- Every adapter has a `var _ Port = (*Adapter)(nil)` compile-time contract.

## Key decisions (see docs/DESIGN.md)
- **Orchestration = Option B**: plain Go control flow + named durable steps; NO graph DSL;
  graph is a *derived output* (RenderMermaid). Validated against Eino & ADK v2.
- Hexagonal: ports in the consumer, adapters at edges, deps point inward
  (`architecture_test.go` fails the build if the core imports an adapter or heavy infra).
- Build on the durable substrate; gsm is the guardrail/convergence engine, not the store.

## Docs
- `docs/DESIGN.md` — thesis, wedge, Go-idiomatic principles, locked decisions.
- `docs/COMPETITIVE.md` + `docs/COMPETITIVE-agenticenv.md` — full cited teardowns
  (LangChainGo, small frameworks, Eino, Genkit/ADK, agenticenv, jetify). Verdict: nobody
  has durable+saga+convergent; typed-parts/schema are table stakes.
- `docs/TIER2-DISTRIBUTED.md` — the gsm-backed distributed-saga research + phased plan
  (Phase 0/1/2 done; federation is the research frontier).
- `docs/KNOWN-LIMITATIONS.md` — deep-tree recursion memory wall (~2500 lvls = 7.4GB; heap
  live-set, not stack; fine <100; iterative rewrite deferred); saga atomicity requirement;
  parallel-tools journal-seq is completion-order (benign); replay reasoning-block collapse.

## Open / next candidates (not done)
- v1 gaps: `Agent.Stream` to the caller (UI streaming — plumbing exists), sessions/multi-turn,
  production retry (backoff + Retry-After), CI workflow (+ `-race` in CI).
- Tier-2: Postgres EventLog (thin drop-in), federation (research), modeling-ergonomics DSL,
  persistent governed-state hardening.
- Housekeeping: pick a real name, `git init`/commit (not done — nothing committed yet),
  LICENSE, godoc examples.

## Notes
- gsm lives at `/Users/dayna/code/gsm` (module github.com/blackwell-systems/gsm), wired via a
  `replace` directive in go.mod. It's the founder's own IP (normalization confluence).
- Nothing has been git-committed; user hasn't asked to commit.
