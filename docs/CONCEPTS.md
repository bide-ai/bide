# Concepts

The vocabulary, defined once. Terms are grouped by the layer they belong to. See the [docs index](README.md) for the guides that use them.

## The durable core

- **Journal**: the append-only log of a run. Every model turn, tool result, and durable step is a named record. Replaying the journal reconstructs the run's state, which is how resume works.
- **Step**: one named, memoized unit of work (`Step`, or the internal tool-result record). A step runs at most once per run: on resume a recorded step returns its stored result without re-executing.
- **At-most-once**: a non-idempotent side effect (a charge, an email, a physical actuation) fires at most once, even across crashes. On an unknown outcome the run halts rather than risk a repeat. Contrast at-least-once systems, which re-run and require idempotent steps.
- **Resume**: re-invoking `Run` with the same run ID replays the journal and continues from where it stopped.
- **ResumeHalt**: returned when a non-retriable side effect was started but its outcome was never recorded. The run stops for confirmation instead of guessing.
- **Reconciler**: what clears a `ResumeHalt` without a human. Most non-idempotent effects still leave a queryable record (a sent-message id, a row); a reconciler queries it, decides, and calls `ResolveHalt` (or `ResolveStepHalt` for a `Step`). `WithMinHaltAge(d)` refuses to resolve a halt younger than `d` (a provider's record can lag the send), returning `HaltTooYoung` so the caller waits; `WithEvidence(v)` marks the injected result `Reconciled` and journals what was read to decide, so a reconciled outcome stays distinguishable from a clean one.
- **Safety**: a tool's declaration of how it may be retried on an unknown-outcome resume. `ReadOnly` (no side effects, always safe), `Idempotent` (safe to repeat), `IdempotencyKey` (derives a stable de-dup key from the args, which also makes an unknown outcome auto-retry instead of halting), and `RequiresApproval` (pause for a durable human decision before the tool runs). Maps onto MCP tool annotations.
- **Retry-safe tool**: a tool that may re-run from the top on an unknown-outcome resume: `Safety.ReadOnly`, `Idempotent`, or one that declares an `IdempotencyKey`. Pausing primitives (`Interrupt`, `Sleep`, `Await`) require one.

## High availability

- **Lease**: a per-run lock so normally only one process drives a run at a time. A crashed holder's lease expires (by the store's clock) and another node takes over. A holder that stalls past its lease can wake still driving; its drive is cancelled with `ErrLeaseLost`, and the attempt claim, not the lease, keeps its side effects at most once.
- **Recover**: after a restart, re-drive in-flight runs, in one pass. `Lister` enumerates a store's runs, `IsComplete` skips finished ones, and the rest resume.
- **RecoverLoop**: `Recover` repeated for the life of a process, so a dead holder's run is taken over automatically once its lease expires.

## Ambient and pauses

- **Sleep / WaitUntil**: durable timers. A run pauses until a wall-clock deadline, journaled so the pause survives a restart.
- **Waker**: the pluggable trigger that re-invokes a sleeping run when its timer is due (or when an event is delivered). `MemWaker` is the in-process reference.
- **Interrupt / Resume**: durable human-in-the-loop. A tool pauses the run to request a typed decision; recording the answer and re-running continues.
- **Approval**: a durable pre-execution gate. A tool marked `Safety.RequiresApproval` halts the run as `*PendingApproval` before it executes; recording `agent.Approve` and re-running lets it proceed or skips it. Distinct from `Interrupt`, which pauses inside a running tool to collect a typed value. See the [approval guide](guides/approval.md).
- **m-of-n approval**: an approval gate that needs k signed decisions from a bounded, named set of n approvers (`Safety.Approval` with an `ApprovalPolicy`). Each approver signs `ApprovalDecisionBytes` over the exact call (run, call id, tool, canonical arguments) and records it with `ApproveAs`. Each distinct decision is its own record, and an approver's first valid one counts (`TallyApprovals`, the rule shared by the gate and the auditor), so a forged or mistaken record never locks an approver out. The gate proceeds at k approvals, denies once k is unreachable, and otherwise pauses with the running `ApprovalTally`; at the outcome it records the tally, including the policy it enforced and every decision it read. `audit.ApprovalEvidence` exports the request, decisions, tally, and result, and `audit.VerifyApprovals` (or `bide-audit verify-approvals`) checks offline that k named approvers approved this call before it ran, detecting an omitted decision, a recount mismatch, or a different enforced policy.
- **Signal / Await**: an external event delivered into a run, journaled at most once. `AwaitFor` races a signal against a durable deadline. Ordered channels (`Send` / `Receive` / `Ack`) give exactly-once streaming consumption. The guarantee: at-least-once transport in, exactly-once application.
- **Saga**: a sequence of steps with compensations that run in reverse order on failure.

