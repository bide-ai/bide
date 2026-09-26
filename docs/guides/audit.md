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

## The four primitives

| API | Proves | A verifier needs |
|---|---|---|
| `Head` + `Sign` / `VerifySignature` | the whole run is intact | the head + signature |
| `Root` / `Prove` / `VerifyInclusion` | **one record** is in a committed run, without revealing the rest | that record + its O(log n) proof + the root |
| `ProveConsistency` / `VerifyConsistency` | history was **only appended**, never rewritten/reordered | two roots + the proof |
| `TreeHead` / `SignTreeHead` / `Verify` | a **signed** commitment binding root ↔ size ↔ time | the STH + public key |

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
`Anchor.Publish`. A memoized replay (resume) does **not** re-anchor; each record is anchored
exactly once, even across a crash. Anchoring is a **side channel**: a `Publish` failure never
fails the durable step (the write already succeeded; failing it could wrongly retry a
non-idempotent step), so publish errors go to an optional `OnError` hook instead.

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

An `EventLog` filled from the live stream is **in-memory**, so a crash loses it, and its Root
even shifts between a fresh run and its own replay (live-only events like token deltas differ).
For the durable audit artifact, don't store a second log: **derive it from the journal**, which
is already the crash-safe, at-most-once substrate.

```go
log, _ := audit.EventLogFromJournal(ctx, store, runID) // projection of the DURABLE journal
sth    := audit.SignTreeHead(log.TreeHead(ts), priv)   // anchor THIS: crash-durable, resume-stable
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
sth    := audit.SignTreeHead(log.TreeHead(ts), priv)
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
party who will not import the SDK at all, [`audit/verify`](../../audit/verify) is a **stdlib-only**
package (no `agent`, no gsm) that checks inclusion, consistency, and STH signatures from raw
leaf bytes: they can vendor just that, or reimplement it from RFC 6962 and check us against it.
The two verification paths are cross-checked bit-for-bit in the tests so the standalone mirror
cannot drift.

## CLI reference: `goagents-audit`

The `goagents-audit` command ([`cmd/goagents-audit`](../../cmd/goagents-audit)) is the auditor-facing
front end for the whole proof surface. It is dependency-light: it imports only the core and `audit`
packages and no store backend, so every produce verb operates on an **exported journal** (a JSON
array of `Record`, obtained with `json.Marshal(store.History(ctx, runID))`) plus a signed tree head,
and every verify verb needs only a bundle and an out-of-band public key. Build it with
`go build ./cmd/goagents-audit`.

Conventions shared across verbs:

- `-pubkey` accepts either a hex string directly or a path to a file whose trimmed contents are
  hex. The key must come from the anchor operator out-of-band, never from the bundle: that is what
  makes it a proof you verify rather than a log you trust.
- Produce verbs (`prove`, `prove-absent`) write the bundle to `-out`, or to stdout if `-out` is
  omitted; the "wrote &lt;file&gt;" line goes to stderr so stdout stays clean for piping.
- Verify verbs print a one-line `OK: ...` / `FAIL: ...` verdict and set the exit code: **0 =
  authentic / all checks passed, 1 = failed** (a usage error exits 2). This is the CI-gate contract.

| Verb | Required flags | Optional flags | Proves / checks |
|---|---|---|---|
| `prove` | `-journal`, `-sth`, and one of `-tool <id>` / `-index <n>` | `-out` | Build a `ProofBundle` for one record (by tool-use id or journal index) against a signed tree head. |
| `verify` | `-bundle`, `-pubkey` | | A `ProofBundle` is authentic under the key and bound to its signed size. |
| `prove-absent` | `-journal`, `-sth`, `-key` (`tool:<id>` or `policy:<digest>`) | `-out` | Build an `AbsenceBundle` proving a tool call / governed policy never appears, against a signed **absence** tree head (`audit.SignAbsenceRoot`). |
| `verify-absent` | `-bundle`, `-pubkey` | | An `AbsenceBundle` is authentic (the key really is absent from the signed key set). |
| `verify-governance` | `-policy` | `-digest <hex>`, `-checker <astchecker>` | Recompute the policy digest from the published bytes (independent of gsm); with `-digest`, assert it matches; with `-checker`, run the external verified oracle to certify the policy converges. |
| `verify-governed-action` | `-action`, `-policy-bundle`, `-pubkey` | `-checker` | End to end: both bundles authentic and in the same signed tree, the action's embedded policy digest links to the anchored policy leaf, the leaf's bytes hash to that digest, and (with `-checker`) the policy converges. |
| `verify-convergence` | `-cert-bundle`, `-policy-bundle`, `-pubkey` | `-checker` | An anchored `ConfluenceCertificate` links to the policy leaf; with `-checker`, the oracle's convergence AND compensation-free verdicts must AGREE with the certificate, so an overstated certificate is caught. |
| `verify-quorum` | `-tally`, `-vote` (repeatable), `-pubkey`, `-k` | `-commit` | A governed k-of-n quorum: the tally and every vote bundle authentic and in the same signed tree and run, the recorded tally recomputes from the disclosed votes (a forged tally is caught), and `votes_for >= k`; with `-commit`, a governed commit is anchored in the same tree. |
| `verify-run` | `-cert`, `-pubkey`, and one of `-approved <digest>` (repeatable) / `-approved-file <file>` | `-checker <astchecker>` | A proof-carrying run certificate: the used-policy set is bound to the run's signed absence root and is a subset of the approved allowlist (only-approved-policies), and every used policy has an anchored, digest-linked convergence certificate in the run's signed tree (policies-convergence-certified); with `-checker`, the oracle's convergence verdict on each used policy must AGREE with its certificate. |

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
re-establishes the claims from the disclosed policy bytes with the external oracle and fails if the
oracle disagrees. It cross-checks both facts. Convergence: the oracle certifies (or refutes) it, and
a certificate that says convergent while the oracle refutes is rejected. Classification: the oracle
also certifies the compensation-free (CRDT-fragment) verdict, via `compensationFree` in
`AstChecker.v` (a machine-checked, axiom-free predicate: no in-domain state ever needs repair, the
AST analogue of "max repair depth = 0"), emitted as a `compensation_free=<bool>` line the CLI parses
and compares. So a certificate that overstates either convergence or the CRDT classification is
caught. `compensationFree_step_no_repair` is in the axiom-free gate alongside `check_sound_converges`.

```
# both bundles authentic and in the same signed tree, the certificate certifies the anchored
# policy's digest, the leaf's bytes hash to it, and the oracle's convergence AND compensation-free
# verdicts agree with the certificate:
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

