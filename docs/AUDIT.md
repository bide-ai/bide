# Verifiable audit trail

The durable journal records every step of a run. The `audit` package turns it into a
**verifiable, tamper-evident, selectively-disclosable** record — the compliance/enterprise
side of the moat: *provable at-most-once side effects* plus *a cryptographic record of exactly
what the agent did*. Stdlib-only (`crypto/sha256`, `crypto/ed25519`), no external deps.

## Security model (read this first)

- **Integrity** — unconditional. Any modify / insert / delete / reorder of a journal record
  changes the commitment.
- **Tamper-evidence** — only when you **anchor the commitment out-of-band**: sign it with a key
  the app tier doesn't fully control, and/or publish it to a separate trust domain. A hash chain
  or Merkle tree stored in the same database an attacker fully controls can be rewritten and
  re-hashed. The guarantee is: *"you committed the root elsewhere, so divergence is provable."*
- This is by design; anchoring is a real deployment requirement, not an optional extra.

## The four primitives

| API | Proves | A verifier needs |
|---|---|---|
| `Head` + `Sign` / `VerifySignature` | the whole run is intact | the head + signature |
| `Root` / `Prove` / `VerifyInclusion` | **one record** is in a committed run — without revealing the rest | that record + its O(log n) proof + the root |
| `ProveConsistency` / `VerifyConsistency` | history was **only appended**, never rewritten/reordered | two roots + the proof |
| `TreeHead` / `SignTreeHead` / `Verify` | a **signed** commitment binding root ↔ size ↔ time | the STH + public key |

`Head` is a linear SHA-256 hash chain (simple whole-run commitment). `Root` is the RFC 6962
Merkle tree — the same commitment, but it supports per-record inclusion proofs and consistency
proofs. Use `Head` when you only ever reveal the whole run; use `Root` (+ STH) when selective
disclosure or append-only proofs matter.

## Continuous anchoring: `AuditedStore` + the `Anchor` port

The primitives above are pull-based (commit when you ask). `AuditedStore` makes anchoring
automatic and push-based, and it's the piece that operationalizes the security model's "anchor
out-of-band" requirement. Wrap any `Durable` and every durable step is signed and published to
a separate trust domain with no changes to the agent loop:

```go
anchor := audit.NewMemAnchorLog()                          // your external transparency log
store  := audit.NewAuditedStore(journal, priv, anchor)     // drop-in Durable
agent.New(model, store, tools...).Run(ctx, runID, input)   // each step → a signed STH, published
```

On every journal growth `AuditedStore` commits the run's Merkle root, signs an STH, and calls
`Anchor.Publish`. A memoized replay (resume) does **not** re-anchor — each record is anchored
exactly once, even across a crash. Anchoring is a **side channel**: a `Publish` failure never
fails the durable step (the write already succeeded; failing it could wrongly retry a
non-idempotent step), so publish errors go to an optional `OnError` hook instead.

**`Anchor` is a bring-your-own port** — implement `Publish(ctx, runID, sth)` against the
transparency log you trust (a CT-style log, a notary/timestamping service, another account's
WORM store, a public ledger). `MemAnchorLog` is the reference: an append-only log that keeps its
**own** RFC 6962 tree over the published STHs, so a monitor can prove a given STH was anchored
(`Prove` + `VerifyAnchorInclusion`) and that the anchor log itself only grew (`ProveConsistency`
+ `VerifyConsistency`). That is the full end-to-end chain: **journal record → inclusion proof →
signed tree head → provably anchored in an independent, append-only log**.

## Committing the event stream, not just the journal

`Head`/`Root` above commit over the durable **journal** (the resume substrate). An `EventLog`
applies the *same* RFC 6962 machinery to `Agent.Stream`'s **semantic event stream**: the
lifecycle a UI/operator actually observes (turn boundaries, tool start/finish, approvals, the
final answer). This closes the seam where durability lived in the journal but the observable
event feed was ephemeral: now "stream for the UI" and "commit a provable audit trail" are one
pass.

```go
log := audit.NewEventLog()
// Record drains the stream, commits every event, and forwards it live to the UI:
msg, err := audit.Record(log, agentStream, func(e agent.AgentEvent) { render(e) })

root := log.Root()                 // RFC 6962 commitment over what was observed, in order
sig  := audit.Sign(root, priv)     // anchor it out-of-band, same caveat as the journal

// Later: prove ONE observed event (e.g. the approval) without revealing the rest.
proof, _ := log.Prove(approvalIndex)
ok, _ := audit.VerifyEventInclusion(root, approvalEvent, proof)
```