## Governed state (Tier-2, gsm)

- **gsm**: the convergence engine underneath governed state. It proves at build time that concurrent agents applying the same events converge to the same valid state.
- **Governed state**: shared state described as a registry of variables, invariants, and events. The governor prevents, repairs, or halts on invariant violations.
- **Convergence**: order-independent agreement on the replayed state, proven, not assumed. `CertifyConvergence` emits a portable `ConfluenceCertificate`, re-checkable offline.
- **Federation**: multiple governed registries connected by morphisms, converging across boundaries (trees, multi-source DAGs, monotone cycles).

## Accountability

- **Merkle tree / STH**: the audit spine. Records hash into a tree; a signed tree head (STH) commits to the whole log. Inclusion and consistency proofs are checkable offline.
- **Anchoring**: publishing an STH to an independent log, which upgrades integrity to tamper-evidence against the operator.
- **ProofBundle**: a portable proof that a specific record is included under a signed tree head.
- **Absence proof**: a checkable proof that no record with a given key exists in the log (`ProveAbsent` / `VerifyAbsence`), so "this never happened" is provable, not just unlogged. Completes the trio with inclusion and consistency proofs.
- **EvidencePackage**: a whole run's evidence in one portable file: a signed tree head plus an inclusion proof per material action, and optionally the run certificate, grant chain, and consistency proof. Built with `audit.Evidence` and checkable offline with `bide-audit verify-evidence`.
- **RunCertificate**: a proof-carrying attestation of behavioral-property compliance over a whole run, checkable offline.
- **Grant / delegation**: a signed capability a sub-agent can only narrow (attenuate). `VerifyDelegationChain` checks the chain offline; `EarnedAuthority` widens scope from a clean trail.
- **Quorum**: governed k-of-n agreement among voters, with the tally anchored and re-checkable offline.
- **Selective disclosure**: revealing some records while proving the rest exist, without showing their content. It limits what you reveal; it does not encrypt. See the [security model](guides/security-model.md).
- **Flow / conformance**: the `plan` package is an optional typed flow builder over the same durable core. A `Flow` is a reified topology of durable steps, authored in typed Go or loaded from declarative config (`plan.Load`); `Topology` and `RenderMermaid` expose its shape, and `Conform` checks a run's journal against it, proving the run followed the declared topology or flagging where it diverged. See the [Flows guide](guides/flows.md).

## Structure

- **Footprint**: the set of invariants (or variables) a tool or event can affect. Disjoint footprints commute, which is what lets governance verification stay local.
- **Port and adapter**: the core defines interfaces (`Model`, `Durable`, `Tool`, `Anchor`, ...); adapters plug in at the edges. The core carries no provider or infrastructure dependency. See [extension points](reference/extension-points.md).
- **Tool-result codec**: `provider.ToolResultCodec` (package `model/provider`, the kit adapters are built from) controls how a tool result is rendered for the model. The default (`provider.JSONToolResultCodec`) passes canonical JSON; `codec/gcf` encodes GCF (Graph Compact Format), a more token-efficient encoding for structured data, wired per adapter with `WithToolResultCodec`. It changes only what the model reads: the journal and audit trail keep the canonical JSON.
