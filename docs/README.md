# Documentation

The map, in the order of the product: start, build, inspect, prove; then the reference and the
proofs. Each entry notes who it is for.

## Start here

- **[Getting started](getting-started.md)** (everyone): prerequisites, installing `bide`, `bide demo` and `bide init`, your first agent, and how to run an example.
- **[A tour of bide](tour.md)** (builders): the API section by section, from graphs and the agent loop to sessions, the audit trail, human approval, errors, middleware and governance.
- **[Recipes](recipes/index.md)** (builders): six complete, copy-paste-runnable flows (a refund with a 2-of-3 approval, invoice approval, ticket triage, a safe customer-record update, an ambient agent, a webhook that tells a person when a run needs them), each runnable with no API key.
- **[Guarantee](GUARANTEE.md)** (evaluator): the precise durability guarantee, stated exactly.
- **[Known limitations](KNOWN-LIMITATIONS.md)** (evaluator): the bounds and edges of the guarantees. Read alongside the guarantee.

## Concepts

- **[Concepts](CONCEPTS.md)** (everyone): the vocabulary in one place (journal, at-most-once, lease, Waker, gsm, ProofBundle, and the rest).
- **[Design](design/design.md)**: how the runtime is put together.

## Guides: build

- **[Plan flows](guides/plan-flows.md)**: the `plan` package. Author a plan flow, prove a run followed the declared graph, with at-most-once and halt-on-ambiguity inherited from the substrate.
- **[Durable steps](guides/durable-steps.md)**: `Step`, `Parallel`/`Task` fan-in, and sagas on the durable substrate.
- **[Reliability](guides/reliability.md)**: retry, hedge, rate limit, cost tracking, and how they compose.
- **[Postgres store](guides/postgres.md)**: multi-node durable resume, `bide migrate`, and opening the store without migrating under a role that has no DDL rights.
- **[Signals and ambient](guides/signals.md)**: timers, human-in-the-loop, and durable signals (`Signal`/`Await`/`AwaitFor`, ordered channels). At-least-once transport in, exactly-once application.
- **[Models](guides/models.md)**: the Anthropic, OpenAI-compatible, and Gemini adapters, `WithBaseURL`, caching, and multimodal input.
- **[MCP](guides/mcp.md)**: Model Context Protocol as a runtime tool source.
- **[Messaging](guides/messaging.md)**: driving an agent from an inbound webhook, redelivery-safe.
- **[RAG and memory](guides/rag-memory.md)**: bring-your-own retrieval.
- **[Observability](guides/observability.md)**: OTel gen_ai spans in one line (`trace.Instrument`), token-to-cost, and the content-capture default.

## Guides: inspect

- **[Flow editor](guides/flow-editor.md)**: `bide edit`, a local page for a flow config: the graph, an outline, each node's approval gate and safety, the assertions and the check (read-only in this release; editing follows).
- **[Run viewer](guides/run-viewer.md)**: `bide view`, a read-only local page for a journal's runs (a run list filtered by state, kind and run ID) and for one run: timeline, flow path, halts, approval gates, and the audit verdict; it follows a run in progress.
- **[Debugging and recovery](guides/debugging.md)**: replay, event reconstruction, Mermaid diagrams, and crash recovery.

## Guides: prove and govern

- **[Audit](guides/audit.md)**: the tamper-evident trail and proof-carrying runs.
- **[Notifications](guides/notifications.md)**: the signed webhook notifier, the all-clear when an event ends, receiver verification (`notify.Verify`), mutual TLS, and the payload; `bide notify test`, `listen` and `relay` to test a receiver and forward to Slack and PagerDuty.
- **[Human approval (human-in-the-loop)](guides/hitl-approval.md)**: human sign-off before a tool runs, 1-of-1 or signed m-of-n, provable offline.
- **[Security model](guides/security-model.md)**: the cryptographic guarantees and their exact scope (confidentiality is out of scope).
- **[Key custody](guides/key-custody.md)**: signing keys in a PKCS#11 HSM, AWS KMS, Google Cloud KMS or Azure Key Vault instead of key files, behind `audit.Signer`.
- **[Delegation](guides/delegation.md)**: signed grants, attenuating delegation, and earned authority.
- **[Governance](guides/governance.md)**: convergent governed state (Tier-2), federation, and gsm-backed synthesis and coordination.
- **[Quorum](guides/quorum.md)**: governed k-of-n model agreement, verifiable offline.

## Trust

For security and risk reviewers. Read with the guarantee and the known limitations.

- **[Threat model](trust/threat-model.md)** (evaluator): assets, actors and trust boundaries, and for each threat what bide prevents, what it detects and how, and what it does not address.
- **[Stability and support](trust/stability.md)** (evaluator): the 0.x versioning policy, how breaking changes are announced, journal and proof format versions, what stays verifiable across versions, and the deprecation process.
- **[Comparison](trust/comparison.md)** (evaluator): bide, Temporal, LangGraph, and retries with idempotency keys, on side effects across crashes, human approval, audit evidence and operations, including where the others are stronger.
- **[Operations guidance](trust/operations.md)** (operator): key custody, backups, clocks, and least-privilege Postgres roles.
- **[Retention and archiving](guides/retention.md)** (operator, risk): what to keep and how to archive it so it verifies offline years later, rotating and retiring keys, what deletion does to the proofs, and immutable storage.
- **[Verifying releases](trust/verify-releases.md)** (operator): cosign signatures, SBOMs and reproducible builds.

## Reference

- **[CLI reference](reference/cli.md)**: every `bide` command (`init`, `demo`, `doctor`, `sample`, `view`, `notify`, `approve`, `keygen`, `migrate`, `check`, `audit`, `version`), its flags and its exit statuses, generated from the flag sets.
- **[SDK API reference](reference/api/index.md)**: every exported symbol of every published package, with its declaration, the first sentence of its doc comment and a [stability label](trust/stability.md#package-stability), generated from the Go sources (`go test ./internal/tools/apiref -run TestAPIReference -update`; CI fails when it is stale).
- **[bide-hsm reference](reference/bide-hsm.md)**: `bide-hsm approve` and `pubkey`, for approvers whose keys are in an HSM, on a smart card or in a cloud KMS: the key references, flags and exit statuses.
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

- **[Durable signals](design/design-durable-signals.md)**, **[m-of-n approval](design/design-mofn-approval.md)**, **[Compaction](design/compaction.md)**, **[Chaos benchmark](design/chaos-benchmark.md)**, **[Formal models](design/formal-models.md)** (accepted, in progress), **[The bide protocol](design/protocol.md)** (accepted; not implemented), **[Probes](design/probes.md)** (draft; not implemented), **[Tooling and onboarding](design/tooling-and-onboarding.md)** (accepted; in progress), **[Visual editor](design/visual-editor.md)** (draft; not implemented).