The event log gets the **full transparency-log surface**, reusing the journal's STH and
consistency machinery unchanged:

```go
sth := audit.SignTreeHead(log.TreeHead(time.Now().UnixNano()), priv) // signs Root↔Size↔Time
sth.Verify(pub)                                                      // anchored commitment
audit.VerifyEventInclusion(sth.Root, event, proof)                   // check proofs vs the STH root

// Between two published event STHs, prove the observed trail was only appended to:
cproof, _ := laterLog.ProveConsistency(sth1.Size)
audit.VerifyConsistency(sth1.Root, sth2.Root, cproof)
```

`TreeHead` / `SignedTreeHead` / `Verify` / `Consistency` are the *same types* used over the
journal (Size counts events instead of records), so an auditor learns one verification flow.

### Durable vs live: where the event log lives

An `EventLog` filled from the live stream is **in-memory**, so a crash loses it — and its Root
even shifts between a fresh run and its own replay (live-only events like token deltas differ).
For the durable audit artifact, don't store a second log: **derive it from the journal**, which
is already the crash-safe, at-most-once substrate.

```go
log, _ := audit.EventLogFromJournal(ctx, store, runID) // projection of the DURABLE journal
sth    := audit.SignTreeHead(log.TreeHead(ts), priv)   // anchor THIS — crash-durable, resume-stable
```

`EventLogFromJournal` projects the journal to the same semantic events `Agent.Stream` re-emits
on resume (`agent.ReplayEvents`: assembled assistant turns + completed tool calls, in order).
Because that sequence is a deterministic function of the persisted records, its Root/STH are
byte-identical before and after a crash, and a crash mid-run leaves a provable append-only
*prefix* of the completed trail (verified by `ProveConsistency`). Token-level deltas aren't
journaled, so they aren't in the durable projection — the durable content is turns and tool
results, which is what a compliance log should commit anyway. The live `EventLog` remains the
real-time UI view; the journal projection is the anchored artifact.

### A separate lifecycle: the BYO EventStore port

The journal is the resume substrate and may be garbage-collected after a run; a compliance
trail often has to outlive it (keep for years, on WORM storage, in a different trust domain).
`EventStore` is the bring-your-own port for that — append canonical event leaves to a backend
you run, on its own retention lifecycle, and rebuild an `EventLog` from it later.

```go
// Mirror the run's durable trail into your store (idempotent — call it whenever).
audit.PersistJournal(ctx, evStore, journal, runID)

// Later, even after the journal is deleted: anchor and prove from the store alone.
log, _ := audit.LoadEventLog(ctx, evStore, runID)
sth    := audit.SignTreeHead(log.TreeHead(ts), priv)
proof, _ := log.Prove(i)   // + audit.VerifyEventInclusion(sth.Root, event, proof)
```

`PersistJournal` is fed from the journal projection, not the live stream, on purpose: the
projection is deterministic and resume-stable, so re-mirroring after a crash appends the same
leaves at the same positions (idempotent, never forks). The store contract is append-only and
idempotent on `(runID, seq)` — a different leaf at an existing position is rejected as a fork.
`MemEventStore` is the in-memory default; a real backend is a Postgres table with
`UNIQUE(run_id, seq)` and insert-only grants, or object storage with object-lock/WORM.

Each leaf is a kind-tagged canonical encoding, so event types never collide, and `ModelEvent`
carries the inner delta's kind. `Root`/`Head`/`Prove`/`Sign` behave exactly as they do over the
journal; the `Inclusion` proof type and signing path are shared. (Prototype: `audit/eventsink.go`.)

## Producing a proof: `ProofBundle`, the CLI, and the standalone verifier

The primitives above are the machinery; a `ProofBundle` is the **one portable artifact** you
hand an auditor. It packages a single disclosed record, its inclusion path, and the signed tree
head it is proven against, and it verifies offline against a public key obtained out-of-band:

```go
// Produce: prove one tool call happened, against an anchored STH. Semantic, not by index.
bundle, _ := audit.ProveToolCall(ctx, store, runID, toolUseID, sth)   // or audit.ProveRecord(..., index, sth)
blob, _  := json.Marshal(bundle)                                      // store / email / publish it

// Verify: offline, trusting only the out-of-band public key.
ok, _ := bundle.Verify(pub)   // checks STH signature, size-binding, and inclusion
```

