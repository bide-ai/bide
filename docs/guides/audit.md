# Verifiable audit trail

The durable journal records every step of a run. The `audit` package turns it into a
**verifiable, tamper-evident, selectively-disclosable** record: the compliance/enterprise
side of the moat: *provable at-most-once side effects* plus *a cryptographic record of exactly
what the agent did*. Stdlib-only (`crypto/sha256`, `crypto/ed25519`), no external deps.

## Security model (read this first)

- **Integrity**: unconditional. Any modify / insert / delete / reorder of a journal record
  changes the commitment.
- **Tamper-evidence**: only when you **anchor the commitment out-of-band**: sign it with a key
  the app tier doesn't fully control, and/or publish it to a separate trust domain. A hash chain
  or Merkle tree stored in the same database an attacker fully controls can be rewritten and
  re-hashed. The guarantee is: *"you committed the root elsewhere, so divergence is provable."*
- This is by design; anchoring is a real deployment requirement, not an optional extra.
- **One key, many trees, no confusion**: the log key signs several kinds of tree for a run (its
  journal, the absence key sets projected from it, its event stream). Every signed tree head
  commits to its **kind** and its **run ID**, and a key-set head also commits to the journal tree it
  was projected from. Every verifier requires the kind it expects, so a head signed for one tree
  never verifies as another, and the run ID a verifier reports is the run the signer committed to.
- **Canonical bytes**: leaves, grants, and seals are hashed or signed over JSON, which is injective
  only over valid UTF-8 (encoding/json rewrites invalid bytes to U+FFFD). A record, grant, or
  anchor entry with invalid UTF-8 in any string is refused rather than committed or verified.
  `bide-audit` reads every artifact with `audit.UnmarshalStrict`, which rejects duplicate keys,
  keys that differ from a field only in case, unknown fields, and invalid UTF-8, so the file a
  person reads is exactly the data that is verified.

## The four primitives

| API | Proves | A verifier needs |
|---|---|---|
| `Head` + `Sign` / `VerifySignature` | the whole run is intact | the head + signature |
| `Root` / `Prove` / `VerifyInclusion` | **one record** is in a committed run, without revealing the rest | that record + its O(log n) proof + the root |
| `ProveConsistency` / `VerifyConsistency` | history was **only appended**, never rewritten/reordered | two roots + the proof |
| `TreeHead` / `SignTreeHead` / `Verify` | a **signed** commitment binding kind, run, root, size, and time | the STH + public key |

`Head` is a linear SHA-256 hash chain (simple whole-run commitment). `Root` is the RFC 6962
Merkle tree: the same commitment, but it supports per-record inclusion proofs and consistency
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
`Anchor.Publish`. A memoized replay (resume) does **not** re-anchor an already anchored head.
Anchoring is serialized per run, so the heads one `AuditedStore` publishes for a run only ever grow,
even when parallel tool calls land at once. Anchoring is a **side channel**: a `Publish` failure never
fails the durable step (the write already succeeded; failing it could wrongly retry a
non-idempotent step), so publish errors go to an optional `OnError` hook instead, and the run's next
step retries the unanchored head. When two processes anchor the same run (around a lease handoff),
a smaller head can reach the anchor after a larger one; both are valid, so monitors compare a run's
heads by size, not by arrival.

**`Anchor` is a bring-your-own port**: implement `Publish(ctx, runID, sth)` against the
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
sth := audit.SignTreeHead(log.TreeHead(runID, time.Now().UnixNano()), priv) // kind "events", run, root, size, time
sth.Verify(pub)                                                      // anchored commitment
audit.VerifyEventInclusion(sth.Root, event, proof)                   // check proofs vs the STH root

