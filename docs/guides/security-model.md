# Security model

This is the authoritative page for what the `audit` package's cryptography does and does not
guarantee. If you are deciding whether the audit trail meets a compliance or trust requirement,
read this first. The package is stdlib-only (`crypto/sha256`, `crypto/ed25519`, and `crypto/mldsa`
for the post-quantum options below), with no external dependencies.

## What IS guaranteed

- **Integrity.** Any modification, insertion, deletion, or reorder of a journal record changes
  the cryptographic commitment (the hash-chain `Head` or the RFC 6962 Merkle `Root`). A verifier
  who holds the committed root detects any such change.
- **Authenticity.** A signed tree head (`SignTreeHead` / `Verify`) binds the root to a size and a
  timestamp under a signing key, and signs the name of its scheme with them. A verifier with the
  corresponding public key confirms the commitment was produced by the key holder and was not
  forged; a signature is never checked under a scheme other than the one it was made with.
- **Domain separation between trees.** One key signs several trees per run: the journal, the
  absence key sets projected from it (tool uses, used policies), and the event stream. The signed
  encoding (`bide.audit.sth.v5`) commits to the scheme, the tree's **kind**, and the **run ID**, and a key-set
  head also commits to the journal tree (size and root) it was projected from. Every verifier
  requires the kind it expects: a journal proof needs a journal head of the bundle's run, an
  absence proof needs a key-set head of the set its key belongs to, and a run certificate needs a
  used-policy head of its own run projected from its own journal tree. So no head can be replayed
  as another kind of tree or for another run, and every run ID a verifier reports is authenticated.
- **Every reported field is verified.** An `EvidencePackage` is checked field by field: its format,
  its key (the `alg` and `public_key` it names must be the verifier's), its run (against the signed head), each item's kind and label (against the proven
  record), its grant chain (against the anchored grant leaves), its run certificate (for this run
  and tree, against the auditor's allowlist), and its consistency proof (between two signed heads
  of the run). The package is sealed with the log key, so its label, the one field no proof covers,
  cannot be edited after sealing.
- **Append-only tamper-evidence.** A consistency proof (`ProveConsistency` / `VerifyConsistency`)
  between two signed tree heads proves the history was only appended to, never rewritten or
  reordered. Retroactive edits are detectable.
- **Non-repudiation of the recorded claim.** A signature over the tree head, and a separate
  issuer signature over a `Grant`, attribute the recorded commitment (and the recorded authority)
  to a key holder who cannot later disown it. The proof commits to the identity *claim* embedded
  in a governed leaf (see the identity boundary below).
- **Selective disclosure.** An inclusion proof (`Prove` / `VerifyInclusion`, or a portable
  `ProofBundle`) proves that one record is in a committed run. The leaf is the bytes the journal
  stores for the record, verbatim, and the proof carries them (`record_bytes`); the verifier hashes
  those bytes and decodes them only for display and role checks, ignoring fields it does not know,
  so a record written by a newer release still verifies. A `ProofBundle` discloses exactly:
  the record (its full stored bytes, including its random 32-byte salt), its index in the journal, the
  run's size at the signed head, the signed head itself (kind, run ID, size, root, timestamp), and
  the O(log n) sibling hashes on its audit path. It does not disclose any other record's content,
  name, or kind, and those hashes cannot be tested against a guess: every journal record carries
  its own random salt, set when the store first journals it and committed in its leaf, so a
  neighbour's leaf hash (the first sibling on the path) is unguessable even when its content has
  only two possible values, such as an approve/deny decision or `{"fraud_flag":true}`. What the
  index and size reveal is the record's position and how many steps the run had taken.
- **Event-log proofs disclose one event.** An event-log inclusion proof (`EventLog.Prove`, an
  `EventInclusion`, checked with `VerifyEventInclusion`) discloses exactly: the proven event's
  random 32-byte salt, its index in the log, the log's size, and the O(log n) sibling hashes on its
  audit path; the verifier holds the event itself, and a signed event head adds kind, run ID, size,
  root, and timestamp. Every event leaf (`bide.audit.event-leaf.v3`) commits to its own salt, so
  the sibling hashes cannot be tested against a guessed neighbouring event, even a two-valued tool
  result. A live `EventLog` draws each salt from `crypto/rand`; the journal projection
  (`EventLogFromJournal`, `PersistJournal`) derives it one-way from the random salt of the journal
  record the event projects, so a journal `ProofBundle` for that record lets its holder recompute
  the event's leaf, which holds nothing the record does not.
- **Anchor-log proofs.** Anchor-log leaves are not salted. An anchor proof's path covers
  neighbouring entries, whose sequence numbers and run IDs may be guessable, but each entry also
  holds a signed tree head: its root commits to salted leaves and its signature needs the
  signing key, so a proof holder confirms a neighbouring entry only by already holding that exact
  signed head. Do not anchor an unsigned head: it has no such entropy.
- **Absence proofs name their neighbours.** An absence proof (`ProveAbsent`, `AbsenceBundle`)
  discloses the absent key, the number of distinct keys in the set, and, in plain text, the one
  or two committed keys adjacent to it in sort order with their positions: for tool calls, up to
  two tool-use IDs the run did make; for used policies, up to two policy digests it did use. This
  is inherent to a sorted-set absence proof (the verifier must see the neighbours to check they
  bracket the key). It does not disclose those calls' records. Key-set leaves are not salted, so
  the sibling hashes on a neighbour's path can be tested against a guessed key (a policy digest
  is usually public).
