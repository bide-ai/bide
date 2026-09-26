# Concepts

The vocabulary, defined once. Terms are grouped by the layer they belong to. See the [docs index](README.md) for the guides that use them.

## The durable core

- **Journal**: the append-only log of a run. Every model turn, tool result, and durable step is a named record. Replaying the journal reconstructs the run's state, which is how resume works.
- **Step**: one named, memoized unit of work (`Step`, or the internal tool-result record). A step runs at most once per run: on resume a recorded step returns its stored result without re-executing.
- **At-most-once**: a non-idempotent side effect (a charge, an email, a physical actuation) fires at most once, even across crashes. On an unknown outcome the run halts rather than risk a repeat. Contrast at-least-once systems, which re-run and require idempotent steps.
- **Resume**: re-invoking `Run` with the same run ID replays the journal and continues from where it stopped.
- **ResumeHalt**: returned when a non-retriable side effect was started but its outcome was never recorded. The run stops for confirmation instead of guessing.
- **Retry-safe tool**: a tool marked `Safety.ReadOnly` or `Idempotent`, which may re-run from the top on resume. Pausing primitives (`Interrupt`, `Sleep`, `Await`) require one.

## High availability

- **Lease**: a per-run lock so only one process drives a run at a time. A crashed holder's lease expires (by the store's clock) and another node takes over. No run is double-driven.
- **Recover**: after a restart, re-drive in-flight runs. `Lister` enumerates a store's runs, `IsComplete` skips finished ones, and the rest resume.

## Ambient and pauses

- **Sleep / WaitUntil**: durable timers. A run pauses until a wall-clock deadline, journaled so the pause survives a restart.
- **Waker**: the pluggable trigger that re-invokes a sleeping run when its timer is due (or when an event is delivered). `MemWaker` is the in-process reference.
- **Interrupt / Resume**: durable human-in-the-loop. A tool pauses the run to request a typed decision; recording the answer and re-running continues.
- **Signal / Await**: an external event delivered into a run, journaled at most once. `AwaitFor` races a signal against a durable deadline. Ordered channels (`Send` / `Receive` / `Ack`) give exactly-once streaming consumption. The guarantee: at-least-once transport in, exactly-once application.
- **Saga**: a sequence of steps with compensations that run in reverse order on failure.

## Governed state (Tier-2, gsm)

- **gsm**: the convergence engine underneath governed state. It proves at build time that concurrent agents applying the same events converge to the same valid state.
- **Governed state**: shared state described as a registry of variables, invariants, and events. The governor prevents, repairs, or halts on invariant violations.
- **Convergence**: order-independent agreement on the replayed state, proven, not assumed.
- **Federation**: multiple governed registries connected by morphisms, converging across boundaries (trees, multi-source DAGs, monotone cycles).

## Accountability

- **Merkle tree / STH**: the audit spine. Records hash into a tree; a signed tree head (STH) commits to the whole log. Inclusion and consistency proofs are checkable offline.
- **Anchoring**: publishing an STH to an independent log, which upgrades integrity to tamper-evidence against the operator.
- **ProofBundle**: a portable proof that a specific record is included under a signed tree head.
- **RunCertificate**: a proof-carrying attestation of behavioral-property compliance over a whole run, checkable offline.
- **Grant / delegation**: a signed capability a sub-agent can only narrow (attenuate). `VerifyDelegationChain` checks the chain offline; `EarnedAuthority` widens scope from a clean trail.
- **Quorum**: governed k-of-n agreement among voters, with the tally anchored and re-checkable offline.
- **Selective disclosure**: revealing some records while proving the rest exist, without showing their content. It limits what you reveal; it does not encrypt. See the [security model](guides/security-model.md).
- **Flow / conformance**: a `plan` flow (rung 1) is a reified, authored topology of durable steps; `Conform` checks a run's journal against it, proving the run followed the declared graph or flagging where it diverged. See the [Flows guide](guides/flows.md).

## Structure

- **Footprint**: the set of invariants (or variables) a tool or event can affect. Disjoint footprints commute, which is what lets governance verification stay local.
- **Port and adapter**: the core defines interfaces (`Model`, `Durable`, `Tool`, `Anchor`, ...); adapters plug in at the edges. The core carries no provider or infrastructure dependency. See [extension points](reference/extension-points.md).