// Between two published event STHs, prove the observed trail was only appended to:
cproof, _ := laterLog.ProveConsistency(sth1.Size)
audit.VerifyConsistency(sth1.Root, sth2.Root, cproof)
```

`TreeHead` / `SignedTreeHead` / `Verify` / `Consistency` are the *same types* used over the
journal (Size counts events instead of records, and Kind is `audit.TreeEvents`, so an event head
never verifies as a journal head), so an auditor learns one verification flow.

### Durable vs live: where the event log lives

An `EventLog` filled from the live stream is **in-memory**, so a crash loses it, and its Root
even shifts between a fresh run and its own replay (live-only events like token deltas differ).
For the durable audit artifact, don't store a second log: **derive it from the journal**, which
is already the crash-safe, at-most-once substrate.

```go
log, _ := audit.EventLogFromJournal(ctx, store, runID) // projection of the DURABLE journal
sth    := audit.SignTreeHead(log.TreeHead(runID, ts), priv) // anchor THIS: crash-durable, resume-stable
```

`EventLogFromJournal` projects the journal to the same semantic events `Agent.Stream` re-emits
on resume (`agent.ReplayEvents`: assembled assistant turns + completed tool calls, in order).
Because that sequence is a deterministic function of the persisted records, its Root/STH are
byte-identical before and after a crash, and a crash mid-run leaves a provable append-only
*prefix* of the completed trail (verified by `ProveConsistency`). Token-level deltas aren't
journaled, so they aren't in the durable projection; the durable content is turns and tool
results, which is what a compliance log should commit anyway. The live `EventLog` remains the
real-time UI view; the journal projection is the anchored artifact.

### A separate lifecycle: the BYO EventStore port

The journal is the resume substrate and may be garbage-collected after a run; a compliance
trail often has to outlive it (keep for years, on WORM storage, in a different trust domain).
`EventStore` is the bring-your-own port for that: append canonical event leaves to a backend
you run, on its own retention lifecycle, and rebuild an `EventLog` from it later.

```go
// Mirror the run's durable trail into your store (idempotent: call it whenever).
audit.PersistJournal(ctx, evStore, journal, runID)

// Later, even after the journal is deleted: anchor and prove from the store alone.
log, _ := audit.LoadEventLog(ctx, evStore, runID)
sth    := audit.SignTreeHead(log.TreeHead(runID, ts), priv)
proof, _ := log.Prove(i)   // + audit.VerifyEventInclusion(sth.Root, event, proof)
```

`PersistJournal` is fed from the journal projection, not the live stream, on purpose: the
projection is deterministic and resume-stable, so re-mirroring after a crash appends the same
leaves at the same positions (idempotent, never forks). The store contract is append-only and
idempotent on `(runID, seq)`: a different leaf at an existing position is rejected as a fork.
`MemEventStore` is the in-memory default; a real backend is a Postgres table with
`UNIQUE(run_id, seq)` and insert-only grants, or object storage with object-lock/WORM.

Each leaf is a kind-tagged canonical encoding, so event types never collide, and `ModelEvent`
carries the inner delta's kind. `Root`/`Head`/`Prove`/`Sign` behave exactly as they do over the
journal; the `Inclusion` proof type and signing path are shared. (Prototype: `audit/eventsink.go`.)

## Producing a proof: `ProofBundle`, the CLI, and the standalone verifier

The primitives above are the machinery; a `ProofBundle` is the **portable artifact for a single
action** you hand an auditor (for a whole run, see the evidence bundle below). It packages a single
disclosed record, its inclusion path, and the signed tree head it is proven against, and it verifies
offline against a public key obtained out-of-band:

```go
// Produce: prove one tool call happened, against an anchored STH. Semantic, not by index.
bundle, _ := audit.ProveToolCall(ctx, store, runID, toolUseID, sth)   // or audit.ProveRecord(..., index, sth)
blob, _  := json.Marshal(bundle)                                      // store / email / publish it

// Verify: offline, trusting only the out-of-band public key.
ok, _ := bundle.Verify(pub)   // checks STH signature, kind and run binding, size binding, and inclusion
```

`Verify` fails closed on a forged record, a proof not bound to the signed size, a head that is not
a journal head of the bundle's `RunID`, or the wrong key. The public key must come from the anchor operator, not the bundle: that is what makes it
**proofs you verify, not logs you trust.**

For the auditor who does not write Go, the `bide-audit` CLI wraps this (`prove` over an
exported journal + STH, `verify` over a bundle + hex key; `verify` exits 0/1). And for a third
party who will not import the SDK at all, [`audit/verify`](../../audit/verify) is a **stdlib-only**
package (no `agent`, no gsm) that checks inclusion, consistency, and STH signatures from raw
leaf bytes: they can vendor just that, or reimplement it from RFC 6962 and check us against it.
The two verification paths are cross-checked bit-for-bit in the tests so the standalone mirror
cannot drift.

### The run-level evidence bundle: `EvidencePackage`

A `ProofBundle` proves one disclosed action. For a whole run, `audit.Evidence` assembles the run's
evidence into a single portable file: one signed tree head, an inclusion proof per material action,
and optionally the run certificate, the authority grant chain, and a consistency proof. It is pure
JSON (store it, email it, publish it) and verifies offline:

```go
pkg, _ := audit.Evidence(ctx, store, runID, priv, time.Now().Unix(),
	audit.WithAllToolCalls(), audit.WithRunCertificate(spec), audit.WithGrants(),
	audit.WithConsistencyFrom(earlierSTH)) // an earlier signed head of this run, e.g. from the anchor log