`Verify` fails closed on a forged record, a proof not bound to the signed size, or the wrong
key. The public key must come from the anchor operator, not the bundle: that is what makes it
**proofs you verify, not logs you trust.**

For the auditor who does not write Go, the `goagents-audit` CLI wraps this (`prove` over an
exported journal + STH, `verify` over a bundle + hex key; `verify` exits 0/1). And for a third
party who will not import the SDK at all, [`audit/verify`](../audit/verify) is a **stdlib-only**
package (no `agent`, no gsm) that checks inclusion, consistency, and STH signatures from raw
leaf bytes: they can vendor just that, or reimplement it from RFC 6962 and check us against it.
The two verification paths are cross-checked bit-for-bit in the tests so the standalone mirror
cannot drift.

## RFC 6962 conformance

The Merkle tree, inclusion proofs, and consistency proofs implement
[RFC 6962](https://datatracker.ietf.org/doc/html/rfc6962) (Certificate Transparency) — the same
construction CT logs use. The implementation is checked against the **published RFC 6962 reference
test vectors** (the canonical 8-leaf tree roots at all sizes), plus inclusion round-trips, a
hand-derived consistency vector, and rewrite-detection tests. It is not a homegrown look-alike.

## End-to-end compliance flow

```go
// 1. After a run, commit to the journal and PUBLISH a signed tree head.
th, _  := audit.NewTreeHead(ctx, store, runID, time.Now().UnixNano())
sth    := audit.SignTreeHead(th, priv)   // publish/anchor sth (out-of-band)

// 2. Later, an auditor asks: "did the agent issue THIS charge?"
//    Disclose only that one record + its inclusion proof — nothing else.
proof, _ := audit.Prove(ctx, store, runID, chargeIndex)
recs, _  := store.History(ctx, runID)

// 3. The auditor verifies, from public artifacts alone:
sth.Verify(pub)                                            // the commitment is authentic
ok, _ := audit.VerifyInclusion(sth.Root, recs[chargeIndex], proof)  // the charge is in it
// ...revealing no other customer, prompt, or PII.

// 4. Prove the log only grew between two published STHs (no retroactive edits):
cproof, _ := audit.ProveConsistency(ctx, store, runID, sth1.Size)
audit.VerifyConsistency(sth1.Root, sth2.Root, cproof)
```

That is a privacy-preserving, third-party-verifiable audit trail: prove a single action happened,
in a committed run whose history is provably append-only, without exposing the rest.

## Governed actions: proving which policy admitted the action

For a governed action (a `gsm` event applied through the Tier-2 governor), the audit trail can
commit not only to the fact that the action happened but to the policy it ran under.
`govern.AttestedEventTool` embeds a policy digest (an opaque identifier, e.g.
`gsm.Registry.PolicyDigest`) and the resulting `state_digest` (`gsm.State.Digest`) in the tool's
journaled result, so one committed leaf binds the action, the policy that admitted it, and the exact
state it produced, and the same `ProofBundle` that proves the action commits to all three. The SDK
treats both digests as opaque; it does not depend on the policy engine's serialization or state
layout.

The verifier then closes a second, independent root of trust:

```
# recompute the policy digest from the published bytes and, with the external verified
# oracle (astchecker, extracted from the axiom-free Coq proof), certify the policy converges:
goagents-audit verify-governance -policy policy.machine -digest <hex-from-bundle> -checker ./astchecker
```

`verify-governance` recomputes the digest from the published format without importing the policy
engine, and runs a checker it did not write, so neither root of trust depends on the producer: the
log is tamper-evident (cryptographic root) and the policy is provably convergent (mathematical
root), over one artifact.

The policy is not only referenced by digest, it is anchored in the log. `audit.RecordPolicy`
commits the serialized policy as a dedicated leaf (a `StepValue` record keyed by its digest,
idempotent per run), so the policy is covered by the same signed tree head and inclusion proofs as
the actions taken under it. `audit.ProvePolicy` builds a `ProofBundle` for that leaf. An auditor
pairs it with an action's `ProofBundle` whose result embeds the same digest: both verify under the
same out-of-band key, both bind to the same tree (same STH root and size), and their digests link,
so the action provably ran under a policy anchored in the same committed run. `audit.PolicyContent`
is the disclosed leaf payload (the serialized policy plus its digest), which the auditor feeds to
`verify-governance` to recompute the digest from the bytes and certify convergence.

The CLI does this whole cross-link in one command:

```
# both bundles authentic and in the same signed tree, the action's policy digest links to the
# anchored policy leaf, the leaf's bytes hash to that digest, and (with -checker) the policy converges:
goagents-audit verify-governed-action -action action.json -policy-bundle policy.json -pubkey <hex> -checker ./astchecker
```

### Anchoring the convergence proof itself

`gsm.Registry.Build` proves convergence exhaustively at build time (WFC over every repair chain, CC
over every independent event pair across the enumerated state space) and returns a `Report`.
`govern.CertifyConvergence(report, digest)` carries that result across as a portable
`ConfluenceCertificate`: the WFC and CC verdicts, the longest compensation chain, the number of
pairs checked, the state count, and a `CompensationFree` flag. That flag is the CRDT.v subsumption
result made visible: CRDTs are exactly the compensation-free fragment, so `MaxRepairLen == 0` marks
a machine that needs no coordinator, while a positive value marks a compensation-bearing policy that
is strictly more expressive than any CRDT. `audit.RecordConvergence` anchors the certificate as a
leaf keyed by the same policy digest, and `audit.ProveConvergence` proves it, so the convergence
evidence rides the same signed tree head and inclusion proofs as the policy and the actions. audit
keeps the certificate opaque, exactly as it does the policy digest: it imports no `gsm`.

The certificate is a producer claim, so the verifier does not trust it: `verify-convergence`
re-establishes convergence from the disclosed policy bytes with the external oracle and fails if the
oracle disagrees with the certificate, so a certificate that overstates convergence is caught.

```
# both bundles authentic and in the same signed tree, the certificate certifies the anchored
# policy's digest, the leaf's bytes hash to it, and the oracle's verdict agrees with the certificate:
goagents-audit verify-convergence -cert-bundle cert.json -policy-bundle policy.json -pubkey <hex> -checker ./astchecker
```

### Binding the resulting state, and checking it by replay

Each governed leaf also commits the `state_digest` the action produced (`gsm.State.Digest`, a stable
domain-separated hash over the packed state, meaningful because the policy pins the layout). Because
`gsm`'s convergence engine is deterministic given a policy and an event set, a verifier holding the
policy and the run's governed events can build a reference machine, replay those events, and
reproduce every committed `state_digest`. Any divergence means the runtime's committed state does not
match what the verified reference computes for that policy. This is a per-run differential check of
the actual execution against the verified reference, not merely of the policy in isolation
(`govern/attested_replay_test.go` demonstrates it end to end). It is not a refinement proof: it
checks the events this run actually took, not the runtime's behavior for all possible inputs, so the
refinement gap stays open.

### Proving a negative: no action under a disallowed policy

The set of policies a run exercised is itself provable. Governed-action leaves are keyed by their
policy digest (`audit.PolicyUsedKey`), so the run's absence commitment (`AbsenceRoot`, a Merkle tree
over the sorted distinct keys with adjacency-checked non-membership) commits exactly the policies
that were used. An auditor:

