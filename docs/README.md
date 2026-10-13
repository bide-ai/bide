# Documentation

The map, in the order of the product: start, build, inspect, prove; then the reference and the
proofs. Each entry notes who it is for.

## Start here

- **[Getting started](getting-started.md)** (everyone): prerequisites, installing `bide`, `bide demo` and `bide init`, your first agent, and how to run an example.
- **[A tour of bide](tour.md)** (builders): the API section by section, from graphs and the agent loop to sessions, the audit trail, human approval, errors, middleware and governance.
- **[Guarantee](GUARANTEE.md)** (evaluator): the precise durability guarantee, stated exactly.
- **[Known limitations](KNOWN-LIMITATIONS.md)** (evaluator): the bounds and edges of the guarantees. Read alongside the guarantee.

## Concepts

- **[Concepts](CONCEPTS.md)** (everyone): the vocabulary in one place (journal, at-most-once, lease, Waker, gsm, ProofBundle, and the rest).
- **[Design](design/design.md)**: how the runtime is put together.

## Guides: build

- **[Plan flows](guides/plan-flows.md)**: the `plan` package. Author a plan flow, prove a run followed the declared graph, with at-most-once and halt-on-ambiguity inherited from the substrate.
- **[Durable steps](guides/durable-steps.md)**: `Step`, `Parallel`/`Task` fan-in, and sagas on the durable substrate.
- **[Reliability](guides/reliability.md)**: retry, hedge, rate limit, cost tracking, and how they compose.
- **[Signals and ambient](guides/signals.md)**: timers, human-in-the-loop, and durable signals (`Signal`/`Await`/`AwaitFor`, ordered channels). At-least-once transport in, exactly-once application.
- **[Models](guides/models.md)**: the Anthropic, OpenAI-compatible, and Gemini adapters, `WithBaseURL`, caching, and multimodal input.
- **[MCP](guides/mcp.md)**: Model Context Protocol as a runtime tool source.
- **[Messaging](guides/messaging.md)**: driving an agent from an inbound webhook, redelivery-safe.
- **[RAG and memory](guides/rag-memory.md)**: bring-your-own retrieval.
- **[Observability](guides/observability.md)**: OTel gen_ai spans in one line (`trace.Instrument`), token-to-cost, and the content-capture default.

## Guides: inspect

- **[Run viewer](guides/run-viewer.md)**: `bide view`, a read-only local page for one run: timeline, flow path, halts, approval gates, and the audit verdict.
- **[Debugging and recovery](guides/debugging.md)**: replay, event reconstruction, Mermaid diagrams, and crash recovery.

## Guides: prove and govern

- **[Audit](guides/audit.md)**: the tamper-evident trail and proof-carrying runs.
- **[Human approval (human-in-the-loop)](guides/hitl-approval.md)**: human sign-off before a tool runs, 1-of-1 or signed m-of-n, provable offline.
- **[Security model](guides/security-model.md)**: the cryptographic guarantees and their exact scope (confidentiality is out of scope).
- **[Delegation](guides/delegation.md)**: signed grants, attenuating delegation, and earned authority.
- **[Governance](guides/governance.md)**: convergent governed state (Tier-2), federation, and gsm-backed synthesis and coordination.
- **[Quorum](guides/quorum.md)**: governed k-of-n model agreement, verifiable offline.

## Reference

- **[CLI reference](reference/cli.md)**: every `bide` command (`init`, `demo`, `view`, `check`, `audit`, `version`), its flags and its exit statuses, generated from the flag sets.
- **[Extension points](reference/extension-points.md)**: the ports and adapters (`Model`, `Store`, `Tool`, ...) and an "implement your own store" walkthrough.
- **[Module structure](reference/module-structure.md)**: the multi-module repo layout.
- **API**: the godoc of every package, at [pkg.go.dev](https://pkg.go.dev/github.com/bide-ai/bide/agent).

## Proofs

- **[Formal verification](formal-verification.md)** (evaluator): an overview of the TLA+ models, what each guarantees and covers, every bug they found or confirmed, what runs in CI, and what they do not cover.
- **[Formal models](../spec/tla/README.md)**: the reference for each TLA+ model (the claim protocol, the approval gate, the bide protocol's claim rules, flow semantics, spend accounting, the tool-call state machine, the run lifecycle, delegation and saga trees, and sessions), what TLC checks on every pull request, and the bounds.
- **[How bide is verified](testing/verification.md)** (evaluator): the discipline behind the guarantees: no fix without a failing test, mutation checks, crash and cancellation sweeps, forced interleavings, conformance suites, and CI.
- **[Testing](testing/testing.md)**: what is tested and how, the chaos crash-injection benchmark, differential oracles, and the statistical `eval` boundary.

## Examples

- **[examples/](../examples/README.md)**: runnable programs, split into authoring basics and accountability/durability/integration showcases.

## Project

- **[Changelog](../CHANGELOG.md)** (everyone): every release's changes, breaking changes marked; highlights per release in [releases/](releases/).
- **[Roadmap](ROADMAP.md)** (everyone): where bide is going next, in order.
- **[Blog](blog/index.md)** (everyone): longer posts on the design and how to use it, starting with the launch post.

## Design notes

- **[Durable signals](design/design-durable-signals.md)**, **[m-of-n approval](design/design-mofn-approval.md)**, **[Compaction](design/compaction.md)**, **[Chaos benchmark](design/chaos-benchmark.md)**, **[Formal models](design/formal-models.md)** (accepted, in progress), **[The bide protocol](design/protocol.md)** (accepted; not implemented), **[Probes](design/probes.md)** (draft; not implemented), **[Tooling and onboarding](design/tooling-and-onboarding.md)** (accepted; in progress).