report, _ := pkg.Verify(pub, audit.WithApprovedPolicies(allowlist...)) // trusting only the out-of-band key
```

`Verify` trusts the key you pass, never the one embedded in the package, and every field of the
package is verified or derived from verified data:

- `Format` must be `audit.EvidenceFormat` (`bide.audit.evidence.v2`), and `PublicKeyHex` must be the
  key you pass.
- The STH must be an authentic journal head of the package's `RunID`, so the run the report names is
  the run the log key signed.
- Each action proof must verify against that same tree, and its `Kind`, `Label`, and `Ref` must be
  what the proven record says (a tool result cannot be shown as an approval by someone else).
- `Grants.Chain` must be exactly the grants the anchored leaves prove, one per leaf, in order.
- The run certificate must be for this run and this tree, and is checked against the allowlist you
  pass with `WithApprovedPolicies`; a package that carries a certificate fails without one.
- The consistency proof is checked from its earlier signed head (an authentic journal head of the
  same run, of the size the proof starts from) to the package STH.
- `Label`, the one field no proof covers, is covered by the seal: `Evidence` ends with `pkg.Seal(priv)`,
  the log key's signature over the whole package, so nothing can be edited, added, or dropped after
  sealing. A caller that adds actions afterwards reseals.

One check needs inputs the package deliberately does not carry: grant issuer signatures (they need
the issuers' keys, checked with `VerifyDelegationChain` against your own PKI). For the auditor who
does not write Go, `bide-audit verify-evidence` verifies the same file and prints a plain-English
PASS/FAIL.

### Approval evidence: k approvers signed off before the action

For a tool gated by an m-of-n approval policy, `audit.ApprovalEvidence` produces the complete
evidence under one signed tree head, in journal order: the model turn that requested the call, every
decision record the gate read (valid or not), the gate's recorded tally, and the call's result. To add
it to a package that already carries the call (built with `WithToolCall` or `WithAllToolCalls`), drop
the trailing result entry and reseal the package:

```go
approvals, _ := audit.ApprovalEvidence(ctx, store, runID, toolUseID, pkg.STH)
pkg.Actions = append(pkg.Actions, approvals[:len(approvals)-1]...) // the result is already packaged
_ = pkg.Seal(priv)                                                 // the package changed after Evidence sealed it
```

`pkg.Verify` checks each item's inclusion under the tree head like any other action. Like grant
issuer signatures, the approval claim needs inputs the package does not carry (the approvers' public
keys and the expected policy), so `audit.VerifyApprovals` checks it from the package and those keys:
it recounts the decisions with the gate's own rule against the proven call, and reports a problem if
the evidence omits a decision the gate read, if the recount disagrees with the recorded tally, or if
the gate enforced a different policy. `bide-audit verify-approvals` runs the same check from the
command line. `audit.ProveApproval` proves a single decision by its record name. See the
[approval guide](approval.md#proving-the-gate-held) and `examples/approval`.

## CLI reference: `bide-audit`

The `bide-audit` command ([`cmd/bide-audit`](../../cmd/bide-audit)) is the auditor-facing
front end for the whole proof surface. It is dependency-light: it imports only the core and `audit`
packages and no store backend, so every produce verb operates on an **exported journal** (a JSON
array of `Record`, obtained with `json.Marshal(store.History(ctx, runID))`) plus a signed tree head,
and every verify verb needs only a bundle and an out-of-band public key. Build it with
`go build ./cmd/bide-audit`.

Conventions shared across verbs:

- `-pubkey` accepts either a hex string directly or a path to a file whose trimmed contents are
  hex, and must decode to a 32-byte ed25519 public key (anything else exits 1 with a message). The
  key must come from the anchor operator out-of-band, never from the bundle: that is what makes it a
  proof you verify rather than a log you trust.
- Every JSON input is read strictly (`audit.UnmarshalStrict`): a duplicate key, a key that matches a
  field only case-insensitively, an unknown field, or invalid UTF-8 exits 1, so a file cannot show a
  reader one value while the verifier checks another.
- Produce verbs (`prove`, `prove-absent`) write the bundle to `-out`, or to stdout if `-out` is
  omitted; the "wrote &lt;file&gt;" line goes to stderr so stdout stays clean for piping.
- Verify verbs print a one-line `OK: ...` / `FAIL: ...` verdict and set the exit code: **0 =
  authentic / all checks passed, 1 = failed** (a usage error exits 2). This is the CI-gate contract.

| Verb | Required flags | Optional flags | Proves / checks |
|---|---|---|---|
| `prove` | `-journal`, `-sth`, and one of `-tool <id>` / `-index <n>` | `-out` | Build a `ProofBundle` for one record (by tool-use id or journal index) against a signed tree head. |
| `verify` | `-bundle`, `-pubkey` | | A `ProofBundle` is authentic under the key, its head is a journal head of the bundle's run, and it is bound to its signed size. |
| `prove-absent` | `-journal`, `-sth`, `-key` (`tool:<id>` or `policy:<digest>`) | `-out` | Build an `AbsenceBundle` proving a tool call / governed policy never appears, against a signed key-set head **of the matching kind** (`audit.SignAbsenceRoot` with `ToolUseKeys` or `PolicyUsedKeys`) whose source journal is the exported journal. |
| `verify-absent` | `-bundle`, `-pubkey` | | An `AbsenceBundle` is authentic: its head is a signed key-set head of the kind the key belongs to (`tooluse:` keys need a tool-use head, `policy_used:` keys a used-policy head) for the bundle's run, and the key is absent from it. Reports the journal size the absence covers. |
| `verify-governance` | `-policy` | `-digest <hex>`, `-checker <astchecker>` | Recompute the policy digest from the published bytes (independent of gsm); with `-digest`, assert it matches; with `-checker`, run the external verified oracle to certify the policy converges. |
| `verify-governed-action` | `-action`, `-policy-bundle`, `-pubkey` | `-checker` | End to end: both bundles authentic and in the same signed tree, the action's embedded policy digest links to the anchored policy leaf, the leaf's bytes hash to that digest, and (with `-checker`) the policy converges. |
| `verify-convergence` | `-cert-bundle`, `-policy-bundle`, `-pubkey` | `-checker` | An anchored `ConfluenceCertificate` links to the policy leaf; with `-checker`, the oracle's convergence verdict must AGREE with the certificate, so overstated convergence is caught; the compensation-free (CRDT) classification is cross-checked only when the oracle emits a `compensation_free=` line, and otherwise stays producer-reported (the CLI prints a note saying so). |
| `verify-quorum` | `-name`, `-tally`, `-vote` (repeatable), `-pubkey`, `-k` | `-commit` | A governed k-of-n quorum: the tally and every vote bundle authentic, in the same signed tree and run, and recorded by the quorum named `-name`; the disclosed votes exactly the votes the tally records; the recorded tally recomputes from them (a forged tally is caught); and `votes_for >= k`; with `-commit`, a governed commit is anchored in the same tree. |
| `verify-run` | `-cert`, `-pubkey`, and one of `-approved <digest>` (repeatable) / `-approved-file <file>` | `-checker <astchecker>` | A proof-carrying run certificate: the used-policy set is bound by a signed used-policy head to this run and to the certificate's journal tree, and is a subset of the approved allowlist (only-approved-policies), and every used policy has an anchored, digest-linked convergence certificate in the run's signed tree (policies-convergence-certified); with `-checker`, the oracle's convergence verdict on each used policy must AGREE with its certificate. |
| `verify-approvals` | `-evidence`, `-pubkey`, `-call`, `-need`, `-approvers`, `-approver-keys` | | An m-of-n approval gate from an `EvidencePackage`: the request, every decision the gate read, its recorded tally, and the call's result all verify in one signed tree and in order; recounting the decisions with the approvers' keys (a JSON object of id to ed25519 public key hex) against the exact call reproduces the recorded tally; the gate enforced the expected policy; and at least k approved. Catches an omitted decision, which `verify-evidence` alone cannot. |
| `verify-evidence` | `-evidence`, `-pubkey` | `-approved <digest>` (repeatable), `-approved-file <file>` | A run-level `EvidencePackage`: the format, seal, and key are right, the signed tree head is an authentic journal head of the package's run, every packaged action proof verifies against it with the kind and label its record says, the grant chain is the anchored grants, the consistency proof holds between its two signed heads, and any run certificate is for this run and passes against the given allowlist (required when the package carries one). Prints one line per item and an overall PASS/FAIL. |

The `-checker` flag points at the external verified oracle binary (the `astchecker` extracted from
the axiom-free Coq proof); the CLI does not ship it, and without it the governance verbs verify only
the cryptographic root and say so. The digest is recomputed here from a hardcoded
`gsm-policy-v1` domain-separation tag rather than taken from gsm, so neither root of trust depends
on the producer. The CLI reads no environment variables.

## RFC 6962 conformance

The Merkle tree, inclusion proofs, and consistency proofs implement
[RFC 6962](https://datatracker.ietf.org/doc/html/rfc6962) (Certificate Transparency): the same
construction CT logs use. The implementation is checked against the **published RFC 6962 reference
test vectors** (the canonical 8-leaf tree roots at all sizes), plus inclusion round-trips, a
hand-derived consistency vector, and rewrite-detection tests. It is not a homegrown look-alike.

## End-to-end compliance flow

```go
// 1. After a run, commit to the journal and PUBLISH a signed tree head.
th, _  := audit.NewTreeHead(ctx, store, runID, time.Now().UnixNano())
sth    := audit.SignTreeHead(th, priv)   // publish/anchor sth (out-of-band)