1. recomputes the used set with `audit.PoliciesUsed(records)` and confirms every digest is in the
   approved set (each approved policy having been oracle-certified convergent, as above);
2. for any digest that is not approved, obtains an anchorable `audit.AbsenceBundle` via
   `audit.ProveAbsentBundle(records, audit.PolicyUsedKey, audit.PolicyUsedKeyFor(digest), runID, sth)`
   and verifies it offline with `AbsenceBundle.Verify(pub)`, proving no governed action ran under
   that policy.

The negative has teeth: the commitment is over the run's actual key set (the STH must commit to the
`AbsenceRoot` of these records, or `ProveAbsentBundle` refuses), so you cannot prove absence of a
policy that was in fact used.

The auditor persona produces and checks these from the command line, as with inclusion. Absence
proofs verify against a separate absence commitment, signed in one call with
`audit.SignAbsenceRoot(records, keyFn, priv, ts)`; keys are built with `audit.ToolUseKeyFor(id)` or
`audit.PolicyUsedKeyFor(digest)`:

```
# prove no tool call with this ID, or no governed action under this policy digest, ever happened:
goagents-audit prove-absent -journal run.json -sth absence-sth.json -key policy:<digest> -out absent.json
goagents-audit verify-absent -bundle absent.json -pubkey <hex>   # exit 0 = authentically absent
```

