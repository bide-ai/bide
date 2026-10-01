# Roadmap

Where bide is going, in order. The priority is to harden what exists before adding breadth: every item below either strengthens a guarantee bide already makes or prepares for bide to be used from more places without weakening it.

This page describes intent, not promises of dates. Shipped work is recorded in the [changelog](../CHANGELOG.md).

## Now: v0.9.0

- The claim protocol, the approval gate, flow semantics, spend accounting and the bide protocol's claim rules as TLA+ models, checked by TLC on every pull request.
- The redesign's Waves 1 to 3: the `Store` port and `Journal` with a journal format header, the sealed pause contract, `ModelCall` and `ModelResponse` with exact spend accounting, `plan` nodes lowered onto `agent.Step`, and proofs over raw stored bytes with signature agility.
- Approval seats counted per signing key (`KeyIDs`), with weak Ed25519 keys refused.
- The bide protocol accepted as the design for SDKs in other languages.
- Postgres stores whose writes are single statements holding no lock between round trips, with every name qualified and the schema pinned by `WithSchema`.

Shipped earlier: v0.8.0 made runs recover themselves (`RecoverLoop`), put one token budget across an agent tree and made every library module installable with `go get`.

## Next: toward 1.0

### A stable API

The pre-1.0 API redesign ([design proposal](https://github.com/bide-ai/bide/pull/64)) settles the shape bide will keep at 1.0:

- Done in v0.9.0: a storage port (`Store`) separated from the journal semantics bide owns (`Journal`), with the store contract stated as numbered requirements and a conformance suite every store must pass (P6a); a sealed pause contract (P10); typed model calls (P9); flow nodes lowered onto `agent.Step` (P5b); and versioned journal and proof formats (P6a, P11).
- Next (P12 to P16): tool internals, construction under `Build`, one run entry point with per-run options that survive recovery, a tool specification type, and the consolidated rewrite that removes the transitional names.
- Go 1.27 generic methods where a generic operation has a natural receiver.

Changes that touch claims, the journal, leases, sagas or proofs are reviewed adversarially before they merge.

### Formal models of the coordination protocols

The hardest bugs in a durable runtime live in interleavings: two drivers, a crash between two writes, a write that committed although its caller saw an error. bide already tests these with crash sweeps, a reference model and multi-process harnesses. The next step is to check the protocol designs themselves, exhaustively within bounds, with TLA+ (written in PlusCal) and the TLC model checker:

1. **Claims and attempts:** claims, not-started records, numbered retries, the resume gate, and halt resolution while a driver may still be live. Invariant: every side effect fires at most once. Done: [model 1](../spec/tla/README.md#model-1-claims-and-attempts).
2. **The bide protocol,** before any SDK is built on it (see below): done, [spec/tla/protocol](../spec/tla/README.md#model-2-the-bide-protocols-claim-rules), including retry-safe re-dispatch.
3. **Leases and recovery:** acquire, renew, release, takeover, and a holder that stalls past its lease. Invariant: safety holds with leases failing arbitrarily, because it rests on claims.
4. **The store contract and journal header:** prefix-closed visibility and first-writer races.
5. **Saga rollback:** parallel siblings, sub-agents and calls that never started.
6. **Approval and halt resolution,** an extension of model 1: 1-of-1 and m-of-n tallies, final denials, contended and crashed halts, resolution while a driver may be live, and approvers' key sets. Done: [model 1b](../spec/tla/README.md#model-1b-the-approval-gate).
7. **Sessions:** concurrent sends, turn ordering, starting points and crashes between turns, when the session code next changes.
8. **Flow semantics:** switch and loop replay, flow completion and per-iteration step scoping. Done: [model 7](../spec/tla/README.md#model-7-flow-semantics), with the lowering of #103.
9. **The whole-tree budget:** how far concurrent sub-agents can overshoot a shared token budget (low priority). The spend accounting of model calls that the budget counts is done: [model 8](../spec/tla/README.md#model-8-spend-accounting), with #104.
10. **The tool-call state machine of P12:** tool middleware, retries, sibling calls and the saga rollback's re-run. Done: [model 9](../spec/tla/README.md#model-9-the-tool-call-state-machine); it found T1 to T6 in #117 before it merged.
11. **The run lifecycle and recovery:** end markers, leases, `Recover` and `RecoverLoop`, and the bounded pickup of a dead holder's run. Done: [model 10](../spec/tla/README.md#model-10-the-run-lifecycle-and-recovery); it found L1 (fixed in #126) and L2 and L3 (open until P14).

Next, in order:

1. **Delegation and sub-run authority, including saga trees:** grants, recorded authority, rollback binding, halts propagating from sub-runs, and links to programmatic sub-runs. Most of P12's and P13's late bugs were here, and model 9 treats a delegation as a black box. Next up.
2. **Model 10 extended for P14:** `Cancel`, `Status` and the per-run tool filter, with L2 and L3 as its open findings. It gates P14 the way model 9 gated P12.
3. **Sessions:** multiple turns, resumes and shared history. Built when P14 or later work touches sessions.
4. **M3 trace validation:** checks real Go runs against the existing models, so they cannot drift.

The store contract and the whole-tree budget bound remain candidates.

Models live in the repository and run in CI. A counterexample the checker finds becomes a deterministic Go regression test.

The design and plan: [formal models of the coordination protocols](design/formal-models.md) (accepted, in progress). Done: models 1, 1b, 2, 7, 8, 9 and 10 are in [spec/tla](../spec/tla/README.md) and checked on every pull request. The code the models describe is marked, and CI fails a change to it that does not change its model or say why. The overview, with every bug the models caught, is [Formal verification](formal-verification.md).

### bide underneath other agent frameworks (Go)

bide is a durability and accountability layer, not only a framework of its own. Go teams using other agent frameworks can keep them and put bide under the dangerous parts: wrapping a tool's side effect in `agent.Step` gives it bide's guarantees (claimed before it runs, recorded after, halted on an ambiguous outcome, provable afterwards) without changing the rest of the framework. The benchmark harness already drives Google's ADK for Go, trpc-agent-go, Eino and langchaingo this way.

- Worked examples and a guide for using bide under ADK for Go, Eino, trpc-agent-go and langchaingo, stating exactly what bide guards (each wrapped side effect, approvals, proofs) and what it does not (the framework's own loop resumes as it always did).
- Where a framework exposes a pluggable persistence hook (a session, checkpoint or memory service), a bide-backed implementation, so the framework's own state is journaled and its resume becomes exact.

## Later: bide beyond Go

### The bide protocol

A versioned wire protocol ([design](design/protocol.md), accepted; not implemented) that lets code in other languages use bide without reimplementing its guarantees. The Go engine stays the only writer of the journal, claims and proofs; other languages run tools and answer pauses. It supports long-lived workers and serverless functions, and comes with a conformance suite that every SDK must pass. Its claim rules are checked by a formal model (model 2). Implementation follows the engine hardening of the pre-1.0 redesign.

### Python and TypeScript SDKs

Thin SDKs over the bide protocol, so agents written in Python and TypeScript get the same guarantees the Go engine enforces, plus a pip-installable `bide-audit` so Python teams can verify bide proofs directly. They follow the engine hardening and the protocol's implementation.

### Adapters for Python and TypeScript agent frameworks

The same idea as for Go, across languages: adapters that route the tool calls of agents built with existing frameworks (for example LangGraph, the OpenAI Agents SDK and CrewAI in Python; the Vercel AI SDK, Mastra and LangChain.js in TypeScript) through bide, and bide-backed implementations of those frameworks' persistence hooks where they have one. Teams keep the framework they chose and gain at-most-once side effects, human approval and verifiable proofs.
