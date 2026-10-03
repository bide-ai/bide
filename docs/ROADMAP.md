# Roadmap

Where bide is going, in order. The priority is to harden what exists before adding breadth: every item below either strengthens a guarantee bide already makes or prepares for bide to be used from more places without weakening it.

This page describes intent, not promises of dates. Shipped work is recorded in the [changelog](../CHANGELOG.md).

## Now: v0.10.0

- The Run API under transitional names (P14): a `Message` input, per-run options journaled in `run:start` that hold for every later drive, recovery included, `Cancel` and `Status`, a per-run tool filter enforced at dispatch, and recovery dispatch through `Resumer` (`ResumeAgent`, `ResumeTyped`).
- Tool internals on a tool specification (P12): `ToolSpec`, the approval gate split from `Safety`, and tool timeouts.
- Construction under `Build` (P13): option scopes checked by the compiler, `RunInfo`, and programmatic sub-runs that a saga's rollback reaches.
- A lapsed lease taken over within about one recovery interval, however many halted runs the store holds.
- Nine TLA+ models in CI, four of them new (models 9 to 12): the tool-call state machine, the run lifecycle and recovery, delegation and saga trees, and sessions. Every bug the new models found is fixed and kept as a regression configuration, and CI keeps the marked Go code and the models in step.
- The claim protocol's safety properties proved at any depth by an inductive invariant that Apalache checks nightly, for two drivers, bounded attempts and claim ids, and without the approval gate (scope under [Proofs beyond the bounds](#formal-models-of-the-coordination-protocols) below).
- The gsm machine gate: every gsm machine the governance examples build is checked in CI by the two checkers extracted from gsm's proof.
- bide on gsm v0.12.0, whose `Build` runs the oracles generated from gsm's proof in-process before it returns a machine.

Shipped earlier: v0.9.0 put the claim protocol under a model checker and rebuilt the engine around a storage port and a journal with its own format; v0.8.0 made runs recover themselves (`RecoverLoop`) and put one token budget across an agent tree.

## Next: toward 1.0

### A stable API

