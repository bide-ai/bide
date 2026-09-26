# Security model

This is the authoritative page for what the `audit` package's cryptography does and does not
guarantee. If you are deciding whether the audit trail meets a compliance or trust requirement,
read this first. The package is stdlib-only (`crypto/sha256`, `crypto/ed25519`, and the
post-quantum options below), with no external dependencies.

## What IS guaranteed

- **Integrity.** Any modification, insertion, deletion, or reorder of a journal record changes
  the cryptographic commitment (the hash-chain `Head` or the RFC 6962 Merkle `Root`). A verifier
  who holds the committed root detects any such change.
- **Authenticity.** A signed tree head (`SignTreeHead` / `Verify`) binds the root to a size and a
  timestamp under a signing key. A verifier with the corresponding public key confirms the
  commitment was produced by the key holder and was not forged.
- **Append-only tamper-evidence.** A consistency proof (`ProveConsistency` / `VerifyConsistency`)
  between two signed tree heads proves the history was only appended to, never rewritten or
  reordered. Retroactive edits are detectable.
- **Non-repudiation of the recorded claim.** A signature over the tree head, and a separate
  issuer signature over a `Grant`, attribute the recorded commitment (and the recorded authority)
  to a key holder who cannot later disown it. The proof commits to the identity *claim* embedded
  in a governed leaf (see the identity boundary below).
- **Selective disclosure.** An inclusion proof (`Prove` / `VerifyInclusion`, or a portable
  `ProofBundle`) proves that one record is in a committed run while revealing nothing else in the
  run: no other customer, prompt, or field.

## What is NOT guaranteed: confidentiality

The audit trail does not provide **confidentiality**. This is scoped out explicitly:

- Journal leaves are **not encrypted**. Anyone who holds the journal (or the event store) can
  read every recorded record in the clear.
- A disclosed record is **visible** to whoever you disclose it to. Selective disclosure limits
  *which* records you reveal; it is not encryption. A `ProofBundle` for one tool call hides the
  other records by not including them, not by encrypting them.
- Confidentiality of leaf content, if you need it, is the deployment's responsibility: encrypt
  sensitive payloads before they enter the journal, or keep the journal itself in a trust domain
  with appropriate access control. The cryptography here proves *what happened*; it does not
  hide it.

## Anchoring: the condition on tamper-evidence

Integrity is unconditional: any edit changes the root. But a hash chain or Merkle tree stored in
the same database an attacker fully controls can simply be rewritten and re-hashed, and no
verifier who trusts that database would notice. **Integrity becomes tamper-evidence against the
operator only once the signed tree head is anchored out-of-band**, meaning:

- signed with a key the app tier does not fully control, and/or
- published to a separate trust domain (a Certificate-Transparency-style log, a
  notary/timestamping service, another account's WORM store, a public ledger).

The guarantee anchoring buys is: *"you committed the root elsewhere, so any later divergence is
provable."* Anchoring is a real deployment requirement, not an optional extra. `AuditedStore`
plus the `Anchor` port make it push-based and automatic: wrap any `Durable`, and every durable
step is signed and published to the external log you configure. `Anchor` is bring-your-own; the
in-memory `MemAnchorLog` is the reference implementation and keeps its own append-only tree over
the published tree heads, so a monitor can prove a given tree head was anchored and that the
anchor log itself only grew.

## Key custody

Every guarantee above rests on the custody of the signing keys.

- The **tree-head key** signs the commitment. If the operator holds it with no independent
  control, an operator who is also the attacker can re-sign a rewritten tree, which is exactly
  why anchoring to an independent trust domain matters: the divergence between your locally
  re-signed root and the externally anchored one is what remains provable.
- The **grant issuer key** (see delegation, below) signs authority statements and is deliberately
  distinct from the tree-head key, so authorization is attributable to the principal rather than
  to the operator that runs the log.

Attribution is only as strong as the key custody behind these signatures. The runtime does not
manage keys; supplying and protecting them is the deployment's responsibility.

## Identity is a claim, not authentication

When a deployment binds an acting identity to a run (`agent.WithIdentity`, carrying `Actor`,
`OnBehalfOf`, and `AuthorityRef`), a governed leaf stamps those fields into the committed record,
so an inclusion proof commits to who acted, on whose behalf, and under what authority. The
boundary to understand:

- The proof commits to the identity **claim** recorded in the leaf.
- **Authenticating** the principal, meaning establishing that the claimed actor really is who
  they say, is the operator's IdP / PKI, not this package.

Identity is assigned by the deployment from its own auth layer (an IdP, a signed grant, a service
identity), never by the model. It rides the context, so governed actions and sub-agents inherit
it. The audit trail proves the claim was recorded and is bound to the run; it does not vouch for
the claim's truth beyond the key custody behind the run's signatures.

## Post-quantum and hybrid signatures

Signed tree heads sign under a pluggable scheme, all in the Go 1.27 standard library, so choosing
one adds no dependency:

- `ed25519` (default): small, fast, FIPS-approved.
- `ml-dsa-65` (FIPS 204): post-quantum.
- `ed25519+ml-dsa-65` (hybrid): accepted only if both signatures verify.

Why it matters for audit specifically: anchors are long-lived, so they face a harvest-now,
forge-later exposure. The signature is the quantum-vulnerable part; the SHA-256 Merkle hashing is
not affected and is unchanged. `SignTreeHeadWith` / `VerifyWith` carry the scheme end to end
(including `ProofBundle.VerifyWith` / `AbsenceBundle.VerifyWith`); the legacy `SignTreeHead` /
`Verify` path is untouched, and existing ed25519 bundles keep verifying. One toolchain
constraint: `crypto/mldsa` is unavailable under the FIPS 140-3 module, so FIPS mode and ML-DSA
are mutually exclusive. A FIPS-required deployment takes ed25519; a post-quantum-focused one
takes ML-DSA or hybrid.

## Offline verification

Verification never requires trusting the producer or importing the producer's runtime. Two paths:

- The **`goagents-audit` CLI** (`cmd/goagents-audit`) is the auditor-facing front end. It imports
  only the core and `audit` packages and no store backend, so it operates on an exported journal
  (a JSON array of `Record`) plus a signed tree head. `verify` and the other verify verbs print a
  one-line verdict and set the exit code (**0 = passed, 1 = failed**, 2 = usage error), which is
  the CI-gate contract. The `-pubkey` flag must come from the anchor operator out-of-band, never
  from the bundle: that is what makes it a proof you verify rather than a log you trust.
- The **`audit/verify` package** is stdlib-only (no `agent` dependency) and checks inclusion,
  consistency, and tree-head signatures from raw leaf bytes. A third party who will not import the
  SDK at all can vendor just this package, or reimplement it from RFC 6962 and check the SDK
  against it. The two verification paths are cross-checked bit-for-bit in the tests, so the
  standalone mirror cannot drift.

The Merkle tree, inclusion proofs, and consistency proofs implement RFC 6962 (the same
construction Certificate Transparency logs use) and are checked against the published RFC 6962
reference test vectors, not a homegrown look-alike.

## See also

- `docs/AUDIT.md`: the full feature walkthrough (proof bundles, governed actions, proof-carrying
  runs, event logs). This page consolidates the caveats that walkthrough scatters.
- `docs/guides/delegation.md`: signed grants, the delegation chain, and capability attenuation.
- `audit/`: the package. `audit/verify/`: the stdlib-only standalone verifier.
- `cmd/goagents-audit`: the offline CLI.
