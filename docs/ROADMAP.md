# Roadmap

Where bide is going, in order. Every item below either strengthens a guarantee bide already makes or prepares for bide to be used from more places without weakening it.

This page lists only work that is not done yet. It describes intent, not promises of dates. Shipped work is recorded in the [changelog](../CHANGELOG.md), and the formal models already checked are listed in [Formal verification](formal-verification.md). Releases stay on 0.x.

## Next

### The API

- Moving the journal format tag from `v1-dev` to its final value, which waits for 1.0.

Changes that touch claims, the journal, leases, sagas or proofs are reviewed adversarially before they merge.

### Bank readiness

bide is meant to be usable by banks and other regulated institutions. What that needs beyond today, first items first:

- Approver identity: approver keys bound to an institution's identity, with certificates from its own CA, revocation that the gate and `bide audit` honor, and the approver's identity shown in `bide view` and `bide approve -list` ([design](design/approver-identity.md)). Shipped: gates accept certificate-bound approvers (v0.19.0: the counting rule, the two-tier revocation check before a gated call runs, `bide approve -cert`, the identity in `bide view` and `bide approve -list`), and `crlrefresh` with `bide identity fetch-crl` keeps their CRLs current (v0.20.0); `bide-hsm csr` makes the certificate request for a key in an HSM or a KMS, signed by that key (unreleased), and `bide identity check` (unreleased) is the pre-flight: it says whether a certificate-bound setup will work and, if not, why (the policy loads, the pinned CAs and intermediates match, the CRLs are reachable and fresh enough for the declared ages, the policy file and `-params` agree with the gate's recorded policy member by member or by policy digest, and whether the gate would accept a given approver certificate), by running the gate's own code; see the [certificate approvers guide](guides/certificate-approvers.md). Left: the RFC 3161 verifier (`audit/rfc3161`) exists but no gate, endpoint or command calls it yet, so an independent time source is not available; a `-params` file that states every member the policy digest covers makes the digest comparable with the gate's, but a gate's `independent_time` cannot be declared, and the files written before those members existed stay comparable only member by member; keys issued through an institution's SSO come after.
- Kill switch: stop every agent, or one tool, flow or agent, at once, as a pause that resumes when the stop is lifted ([design](design/kill-switch.md)). Basic mode (no keys) and audited mode (signed records, a lift quorum, an anchored trail). Shipped so far (unreleased): the control run and its fold with model 16 (`agent.StopControl`), and the engine's holds (`agent.StopSwitch`, `*Stopped`, recovery) ([guide](guides/kill-switch.md)). Plan flows are checked too (`Builder.WithStopSwitch`), and an audited control writes a signed, anchored stop trail that `bide audit stops` verifies. Left: `bide stop`, the `bide view` banner and notifications.
- Key custody: a trial against a hardware HSM ([key custody guide](guides/key-custody.md)).
- Anchoring: the S3 Object Lock anchor ships ([anchoring](guides/anchoring.md)). Still open: a one-time run of it against real AWS (the [runbook](guides/anchoring.md#trying-it-against-aws)), an RFC 3161 timestamp-authority anchor, and a per-trail minimum interval between anchored heads, so that a busy trail does not leave one retained object per write.

This is engineering toward those requirements, not a compliance certification.

### Notifications and approvals

- Passkey (WebAuthn) approvals on the approve page, as a new signature algorithm. Gates for high-value actions should wait for them.

Out of scope: approve buttons in chat that a server signs on an approver's behalf. Keeping each approver's key with that approver is what makes a k-of-n gate k independent decisions.

### Formal models of the coordination protocols

bide checks its protocol designs with TLA+ models ([spec/tla](../spec/tla/README.md)); a counterexample the checker finds becomes a deterministic Go regression test. Left to do:

- **M3 trace validation,** which checks real Go runs against the existing models, so they cannot drift.
- **Leases and recovery, the rest:** model 1's numbered attempts and remembered claims under a lease that lapses while its holder lives, renewal errors short of a lapse, `Recover` without a `Leaser`, and a recovery pass's concurrency above 1. Leases are not fenced: a stalled holder that wakes drives beside the run's new holder until its renewer notices, a limit model 10 records.
- **Candidates:** the store contract and journal header (prefix-closed visibility and first-writer races); saga rollback across parallel siblings, sub-agents and calls that never started; and how far concurrent sub-agents can overshoot a shared token budget (low priority).
- **Proofs beyond the bounds (Apalache):** inductive invariants for model 9 (tool calls; its typed wrapper is in place, the invariant is not written) and model 10 (the run lifecycle), and checking the claim invariant at more drivers, attempts and claim ids.

The design and plan: [formal models of the coordination protocols](design/formal-models.md).

### gsm convergence

bide's governance tier (`govern`) builds its state machines with [gsm](https://github.com/blackwell-systems/gsm), whose convergence theorem is proved in Coq/Rocq ([known limitations](KNOWN-LIMITATIONS.md#governed-state-gsm)).

- gsm adopting the restructured rules oracle (proved to give the original checker's verdicts in [normalization-confluence#14](https://github.com/blackwell-systems/normalization-confluence/pull/14) and [#15](https://github.com/blackwell-systems/normalization-confluence/pull/15)), with its cost cap re-derived.
- Oracle-certified federations. Today the federation's own conditions (acyclicity, M1, the resolvers' R1/R2 and the monotone-cycle iteration) are checked by gsm's Go code, not by a checker generated from the proof. Left: mechanize what remains paper-level (the necessity of acyclicity and M1, and R1/R2 as the gluing axiom with the sheaf assembly over the full cover), prove a federation checker sound against the mechanized theorems, extract it to Go like the table oracle, and make `Federation.Build` and `Certificate.Verify` fail closed unless it certifies the federation.
- Before 1.0, a change to gsm's policy format that puts names in the policy bytes, with `bide audit` still verifying records written under the old format (adding variable kinds to the policy bytes is under consideration).

### bide underneath other agent frameworks (Go)

Go teams using other agent frameworks can keep them and put bide under the dangerous parts, by wrapping a tool's side effect in `agent.Journal.Step`.

- Worked examples and a guide for using bide under ADK for Go, Eino, trpc-agent-go and langchaingo, stating exactly what bide guards (each wrapped side effect, approvals, proofs) and what it does not (the framework's own loop resumes as it always did).
- Where a framework exposes a pluggable persistence hook (a session, checkpoint or memory service), a bide-backed implementation, so the framework's own state is journaled and its resume becomes exact.

### The visual builder

- An editor that writes a flow config, `bide edit` (in progress: [design](design/visual-editor.md)). Next after its first phase: editing structure (nodes, edges, switches), a typed mode that knows the program's registered blocks, and comparing a change against past runs.
- A GitHub Action that posts a config diff on a pull request, and an HTML export of a page.

## Later: bide beyond Go

### The bide protocol

A versioned wire protocol ([design](design/protocol.md), accepted; not implemented) that lets code in other languages use bide without reimplementing its guarantees. The Go engine stays the only writer of the journal, claims and proofs; other languages run tools and answer pauses. It supports long-lived workers and serverless functions, and comes with a conformance suite that every SDK must pass. Its claim rules are checked by a formal model (model 2).

### Python and TypeScript SDKs

Thin SDKs over the bide protocol, so agents written in Python and TypeScript get the same guarantees the Go engine enforces, plus a pip-installable verifier (the counterpart of `bide audit`) so Python teams can verify bide proofs directly. They follow the protocol's implementation.

### Adapters for Python and TypeScript agent frameworks

The same idea as for Go, across languages: adapters that route the tool calls of agents built with existing frameworks (for example LangGraph, the OpenAI Agents SDK and CrewAI in Python; the Vercel AI SDK, Mastra and LangChain.js in TypeScript) through bide, and bide-backed implementations of those frameworks' persistence hooks where they have one. Teams keep the framework they chose and gain at-most-once side effects, human approval and verifiable proofs.