// 2. Later, an auditor asks: "did the agent issue THIS charge?"
//    Disclose only that one record + its inclusion proof, nothing else.
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

If the deployment binds an acting identity to the run (`agent.WithIdentity`, carrying `Actor`,
`OnBehalfOf`, and `AuthorityRef`), `AttestedEventTool` also stamps those fields into the same leaf,
so an inclusion proof commits to who acted, on whose behalf, and under what authority, not merely
that the action happened under the policy. Identity is assigned by the deployment from its own auth
layer (an IdP, a signed grant, a service identity), never by the model, and it rides the context so
governed actions and sub-agents inherit it. The proof commits to the identity CLAIM; authenticating
the principal is the operator's IdP/PKI, and the attribution is only as strong as the key custody
behind the run's signatures.

The verifier then closes a second, independent root of trust:

```
# recompute the policy digest from the published bytes and, with the external verified
# oracle (astchecker, extracted from the axiom-free Coq proof), certify the policy converges:
bide-audit verify-governance -policy policy.machine -digest <hex-from-bundle> -checker ./astchecker
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
bide-audit verify-governed-action -action action.json -policy-bundle policy.json -pubkey <hex> -checker ./astchecker
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
re-establishes the claims from the disclosed policy bytes with the external oracle and fails if the
oracle disagrees. It cross-checks both facts. Convergence: the oracle certifies (or refutes) it, and
a certificate that says convergent while the oracle refutes is rejected. Classification: the oracle
also certifies the compensation-free (CRDT-fragment) verdict, via `compensationFree` in
`AstChecker.v` (a machine-checked, axiom-free predicate: no in-domain state ever needs repair, the
AST analogue of "max repair depth = 0"), emitted as a `compensation_free=<bool>` line the CLI parses
and compares. So a certificate that overstates convergence is always caught, and one that overstates
the CRDT classification is caught when the oracle emits that line. An oracle that does not emit it
leaves the classification producer-reported: the CLI prints a note saying so rather than passing
it silently. `compensationFree_step_no_repair` is in the axiom-free gate alongside `check_sound_converges`.

```
# both bundles authentic and in the same signed tree, the certificate certifies the anchored
# policy's digest, the leaf's bytes hash to it, and the oracle's convergence AND compensation-free
# verdicts agree with the certificate:
bide-audit verify-convergence -cert-bundle cert.json -policy-bundle policy.json -pubkey <hex> -checker ./astchecker
```

### Binding the resulting state, and checking it by replay

Each governed leaf also commits the `state_digest` the action produced (`gsm.State.Digest`, a stable
domain-separated hash over the packed state, meaningful because the policy pins the layout) and the
event's `position` in the governor's order. Because `gsm`'s convergence engine is deterministic given
a policy and an event sequence, a verifier holding the policy can build a reference machine, replay
the governor's events at positions `[0, position]`, and reproduce the committed `state_digest`. For a
`PersistentGovernor` those events are its shared `EventLog`, whichever processes and runs wrote them;
for an in-memory `Governor` shared by several runs, they are the governed leaves of all those runs
ordered by position. Replaying one run's own events is enough only when nothing else acts on the same
governor. Any divergence means the runtime's committed state does not match what the verified
reference computes for that policy. This is a per-run differential check of
the actual execution against the verified reference, not merely of the policy in isolation
(`govern/attested_replay_test.go` demonstrates it end to end). It is not a refinement proof: it
checks the events this run actually took, not the runtime's behavior for all possible inputs, so the
refinement gap stays open.

### Proving a negative: no action under a disallowed policy

The set of policies a run exercised is itself provable. Governed-action leaves are keyed by their
policy digest (the `audit.PolicyUsedKeys` key set), so the run's used-policy commitment
(`AbsenceRoot`, a Merkle tree over the sorted distinct keys with adjacency-checked non-membership)
commits exactly the policies that were used. An auditor:

1. recomputes the used set with `audit.PoliciesUsed(records)` and confirms every digest is in the
   approved set (each approved policy having been oracle-certified convergent, as above);
2. for any digest that is not approved, obtains an anchorable `audit.AbsenceBundle` via
   `audit.ProveAbsentBundle(records, audit.PolicyUsedKeys, audit.PolicyUsedKeyFor(digest), sth)`
   and verifies it offline with `bundle.Verify(pub, audit.PolicyUsedKeys)`, proving no governed
   action ran under that policy.

The negative has teeth: the commitment is over the run's actual key set (the head must commit to
the key set of the journal it names, or `ProveAbsentBundle` refuses), so you cannot prove absence of
a policy that was in fact used. And it cannot be borrowed from another tree: each key set is a
`KeySet` with its own tree kind and key prefix (`ToolUseKeys` is `absence/tool-use` over
`tooluse:` keys, `PolicyUsedKeys` is `absence/policy-used` over `policy_used:` keys), the signed
head commits to its kind, its run, and its source journal tree, and `Verify` requires the head to be
of the set you name and the key to carry that set's prefix. A tool-use head cannot prove a policy
absent, and a journal head cannot prove anything absent. The absence covers the journal up to the
size the head names; that it is the run's final head comes from the anchor log.

The auditor persona produces and checks these from the command line, as with inclusion. Absence
proofs verify against a separate key-set commitment, signed in one call with
`audit.SignAbsenceRoot(records, keySet, journalHead, priv, ts)` (it refuses records that are not the
journal `journalHead` commits to); keys are built with `audit.ToolUseKeyFor(id)` or
`audit.PolicyUsedKeyFor(digest)`:

```
# prove no tool call with this ID, or no governed action under this policy digest, ever happened:
bide-audit prove-absent -journal run.json -sth used-policy-sth.json -key policy:<digest> -out absent.json
bide-audit verify-absent -bundle absent.json -pubkey <hex>   # exit 0 = authentically absent
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

