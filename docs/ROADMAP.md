# Roadmap

Where bide is going, in order. The priority is to harden what exists before adding breadth: every item below either strengthens a guarantee bide already makes or prepares for bide to be used from more places without weakening it.

This page describes intent, not promises of dates. Shipped work is recorded in the [changelog](../CHANGELOG.md).

## Now: v0.8.0

- Runs that recover themselves (`RecoverLoop`), one token budget across an agent tree, exact replay of usage and spend.
- Every library module installable with `go get`, released together with the core.
- Journaling throughput back above v0.7.0, with the published numbers re-measured on a standard CI runner.

## Next: toward 1.0

### A stable API

The pre-1.0 API redesign ([design proposal](https://github.com/bide-ai/bide/pull/64)) settles the shape bide will keep at 1.0:

- A storage port (`Store`) separated from the journal semantics bide owns (`Journal`), with the store contract stated as numbered requirements and a conformance suite every store must pass.
- One run entry point with per-run options that survive recovery, a sealed pause contract, a tool specification type, and versioned journal and proof formats.
- Go 1.27 generic methods where a generic operation has a natural receiver.

Changes that touch claims, the journal, leases, sagas or proofs are reviewed adversarially before they merge.

### Formal models of the coordination protocols

The hardest bugs in a durable runtime live in interleavings: two drivers, a crash between two writes, a write that committed although its caller saw an error. bide already tests these with crash sweeps, a reference model and multi-process harnesses. The next step is to check the protocol designs themselves, exhaustively within bounds, with TLA+ (written in PlusCal) and the TLC model checker:

1. **Claims and attempts:** claims, not-started records, numbered retries, the resume gate, and halt resolution while a driver may still be live. Invariant: every side effect fires at most once.
2. **The bide protocol,** before any SDK is built on it (see below).
3. **Leases and recovery:** acquire, renew, release, takeover, and a holder that stalls past its lease. Invariant: safety holds with leases failing arbitrarily, because it rests on claims.
4. **The store contract and journal header:** prefix-closed visibility and first-writer races.
5. **Saga rollback:** parallel siblings, sub-agents and calls that never started.

Models live in the repository and run in CI. A counterexample the checker finds becomes a deterministic Go regression test.

The design and plan: [formal models of the coordination protocols](design/formal-models.md) (accepted, in progress). Started: model 1 (claims and attempts) is in [spec/tla](../spec/tla/README.md) and checked on every pull request. Next: trace validation, so the Go test suites check that the code implements the model.

### bide underneath other agent frameworks (Go)

bide is a durability and accountability layer, not only a framework of its own. Go teams using other agent frameworks can keep them and put bide under the dangerous parts: wrapping a tool's side effect in `agent.Step` gives it bide's guarantees (claimed before it runs, recorded after, halted on an ambiguous outcome, provable afterwards) without changing the rest of the framework. The benchmark harness already drives Google's ADK for Go, trpc-agent-go, Eino and langchaingo this way.

- Worked examples and a guide for using bide under ADK for Go, Eino, trpc-agent-go and langchaingo, stating exactly what bide guards (each wrapped side effect, approvals, proofs) and what it does not (the framework's own loop resumes as it always did).
- Where a framework exposes a pluggable persistence hook (a session, checkpoint or memory service), a bide-backed implementation, so the framework's own state is journaled and its resume becomes exact.

## Later: bide beyond Go

### The bide protocol

A versioned wire protocol ([design proposal](https://github.com/bide-ai/bide/pull/95)) that lets code in other languages use bide without reimplementing its guarantees. The Go engine stays the only writer of the journal, claims and proofs; other languages run tools and answer pauses. It supports long-lived workers and serverless functions, and comes with a conformance suite that every SDK must pass.

### Python and TypeScript SDKs

Thin SDKs over the bide protocol, so agents written in Python and TypeScript get the same guarantees the Go engine enforces, plus a pip-installable `bide-audit` so Python teams can verify bide proofs directly.

### Adapters for Python and TypeScript agent frameworks

The same idea as for Go, across languages: adapters that route the tool calls of agents built with existing frameworks (for example LangGraph, the OpenAI Agents SDK and CrewAI in Python; the Vercel AI SDK, Mastra and LangChain.js in TypeScript) through bide, and bide-backed implementations of those frameworks' persistence hooks where they have one. Teams keep the framework they chose and gain at-most-once side effects, human approval and verifiable proofs.