Scope, stated precisely: this proves the action ran under a policy that is anchored in the log and
provably convergent, in a committed, append-only run; that each governed leaf binds the action, the
policy, and the resulting state, and that replaying the policy over the run's governed events
reproduces every committed state; and that no governed action ran under a policy outside the approved
set. Combined with the approved policies being oracle-certified convergent and invariant-preserving,
that supports "no violation was admitted." Two limits remain, stated plainly: it is a policy-level
negative, not a per-action state-validity proof; and the replay checks the events this run actually
took, not the runtime's behavior for all inputs, so the runtime refinement gap is open and this is
not an end-to-end execution proof. No proof feature remains on the roadmap; what is left is
operational (a hosted anchor service).

## Signature schemes and post-quantum anchoring

Signed tree heads sign under a pluggable scheme. `SignedTreeHead` carries an `Alg` field
(`omitempty`), so existing ed25519 bundles are unchanged and keep verifying; the legacy
`SignTreeHead` / `Verify` path is untouched. `SignTreeHeadWith` / `VerifyWith` (and
`ProofBundle.VerifyWith` / `AbsenceBundle.VerifyWith`) carry the scheme end to end. Three schemes
are available, all in the Go 1.27 standard library, so this adds no dependency:

- `ed25519` (default): small, fast, FIPS-approved.
- `ml-dsa-65` (FIPS 204): post-quantum.
- `ed25519+ml-dsa-65` (hybrid): accepted only if both signatures verify.

Why it matters here specifically: audit anchors are long-lived, so they face a harvest-now,
forge-later exposure. The signature is the quantum-vulnerable part; the SHA-256 Merkle hashing is
not affected and is unchanged. Choosing ML-DSA or hybrid for the anchor signature addresses the
signature exposure without touching the tree. One toolchain constraint: `crypto/mldsa` is
unavailable under the FIPS 140-3 module, so FIPS mode and ML-DSA are mutually exclusive in this
toolchain (ed25519 keeps working in FIPS mode). Pick per buyer: a FIPS-required buyer takes
ed25519, a post-quantum-focused buyer takes ML-DSA or hybrid.

## Next

RFC 6962 is fully covered (Head, inclusion, consistency, STH) over both the journal and, via
the event→audit sink (`EventLog` / `Record` / `TreeHead` / `ProveConsistency`), the semantic
event stream (live via `EventLog`, crash-durable via `EventLogFromJournal` / `agent.ReplayEvents`,
and on a separate lifecycle via the BYO `EventStore` port / `PersistJournal`), all sharing one
verification surface. Continuous anchoring is done: `AuditedStore` auto-signs an STH per durable
step and publishes it through the `Anchor` port to a reference external transparency log
(`MemAnchorLog`) that is itself append-only and verifiable. Proof ergonomics are done too: a
portable `ProofBundle` (`ProveToolCall` / `ProveRecord` / `Verify`), the `goagents-audit` CLI,
and a stdlib-only standalone verifier (`audit/verify`).

Candidate extensions if a use case needs them, in rough priority:

- ~~Absence proofs~~ **Done** (`audit/absence.go`): prove a thing did NOT happen (no charge, no
  approval for an ID) over an RFC 6962 tree of the run's sorted distinct keys. `AbsenceRoot`,
  `ProveAbsent` / `VerifyAbsence`, and the anchorable `AbsenceBundle`. Adapted dependency-free
  from the sibling `merkle-strata` module, with one soundness correction: we verify the two
  bracketing neighbors are ADJACENT (consecutive indices), which merkle-strata omits, so a
  present key cannot be hidden between non-consecutive neighbors.
- **Stratified / grouped trees** (`merkle-strata`, MIT, stdlib-only): group leaves by step type
  or agent for O(groups) diffs and per-agent scoped verification/disclosure, useful at
  multi-agent scale. Note: this restructures the currently-flat RFC 6962 tree, so weigh it
  against the clean CT-compatible consistency proofs we already have; add alongside, do not
  replace, the linear tree.
- **`Anchor` adapters** against real external logs (Trillian/CT, a public ledger, a
  notary/timestamping service), and **witness cosigning** so a shared anchor's forks are
  detectable (only relevant if the anchor is a service you do not control).
- **Batched/periodic anchoring** rather than per-step, for high-throughput runs.
- **Pinned cross-language canonicalization** so non-Go verifiers can reproduce leaf bytes (today
  leaves are Go `json.Marshal`, deterministic in-ecosystem but not a pinned wire format).