## Proof-carrying runs

The primitives above prove one fact at a time: this action happened, this policy was anchored, this
policy is convergent, this policy was not used. A **proof-carrying run** composes them into one
portable certificate asserting behavioral-property compliance over a WHOLE run, checkable offline
against a single signed tree head. The innovation is the composition, not new cryptography: a
`RunCertificate` binds together the used-policy absence commitment, the anchored policy and
convergence leaves, and the run's STH, and `VerifyRun` re-derives every property from the disclosed
proofs.

v1 asserts three properties, each dischargeable from a committed leaf:

- **only-approved-policies**: every policy digest exercised by a governed action in the run is a
  member of the auditor's approved allowlist (an argument to `VerifyRun`; the certificate carries
  no allowlist of its own). This is the **completeness-bearing** property: it commits WHICH policies
  were used, via the used-policy commitment (a `PolicyUsedKeys` head). The used set is exactly the
  sorted distinct keys that tree commits to, so the verifier recomputes that root from the disclosed
  set and confirms the signed head commits to it. That is what gives the negative teeth: a policy
  that was in fact used cannot be dropped from the disclosed set without changing the root and
  breaking the signature check.
- **binding**: the used-policy head is bound to this run and this journal, not merely signed by the
  right key. Its signed encoding commits to kind `absence/policy-used`, to the run ID, and to the
  journal tree (size and root) the set was projected from, and `VerifyRun` requires that journal
  tree to be exactly the certificate's signed journal head, itself a journal head of the
  certificate's run. A used-policy head from another run, from another point or history of this
  run, or a tool-use head cannot stand in, and the used set is the projection of the same records
  the convergence bundles are proven against.
