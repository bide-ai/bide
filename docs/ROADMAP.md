# Roadmap

Where bide is going, in order. The priority is to harden what exists before adding breadth: every item below either strengthens a guarantee bide already makes or prepares for bide to be used from more places without weakening it.

This page describes intent, not promises of dates. Shipped work is recorded in the [changelog](../CHANGELOG.md).

## Now: v0.8.0

- Runs that recover themselves (`RecoverLoop`), one token budget across an agent tree, exact replay of usage and spend.
- Every library module installable with `go get`, released together with the core.
- Journaling throughput back above v0.7.0, with the published numbers re-measured on a standard CI runner.

## Next: toward 1.0

### A stable API

The pre-1.0 API redesign ([design proposal](design/api-v1.md)) settles the shape bide will keep at 1.0:

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

## Later: bide beyond Go

### The bide protocol

A versioned wire protocol ([design proposal](design/protocol.md)) that lets code in other languages use bide without reimplementing its guarantees. The Go engine stays the only writer of the journal, claims and proofs; other languages run tools and answer pauses. It supports long-lived workers and serverless functions, and comes with a conformance suite that every SDK must pass.

### Python and TypeScript SDKs

Thin SDKs over the bide protocol, plus adapters that bring bide's guarantees to agents built with existing Python and TypeScript frameworks. A pip-installable `bide-audit` lets Python teams verify bide proofs directly.
