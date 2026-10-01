# Documentation

The map. Read in roughly this order; each entry notes who it is for.

## Start here

- **[Getting started](getting-started.md)** (everyone): prerequisites, install, your first agent, and how to run an example.
- **[Concepts](CONCEPTS.md)** (everyone): the vocabulary in one place (journal, at-most-once, lease, Waker, gsm, ProofBundle, and the rest).
- **[Guarantee](GUARANTEE.md)** (evaluator): the precise durability guarantee, stated exactly.
- **[Known limitations](KNOWN-LIMITATIONS.md)** (evaluator): the bounds and edges of the guarantees. Read alongside the guarantee.
- **[Changelog](../CHANGELOG.md)** (everyone): every release's changes, breaking changes marked; highlights per release in [releases/](releases/).
- **[Roadmap](ROADMAP.md)** (everyone): where bide is going next, in order.

## Guides (how to build on it)

Authoring:
- **[Reliability](guides/reliability.md)**: retry, hedge, rate limit, cost tracking, and how they compose.
- **[Durable steps](guides/durable-steps.md)**: `Step`, `Parallel`/`Task` fan-in, and sagas on the durable substrate.
- **[Flows](guides/flows.md)**: the `plan` builder. Author a typed flow, prove a run followed the declared graph, with at-most-once and halt-on-ambiguity inherited from the substrate.
- **[Signals and ambient](guides/signals.md)**: timers, human-in-the-loop, and durable signals (`Signal`/`Await`/`AwaitFor`, ordered channels). At-least-once transport in, exactly-once application.
- **[Observability](guides/observability.md)**: OTel gen_ai spans in one line (`trace.Instrument`), token-to-cost, and the content-capture default.
- **[Models](guides/models.md)**: the Anthropic, OpenAI-compatible, and Gemini adapters, `WithBaseURL`, caching, and multimodal input.
- **[MCP](guides/mcp.md)**: Model Context Protocol as a runtime tool source.
- **[Messaging](guides/messaging.md)**: driving an agent from an inbound webhook, redelivery-safe.
- **[RAG and memory](guides/rag-memory.md)**: bring-your-own retrieval.
- **[Debugging and recovery](guides/debugging.md)**: replay, event reconstruction, Mermaid diagrams, and crash recovery.

Accountability and governance:
- **[Audit](guides/audit.md)**: the tamper-evident trail and proof-carrying runs.
- **[Delegation](guides/delegation.md)**: signed grants, attenuating delegation, and earned authority.
- **[Security model](guides/security-model.md)**: the cryptographic guarantees and their exact scope (confidentiality is out of scope).
- **[Governance](guides/governance.md)**: convergent governed state (Tier-2), federation, and gsm-backed synthesis and coordination.
- **[Human approval (human-in-the-loop)](guides/hitl-approval.md)**: human sign-off before a tool runs, 1-of-1 or signed m-of-n, provable offline.
- **[Quorum](guides/quorum.md)**: governed k-of-n model agreement, verifiable offline.

## Reference

- **[Extension points](reference/extension-points.md)**: the ports and adapters (`Model`, `Store`, `Tool`, ...) and an "implement your own store" walkthrough.
- **[Module structure](reference/module-structure.md)**: the multi-module repo layout.

## Design notes

- **[Design](design/design.md)**, **[Durable signals](design/design-durable-signals.md)**, **[m-of-n approval](design/design-mofn-approval.md)**, **[Compaction](design/compaction.md)**, **[Chaos benchmark](design/chaos-benchmark.md)**, **[Formal models](design/formal-models.md)** (accepted, in progress), **[The bide protocol](design/protocol.md)** (accepted; not implemented).

## Testing and evidence

- **[How bide is verified](testing/verification.md)** (evaluator): the discipline behind the guarantees: no fix without a failing test, mutation checks, crash and cancellation sweeps, forced interleavings, conformance suites, and CI.
- **[Testing](testing/testing.md)**: what is tested and how, the chaos crash-injection benchmark, differential oracles, and the statistical `eval` boundary.
- **[Formal verification](formal-verification.md)** (evaluator): an overview of the TLA+ models, what each guarantees and covers, every bug they caught before release, what runs in CI, and what they do not cover.
- **[Formal models](../spec/tla/README.md)**: the reference for each TLA+ model (the claim protocol, the approval gate, the bide protocol's claim rules, flow semantics, spend accounting, the tool-call state machine and the run lifecycle), what TLC checks on every pull request, and the bounds.

## Examples

- **[examples/](../examples/README.md)**: runnable programs, split into authoring basics and accountability/durability/integration showcases.