The pre-1.0 API redesign ([design proposal](https://github.com/bide-ai/bide/pull/64)) settles the shape bide will keep at 1.0:

- Done in v0.9.0: a storage port (`Store`) separated from the journal semantics bide owns (`Journal`), with the store contract stated as numbered requirements and a conformance suite every store must pass (P6a); a sealed pause contract (P10); typed model calls (P9); flow nodes lowered onto `agent.Step` (P5b); and versioned journal and proof formats (P6a, P11).
- Done in v0.10.0: tool internals on a tool specification, with approval split from safety and tool timeouts (P12); construction under `Build`, with option scopes and `RunInfo` (P13); and one run entry point with per-run options that survive recovery, `Cancel`, `Status` and recovery dispatch, under transitional names (P14).
- In progress (P15, unreleased): the consolidated rewrite that gives the transitional API its final names and removes the old one (the `Durable` interface, the string entry points, the builder methods, the old `Tool` method set), with a migration tool (`internal/tools/migrate`) that rewrites code written against v0.10.0. P16 (docs and cleanup) follows.
- Go 1.27 generic methods where a generic operation has a natural receiver.

Changes that touch claims, the journal, leases, sagas or proofs are reviewed adversarially before they merge.

### Formal models of the coordination protocols

The hardest bugs in a durable runtime live in interleavings: two drivers, a crash between two writes, a write that committed although its caller saw an error. bide already tests these with crash sweeps, a reference model and multi-process harnesses. The next step is to check the protocol designs themselves, exhaustively within bounds, with TLA+ (written in PlusCal) and the TLC model checker:

1. **Claims and attempts:** claims, not-started records, numbered retries, the resume gate, and halt resolution while a driver may still be live. Invariant: every side effect fires at most once. Done: [model 1](../spec/tla/README.md#model-1-claims-and-attempts).
2. **The bide protocol,** before any SDK is built on it (see below): done, [spec/tla/protocol](../spec/tla/README.md#model-2-the-bide-protocols-claim-rules), including retry-safe re-dispatch.
3. **Leases and recovery:** acquire, renew, release, takeover, and a holder that stalls past its lease. Invariant: safety holds with leases failing arbitrarily, because it rests on claims. Model 10 covers the lease and recovery part: lease acquisition, renewal, lapse and takeover, a holder that stalls past its TTL and wakes, `Recover` and `RecoverLoop` passes, and the lease the operator's live-driver check takes, under crashes and ambiguous writes, with each call firing at most once and one live lease holder per epoch. Left, among others: model 1's numbered attempts and remembered claims under a lease that lapses while its holder lives (model 10 reduces each call's claim to one marker, and model 1 holds a lease for a whole drive), renewal errors short of a lapse, `Recover` without a `Leaser`, and a recovery pass's concurrency above 1. Leases are not fenced: a stalled holder that wakes drives beside the run's new holder until its renewer notices, a limit model 10 records.
4. **The store contract and journal header:** prefix-closed visibility and first-writer races.
5. **Saga rollback:** parallel siblings, sub-agents and calls that never started.
6. **Approval and halt resolution,** an extension of model 1: 1-of-1 and m-of-n tallies, final denials, contended and crashed halts, resolution while a driver may be live, and approvers' key sets. Done: [model 1b](../spec/tla/README.md#model-1b-the-approval-gate).
7. **Sessions:** concurrent sends, turn ordering, starting points, crashes between and within turns, and P14's `Cancel` of a turn's run. Done: [model 12](../spec/tla/README.md#model-12-sessions); it found S1, S2 and S4 (fixed in [#137](https://github.com/bide-ai/bide/pull/137)) and S3 (its rule implemented by P14, [#138](https://github.com/bide-ai/bide/pull/138)).
8. **Flow semantics:** switch and loop replay, flow completion and per-iteration step scoping. Done: [model 7](../spec/tla/README.md#model-7-flow-semantics), with the lowering of #103.
9. **The whole-tree budget:** how far concurrent sub-agents can overshoot a shared token budget (low priority). The spend accounting of model calls that the budget counts is done: [model 8](../spec/tla/README.md#model-8-spend-accounting), with #104.
10. **The tool-call state machine of P12:** tool middleware, retries, sibling calls and the saga rollback's re-run. Done: [model 9](../spec/tla/README.md#model-9-the-tool-call-state-machine); it found T1 to T6 in #117 before it merged.
11. **The run lifecycle and recovery:** end markers, leases, `Recover` and `RecoverLoop`, and the bounded pickup of a dead holder's run. Done: [model 10](../spec/tla/README.md#model-10-the-run-lifecycle-and-recovery); it found L1 (fixed in #126), L2 and L3, and, extended for P14 in #129, L4 to L7. P14 ([#138](https://github.com/bide-ai/bide/pull/138)) implements the rules adopted for L2 to L7, each tested by a Go test, and each finding is kept as a regression configuration that still fails under its old rule.
12. **Delegation and sub-run authority, including saga trees:** grants, recorded authority, rollback binding, halts propagating from sub-runs, and programmatic sub-runs. Done: [model 11](../spec/tla/README.md#model-11-delegation-sub-run-authority-and-saga-trees); it found D1 to D3, fixed in [#133](https://github.com/bide-ai/bide/pull/133).

Next: **M3 trace validation,** which checks real Go runs against the existing models, so they cannot drift.

The store contract and the whole-tree budget bound remain candidates.

**Proofs beyond the bounds (Apalache).** TLC's results hold only within each configuration's bounds. The claim model also has an inductive invariant, checked nightly by Apalache, that proves, for two drivers over two processes, `AtMostOnce` and `NotStartedExclusive` on one call (attempts 0..3, 8 claim ids) and all four of `AtMostOnce`, `NotStartedExclusive`, `NoLiveOverride` and `AtMostOncePerIntent` with halt resolution and the caller's second call (attempts 0..3, 6 claim ids), without the approval gate, and, under the lease check, assuming no plain run holds the live attempt at the check (`PlainRunIdleAtCheck`), at any depth and for any number and mix of faults within the run's 8 (6) claim ids and attempts 0..3 (claim ids are never reused, so this bounds the number of claims) ([Apalache results](../spec/tla/README.md#apalache-results)). Next: the same for model 9 (tool calls; its typed wrapper is in place, the invariant is not written) and model 10 (the run lifecycle), and checking the claim invariant at more drivers, attempts and claim ids.

Models live in the repository and run in CI. A counterexample the checker finds becomes a deterministic Go regression test.

The design and plan: [formal models of the coordination protocols](design/formal-models.md) (accepted, in progress). Done: models 1, 1b, 2, 7, 8, 9, 10, 11 and 12 are in [spec/tla](../spec/tla/README.md), checked by TLC on every pull request that changes them (all of them when it changes `check.sh`, `tools.lock`, the shard script or the Models workflow), and all of them in the merge queue and on main. The code the models describe is marked, and CI fails a change to it that does not change its model or say why. The overview, with every bug the models caught, is [Formal verification](formal-verification.md).

### gsm convergence

bide's governance tier (`govern`) builds its state machines with [gsm](https://github.com/blackwell-systems/gsm), whose convergence theorem is proved in Coq/Rocq. bide requires gsm v0.12.0, the release that carries the gsm work below ([known limitations](KNOWN-LIMITATIONS.md#governed-state-gsm)).

- Done in gsm: `Build` soundness fixes: the commute check accounts for what guards and effects read, and certificates are re-checked ([gsm#2](https://github.com/blackwell-systems/gsm/pull/2)); duplicate event and variable names are refused ([gsm#5](https://github.com/blackwell-systems/gsm/pull/5)); closure results that are not states of the machine are refused ([gsm#6](https://github.com/blackwell-systems/gsm/pull/6)); declared rules copy the caller's slices, and `Certify` certifies the federation as called ([gsm#7](https://github.com/blackwell-systems/gsm/pull/7)); and hardening for unknown registries, coordination points, enum labels, ports, digest names and `DiagnoseCycle` ([gsm#11](https://github.com/blackwell-systems/gsm/pull/11)).
- Done in gsm: an in-process gate. `Build`, synthesis and `BuildCompositional` return a machine only once the table oracle, Go generated from the Rocq proof, certifies its tables (per component for `BuildCompositional`, where independence across components rests on gsm's footprint check) ([gsm#12](https://github.com/blackwell-systems/gsm/pull/12)); `Build` also runs the rules oracle, generated the same way, when the machine's rules are combinators inside the oracle's fragment and its work is within a cost cap, and reports why when it does not ([gsm#17](https://github.com/blackwell-systems/gsm/pull/17)).
- Done in bide: **gsm machine gate**, a required CI check that runs the two checkers extracted from gsm's proof on every gsm machine bide's examples build ([#144](https://github.com/bide-ai/bide/pull/144)), against a pinned gsm commit: now the v0.12.0 release commit, which carries both in-process oracles ([#150](https://github.com/bide-ai/bide/pull/150), [#151](https://github.com/bide-ai/bide/pull/151), [#154](https://github.com/bide-ai/bide/pull/154)). It checks the examples' machines in CI, not the machines an application builds at runtime.
- Done in bide: bide requires gsm v0.12.0, so a `Build` in bide's runtime runs the in-process gate ([#154](https://github.com/bide-ai/bide/pull/154)).
- Next: in gsm, restructuring the rules oracle to work from tables (in progress), which is expected to lift its cost cap; and, before 1.0, a change to gsm's policy format that puts names in the policy bytes, with `bide-audit` still verifying records written under the old format (adding variable kinds to the policy bytes is under consideration).

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