- **Canonical encodings.** A journal record is hashed over its stored bytes, never re-encoded.
  Grants, events, anchor entries, and evidence seals are hashed or signed over their JSON encoding, which is one-to-one only over valid UTF-8 (JSON rewrites invalid bytes
  to U+FFFD). A value with invalid UTF-8 in any string is refused, never committed, signed, or
  verified, so two different values never share a leaf or a signature. `bide-audit` reads every
  artifact with `audit.UnmarshalStrict`, which rejects duplicate keys, keys that match a field only
  case-insensitively, unknown fields, invalid UTF-8, escaped lone surrogates, and non-standard
  base64, so a file cannot show a reader one value while the verifier checks another. A proof's
  `record_bytes` are held to the same rules before they are read (only an unknown field is
  tolerated, since a newer release may add one). Tool
  arguments are read by the same strict decoder (it also requires every field the tool's schema
  lists as required), so a `Func` tool reads exactly the values its arguments spell out: no
  dropped, case-folded, or duplicated name stands for a value the text does not show. Malformed keys (the wrong length, or none) verify nothing;
  they never panic.
- **Verdicts are errors.** Every verifier returns an `error`, and only `nil` means verified. A failure
  wraps exactly one of `audit.ErrNotVerified` (the artifact was read and does not hold),
  `audit.ErrFormat` (not the format this version reads), or `audit.ErrMalformed` (cannot be read as
  what it claims to be). Before 1.0 a verifier reads only the current format of each artifact.

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

### Retrieved documents are stored like tool results

Retrieved documents are durable content at rest, like tool results. A `RetrievalTool` call's
documents are its journaled result, and `WithRetrieval` journals the query and the documents it
retrieved as a step of the run, so a resumed run shows the model the same documents and the
journal records what the model was given. The retrieval runs before the model middleware chain,
so a model middleware that refuses the call does not keep the query from the store or the
documents out of the journal; a policy that must wraps the `Retriever` (see
[RAG and memory](rag-memory.md)). The documents are stored as written: no redaction applies to
them, and anyone who holds the journal can read them. Salting keeps them out of other records'
proofs: a `ProofBundle` for a different record of the run discloses a neighbouring leaf only as a
sibling hash over salted content, which cannot be tested against a guessed document. A proof
of the retrieval record itself discloses the documents in full. If documents are sensitive,
keep them out of the `Retriever`'s results or keep the journal in a trust domain where they may
be read (see [RAG and memory](rag-memory.md)).

### Tool errors are redacted before they are journaled

When a tool call fails, its error text becomes the call's result: it is journaled, sent to the
model, and hashed into the Merkle tree, where a proof bundle for that call discloses it. Error
text often quotes a URL (a net/http `*url.Error` quotes the whole request URL), and a URL can
carry a credential. So by default the agent redacts every URL in the text before it is recorded:
its userinfo, the value of each query parameter (the parameter names stay), and its fragment
each become `REDACTED`, while the scheme, host, and path stay, so the model still learns what was
called and why it failed:

```
Get "https://REDACTED@api.example.com/v1/items?key=REDACTED&page=REDACTED": dial tcp ...: connection refused
```