- **policies-convergence-certified**: for each used policy digest, an anchored convergence
  certificate leaf exists in the same signed tree as the policy leaf and links to its digest,
  exactly as `verify-convergence` establishes. With `-checker`, the external oracle is run on each
  disclosed policy's bytes and its verdict must AGREE with the certificate, so a certificate that
  overstates convergence is caught.

Composing them yields "every governed state in the run was produced by an approved,
oracle-certified-convergent policy," so the enforced invariant held throughout the governed
boundary.

```go
// Emit: recompute the used-policy set, confirm it is a subset of the allowlist, and assemble the
// anchored policy + convergence proofs for each used policy against the run's signed tree head.
sth  := audit.SignTreeHead(th, priv)
cert, _ := audit.CertifyRun(ctx, store, runID, sth,
	audit.RunCertSpec{ApprovedPolicies: []string{policyDigest}}, priv, time.Now().UnixNano())

// Anchor the certificate itself so it is provable in the run (mirrors RecordPolicy / RecordConvergence):
audit.RecordRunCertificate(ctx, store, runID, cert)   // + audit.ProveRunCertificate(..., laterSTH)

// Verify: offline, against the auditor's allowlist, trusting only the out-of-band public key.
res, _ := audit.VerifyRun(cert, allowlist, pub)   // res.OnlyApprovedPolicies && res.ConvergenceCertified
```