## Proof-carrying runs

The primitives above prove one fact at a time: this action happened, this policy was anchored, this
policy is convergent, this policy was not used. A **proof-carrying run** composes them into one
portable certificate asserting behavioral-property compliance over a WHOLE run, checkable offline
against a single signed tree head. The innovation is the composition, not new cryptography: a
`RunCertificate` binds together the used-policy absence commitment, the anchored policy and
convergence leaves, and the run's STH, and `VerifyRun` re-derives every property from the disclosed
proofs.

v1 asserts two properties, each dischargeable from a committed leaf:

- **only-approved-policies**: every policy digest exercised by a governed action in the run is a
  member of a caller-supplied approved allowlist. This is the **completeness-bearing** property: it
  commits WHICH policies were used, via the policy-used absence commitment (`AbsenceRoot` over
  `PolicyUsedKey`). The used set is exactly the sorted distinct keys the absence tree commits to, so
  the verifier recomputes that root from the disclosed set and confirms the signed absence STH
  commits to it. That is what gives the negative teeth: a policy that was in fact used cannot be
  dropped from the disclosed set without changing the root and breaking the signature check.
- **policies-convergence-certified**: for each used policy digest, an anchored convergence
  certificate leaf exists in the same signed tree as the policy leaf and links to its digest,
  exactly as `verify-convergence` establishes. With `-checker`, the external oracle is run on each
  disclosed policy's bytes and its verdict must AGREE with the certificate, so a certificate that
  overstates convergence is caught.

