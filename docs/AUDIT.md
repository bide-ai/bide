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

Each leaf is a kind-tagged canonical encoding, so event types never collide, and `ModelEvent`
carries the inner delta's kind. `Root`/`Head`/`Prove`/`Sign` behave exactly as they do over the
journal; the `Inclusion` proof type and signing path are shared. (Prototype: `audit/eventsink.go`.)

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

## Next

RFC 6962 is fully covered (Head, inclusion, consistency, STH) over both the journal and, via
the event→audit sink (`EventLog` / `Record` / `TreeHead` / `ProveConsistency`), the semantic
event stream (live via `EventLog`, and crash-durable via `EventLogFromJournal` /
`agent.ReplayEvents`), all sharing one verification surface. Possible extensions if a use case
needs them: an audited `Durable` decorator that emits an STH automatically per run; a BYO
`EventStore` port for callers who want the event trail on a separate retention lifecycle from
the resume journal; and integration with an external transparency log for out-of-band anchoring.