The CLI verifies the same certificate for an auditor who does not write Go, and takes the allowlist
as its own input (the certificate carries none, so a producer cannot pass by widening its own set):

```
# only-approved-policies (used set bound to the run's signed used-policy head and a subset of the allowlist)
# and policies-convergence-certified (each used policy anchored and digest-linked); with -checker the
# oracle's verdict on each policy must agree with its certificate:
bide-audit verify-run -cert runcert.json -pubkey <hex> -approved <digest> -checker ./astchecker
```

Scope, stated precisely: the certificate proves properties of the **governed, committed boundary**
of the run: which policies ran, that each is on the approved allowlist, and that each has an
anchored convergence certificate that (with the oracle) is confirmed convergent. It does NOT prove
the model's judgment was correct, that ungoverned side effects were appropriate, or the
runtime-refinement claim (the replay differential check covers the events this run took, not all
inputs). The used-policy completeness rests entirely on the used-policy key-set commitment
described above, and covers the journal up to the certificate STH's size; that this STH is the
run's final head is a fact the anchor log supplies, not the certificate. Property support for **authority-bounded** (every governed action under a grant
descending from the root, via `VerifyDelegationChain`) and **quorum-backed** commits composes from
the same seams and is deferred to a later version. Runnable end to end in
`examples/proof-carrying-run`.

## Signature schemes and post-quantum anchoring

Signed tree heads sign under a pluggable scheme. `SignedTreeHead` carries an `Alg` field
(`omitempty`; empty means ed25519), so ed25519 heads from `SignTreeHead` / `Verify` carry no extra
field. `SignTreeHeadWith` / `VerifyWith` (and `ProofBundle.VerifyWith` /
`AbsenceBundle.VerifyWith`) carry the scheme end to end. Every scheme signs the same encoding
(`bide.audit.sth.v2`: kind, run ID, size, root, timestamp, and the source journal of a key-set
head), so the kind and run binding hold whichever scheme signs. Three schemes
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

## Signed authority grants and the delegation chain

Identity binds who acted (see above); a **grant** binds *by what authority*, non-repudiably.
`audit.Grant{Issuer, Subject, Scope, NotAfter, ParentRef}` is a statement by a principal authorizing
an actor within a scope. `SignGrant` signs it with the issuer's own key (an `audit.Signer`:
Ed25519, ML-DSA, or hybrid), distinct from the log's tree-head key, so the authorization is
attributable to the principal, not the operator. `RecordGrant` / `ProveGrant` anchor and prove it as
a leaf like a policy, and a governed action's `agent.Identity.AuthorityRef` is set to the grant's
`Digest`, so the action links to an in-log, issuer-signed authority.