Composing the two yields "every governed state in the run was produced by an approved,
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

// Verify: offline, trusting only the out-of-band public key.
res, _ := audit.VerifyRun(cert, pub)   // res.OnlyApprovedPolicies && res.ConvergenceCertified
```

The CLI verifies the same certificate for an auditor who does not write Go, and takes the allowlist
as its own input (never the certificate's embedded list, so a producer cannot pass by widening its
own set):

```
# only-approved-policies (used set bound to the signed absence root and a subset of the allowlist)
# and policies-convergence-certified (each used policy anchored and digest-linked); with -checker the
# oracle's verdict on each policy must agree with its certificate:
goagents-audit verify-run -cert runcert.json -pubkey <hex> -approved <digest> -checker ./astchecker
```

Scope, stated precisely: the certificate proves properties of the **governed, committed boundary**
of the run: which policies ran, that each is on the approved allowlist, and that each has an
anchored convergence certificate that (with the oracle) is confirmed convergent. It does NOT prove
the model's judgment was correct, that ungoverned side effects were appropriate, or the
runtime-refinement claim (the replay differential check covers the events this run took, not all
inputs). The used-policy completeness rests entirely on the absence-root key-set commitment
described above. Property support for **authority-bounded** (every governed action under a grant
descending from the root, via `VerifyDelegationChain`) and **quorum-backed** commits composes from
the same seams and is deferred to a later version. Runnable end to end in
`examples/proof-carrying-run`.

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
`VerifyDelegationChain` checks a root-to-leaf chain: each hop signed by its issuer, each `ParentRef`
equal to the parent's digest, and each hop an attenuation of its parent (`AttenuatesNumericScope`
covers the "lower the limit" case; scope semantics are otherwise domain-defined). An auditor thus
confirms a sub-agent's authority descends, unbroken and never widened, from a root principal each
hop signed. Runnable end to end in `examples/delegation`.

**Attenuation by default.** `AttenuatingSubAgent` wires this into the sub-agent seam so narrowing is
the default, not something the caller remembers to do. Bind the acting grant and signer once at the
root with `WithGrant`; then each delegation through the tool mints a narrower child grant (linked to
the parent, signed, and anchored as a leaf), rebinds the sub-run's identity to it, and propagates it
so a deeper delegation narrows again. With no grant on the context it is a plain sub-agent that
inherits identity, so it is safe either way. The wrapped sub-agent still runs its own full loop and
reasons autonomously; only its authority shrinks. The result is that capabilities monotonically
decrease down a delegation tree by construction, and the chain stays provable via
`VerifyDelegationChain`.

**Earned authority.** `audit.EarnedAuthority` drives a grant's scope from the agent's track record:
authority starts at a baseline rung, is promoted one rung after a clean streak (capped by the
ladder's top), and resets to baseline the instant an anomaly is flagged. Each change re-issues a
signed grant that is a child of the root, so the earned limit provably never exceeds the root
ceiling (every earned grant passes `VerifyDelegationChain`, so even a buggy controller cannot widen
past what the root principal authorized). The asymmetry is the safety property: promotion is slow,
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
- **Journal compaction with proof continuity** (design note: [COMPACTION.md](../design/compaction.md)): seal
  a closed prefix into a signed checkpoint plus a deterministic state snapshot, carry the previous
  root forward as the next segment's first leaf, and drop the sealed raw records. Proofs survive the
  boundary via a signed checkpoint chain; the load-bearing invariant is that only a closed prefix
  may be sealed, so at-most-once is never broken. Not implemented.
- **Pinned cross-language canonicalization** so non-Go verifiers can reproduce leaf bytes (today
  leaves are Go `json.Marshal`, deterministic in-ecosystem but not a pinned wire format).
