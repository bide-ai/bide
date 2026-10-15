# Roadmap

Where bide is going, in order. Every item below either strengthens a guarantee bide already makes or prepares for bide to be used from more places without weakening it.

This page lists work that is not done yet. It describes intent, not promises of dates. Shipped work is recorded in the [changelog](../CHANGELOG.md), and the formal models already checked are listed in [Formal verification](formal-verification.md). Releases stay on 0.x.

## Next

### The API

- Done: a deadline for a gate that never reaches its approvals (`ApprovalPolicy.Timeout`; the expiry is recorded as the gate's outcome and checked by model 1b), and a stale decision written before a flow gate's request no longer blocks it ([design](design/approval-deadlines.md)).
- Moving the journal format tag from `v1-dev` to its final value, which waits for 1.0.

Changes that touch claims, the journal, leases, sagas or proofs are reviewed adversarially before they merge.

### Bank readiness

bide is meant to be usable by banks and other regulated institutions. What that needs beyond today, first items first:

- Key custody: Ed25519 and ECDSA P-256 audit-log and approver signing keys in a PKCS#11 HSM, AWS KMS or Google Cloud KMS, and ECDSA P-256 keys in Azure Key Vault or Managed HSM, through `audit.Signer` have shipped ([key custody guide](guides/key-custody.md)). The command-line approval tool takes PKCS#11, AWS KMS, Google Cloud KMS and Azure Key Vault keys too (`bide-hsm approve`, with a PKCS#11 URI, a KMS key ARN, a Cloud KMS key version or a Key Vault key identifier). Next: a trial against a hardware HSM.
- Release evidence: every release is signed keylessly with cosign, rebuilt byte for byte on a second operating system, and carries signed SPDX SBOMs for each binary archive and for the published Go module set ([verifying releases](trust/verify-releases.md)). Next: build provenance, SLSA provenance attestations for the release archives that record how and from what each was built.
- Approver identity: approver keys bound to an institution's identity, with certificates from its own CA or keys issued through its SSO, and revocation that the gate and `bide audit` honor.

This is engineering toward those requirements, not a compliance certification.

### Notifications and approvals

The notifier tells a person when a run needs them, routes and escalates, sends an all-clear and can sign its trail; approvers decide with `bide approve`, `bide-hsm approve` or the approve page ([notifications guide](guides/notifications.md), [human approval guide](guides/hitl-approval.md)). Next:

- Passkey (WebAuthn) approvals on the approve page, as a new signature algorithm. Gates for high-value actions should wait for them.

Out of scope: approve buttons in chat that a server signs on an approver's behalf. Keeping each approver's key with that approver is what makes a k-of-n gate k independent decisions.

### Formal models of the coordination protocols

The hardest bugs in a durable runtime live in interleavings: two drivers, a crash between two writes, a write that committed although its caller saw an error. bide checks its protocol designs with TLA+ models ([spec/tla](../spec/tla/README.md)); a counterexample the checker finds becomes a deterministic Go regression test. Left to do:

- **M3 trace validation,** which checks real Go runs against the existing models, so they cannot drift.
- **Leases and recovery, the rest:** model 10 covers lease acquisition, renewal, lapse and takeover. Left, among others: model 1's numbered attempts and remembered claims under a lease that lapses while its holder lives, renewal errors short of a lapse, `Recover` without a `Leaser`, and a recovery pass's concurrency above 1. Leases are not fenced: a stalled holder that wakes drives beside the run's new holder until its renewer notices, a limit model 10 records.
- **Candidates:** the store contract and journal header (prefix-closed visibility and first-writer races); saga rollback across parallel siblings, sub-agents and calls that never started; and how far concurrent sub-agents can overshoot a shared token budget (low priority).
- **Proofs beyond the bounds (Apalache):** the claim model's inductive invariant is checked nightly ([Apalache results](../spec/tla/README.md#apalache-results)). Next: the same for model 9 (tool calls; its typed wrapper is in place, the invariant is not written) and model 10 (the run lifecycle), and checking the claim invariant at more drivers, attempts and claim ids.

The design and plan: [formal models of the coordination protocols](design/formal-models.md).

### gsm convergence

bide's governance tier (`govern`) builds its state machines with [gsm](https://github.com/blackwell-systems/gsm), whose convergence theorem is proved in Coq/Rocq ([known limitations](KNOWN-LIMITATIONS.md#governed-state-gsm)).

- gsm adopting the restructured rules oracle (proved to give the original checker's verdicts in [normalization-confluence#14](https://github.com/blackwell-systems/normalization-confluence/pull/14) and [#15](https://github.com/blackwell-systems/normalization-confluence/pull/15)), with its cost cap re-derived.
- Oracle-certified federations. Today `Federation.Build` and `Certificate.Verify` run every component through the in-process gate, but the federation's own conditions (acyclicity, M1, the resolvers' R1/R2 and the monotone-cycle iteration) are checked by gsm's Go code, not by a checker generated from the proof. The plan: mechanize the federation theorems those checks rely on (the operator core, order-independence and the monotone fixed point are mechanized; the federated and resolved convergence theorems in their corrected form, under C1 and C2, and the authority lemma are mechanized in `FederationGRS.v`, with C1 and C2 exact at reachable witnesses in `FederationEventsConverse.v`; the necessity of acyclicity and M1, and R1/R2 as the gluing axiom with the sheaf assembly over the full cover, remain paper-level), prove a federation checker sound against them, extract it to Go like the table oracle, and make `Federation.Build` and `Certificate.Verify` fail closed unless it certifies the federation. Then every machine and every federation gsm returns is certified by a checker generated from the proof.
- Before 1.0, a change to gsm's policy format that puts names in the policy bytes, with `bide audit` still verifying records written under the old format (adding variable kinds to the policy bytes is under consideration).

### bide underneath other agent frameworks (Go)

bide is a durability and accountability layer, not only a framework of its own. Go teams using other agent frameworks can keep them and put bide under the dangerous parts: wrapping a tool's side effect in `agent.Journal.Step` gives it bide's guarantees (claimed before it runs, recorded after, halted on an ambiguous outcome, provable afterwards) without changing the rest of the framework. The benchmark harness already drives Google's ADK for Go, trpc-agent-go, Eino and langchaingo this way.

- Worked examples and a guide for using bide under ADK for Go, Eino, trpc-agent-go and langchaingo, stating exactly what bide guards (each wrapped side effect, approvals, proofs) and what it does not (the framework's own loop resumes as it always did).
- Where a framework exposes a pluggable persistence hook (a session, checkpoint or memory service), a bide-backed implementation, so the framework's own state is journaled and its resume becomes exact.

### The visual builder

A visual builder for `plan` flows, built up from the read-only tools that exist today (`bide view`, `bide view -config`, `bide check -diff`; [run viewer guide](guides/run-viewer.md)):

- A GitHub Action that posts a config diff on a pull request, and an HTML export of a page.
- Later: an editor that writes the config.

## Later: bide beyond Go

### The bide protocol

A versioned wire protocol ([design](design/protocol.md), accepted; not implemented) that lets code in other languages use bide without reimplementing its guarantees. The Go engine stays the only writer of the journal, claims and proofs; other languages run tools and answer pauses. It supports long-lived workers and serverless functions, and comes with a conformance suite that every SDK must pass. Its claim rules are checked by a formal model (model 2).

### Python and TypeScript SDKs

Thin SDKs over the bide protocol, so agents written in Python and TypeScript get the same guarantees the Go engine enforces, plus a pip-installable verifier (the counterpart of `bide audit`) so Python teams can verify bide proofs directly. They follow the protocol's implementation.

### Adapters for Python and TypeScript agent frameworks

The same idea as for Go, across languages: adapters that route the tool calls of agents built with existing frameworks (for example LangGraph, the OpenAI Agents SDK and CrewAI in Python; the Vercel AI SDK, Mastra and LangChain.js in TypeScript) through bide, and bide-backed implementations of those frameworks' persistence hooks where they have one. Teams keep the framework they chose and gain at-most-once side effects, human approval and verifiable proofs.
