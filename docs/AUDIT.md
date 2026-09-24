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

RFC 6962 is fully covered (Head, inclusion, consistency, STH). Possible extensions if a use case
needs them: an audited `Durable` decorator that emits an STH automatically per run, and
integration with an external transparency log for the out-of-band anchoring.