`ParentRef` hash-links a grant to the one it was attenuated from, so a holder can mint a strictly
narrower sub-grant for a sub-agent without returning to the root issuer (capability attenuation).
`VerifyDelegationChain` checks a root-to-leaf chain: each hop signed by its issuer, and each child a
valid delegation of its parent (`CheckAttenuation`): its `ParentRef` is the parent's digest, its
issuer is the parent's subject (only the holder of a grant can delegate it), it expires no later than
the parent (a child of an expiring grant is never non-expiring), and it keeps every one of the
parent's scope constraints, unchanged or narrowed by the `ScopeRule` you supply for that key
(`audit.ScopeRules{"limit": audit.NumericAtMost}` for a limit). An auditor thus confirms a
sub-agent's authority descends, unbroken and never widened, from a root principal each hop signed.
Runnable end to end in `examples/delegation`; the [delegation guide](delegation.md) states the rules
in full.

**Attenuation by default.** `AttenuatingSubAgent` wires this into the sub-agent seam so narrowing is
the default, not something the caller remembers to do. Bind the acting grant and signer once at the
root with `WithGrant`; then each delegation through the tool mints a narrower child grant (linked to
the parent, expiring no later, checked with `CheckAttenuation` before it is signed, and anchored as a
leaf of the call's own sub-run), rebinds the sub-run's identity to it, and propagates it so a deeper
delegation narrows again. With no grant on the context it is a plain sub-agent that
inherits identity, so it is safe either way. The wrapped sub-agent still runs its own full loop and
reasons autonomously; only its authority shrinks. The result is that capabilities monotonically
decrease down a delegation tree by construction, and the chain stays provable via
`VerifyDelegationChain`.

**Earned authority.** `audit.EarnedAuthority` drives a grant's scope from the agent's track record:
authority starts at a baseline rung, is promoted one rung after a clean streak (capped by the
ladder's top), and resets to baseline the instant an anomaly is flagged. Each change re-issues a
signed grant that is a child of the root (keeping the root's expiry and every root constraint), so
the earned limit provably never exceeds the root ceiling (every earned grant passes
`VerifyDelegationChain` with `audit.EarnedRules`, so even a buggy controller cannot widen past what
the root principal authorized). A demotion takes the higher grant out of use at once: every issued
grant is appended to a ledger run, and only the ledger's last leaf is current
(`ProveCurrentGrant` / `VerifyCurrentGrant` against the ledger's latest signed head, which an offline
verifier takes from the anchor log; the proof must name the ledger run, and must extend the last
ledger head the verifier saw, so neither another run's leaf nor an older head passes as current).
The asymmetry is the safety property: promotion is slow,
capped, and evidence-gated; attenuation is immediate and ungated, because shrinking authority is
always safe. It is deliberately a durable, sequential controller rather than a convergent machine,
because earning is temporal and order-dependent (a promotion does not commute with a compliant
action); the enforcement of the limit it sets stays a convergent gsm invariant on the work machine.
Runnable in `examples/earned-authority`.

## Next

RFC 6962 is fully covered (Head, inclusion, consistency, STH) over both the journal and, via
the event→audit sink (`EventLog` / `Record` / `TreeHead` / `ProveConsistency`), the semantic
event stream (live via `EventLog`, crash-durable via `EventLogFromJournal` / `agent.ReplayEvents`,
and on a separate lifecycle via the BYO `EventStore` port / `PersistJournal`), all sharing one
verification surface. Continuous anchoring is done: `AuditedStore` auto-signs an STH per durable
step and publishes it through the `Anchor` port to a reference external transparency log
(`MemAnchorLog`) that is itself append-only and verifiable. Proof ergonomics are done too: a
portable `ProofBundle` (`ProveToolCall` / `ProveRecord` / `Verify`), the `bide-audit` CLI,
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
- **Journal compaction with proof continuity** (design note: [compaction](../design/compaction.md)): seal
  a closed prefix into a signed checkpoint plus a deterministic state snapshot, carry the previous
  root forward as the next segment's first leaf, and drop the sealed raw records. Proofs survive the
  boundary via a signed checkpoint chain; the load-bearing invariant is that only a closed prefix
  may be sealed, so at-most-once is never broken. Not implemented.
- **Pinned cross-language canonicalization** so non-Go verifiers can reproduce leaf bytes (today
  a leaf is `agent.EncodeRecord` of the record, which is exactly the bytes every store persists
  and is pinned by golden tests, and a record with invalid UTF-8 in a string field is refused, so
  leaves are injective; but the encoding is specified by its Go implementation rather than a
  written wire format).