A `*url.Error`'s URL is redacted as a whole; every other URL is found by scanning the text.
`Agent.WithToolErrorRedactor(func(tool string, err error) string)` chooses the recorded text for
a failed call when URL redaction is not enough (an account number, a token a service echoed
back); URL redaction still applies to what it returns. This covers every failed tool call, a
failed sub-agent (its failure is its tool call's error), and the two records a saga journals
for its failure. The error returned to the caller (for example `SagaAborted.Cause`) is the
tool's own, unredacted. With content capture on, `trace` records a failed call's journaled text
on its span, and redacts the URLs in the chat and run spans' error text the same way. A secret that is not in a URL, and not removed by your redactor, is
journaled as written.

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
plus the `Anchor` port make it push-based and automatic: wrap any `Store`, and every journal
write is signed and published to the external log you configure. `Anchor` is bring-your-own; the
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
  to the operator that runs the log. A delegation chain holds only if each child is issued by its
  parent's subject, expires no later than its parent, and keeps every one of its parent's scope
  constraints, equal or narrower (`CheckAttenuation`); an earned-authority grant is current only
  while it is the last leaf of its controller's ledger run, so a demotion revokes the higher grant
  at once for any verifier holding the ledger's latest anchored head.

A delegation's runtime authority comes from the journal: a saga rollback compensates under the
child grant the sub-run journaled, after checking its signature and that it narrows a bound grant.
It does not prove that this delegation minted that grant, so whoever can write the journal could
plant another validly signed child grant there. The journal itself is trusted at run time; that
it was not rewritten is what anchoring (above) makes provable afterward.

Attribution is only as strong as the key custody behind these signatures. The runtime does not
manage keys; supplying and protecting them is the deployment's responsibility.

## Identity is a claim, not authentication

When a deployment binds an acting identity to a run (the `agent.WithIdentity` option, carrying `Actor`,
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

- `ed25519` (`Ed25519Signer`): small, fast, FIPS-approved.
- `ml-dsa-65` (`MLDSASigner`, FIPS 204): post-quantum.
- `ed25519+ml-dsa-65` (`HybridSigner`): accepted only if both signatures verify. Each component
  signs a distinct label followed by the message (`bide.hybrid.ed25519.v1`,
  `bide.hybrid.mldsa65.v1`), so the ed25519 half stripped out of a hybrid signature verifies neither
  as a hybrid signature nor as a plain ed25519 signature over the same head.

Why it matters for audit specifically: anchors are long-lived, so they face a harvest-now,
forge-later exposure. The signature is the quantum-vulnerable part; the SHA-256 Merkle hashing is
not affected and is unchanged. Every signing entry point takes an `audit.Signer` and every
verifier an `audit.Verifier`, so the scheme is carried end to end (tree heads, evidence seals,
run certificates, grants, the anchored store), and every scheme signs the same scheme-, kind- and
run-bound encoding. An approver's m-of-n decision is journaled with the scheme it was signed under
(`Record.ApproverAlg`), and the gate counts it only under a verifier of that scheme. One toolchain
constraint: `crypto/mldsa` is unavailable under the FIPS 140-3 module, so FIPS mode and ML-DSA
are mutually exclusive. A FIPS-required deployment takes ed25519; a post-quantum-focused one
takes ML-DSA or hybrid.

## Offline verification

Verification never requires trusting the producer or importing the producer's runtime. Two paths:

- The **`bide-audit` CLI** (`cmd/bide-audit`) is the auditor-facing front end. It imports
  only the core and `audit` packages and no store backend, so it operates on an exported journal
  (an `audit.JournalExport` of each record's stored bytes) plus a signed tree head. `verify` and the other verify verbs print a
  one-line verdict and set the exit code (**0 = verified, 1 = not verified**, 2 = usage error,
  3 = no verdict, 4 = an input is unreadable or unusable; only 0 means verified), which is
  the CI-gate contract. The `-pubkey` flag (`<alg>:<hex>`, or bare ed25519 hex) must come from the anchor operator out-of-band, never
  from the bundle: that is what makes it a proof you verify rather than a log you trust.
- The **`audit/verify` package** is stdlib-only (no `agent` dependency) and checks inclusion,
  consistency, and tree-head signatures under all three schemes (`verify.TreeHead` with a
  `verify.NewVerifier`) from raw leaf bytes (`verify.JournalLeaf` builds a record's from the bytes a
  store persists for it, a proof's `record_bytes`, salt included). A third party who will not import the
  SDK at all can vendor just this package, or reimplement it from RFC 6962 and check the SDK
  against it. The two verification paths are cross-checked bit-for-bit in the tests, so the
  standalone mirror cannot drift.

The Merkle tree, inclusion proofs, and consistency proofs implement RFC 6962 (the same
construction Certificate Transparency logs use) and are checked against the published RFC 6962
reference test vectors, not a homegrown look-alike.

## See also

- [docs/guides/audit.md](audit.md): the full feature walkthrough (proof bundles, governed actions, proof-carrying
  runs, event logs). This page consolidates the caveats that walkthrough scatters.
- `docs/guides/delegation.md`: signed grants, the delegation chain, and capability attenuation.
- `audit/`: the package. `audit/verify/`: the stdlib-only standalone verifier.
- `cmd/bide-audit`: the offline CLI.
