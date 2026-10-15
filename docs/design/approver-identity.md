# Design: approver identity and revocation

Status: proposed, revision 5. All 17 decisions are settled (the user decided on 2026-10-15; see Decisions). Revision 5 folds in the external assessor's confirmation pass on revision 3 (seven new medium findings and two partial resolutions). Design only; nothing here is built. Revision 2 folded in an adversarial
review (the time rule, the status rule, anchoring order, record placement) and the `crypto/x509`
behaviors checked by running code. Revision 3 folds in an external PKI assessment (one critical, five
high, eleven medium findings): the absence-of-entry rule for CRLs, revocation latency and a status
re-check at execution, CA compromise, time-source independence, one person one seat on an immutable
identifier, the identity-bound downgrade, and an algorithm and issuance-assurance profile. The
assessment disagreed with five of the earlier recommended decisions; they are reconsidered below.
The 17 decisions at the end are settled; the build can start once this revision is confirmed. This is bank readiness item 1.

Today an approver is a hand-enrolled public key: a map from an approver id to a key
(`agent.WithApproverVerifiers`, the `-approver-keys` file). That is enough to show that the holder of
a key approved a call. It does not show who the holder is, it has no end date, and nothing in the
journal says whether the key still belonged to an authorized person when the decision was made. A
bank needs three things on top of the signature:

1. the key is bound to a person in the bank's identity system;
2. the right to approve ends when the person leaves or changes role, without anyone remembering to
   edit a file on every host;
3. an audit, years later and offline, that shows which identity was valid when each decision was made.

This note designs that with X.509 certificates issued by the bank's own PKI. It builds on
[m-of-n approval](design-mofn-approval.md), the [approval intake and `bide approve -to`](../guides/hitl-approval.md#taking-decisions-over-http),
[decision receipts](../guides/audit.md#decision-receipts-noticing-a-decision-deleted-before-it-was-anchored)
and evidence v6, and it does not change what they already guarantee for a bare-key approver.

## Scope and what is claimed

In scope: verifying that a decision was signed under a certificate that chained to a trust anchor the
policy names, whose identity is the approver's, and which was valid and not revoked at a time the
journal can bound; recording what is needed to check that again offline; failing closed when the
needed status or time evidence is missing.

Not claimed:

- That the person was at the keyboard. Proof of presence is the bank's SSO and its MFA when it issues
  the certificate (and, later, a hardware or passkey signer). bide sees a certificate and a signature.
- That the CA, the CRL publisher, the time source or the identity provider behaved correctly. A
  compromised CA can issue a certificate for anyone; this design limits that with name constraints and
  per-policy trust anchors, and says exactly what it cannot detect.
- A trusted clock in the default tier. Time is bounded by signed tree heads, which rest on the log
  signer's clock. The top verdict tier requires a time source independent of the log signer (below).
- Non-repudiation or a qualified/advanced electronic signature. A software key in a 0600 file under an
  SSO-issued certificate does not give sole control of the signing key, and the tally "inherits
  non-repudiation" wording elsewhere should be read as "attributable to a key and a certificate", not as
  a legal term. Verdicts are worded "status as of S under policy P", never "verified" alone.
- Compliance with any regime. On a toolchain with the Go FIPS 140-3 module enabled
  (`GODEBUG=fips140=on`), the signature verification and hashing that `crypto/x509` performs use the
  module's approved implementations. Certificate parsing and chain building (ASN.1, name constraints,
  path building) are ordinary Go code outside the module boundary. Validation belongs to the module,
  not to bide or to `crypto/x509`, and which algorithms are available depends on the module version
  (ML-DSA is not in v1.0.0, as the audit guide notes). This note claims no validation and no
  compliance; whether the arrangement satisfies an assessor is the bank's call.
- Post-quantum certificates for every scheme. `crypto/x509` in Go 1.27 parses and verifies ML-DSA keys
  and ML-DSA signatures in certificates (pinned by a test), so an `ml-dsa-65` approver can be
  certificate-bound under a CA that issues them. The hybrid scheme (`ed25519+ml-dsa-65`) has no single
  X.509 form, so a hybrid approver stays bare-key for now. That is a gap, stated here, not closed.

## Constraints agreed with the user

- The core module is standard library only for this feature: `crypto/x509`
  (`ParseCertificate`, `Certificate.Verify` with `VerifyOptions.CurrentTime`,
  `ParseRevocationList`, `RevocationList.CheckSignatureFrom`). OCSP
  (`golang.org/x/crypto/ocsp`) may live only in an optional module. RFC 3161 token verification is also
  standard library only: a narrow, strict CMS verifier on `encoding/asn1` (decision 17), with no third-party
  CMS dependency.
- SSO is the bank's. Its SSO issues short-lived certificates through its existing PKI (smallstep,
  Vault PKI, ADCS, or another). bide verifies certificates. It never speaks OIDC.

## Threat model

Adversaries and what the design does about each. "Residual" says what remains after the mitigation.

| Threat | Mitigation | Residual |
|---|---|---|
| **Stolen approver key** (disk, backup, malware) | Short-lived certificates (hours) bound the use window; revocation ends it earlier; the effective revocation time `E` invalidates every decision first provable at or after `E`; status is checked fresh when the decision is counted **and again immediately before the call executes** (below), so the revocation latency is a published number, not the certificate lifetime; keys in an HSM or KMS (bide-hsm) make theft harder | Decisions provable before `E` stand, including ones the thief made before the theft was noticed. A late-reported compromise is caught only if the CA dates it back (invalidity date) and the status evidence is late enough (window `W'`). `keyCompromise` with no invalidity date is retroactive to `notBefore` |
| **Departed employee** | Certificates are not renewed after SSO access ends; revocation on departure; the approver id's identity rule no longer matches any live certificate | A decision made before departure stays valid by design. A bank that wants waiting gates to drop a leaver's approvals re-asks the gate (an operational step) |
| **Compromised CA** (or a mis-issuing RA) | Trust anchors per gate or policy, not global; name constraints on the CA; the identity rule fixes the exact identifier per approver id; permitted intermediates are pinned; exactly one SAN; EKU required and enforced along the chain; status required for **every** non-anchor certificate; a per-anchor and per-intermediate **`DistrustAfter`** time, applied against `A(d)`, ends trust in a compromised CA from its compromise time without voiding earlier history | A CA inside its name constraints can still issue a certificate for any approver in its domain. bide cannot detect that. The offline kit shows which issuer each decision rests on |
| **Backdated decision** (a thief signs with a revoked or expired key and sets `decided_at` inside the window) | `decided_at` is the approver's claim. The time that decides validity is `A`, the time of the smallest-size signed head that covers the record, anchored before the gate counts the decision. A decision whose `A` is at or after `E`, or after `notAfter` plus a grace, does not count | The kit's time is only as independent as its source: with the log signer's clock alone the top tier is not reached (see Time). A late covering head means a late `A`, which fails closed |
| **Operator as adversary** (an insider with journal and evidence-producer access, but without the log key, the CA key or an approver key) | `A` is the timestamp of the smallest covering head and head times must be non-decreasing in size, so one slow or edited clock cannot pull `A` earlier; the auditor enumerates heads from the anchor log, not from the producer; the top tier needs an independent time source | An operator who also holds the log signer key can mint heads. That is outside every audit claim in bide today (the log key is the root of trust) and needs the independent time source to bound |
| **Revocation list withheld, replayed or rolled back** | Live: a CRL older than `MaxStatusAge`, past `nextUpdate`, or with a lower `CRLNumber` than the cached one is refused; stale status pauses the gate. Offline: only status issued in `[A, L(C)]` (after the decision, and no later than the first scheduled CRL beyond `notAfter`) counts as evidence of absence; a gap in the CRL number series is "no information" | A revocation the CA never published, or published only after the retention ended, is invisible |
| **Someone enrolls themselves** (a key or identity for their own id, a second id for themselves, a second certificate) | The identity map (approver id to canonical identity) and the trust anchors are policy configuration, change-controlled by the bank; `bide check -diff` flags a weakened gate; one canonical identity under two approver ids is refused, bare-key approvers included; `ExcludePrincipal` keeps the requester out by identity as well as by id | Whoever can change the resolver configuration can still enroll. The note moves the question from "who may add a key" to "who may issue a certificate and edit the identity map" |

Two assumptions are explicit. The log signer's key is trusted to sign tree heads (the root of every
audit claim in bide). The bank's CRL publication point is trusted to publish correctly when reachable;
failures of availability must stop approvals, not weaken them.

## How an approver id binds to a certificate

Today: approver id `ops` resolves to a key. New: approver id `alice` resolves to an **identity rule**
and a **trust set**; the key is whatever key the presented certificate carries.

**What the certificate must say.**

- Identity: **exactly one SAN, of the configured kind, and no other SAN of any kind**, matched
  exactly (after the canonicalization below) against the value the policy's identity map gives for
  that approver id. A certificate with two emails, or an email and a DNS name, is refused: the parser
  returns every SAN, and an ambiguity about which one is "the" identity is a way to be two people
  (pinned by a test). **What the identifier is matters more than its syntax, because the seat is the
  identifier** (decision 9): it must be **immutable and never reassigned**, one per natural person. The
  recommended identifier is therefore an **opaque person id in a URI SAN with a host**
  (`https://idp.bank.example/person/8f3a9c...`, a stable HR or directory key, not a name). An
  **rfc822Name** (`alice@bank.example`) is accepted as the identifier only when the bank states and
  evidences that its mailboxes are one per person, never aliased and never reassigned after a leaver
  (shared mailboxes, aliases, admin accounts and recycled addresses each let one human hold two seats
  or hand a seat to someone else); otherwise the email may ride along as informational data and is not
  the seat key. Go's chain verification enforces name constraints on both email and host-ful URI SANs. A
  URI with no host (`urn:...`) is not usable: with URI name constraints in the chain, Go refuses it
  ("URI with empty host cannot be matched against constraints"). The Subject DN (`CN`) is **not** used:
  a CN is free text, and `crypto/x509` does not enforce directory-name constraints (a non-critical one
  is ignored; a critical one makes the chain fail with "unhandled critical extension", which is safe
  but protects nothing). These behaviors were run against Go 1.27.0 and are pinned by the tests in
  `internal/identitycheck` (test plan item 23).
- Two details from those runs shape the rules. A permitted email constraint written `bank.example`
  also permits subdomains (`alice@evil.bank.example` verifies), unlike RFC 5280's reading, so a CA
  that must exclude subdomains adds an excluded subtree `.bank.example`, and the design never relies
  on a permitted subtree alone to pin a domain: the identity map matches the SAN exactly, and the
  constraints are defense in depth against a CA issuing outside its remit. And `Certificate.Verify`
  requires the ServerAuth extended key usage when `VerifyOptions.KeyUsages` is empty, so a leaf
  carrying only the bank's private OID fails; the verifier passes `ExtKeyUsageAny` and checks the
  required OID itself against `Certificate.UnknownExtKeyUsage` (a leaf with no EKU at all passes
  `ExtKeyUsageAny`, so the by-hand check requires the OID to be present).
- Key usage: digital signature. Extended key usage: a bide approver EKU (a private OID the bank
  assigns, documented in the guide), checked by hand as above, so a certificate issued for TLS or mail
  cannot approve. `id-kp-emailProtection` is accepted only when the certificate comes from a **dedicated
  issuing CA** for approvers (every S/MIME certificate carries it, often with different key handling).
- **The CA's own EKU is enforced by bide.** `Certificate.Verify` with `ExtKeyUsageAny` does not apply EKU
  nesting along the chain (pinned by a test: a private-EKU leaf under a serverAuth-only intermediate
  verifies), so the verifier walks the accepted chain and requires every CA certificate that carries an
  EKU to permit the approver EKU. Scoping a CA by EKU is bide's job here, not the standard library's.
- **Issuance assurance is stated in the certificate.** The leaf carries a certificate policy OID
  (`Certificate.Policies`) that the bank's CA asserts only after SSO with MFA step-up and a key
  generated or held in hardware (or attested), and the gate requires it. bide cannot see MFA or key
  protection; it can require that the CA says so. The CA enforces lifetime limits and proof of
  possession (RFC 2986/4211) at issuance. A key must not be reused across certificates with different
  EKUs or across approvers (the dynamic shared-key check covers the second).
- Not a CA (`basicConstraints` CA false). Serial numbers are at most 20 octets. **Algorithm profile**:
  leaf key schemes are only those the decision schemes can verify: `ed25519`, `ecdsa-p256` and
  `ml-dsa-65` (RSA and P-384 leaves are refused until a decision scheme exists for them). The scheme is
  **derived from the certificate's SPKI**, and a record whose `approver_alg` differs from it is not
  counted (algorithm-confusion guard); the ECDSA low-S rule and DER strictness of the existing schemes
  apply to `v4` unchanged. CA certificates and CRLs may be signed with: **RSA (PKCS#1 v1.5 or PSS) with SHA-256 or better and a
  modulus of at least 3072 bits** (2048 only through an explicit `AllowRSA2048CA` policy value with an end
  date, since many bank roots and AD CS issuing CAs are RSA-2048 today), **ECDSA P-256 or P-384** with
  SHA-256 or SHA-384, Ed25519, or ML-DSA-65; never SHA-1 or MD5, and the same allowlist applies to **every signed object the verifier accepts**: CRLs, OCSP responses and their responder certificates, and RFC 3161 tokens (CMS digest and signature algorithms) and the TSA certificate. Whether `x/crypto/ocsp` accepts SHA-1 signatures is confirmed by a test when that module is built; bide's own check does not depend on the answer. Go refuses a SHA-1 certificate signature but **accepts a SHA-1 CRL signature** (`TestSHA1CRLSignatureHandling`), and reads no RSA modulus limit above 1024 bits, so bide checks the algorithm of every CRL and the modulus of every CA key itself (`TestRSA2048CAVerifiesInGoSoBideMustEnforceModulus`). The leaf schemes above are narrower than
  the CA schemes on purpose: a leaf must be something a decision scheme can verify, a CA need not be. A hybrid approver stays bare-key
  (see Scope), and certificate-bound approvers therefore give up hybrid protection: a recorded
  limitation.
- Unknown critical extensions on the leaf or an intermediate make the chain fail (`Verify` does this
  itself, pinned by a test).

**Canonicalization.** One bank-defined function, `Canonical`, maps an identity string to its
canonical form, and it is used in **every** place an identity is compared: the identity map, the SAN,
the `run:principal` requester (`ExcludePrincipal`), and the approver-id mapping for bare-key
approvers. The default: Unicode NFC, then lowercase the whole email (local part included; the bank
states that its directory is case-insensitive), ASCII only (internationalized addresses are refused
until a bank asks), no trimming or alias folding (`a.smith` and `asmith` are different identities
unless the identity map says otherwise). URI identifiers use a **strict profile** and anything else is refused (Go returns a URI SAN exactly as issued, so two spellings stay two strings: `TestURISANIsNotNormalisedByGo`): scheme `https` (or the bank's one
fixed scheme), no userinfo, no explicit port, no query, no fragment, no percent-encoding, no trailing
slash, a lowercase host equal to the configured host, and a path that matches the policy's fixed pattern
(default `/person/` followed by 32 lowercase hex digits, or a lowercase UUID). A CA that could issue one
person's id in two spellings would give one person two seats; the profile leaves one spelling. The
case-folding of the seat key therefore applies to the email kind only, and is a no-op for the URI kind. The comparison
is bytes after this, never substring or suffix. A bank with a different directory rule supplies its
own `Canonical`; the verdict records its name and version.

**Where the rules live.** Not in the journal's `ApprovalPolicy` (which records `need` and approver
ids and stays as it is). A new value `audit.IdentityPolicy`, handed to the resolver, holds:

<!-- docsnip: skip proposed API in a design note; these types do not exist until build step 1 -->
```go
type IdentityPolicy struct {
    Anchors   *x509.CertPool          // trust anchors for THIS gate (not the system roots)
    Inter     []SPKIHash              // PINNED intermediates: the chain may only pass through these
    Distrust  map[SPKIHash]time.Time  // per anchor or intermediate: ignore it for decisions with A(d) >= the time
    Identity  map[string]string       // approver id -> canonical identifier, for EVERY approver of the gate
    IDKind    IDKind                  // PersonURI (recommended) or Email (only with a stated injectivity control)
    EKU       asn1.ObjectIdentifier   // required extended key usage
    Policy    x509.OID                // required certificate policy (MFA step-up and hardware or attested key)
    Canonical Canonicalizer           // one function for identifiers, principals and SANs; case-folds the seat key
    Status    StatusPolicy            // CRL required, OCSP optional, MaxStatusAge, ExecStatusAge, W, graces; CRL URLs
    MaxChain  int                     // path length bound
    IndependentTime bool              // require an independent time source for a counted decision (gates above a value threshold)
}
```

`Identity` lists every approver of the gate, bare-key approvers included. A bare-key approver has an
identity in the map too (their person), so **one person is one identity** whatever kind of approver id
they hold: two approver ids that canonicalize to one identity are refused with `ErrConfig`, and the
seat is the identity (below). A resolver returns an `ApproverVerifier` for an id as it does today; for
a certificate-bound id it returns a verifier that also implements the new decision-level interface.
One process can serve some gates with bare keys and some with identity policies, and one policy can
mix approvers of both kinds.

**Trust anchors and pinned intermediates.** A refund gate may trust the "treasury approvers" issuing CA
and a deploy gate the "release approvers" CA. The chain a decision carries is **not signed** (only the
leaf hash is), and `Certificate.Verify` can return several chains when the intermediate pool holds
reissued certificates for one key (pinned by a test). So the permitted intermediates are pinned in the
policy by public key hash; the verifier builds the chain from the pinned set only (a carried
certificate outside it is ignored, and cross-certificates are not accepted), requires **exactly one**
valid chain (more than one is `ReasonCertChain`, ambiguity being a way to route a decision to another
anchor), and records the chain it accepted. Requirements on status and constraints apply to every
candidate, not only the first. Name constraints on each CA are checked by `Verify`. The anchors a
decision was counted under are named in the gate's identity record (see Journal changes).

## Time: what makes a decision valid

This is the hard part, so it is stated as definitions and one rule.

**Facts, and who controls them**

| Fact | Source | Controlled by |
|---|---|---|
| `decided_at` | signed by the approver | the approver (or a thief): a claim only |
| `recorded_at` | journal record, unsigned | the process that wrote the record: not trusted for validity |
| `T_head(k)` | `timestamp_nanos` of the signed tree head `k` | the log signer; a head whose size exceeds the decision's journal position **provably covers** the record (the receipt machinery already checks this) |
| `A(d)` | `T_head` of the **smallest-size** signed head that covers the decision record | derived (below) |
| `notBefore`, `notAfter` | certificate | the CA |
| `E` | revocation effective time (below) | the CA |
| `thisUpdate`, `nextUpdate`, `CRLNumber` | CRL | the CA (or its CRL issuer) |

**`A(d)` is the timestamp of the smallest covering head, not the minimum timestamp over covering
heads.** Taking a minimum would let one slow or edited clock (a head stamped early by a skewed
process, or one planted) pull `A` earlier than the truth. With the smallest-size rule `A` is the time
the log signer claimed for the first head that included the record, and two more rules protect it:

- **Head times must be non-decreasing in size.** Over all signed heads of the run (sorted by size), a
  larger head with a timestamp earlier than a smaller head's, beyond `ClockTolerance` (default 60 s,
  covering two writers with skewed clocks), marks **both members of each inverted pair** (the larger head stamped early and the smaller head stamped later) as
  **contradictory**; the larger head stamped early is the dangerous one for revocation, because it can pull `A` earlier for the decisions it first covers. A decision is `not verified: contradictory head times` only when its smallest
  covering head, or the pair that fixes its `A`, is implicated (any decision whose `A` comes from either member of an inverted pair is `not verified` unless waived); other decisions of the run stay
  verifiable, so one skewed or planted head does not make a whole run unauditable. Nothing is repaired
  silently: an **explicit, recorded waiver** (`-waive-head-times`, naming the heads and the reason, signed
  by the auditor) is the only way past it.
- **The auditor enumerates heads from the anchor log, not from the producer.** How an auditor knows
  there is no earlier covering head than the one in the kit: they list the run's heads in the anchor
  log (an independent party) and compute the smallest covering size over that list. A head that the
  producer put in the kit but the anchor log does not show is not used. If the auditor has only the
  producer's kit, the verdict says "earlier covering heads cannot be excluded" and the decision cannot
  reach the top tier.

**The time source is part of the verdict.** A head's timestamp comes from the log signer's clock, which
is the same party the operator-adversary may influence. Therefore:

- The **top tier requires an independent time source**, and a gate above the bank's value threshold
  sets `IndependentTime` so a decision **does not count** without one (RFC 3161 token verification is
  therefore in the first build step, not the last). `A(d)` is the earliest time that an independent
  source attests, otherwise the smallest covering head's time. "Independent" means a different administrative domain and different key
  custody from the log signer, and the auditor lists the time authorities they trust (`-trusted-tsa`). The
  independent source is: an RFC 3161 token over the covering head (or its hash), whose imprint must match
  and whose `genTime` must not be earlier than the head it stamps (less `ClockTolerance`), from a TSA whose
  certificate carries the critical `id-kp-timeStamping` EKU (RFC 3161 section 2.3), from a TSA the auditor trusts, or an anchor-log receipt carrying the log operator's own
  signed time (today `audit.AnchorEntry` carries a sequence number and a head, no time; a time-bearing
  anchor receipt is added in the build). The verdict prints the source.
- With only the log signer's clock the best tier is lower and says so.

**The rule.** A decision `d` by approver `a` counts iff all of:

1. the certificate chain, built from the pinned intermediates, verifies to a trust anchor of the gate's
   trust set with the required EKU (also along the chain), policy OID, name constraints and key usage,
   at time `A(d)` for every CA certificate and at `min(A(d), notAfter)` for the leaf (so a decision
   anchored inside `anchorGrace` after `notAfter` is not rejected by the standard library's own validity
   check and rule 3 bounds it instead); no certificate of the chain is past its `DistrustAfter` for
   `A(d)`; the leaf has **exactly one** SAN, of the identifier kind, and its canonical value equals
   `a`'s identity rule;
2. `notBefore <= decided_at <= notAfter` and `decided_at <= A(d) + skew` (the signed time is
   plausible and not from the future of its own proof);
3. `A(d) <= notAfter + anchorGrace`;
4. the signature verifies under the leaf key over decision bytes that include the leaf's hash (see
   Journal changes);
5. the status of **every non-root certificate of the accepted chain** (the leaf and each intermediate)
   is known and good (status rule below): none has `E <= A(d)` in the usable status data.

**Why `A(d)` and not `decided_at`.** A stolen key can set `decided_at` to anything inside the validity
window. It cannot make a signed tree head that does not cover its record exist earlier than it was
signed. If the thief's write is after `E`, rule 5 rejects it. If the thief wrote before `E` and the CA
later records an earlier compromise date, `E` moves earlier and rule 5 rejects it too.

**Reanchor is not a free signing oracle.** The gate verifies a decision's chain, identifier,
EKU and signature **before** it asks the application to anchor, so a junk decision cannot cause a head to
be signed, and it rate-limits anchoring per run (by default one forced head per 10 s). The log key may be
an HSM or KMS key with quotas and cost, and any submitter of a direct-write decision could otherwise drive
it.

**Anchor first.** A cert-bound decision is **not counted until a signed head covers it.** The live gate
and the auditor therefore use the same `A(d)`, computed by the same function over heads from the same
kind of source: there is no separate "verification time" the gate trusts and the auditor cannot see
(an earlier draft had one; it is dropped). The cost, and its fix, is below.

**The live gate must not deadlock on a direct write.** A decision journaled by `bide approve` directly
has no covering head until the application next writes to the run, and if the gate waits for one the
run can wait forever (the application is waiting for the gate). So **before tallying a cert-bound
decision that no head covers, the gate asks the application to anchor**: the gate's store wrapper
signs and publishes a head over the journal (`AuditedStore.Reanchor`) and re-reads. The application's
store must therefore be an audited store, or a configuration with a cert-bound approver is refused at
build time with `ErrConfig` ("identity policy needs an audited store or a head source"). The model's
`NoStuckPause` is extended to include this step (Formal model impact).

The gate and the auditor both need the run's heads after the fact, including heads signed before a
process restart. `AuditedStore` keeps only what it published since it started, so the build adds an
optional read capability to the anchor port (`HeadsFor(ctx, runID)`; `MemAnchorLog` implements it and a
real anchor exposes its list), and the application passes a `HeadSource` to the identity environment.
Without it a restarted process would compute a late `A` for an old decision and reject it.

**Live gate versus offline audit.** Both run one function, `TallyApprovals` extended with an **identity
environment**: the trust sets, a status snapshot (CRL and OCSP bytes with fetch times), and a head
source. The terminal tally's identity data (digest of the environment used) is recorded in its own
record (see Journal changes), so a difference between the gate's snapshot and the auditor's is reported.

### Status rule (what makes status "usable")

**Absence of an entry is evidence only from a CRL that is guaranteed to still list it.** RFC 5280
section 3.3 lets a CA remove the entry of an expired certificate once it has appeared on one regularly
scheduled CRL issued after the certificate's `notAfter`, unless the CRL carries `expiredCertsOnCRL` (OID
2.5.29.60). A one-hour certificate revoked at minute 30 is normally absent from a CRL issued a day later,
and a verifier that reads that absence as "not revoked" would report a false assurance, for exactly the
short-lived certificates this design recommends. So, for a certificate `C` and a decision `d`:

- `L(C)` is the `thisUpdate` of the **first CRL in the archived series issued strictly after
  `notAfter(C)`**, derived from the series itself rather than from a configured schedule. RFC 5280
  section 3.3 lets a CA drop the entry once it has appeared on one regularly scheduled CRL issued beyond
  the certificate's validity period, so that CRL still lists every revocation up to its issue time. `L(C)`
  is unbounded when the CRL carries `expiredCertsOnCRL` or retention is contractual and recorded in the
  policy. While no CRL after `notAfter` exists yet, `L(C)` is unknown and the verdict cannot be `final`.
- A CRL is **usable for absence** iff `A(d) <= thisUpdate <= L(C)`, it is the right issuer, its signature
  verifies, and its `CRLNumber` fits the series. An OCSP response is usable on the same terms with
  `thisUpdate`, and only while the certificate is within its validity period (RFC 6960 section 2.2: "good"
  does not mean the certificate was issued or is within its validity period). A "good" taken after expiry
  is no information.
- A CRL with `thisUpdate > L(C)` is **listing-only**: if it lists the certificate with `E <= A(d)` it adds a
  rejection; its silence never upgrades a decision. Later CRLs can only take trust away.
- The effective window is `W' = min(W, L(C) - A(d))`: the bank's compromise-reporting window `W`
  (default 24 h) cut to what the certificate's own life and the CA's schedule leave. A decision made near
  the end of a certificate's life has little window; that is a property of short-lived certificates,
  stated rather than hidden.
- Verdicts: **final** iff all of: there is usable status `S >= A(d) + W'`; `W' >= MinW` (a floor, default
  30 min: a decision made a minute before `notAfter` has a window of about one CRL period and is
  `provisional (short window)`, whatever the tier would otherwise be); and **the series is complete from
  `S` to `L(C)`**, meaning the CRL at `L(C)` is present and verified, and the series from `S` to it has
  **time coverage**: ordered by `thisUpdate`, each CRL's `thisUpdate` is no later than its predecessor's
  `nextUpdate` (for a base with deltas, the same holds for the merged view, and the view at `L(C)` is
  built from the base the delta names). `CRLNumber` need only increase (RFC 5280 section 5.2.3 requires
  nothing more, and a CA may skip numbers or share one counter between bases and deltas), so a gap in the
  numbers is not by itself a gap in coverage. Without this, a producer
  could omit a usable CRL inside `[A + W, L(C)]` that lists the certificate and present an earlier clean
  one as `S`. **provisional** iff the best usable status has `S < A(d) + W'`, or the series is not complete
  to `L(C)`, and a later usable one could still exist; **no information** iff no status falls in
  `[A(d), L(C)]`, which is `not verified: status missing`. The kit-hash record is tamper evidence (the
  bundle was not changed after it was assembled), **not completeness evidence**; completeness comes from
  the CRL at `L(C)`, which RFC 5280 section 3.3 makes list every revocation up to its issue time, and
  from time coverage, which also guarantees that a `certificateHold` listed and released before `L(C)` was
  seen. A pilot CA is checked for its number scheme and prune timing before go-live (test plan item 32). The verdict states that
  "revocations first reported after `L(C)` are not covered" unless `expiredCertsOnCRL` or contractual
  retention applies.
- **Evidence is journaled when the decision is counted.** The gate records, in the `approval-identity`
  record (insert-once, under the Merkle trail), the exact status bytes it relied on, so what the gate knew
  survives the loss of an archive (long-term validation, as PAdES-LTV does). That snapshot may predate `A`
  (it was fetched before the covering head), so it cannot by itself satisfy `thisUpdate >= A(d)`; the
  archived series supplies that, and the verifier reports both.

Rules for reading entries:

- `E` is the earlier of the invalidity date (entry extension 2.5.29.24, when present) and the
  revocation time: `E = min(invalidityDate, revocationTime)` (a later invalidity date never extends
  trust; RFC 5280 section 5.3.2). For reasons `keyCompromise`, `cACompromise` and `aACompromise` with no
  invalidity date, `E = notBefore` of the revoked certificate (the compromise time is unknown). This
  voids a genuine holder's valid history back to `notBefore`, and every already-counted decision it
  reaches is flagged `disputed` for the bank's process (decision 5, decided 2026-10-15).
- Entries are keyed by **(issuer key id, serial)**, never by serial alone (serials are unique per issuer
  only).
- **Series, not snapshots.** The verifier reads the whole archived series of CRLs for each issuer in
  `[A, L]`. A listing in any CRL of the series is a revocation from its time on; a later CRL that no
  longer lists the serial does not undo it. `certificateHold` (6) is treated as revoked for any decision
  during the hold, and a release (`removeFromCRL`, 8, which appears only in delta CRLs) never
  retroactively validates it. A gap in time coverage (a CRL's `thisUpdate` later than the previous CRL's `nextUpdate`) is "no
  information" for the interval; a gap in the `CRLNumber` values alone is not. A cache refuses a CRL whose `CRLNumber` is lower than the cached one for the same issuer key
  (a rollback).
- **Every non-anchor certificate** of the accepted chain needs status under the same rule (RFC 5280
  section 6.1.3(a)(3)): an intermediate revoked with `E_int <= A(d)` invalidates the decision. For CA
  certificates `cACompromise` and `DistrustAfter` apply (below).
- **`DistrustAfter` also governs status.** A CRL or OCSP response signed by a distrusted issuer key, with
  `thisUpdate` at or after that key's distrust time, is "no information": an attacker holding a
  compromised CA key can sign clean CRLs for the period after the compromise.
- **The auditor can supply distrust.** A compromise is usually found after the decisions it affected, so
  the recorded policy of the time has no entry. `bide audit verify-approvals -distrust spki=time` (repeatable)
  applies the auditor's distrust times **in addition to** the recorded policy, to chains and to status.
- **CA compromise and rollover.** `Distrust[spki]` marks a time after which an anchor or intermediate is
  not trusted for decisions with `A(d) >=` that time, so the bank can end trust in a compromised CA from
  its compromise time without voiding the history before it (removing the anchor would void all of it).
  When a CA rolls its key, CRLs signed by the old key remain valid for certificates issued under it until
  all of them expire (RFC 5280 section 6.3.3(f)); the policy lists both CA certificates (same subject,
  different keys) and a CRL is checked against whichever issuer certificate its key matches, so a rollover
  does not read as "status unknown" and pause every gate.
- **The exact sequence that must be tested** (test plan 12): a certificate is issued; a decision is made
  at `A`; the certificate is revoked at `R > A` and listed in CRL 1 (issued before `notAfter`); the
  certificate expires; CRL 2 (issued after the first scheduled issuance beyond `notAfter`) no longer lists
  it. The verifier given CRL 2 alone answers "no information", never "not revoked"; given CRL 1 it answers
  per `E` versus `A`; given both it answers per CRL 1.

### What `bide audit` can prove offline, years later

Given the kit and an auditor-held trust anchor, and the anchor log's list of heads for the run:

- the leaf chained to the auditor's anchor through the pinned intermediates, with the constraints, EKU,
  single-SAN and identity rule, **at `A(d)`**, using `Certificate.Verify(CurrentTime: A(d))`;
- the signature verifies under that leaf over bytes that include the leaf hash;
- the decision existed by `A(d)`: the inclusion proof of the record under the smallest covering head
  (the same proof `audit.AnchorReceipt` builds), and that head's signature under the log key;
- the time source of that head, and whether an independent one covers it;
- no usable status lists the leaf or an intermediate with `E <= A(d)`, and the best usable status
  time `S` against `A(d) + W'`.

What it cannot prove, and says so in the verdict: the real moment the person signed (only that it was
no later than `A(d)`); that nothing was revoked after the last usable status; that the CA, the CRL
publisher and the IdP behaved correctly; presence of the person; anything about a decision whose
covering head is missing; anything about a compromise reported after `L(C)`.

### The verdict

Every cert-bound decision gets one line that carries its basis, not a bare word:

`counts  A=<time> (source: rfc3161 <tsa> | anchor-receipt <log> | log-signer clock only), status S=<time>
(<issuer> CRL #<n>), window W'=<d>, policy P=<digest>, rules v<n>, verifier go<version>`

and a tier: **independent-final** (independent time source, status final, series complete), **logclock-final**
(status final, time from the log signer only, earlier heads excluded by the anchor list), **provisional**
(a later status could change it, the window is short, or the series is incomplete), **disputed** (below), **not verified** (with the reason: chain, SAN, EKU, expired,
revoked at `E`, no covering head, contradictory head times, status missing). The word "verified" is
not a tier: a verdict without its time source and status time would overclaim. The verdict records the
Go version of the verifier and the rules version, and the build ships **pinned test vectors** (a fixed
kit with its expected verdicts, run in CI on each Go version bumped), so an auditor can tell a change
in `crypto/x509` from a change in the kit.

### A revocation after a counted decision

Under the rule a revocation with `E > A(d)` does not change a decision. A terminal tally already
recorded is never recounted. If newer status shows `E <= A(d)` for a decision that **was** counted
(a compromise dated back), the audit does not silently mark the tally wrong or right: it reports
**disputed**, with the certificate, the CRL entry, `E`, the reason code and the decision, and states
that whether the action stands is a matter for the bank's process. A wrongly honored approval is the
audit's to show; deciding what to do about an executed action is not.

### Defaults and tolerances

| Value | Meaning | Default | Guidance |
|---|---|---|---|
| `skew` | `decided_at <= A + skew` | 2 min | the approver's clock against the log signer's |
| `ClockTolerance` | head times non-decreasing in size | 60 s | two writers; larger values weaken the operator-adversary bound |
| `anchorGrace` | `A <= notAfter + anchorGrace` | 60 s | recommend 60 s with synchronous anchoring (anchor-first makes `A` follow the write by seconds); a bank that anchors slowly sets its worst case, which weakens the bound by that much |
| `RoutineGrace` | for revocation reasons other than compromise (affiliationChanged 3, superseded 4, cessationOfOperation 5, privilegeWithdrawn 9), a decision with `A < E + RoutineGrace` still counts | 0 | covers a CRL published minutes after an offboarding; minutes, never hours; compromise reasons and unspecified never get grace |
| `W` | compromise-reporting window for the offline `final` verdict | 24 h | from the bank's incident process; cut to `W'` by the certificate's life |
| `MaxStatusAge` | live: oldest usable status when a decision is counted | **1 h** | an earlier draft said 24 h, which exceeds the lifetime of the certificates it protects and makes revocation add almost nothing beyond expiry; choose from risk and certificate lifetime, never above a quarter of the shortest lifetime |
| `ExecStatusAge` | live: oldest usable status immediately before the call executes | **set from the CA's actual period; no constant default** | closes the gap between counting and acting. The policy declares the CA's CRL period (`CAPeriod`) and **startup refuses a policy with `CAPeriod > ExecStatusAge / 2`** (a CRL on a 30 min period is up to 30 min old, so a 15 min age would pause about half the time). 15 min needs a CA period of 7 min or less, or OCSP; a 30 min CA period gives `ExecStatusAge` of 1 h. The resulting latency is printed |
| refresher | CRL fetch interval | `min(MaxStatusAge, ExecStatusAge) / 4`; alarm at 50% of the smaller of the two | |
| `MinW` | floor on `W'` for a `final` verdict | 30 min | a decision near `notAfter` is `provisional (short window)` below it |

**The published revocation latency.** A bank can state the number: the time from the CA recording a
revocation to a gate refusing to act on it is at most *CA publication delay + the CA's CRL period (or the
delta period) + `ExecStatusAge`*, with `ExecStatusAge` required to be at least twice the declared CA
period. The guide makes the operator compute and record it. With a daily CRL it is a day; the design does
not hide that.

### CRL cadence and the CA that cannot meet it

A short-lived certificate (say 8 h) needs status inside its life: a CRL every 2 h, or OCSP responses
archived at decision time. Mapping CA behavior to what this design needs:

| CA behavior | Works? | What to do |
|---|---|---|
| Full CRLs, period at most `MaxStatusAge / 2` | yes | the default `MaxStatusAge` of 1 h needs a period of 30 min or less; a bank with a 4 h period sets `MaxStatusAge` to 8 h only for certificates of at least 32 h, and accepts the longer revocation latency |
| Weekly base CRL plus daily or hourly **delta** CRLs (the Microsoft AD CS default) | yes | the verifier merges a base and its delta (below). The status time is the delta's `thisUpdate`, so the delta period is the one that counts toward `MaxStatusAge`; the weekly base only has to be present and numbered consistently |
| Daily full CRL, certificates of 8 h | not final | a decision usually has no usable CRL inside its certificate's life: provisional or "status missing". Shorten the CRL period, set `expiredCertsOnCRL`, or use OCSP |
| No revocation for short-lived certificates (expiry is the control) | only by explicit policy | `Status: NotRequired` is accepted only for a leaf whose lifetime is at most `ShortLived` (default off, bank-set, capped at 24 h) **and** whose certificate carries a CA-asserted policy OID for short-lived issuance; intermediates still need status; revocation latency then equals the lifetime, and the verdict says status was not required (decision 6) |

## Revocation

- **CRL** is required and enough (unless the policy says otherwise). Parsed with
  `x509.ParseRevocationList`; the CRL signature is checked against the issuing CA certificate whose key
  matches (`CheckSignatureFrom`, which fails for any other CA). `nextUpdate` is mandatory in a CRL
  (RFC 5280 section 5.1.2.5); a zero or absent value is "status unknown". The delta CRL indicator
  (2.5.29.27), the issuing distribution point (2.5.29.28), `indirectCRL`, `onlySomeReasons` and any
  unrecognized critical extension are visible in `RevocationList.Extensions` with their critical flag
  (pinned by tests). **The first build refuses a CRL that carries any issuing distribution point, is
  indirect, or has `onlySomeReasons`**: matching them correctly (RFC 5280 sections 5.2.5 and 6.3.3(b)) is
  not in the standard library, and a wrong match is a wrong "good". A CA that can publish only
  partitioned or indirect CRLs is a deployment blocker, not a case to approximate.
- **Delta CRLs are supported (decided 2026-10-15), so an AD CS default works.** A delta is merged with its
  base, never read alone. The merge accepts a delta `D` and a base `B` only when: both verify under the
  same issuer key and carry no issuing distribution point; `D` has the critical delta CRL indicator
  (2.5.29.27, a DER INTEGER, the base CRL number, pinned by a test); `D.BaseCRLNumber <= B.CRLNumber` and
  `D.CRLNumber > B.CRLNumber` (RFC 5280 section 5.2.4 and 6.3.3); and `D.thisUpdate >= B.thisUpdate`. A
  delta that fails any check is "status unknown" and never a partial answer. The merged view is the
  base's entries, plus the delta's entries, minus entries the delta marks `removeFromCRL` (8). The merged
  status time is **the delta's `thisUpdate`**, and its `nextUpdate` is the earlier of the two. For the
  *series* rule the removal is a view operation only: the archived listing of a held certificate stays a
  revocation for decisions during the hold (the `certificateHold` rule), whatever a later delta removes. A
  delta is cumulative since its base, so one delta with the base is enough, and every delta and base is
  archived in the series; a gap in time coverage between the archived base and the delta used is "no
  information" for the interval. The first CRL after `notAfter` is evaluated as a merged view built from
  the base its delta names: a base issued just after expiry may already have pruned the entry, so the
  merged view, not the base alone, is the `L(C)` evidence, and the entry-level rule below applies. The usable-for-absence window applies to the delta's `thisUpdate` as it
  does to a full CRL's, and the listing-only rule applies to anything after `L(C)`.
- **Critical entry extensions make a CRL unusable.** A CRL entry carrying a critical extension bide does not
  process (for example `certificateIssuer`, 2.5.29.29, which marks an indirect entry) makes the whole CRL
  "status unknown" (RFC 5280 sections 5.3 and 6.3.3), as a critical CRL-level extension does. Go exposes
  entry extensions with their critical flag, so this is checked by bide and tested. The merged
  `nextUpdate` (the earlier of base and delta) governs staleness, so a week-old base does not make a
  fresh delta fresh and is not accepted on its own `nextUpdate`.
- **Where CRLs come from.** The refresher fetches **only from URLs configured in the `IdentityPolicy`**;
  a certificate's CRL distribution point is a consistency check (it must equal a configured URL), never
  a thing to fetch. The intake accepts a chain from a client, and fetching URLs named in client-supplied
  or misprofiled certificates would be an SSRF and denial-of-service surface. The refresher alone
  fetches (never in the request path), with size, time and redirect limits, verifies before replacing
  the cache, and keeps every CRL in a dated archive.
- **OCSP** is optional, in a separate module so the core does not import `golang.org/x/crypto/ocsp`. A
  response is archived whole and verified like a CRL, signed by the CA or a delegated responder holding
  the OCSP-signing EKU (RFC 6960 section 4.2.2.2); a nonce is used where the responder supports it
  (section 4.4.1); `unknown` and any parse failure are "status unknown"; "good" is read as in the status
  rule (section 2.2). It gives the intake and the gate fresher status.
- **Fail closed.** No status for a certificate that requires it, a stale one, a signature that does not
  verify, a CRL that does not cover the certificate: the decision does not count. For the live gate this
  is a **pause**, not a deny: the gate stays waiting, the needs-attention state names "approver identity
  status unavailable", and the notifier says so (decision 7). A deny is reserved for cases where the gate
  can already never pass.
- **Freshness and caching.** A CRL is usable live while `now < nextUpdate`, `now - thisUpdate <=
  MaxStatusAge`, and its `CRLNumber` is not lower than the cached one for the same issuer key. A failed
  refresh keeps the previous CRL until it goes stale and raises an `OnStatusError` hook (like
  `AuditedStore.OnError`).
- **Status is re-checked immediately before the call executes.** A terminal tally is never recounted,
  so without this a decision counted at 10:00 would still authorize an action at 11:30 after a 10:30
  revocation. Before the tool runs, the gate re-verifies the status of the counted decisions' chains
  against a snapshot no older than `ExecStatusAge`. If a counted decision's chain now shows `E <= A(d)`,
  or status is stale, the call **does not run**: the gate pauses with a named needs-attention reason, and
  the operator re-asks or cancels (a revoked decision is a dispute, not an automatic deny). The model
  adds this as a property (`NoExecAfterKnownRevocation`). The state transitions are specified, not left
  to the implementer:
  - the terminal tally stays in the journal as history and is the record of what was counted;
  - a **stale-status hold** writes nothing authoritative: the run returns a pending state with a named
    reason (`PendingIdentityStatus`), and executes when a fresh snapshot passes, with the same tally;
  - a **revoked-before-execution hold** (`E <= A(d)` for a counted decision) writes an insert-once
    `approval-exec-hold:<id>` record (the status digest, the decision, `E`, the reason) and supersedes the
    tally through the existing re-ask path under the name `approval-tally:<id>:ident-reask`, the way the
    `:sod` re-ask does; the approvers decide again, and the earlier tally is no longer authoritative;
  - **every path that can run the tool goes through one pre-execution function**: the first execution,
    crash recovery and `RecoverLoop` resume, a retry of a retry-safe tool, and any halt-resolution that
    would proceed. `RoutineGrace`, `E` and `A(d)` are the same functions at execution as at count time.
- **Counting is sticky in one direction.** Stale or missing status never removes a decision that was
  counted: the gate pauses (it does not flap to deny and back). A revocation with `E <= A(d)` removes it
  permanently. A decision counted in a terminal tally stays counted in that tally; only the execution
  re-check and the audit's `disputed` verdict can speak to a later revocation.
- **A revocation after a decision.** Under the rule a revocation with `E > A(d)` does not change that
  decision: the approver was authorized when it became provable. If the bank wants a leaver's earlier
  approvals on a waiting gate dropped, that is a separate, explicit operation (re-ask the gate), not an
  implicit property of revocation.

## Rotation, renewal and continuity of identity

Certificates are short-lived (hours to a day, the bank's choice). The approver's key may be ephemeral
(a fresh key per certificate, smallstep or Vault PKI after SSO, held in a 0600 key file for the
certificate's life) or long-lived in an HSM or KMS (bide-hsm), with the certificate renewed for the
same key.

Continuity of identity across certificates is the approver id plus the identity rule, not the key.

- A new certificate for the same canonical identity is the same approver. Decisions under different
  certificates by one approver id still obey "an approver's first valid decision counts".
- The seat is the **canonical identity**. The static policy check (`ValidateKeys`, one seat per key)
  cannot see a key that is unknown until a decision arrives, so a certificate-bound verifier reports
  `KeyIDs() = ["ident:" + canonical identity]` and two approver ids that resolve to one identity are
  refused with `ErrConfig`, as two ids on one key are today. A **dynamic** check closes the rest: two
  counted decisions by different approver ids whose leaf public keys hash equal do not both count
  (`ReasonSharedKey`).
- Renewal in flight: a decision signed under certificate C1 counts under C1's rules at `A(d)` even if
  C1 has expired by the time the run resumes.
- Rotation of the **trust anchor**: the old root stays in the gate's trust set for decisions made under
  it; the kit holds both, and the identity record names which anchor each counted decision chained to.

## Interplay with what exists

**k-of-n and one seat per key.** Unchanged for bare-key approvers apart from the identity map. For
certificate-bound approvers the seat is the identity, plus the dynamic key check.
`ApproverVerifier.KeyIDs` keeps its contract; `ident:` is one more kind of entry, and the existing
guard that refuses two approvers sharing an entry applies unchanged.

**`ExcludePrincipal` and `run:principal`.** Today the requester recorded in `run:principal` is compared
with approver ids under normalization. With the shared `Canonical` the check also compares the
requester principal with the counted decision's **verified canonical SAN** (and with the identity map
value of a bare-key approver). A requester equal to a counted identity never counts
(`ReasonRequester`), however many approver ids that person holds. A principal that is not an email or
URI cannot be matched to an identity; the verdict says "identity match not possible" for it.

**`EvaluateGate` and the intake.** `EvaluateGate(recs, subject, policy, resolver)` gains the identity
environment by way of the resolver, so its signature stays; its `GateStanding` gains per-approver
reasons (below). The approvals intake takes the certificate chain in the request (`cert`, base64 DER
chain, leaf first), verifies it with the **same** verifier before writing (`WithDecisionCheck`), and
records it in the decision record. A status failure returns 503 `status_unavailable` (fail closed,
retry), not 422. After the write the intake anchors (it writes through the application's audited
store), so the covering head follows within the write. `GET /pending` shows the gate's identity rule per
approver (the expected canonical identity) so a client can check its own certificate before signing.

**`bide approve`.** `-cert chain.pem` names the leaf-first chain; `-key` is the key or an HSM reference.
Before signing it checks: the chain verifies at "now" against `-trust-anchors` (when absent it reads the
gate's identity rule from `GET /pending` or the recorded policy, but never trusts the endpoint's
anchors), the leaf key equals the signing key, the canonical SAN matches the `-as` id's identity rule,
and the certificate has a minimum time left (default 60 s). The signature includes the leaf hash. The
receipt gains the chain. A direct journal write is allowed but its `A` is late until the gate anchors
(it does, before tallying).

**bide-hsm.** The certificate sits next to the HSM or KMS key: a PEM chain file, or a PKCS#11
certificate object on the token (matched by `CKA_ID`). `bide-hsm` refuses to open a key whose public
key does not equal the certificate's. The signer interface does not change: the `audit.Signer` signs the
`v4` decision bytes and the chain travels with the decision. For a KMS key the bank's CA issues the
certificate against the KMS public key (the CSR proof of possession is signed by the KMS key).

**Decision receipts.** A receipt carries the certificate chain and, once a head covers the decision, the
anchoring proof (`audit.AnchorReceipt`, unchanged: it already picks the smallest covering head, which
is `A(d)`). `Verify` for an identity receipt also checks chain, SAN and identity against anchors the
caller supplies.

**Evidence v6.** The identity data is **not** put into the evidence package. Before 1.0 a verifier reads
only the current format of each artifact, so a v7 package would orphan every v6 package. Instead an
**identity bundle** (`bide.audit.identity.v1`) accompanies the package: per counted decision, the chain,
the smallest covering head with its inclusion proof, the time attestation (RFC 3161 token or anchor
receipt, when there is one), and the status series. `bide audit verify-approvals` takes `-identity
bundle.json -trust-anchors roots.pem -anchor-log log.json` and prints the verdict lines above. A package
with bare-key approvers verifies exactly as today.

## RFC 3161 token verifier (step 1 component)

The standard library has no CMS (`SignedData`) parser, and the top tier and the archive timestamp both
rest on RFC 3161 tokens. The project verifies them with **a small strict verifier in the core module on
`encoding/asn1`** (decision 17, the user's), not a dependency. Its safety comes from a narrow profile:
anything outside it is refused, never interpreted.

**Status: built** in `audit/rfc3161` (step 1 component, not yet called by any gate). Where the profile
below left a choice, the build reads it this way: `unsignedAttrs` may be present and are held to DER but never
read; a certificate in the token that Go's parser rejects refuses the token even when unused (found by the
differential test); `accuracy` is refused above 24 hours; `Policy.RSA2048Until` applies the 2048-bit
exception against `genTime`; the TSA certificate status rule is a function the caller must supply
(`Policy.CertStatus`), so it is never skipped silently; Ed25519 requires the SHA-512 digest algorithm
(RFC 8419). The independent PKI review of the build added: accepted `accuracy` is capped (`Policy.MaxAccuracy`,
default one second) and `Result.Latest()` is the time to use for "existed by"; the `tsa` name must match the
certificate; the TSA certificate may not be a CA and its key usage must be signing only; `Policy.DistrustAfter`
retires TSA keys; `Policy.AcceptedPolicies` pins policy OIDs; `Expected.Now` bounds `genTime` from above for a
live gate; a SHA-1 `signingCertificate` beside a V2 attribute is checked for consistency only. Of ten public
TSA endpoints tried on 2026-10-15 the profile accepts six by default (DigiCert, GlobalSign, QuoVadis, SSL.com,
Microsoft, SwissSign), three with the opt-in (Sectigo, Entrust, FreeTSA: SHA-1 `signingCertificate` only) and
refuses one (Apple: SHA-1 as the digest algorithm); a bank must choose its TSA from the first group, or opt in per TSA key for the V1-only ones (decision 17, amended). Unused certificates in a token must parse and are then ignored (the path comes from the auditor's anchors); `Normalize` returns the token with only the path's certificates, and because two encodings of one attestation are distinct byte strings, the identity bundle and kit must key and store tokens by their normalized bytes (or by the attested fields), never by the received bytes.

**Prerequisite before any gate calls the verifier (PKI review, M3):** the reference `Policy.CertStatus` hook
built on the status rule (the status-rule PR) must exist and be wired. The token's path is judged at its own
`genTime`, which the holder of a retired or compromised TSA key chooses, so the status function (every non-anchor
certificate as of `genTime`, with the usable-for-absence window), `Policy.DistrustAfter` and the accuracy cap are
the only controls on back-dating. `rfc3161.StatusUnknown` is the fail-closed placeholder until then.

**Profile (each point a refusal when violated):**

- DER only. The token is re-parsed with `asn1.Unmarshal` and the remaining bytes must be empty; BER,
  indefinite lengths and non-minimal encodings are refused. The signed attributes are re-encoded as a
  DER `SET OF` for the signature check (RFC 5652 section 5.4), and the verifier checks that the original
  bytes already were that encoding.
- A `ContentInfo` of type `id-signedData` with `SignedData` version 3, **exactly one `SignerInfo`**, exactly
  one `digestAlgorithms` entry equal to the signer's, encapsulated content type `id-ct-TSTInfo`
  (1.2.840.113549.1.9.16.1.4), and the content present (not detached).
- `TSTInfo` version 1; a message imprint hash algorithm of **SHA-256 or better** and an imprint equal to
  the hash the verifier computed over the covering head (or the bundle); `genTime` as a `GeneralizedTime`
  with no fraction beyond what the profile reads; `accuracy` read when present and added to the tolerance;
  `nonce` equal to the request's when the verifier made the request; `ordering` ignored; unknown critical
  extensions in `TSTInfo` refused.
- **Required signed attributes**, each exactly once: `contentType` (equal to `id-ct-TSTInfo`),
  `messageDigest` (equal to the hash of the encapsulated `TSTInfo` octets, with the signer's digest
  algorithm), and `signingCertificateV2` (`ESSCertIDv2`, hash SHA-256 or better, equal to the hash of the
  signer certificate the verifier selects, and the `issuerSerial` equal when present). A token with
  `signingCertificate` (ESSCertID, SHA-1) only is refused. Other signed attributes are ignored but must be
  well-formed DER.
- Signature algorithms: the same allowlist as every signed object (ECDSA P-256 or P-384 with SHA-256 or
  better, RSA PKCS#1 v1.5 or PSS with SHA-256 or better and a 3072-bit minimum modulus, 2048 only through the same explicit
  exception with an end date, Ed25519); never SHA-1 or MD5. The signature is verified over the DER signed
  attributes with the signer certificate's key.
- The signer certificate comes from the token's `certificates` field or from the policy, is chained to a
  TSA anchor the auditor lists (`-trusted-tsa`) at `genTime`, carries the critical `id-kp-timeStamping`
  EKU as its **only** EKU (RFC 3161 section 2.3), and has its own status checked at `genTime` by the status
  rule (the archive timestamp covers that status, as described under long-term validation).
- Limits: the token size, the number of certificates, and the nesting depth are capped before parsing;
  the verifier never allocates from a length it has not bounded against the input.
- Everything else is refused with a named reason: more than one `SignerInfo`, `unsignedAttrs` used for
  anything the verifier reads (they are never trusted), `crls` or `attrCertificates` present, an unknown
  `SignerIdentifier` form, or a `version` other than the expected one.

**Evidence of correctness (exit criteria for step 1):**

- *Test vectors from real TSAs*, kept in the repository as fixtures with the request that produced them:
  tokens from OpenSSL `ts` (own test TSA), DigiCert, and FreeTSA, plus tokens from the bank's TSA when it
  has one. Each vector is verified and its `genTime`, imprint and signer are asserted. **FreeTSA is a
  refused vector, not a verified one:** its tokens identify the signer by the SHA-1 `signingCertificate`
  attribute only (no `signingCertificateV2`), which the profile never accepts, so its token is kept as the
  real-world case of a refusal for being outside the profile (OpenSSL accepts it; DigiCert's tokens carry
  both attributes and verify).
- *Differential tests* against `openssl ts -verify` on valid and mutated tokens: the Go verifier must never
  accept what OpenSSL rejects for a reason inside the profile, and any token the Go verifier refuses for
  being outside the profile is recorded as such, not as a failure.
- *Negative vectors* for each refusal above (BER, two signers, SHA-1 `ESSCertID`, missing or duplicated
  signed attribute, wrong `messageDigest`, wrong imprint, `genTime` earlier than the head it stamps,
  non-timestamping EKU, an extra EKU, expired TSA certificate, revoked TSA certificate, trailing bytes).
- *Fuzzing*: `go test -fuzz` on the token parser (no panic, no unbounded allocation, no accept of a
  mutated valid token unless the mutation is in an ignored field), run in CI for a fixed budget and
  corpus-checked in.
- *Independent review*: the verifier is the single piece of hand-written cryptographic parsing in the
  feature; the step-1 PKI review reads it first.

## Journal and format changes

**The decision record** (`ApproverSignature`) gains `approver_cert` (the leaf-first DER chain, base64 in
JSON) beside `approver_alg`, `signature`, `decided_at`, `recorded_at`. It is additive and optional. The
chain is stored, so the journal is self-contained and its Merkle trail covers it. A chain is about 1 to
4 KB per decision.

**The signed decision bytes** get a new version tag, `bide.approval.v4`, **only for certificate-bound
decisions**. Layout = v3 plus `len32(SHA-256(leaf DER)) leaf-hash` before the decision time. Reasons:

- the signature is only valid in the context of the certificate, so a valid certificate for the same
  key but another identity, or a swapped-in certificate, cannot be attached to someone else's
  signature;
- an old binary does not know `v4`, so it does not count it (the record still decodes: unknown fields in
  a decision record are tolerated, pinned by a test, and the bare-key verification fails): no downgrade
  in which an old process counts a certificate-bound approver's decision as a bare-key decision;
- bare-key decisions stay `v3`, unchanged.

A certificate-bound approver's verifier accepts `v4` only; a `v3` signature by that approver is not
counted (`ReasonNoCert`). A bare-key approver's verifier accepts `v3` only. The gate decides by the
resolver's kind for the approver id, never by what the record contains.

**The decision step name** keeps the identity-keyed shape (#288: call, approver, verdict, scheme,
decision time) and, for `v4`, adds the leaf hash. Two decisions at one `decided_at` under different
certificates are then two decisions with two names, and a junk record cannot take the name of a
certificate-bound decision by guessing its time alone.

**Where the identity data goes: its own record, never the tally.** The recorded tally is decoded
strictly (`decodeTally`; an unknown field is `ErrStorage`, pinned by a test), so a field added to it
would make an older binary fail the read, not pause. The identity environment of a terminal count is
written as a separate insert-once record, `approval-identity:<encoded call id>`, beside the tally. It
holds the **effective-dated `IdentityPolicy`** in force (anchors by key id and fingerprint, pinned
intermediates, `Distrust` times, EKU, policy OID, `Canonical` name and version, status and grace values),
**a digest of the identity roster and only the counted approvers' identifiers** (not the whole roster, so
erasure stays tractable and a run does not carry every person id of the gate), the accepted chain and
anchor per counted decision, the **hashes of the status bytes the gate relied on**, the time source, and
the rules and verifier versions. The status bytes themselves (a CRL can be megabytes) are stored once, by
content hash, in an insert-once `approval-status:<sha256>` record that is **per run** (deduplicated within a run, never a
global store, so a run stays self-contained and per-run redaction works) and shared by every call of the run that relied on
them, so the journal does not repeat a CRL per call (long-term validation keeps its copy). Older binaries ignore it. The identity map, grace values and the rest change over time; without
them in the record the mapping at `A(d)` is unknowable years later.

**The downgrade is detectable, not prevented, and what each part can do is stated plainly.** There are
three layers, and only the first prevents anything:

1. *Live, prevention.* A process that predates this feature has none of this code and cannot refuse to
   count a bare-key signature, so **the protection is to roll the whole fleet before enabling a
   certificate-bound approver and to remove the old bare key from every resolver at the switch** (an old
   process without the old key cannot count a stolen v3 signature). There is no version-floor mechanism
   that an old binary would honor, and the note does not claim one.
2. *Live, for the new fleet.* An identity-bound gate records its terminal tally under a distinct name,
   `approval-tally:<id>:ident`, the same device the format and separation-of-duties re-tallies use
   (`:v3`, `:sod`). A tally under the plain name on an identity-bound gate (written by a stale replica or
   an old process that counted bare keys) is treated by the new gate as **superseded, and the gate
   re-asks** through the existing re-ask mechanism. This does not stop that old process from having acted
   on its own count; it stops the new fleet from trusting it, and leaves a record an auditor can see.
   If the superseded plain-name tally already led to an execution by an old process, the re-ask **must not
   re-run the call**: the execution record is kept and the decision is flagged `disputed`. Anyone able to
   write a plain-name tally can force identity-bound gates into re-asks; that is an availability effect
   only and the same trust boundary as the journal itself.
3. *Audit.* The pause note (`ApprovalWait`, decoded leniently, pinned by a test) gains additive fields
   written when the gate is created: `identity_bound`, `identity_kinds` (a digest of the sorted
   `approver=bare|cert` pairs) and `identity_policy_digest`. The auditor requires the
   `approval-identity` record whenever the note says the gate had a certificate-bound approver, requires
   its policy digest to equal the note's, and reports a missing record as `not verified: identity data
   stripped`. **A gate created by an old process has no such field, which looks like a bare-key gate.**
   So for any gate the bank classifies as identity-required, **the auditor's expected policy of record is a
   required input** (`-expected-policy`); without it the verdict says "identity binding of this gate is
   not established by the journal". The expected policy also fails any decision from an approver it marks
   identity-bound when the tally lacks the record or lacks the `:ident` name.

Observe mode is **time-boxed** (default 30 days, after which the policy refuses to start in observe
mode).

**The kit is bound to the journal.** `bide identity kit` writes an insert-once record,
`approval-identity-kit:<encoded call id>`, with the hash of the bundle it assembled; the head that covers
it anchors the hash. A producer who later substitutes or trims a bundle fails the hash check. The bundle
carries nothing the journal and the signed archive do not already authenticate (chains and status from
the identity record, head proofs, CRLs verified by their issuers' signatures and `CRLNumber` series).

**Personal data in an append-only log.** A leaf certificate carries an identifier and possibly a name, and
`GET /pending` shows expected identifiers. Erasure and minimisation duties (GDPR Articles 5(1)(c) and
17, for instance) sit uneasily with an immutable journal. Three controls: prefer an opaque person id as
the identifier (above), which is not itself a name; keep the chain **by reference** (a salted commitment
in the record, the certificate in a separately retained store) when a bank's privacy review requires it,
at the cost of a self-contained journal (decision 12: the full chain is the default, by-reference a
configured option); require authentication on `GET /pending`. The
existing redaction tombstone keeps the leaf hash, so a redacted decision stays in the tree but can no
longer be verified from the journal alone.

**Reasons** (additions to the `Reason*` set, each tested): `ReasonNoCert`, `ReasonCertChain`,
`ReasonCertIdentity` (SAN count, kind or value), `ReasonCertUsage` (EKU or key usage),
`ReasonCertWindow` (decided_at or `A(d)` outside validity), `ReasonRevoked`, `ReasonStatusUnknown`,
`ReasonStatusStale`, `ReasonNoCoveringHead` (the live "not provable yet"; the gate anchors and retries),
`ReasonSharedKey` (reused for the dynamic key check).

**Backward compatibility.** Journals without `approver_cert` read and verify as before. A policy whose
resolver returns only bare-key verifiers behaves exactly as today. A mixed fleet is safe in the
direction that matters: an old binary cannot count a `v4` decision, so a gate it evaluates pauses (the
decision record decodes, the bare-key check fails, nothing is counted, nothing errors). Identity
enforcement still requires every process that evaluates the gate to run the new version, and the guide
says to roll the fleet before enabling a certificate-bound approver.

## Formal model impact (model 1b)

The approval-gate model checks the counting rule over signed, possibly invalid records. It needs the
following, as a new configuration set with the existing configurations unchanged.

State: a set of certificates, each with a holder (person), an identity (SAN), a validity interval
`[nb, na]` and a revocation time `E` (or none); the CA's constraint set; trust anchors per gate; a status
snapshot (which serials are listed, as of which time, with a CRL number); a clock; a set of signed heads
with sizes and times; decisions carry a certificate. Actions: `IssueCert(person, identity)`
(constrained, plus an adversarial `MisIssue` that ignores the constraint), `Revoke(cert, E)`,
`CRLPublish`, `WithholdCRL`, `RollbackCRL`, `ClockTick`, `SubmitCert` (genuine or stolen key),
`Backdate` (a submission whose `decided_at` is earlier than its true time), `Anchor` (a head covering
the journal so far; the gate's own step before a tally), `SkewHead` (a head with a time out of order).

Properties that must hold (invariants unless noted):

- **IdentityBound**: a counted decision's certificate chains to a trust anchor of the gate, through
  pinned intermediates, with exactly the approver's SAN, valid at `A(d)`.
- **RevocationSound**: a counted decision's chain has no `E <= A(d)` in the status series the count used.
  With `WithholdCRL` or `RollbackCRL` enabled and no freshness and number rule the property must
  **fail** (documented counterexamples), and hold once the rules are in.
- **FailClosed**: unknown, stale or out-of-window status never counts.
- **NoBackdate**: a decision signed after `E` and first covered after `E` never counts, however its
  `decided_at` reads; with `SkewHead` and no non-decreasing rule the property **fails**, and holds with it.
- **SeatPerPerson**: counted decisions come from distinct people, also when two approver ids map to
  one identity or one key sits under two certificates. With `MisIssue` and no name constraints this
  fails; with them it holds.
- **TerminalStable**: a terminal tally is not changed by a later revocation or snapshot.
- **NoExecAfterKnownRevocation**: the call never runs while a counted decision's chain shows `E <= A(d)`
  in a snapshot no older than `ExecStatusAge`, or while status is stale. Without the execution re-check
  the property fails (a decision counted before a revocation still authorizes the action).
- **NoExecAfterKnownRevocation covers recovery**: the property holds for first execution, crash
  recovery, `RecoverLoop` resume and retry of a retry-safe tool alike (one pre-execution function), with
  `RoutineGrace` applied the same at execution as at count time.
- **FinalNeedsCompleteSeries**: a decision is never `final` unless the CRL at `L(C)` is in the series and
  the series from `S` to it has time coverage, and `W' >= MinW`; omitting a usable listing CRL from the
  middle of the series must fail the property (the N2 counterexample).
- **DistrustAfter**: no decision with `A(d)` at or after an anchor's or intermediate's distrust time
  counts under it, status signed by that issuer at or after the time is "no information", the auditor's
  overlay applies in addition to the recorded policy, and decisions before it are unaffected, except where their status evidence comes from the distrusted issuer after that time (that evidence is "no information", so such a decision drops to `provisional` or `not verified: status missing`); with a compromised-CA action (`MisIssue`
  after the compromise time) and no `Distrust` the forged decisions count, with it they do not.
- **StaleNeverUncounts**: stale or missing status never removes a counted decision (the gate pauses); a
  revocation with `E <= A(d)` removes it permanently; no flapping.
- **AbsenceOnlyInWindow**: a decision is never `final` on a status whose `thisUpdate` is outside
  `[A(d), L(C)]`; a post-expiry CRL that pruned the entry never upgrades it (the C1 sequence is a model
  configuration as well as a Go test).
- **SeatPerIdentifier** (the precise scope of `SeatPerPerson`): counted decisions come from
  distinct *identifiers*. The model does not assume an identifier is one person; it takes injectivity
  from identifier to person as a stated, bank-evidenced control, and shows that when it fails (an alias)
  one person fills two seats.
- **NoStuckPause** (extended): a gate with enough genuinely valid cert-bound decisions, fresh status
  and an audited store is not stuck, **including when a decision was written directly with no covering
  head**: the gate's `Anchor` step supplies it. Without the `Anchor` step the model must show the
  deadlock, and with it not.

A Go refinement test runs the same scenarios through `TallyApprovals` with the stdlib CA (below) and
compares the counted set with the model's, trace by trace, as the existing models do. The change to
`spec/tla` and its README map is part of the build, and `modelsync` gates the PR.

## Operations for a bank

**Setup.**

1. The CA issues approver certificates after SSO with MFA step-up and a hardware-bound or attested key:
   exactly one SAN (the opaque person id URI, or a never-reassigned mailbox), the approver EKU on a
   dedicated issuing CA, the certificate policy OID that asserts the issuance controls, a short
   lifetime, name constraints (an excluded subtree for subdomains if it must exclude them; checked
   against another validator, since Go reads a permitted `bank.example` as also permitting subdomains),
   a lower-case issuance profile, serials of at most 20 octets, a published CRL schedule with full CRLs
   at a period within the table above (full CRLs, or a base with deltas; no issuing distribution point), and a CRL
   distribution point equal to a URL in the policy. The bank states and evidences, as a control, that an
   identifier maps to one person and is never reassigned.
2. The platform team writes the gate's `IdentityPolicy`: anchors, pinned intermediates, the identity map
   for every approver (bare-key included), EKU, `Canonical`, status and grace values. It lives with the
   code and the flow config, reviewed like a policy. `bide check` learns `assert every approver of gate
   X is identity-bound`, and `-diff` flags a gate loosened from identity-bound to bare keys.
3. The application runs on an audited store with a head source (anchor read capability) and, for the
   top tier, an RFC 3161 TSA or a time-bearing anchor.
4. Approvers run `bide approve -cert`, or the browser page once it can hold a certificate (out of
   scope here), against the intake.
5. Run **observe mode** first (migration below).

**CRL distribution.** A `bide identity` command group (core, `net/http` only, no new module):

- `bide identity fetch-crl -cache dir -policy file` fetches, verifies and stores CRLs atomically and
  keeps a dated archive (`crl/<issuer-key-id>/<number>-<thisUpdate>.crl`) for the kit;
- `bide identity check -cert chain.pem -policy file` tells an approver why their certificate would not
  count (chain, SAN, EKU, window, status), the same function the gate uses;
- `bide identity kit -evidence pkg.json -out dir` assembles the identity bundle for a package.

The application embeds the same refresher when it does not run a cron.

**Long-term validation.** Retention is commonly 5 to 10 years or more, and SHA-256, ECDSA P-256 and
Ed25519 (heads, CA signatures) are on NIST's deprecation path (IR 8547). The kit therefore: records the
verifier version, the Go version and the relevant `GODEBUG` settings (`x509usepolicies`, `x509sha1`,
`x509negativeserial`) so a verdict is reproducible; freezes the identity bundle format for the
retention period with pinned test vectors (a "current format only" policy would orphan archived
bundles); **archives the verifier itself** (the source revision and the hash of a reproducible build, and
recommends archiving the binary) so a verdict can be re-run when toolchains move; and carries an
**archive timestamp** (an RFC 3161 token over the bundle hash) from the first build that ships the kit
(step 3), renewed periodically in the manner of ERS (RFC 4998) and PAdES-LTA/CAdES-A (ETSI EN 319 142,
319 122), with the renewal tool in step 4. The archive timestamp covers the TSA certificate's own status
and validity at token time, which is the same problem again one level up. The bundle contains
covering-head proofs in the evidence formats of its day, and the project's pre-1.0 policy is that a
verifier reads only the current format of each artifact: until 1.0, freezing the bundle alone does not
protect the chain of evidence, which is why the verifier is archived with it. The retention trust
assumptions (the algorithms and the TSA trusted at archive time) are stated in the kit.

**The offline auditor's kit** is a directory: the evidence package, the identity bundle (chains,
covering-head proofs, time attestations, the archived CRL/OCSP series), and a README listing what the
auditor must bring: the trust anchors, from the bank's PKI team over a channel not shared with the
producer; the log public key; and the anchor log's list of heads for the run (so the smallest covering
head can be computed independently). `bide audit verify-approvals -identity ... -trust-anchors ...
-anchor-log ...` needs nothing else and no network. Keep the archive for the retention period,
including the CRLs: the archived series is the evidence, because a CA drops expired entries.

**Stale CRL in production.** The refresher alarms before staleness (at 50% of `MaxStatusAge`, the same figure as everywhere else in this note). At
staleness gates pause with a named needs-attention reason; the notifier says which CA and since when.
There is **no break-glass in the core**: weakening the policy is a deploy of a changed `IdentityPolicy`,
a visible, reviewable diff.

**Migration from bare keys.** (1) Deploy the new version everywhere. (2) **Observe**: the identity
policy in `Observe` mode counts bare keys as before and logs what the identity verdict would have been.
(3) Issue certificates; fix SAN mismatches the observe log shows. (4) **Enforce per approver**: switch
an approver id from a bare key to an identity rule; an approver is one or the other, never both. (5)
Decisions recorded under the old bare key by a switched approver do not count after the switch (a
waiting gate needs them re-signed); switch approvers when no gate waits on them, or accept the re-sign.

## Alternatives considered and rejected

- **Verify OIDC tokens directly (the bank's SSO as the identity source).** Rejected. The costs: JWT/JWS
  and JWKS handling outside the standard library (a new dependency in the verification path, and a larger
  FIPS and audit surface); the live gate and the verifier need network access to the IdP's JWKS at
  verification time, and an offline auditor years later needs the archived key history of the IdP; a
  bearer token proves a login, not that this person signed this exact call (it needs a holder-of-key
  binding such as DPoP, or a signature on top, which is the certificate design again); token lifetimes
  and claims are IdP-specific, so the identity rule is a per-IdP parser; revocation is IdP session state
  with no standard offline-checkable list; and bide would speak OIDC, which the user ruled out.
  Certificates issued by the bank's PKI from its SSO keep the SSO on the bank's side and give bide a
  signature to check.
- **Check the certificate once at enrollment.** Rejected: it gives enrollment-time binding and no
  per-decision validity or revocation.
- **Subject CN as the identity.** Rejected: free text, and no directory-name constraint enforcement.
- **Trust `decided_at` or `recorded_at` for validity.** Rejected: the first is signed by the party being
  checked, the second is set by the submitter's clock.
- **`A(d)` as the minimum covering head time; a verifier-side `t_verify` for the live gate.** Rejected
  by the review: the first lets one skewed or edited clock pull `A` early; the second gives the live
  gate a time the auditor cannot reproduce. Replaced by the smallest-size covering head and anchor-first.
- **Put identity data in the tally or in evidence v7.** Rejected: a strict tally decode would fail an
  older binary's read instead of pausing it, and a v7 package would orphan v6.
- **A global revocation service bide runs.** Rejected: a new always-on component in the trust path.
- **WebAuthn attestation as identity.** Not an identity system; a separate roadmap item (a new signature
  scheme) that composes with this one later, as the signing key under a certificate.

## Test plan

A **test CA** helper in the test packages, standard library only: a root and intermediates
(`x509.CreateCertificate`), name constraints, leaf certificates with chosen SAN, EKU, validity and key
(ECDSA P-256, Ed25519, ML-DSA), and CRLs (`x509.CreateRevocationList`) with chosen entries, revocation
times, reason codes, an invalidity date and the extensions above (added as raw extensions). It runs
in-process, deterministic with injected clocks and a fake head source. Each scenario is a test of
`TallyApprovals` (live environment) and of `bide audit verify-approvals` (offline, from a kit),
asserting the same counted set and the same reason:

1. valid chain, matching SAN, in window, good status: counts.
2. expired at `A(d)`; not yet valid at `decided_at`; `decided_at` in the future of `A(d)`.
3. wrong SAN; two SANs (email and DNS, or two emails); SAN under a different approver id; two approver
   ids on one canonical identity, bare-key included (`ErrConfig`); canonicalization cases (case, NFC,
   non-ASCII refused).
4. name-constraint violation; subdomain of a permitted domain without an excluded subtree.
5. wrong EKU; no EKU; key usage without digital signature; leaf marked CA.
6. unknown CA; right CA but not this gate's anchors; chain longer than `MaxChain`; an intermediate not
   in the pinned set; two chains for a reissued intermediate (the accepted one is recorded).
7. revoked before the decision (`E <= A`): does not count. Revoked after (`E > A`): counts.
   `E = min(invalidityDate, revocationTime)`, and an invalidity date later than the revocation time.
8. `keyCompromise` with no invalidity date: retroactive to `notBefore`. Routine reason inside
   `RoutineGrace`: counts; outside: does not. Compromise reason never gets grace.
9. backdated `decided_at` after revocation, with a late covering head: does not count.
10. CRL stale; `nextUpdate` passed; `CRLNumber` lower than cached (rollback refused); signed by the wrong
    key; issued by another CA; partitioned CRL; critical unknown extension: all "status
    unknown", live gate pauses with `ReasonStatusStale`, no deny.
11. withheld CRL: the kit has only a CRL with `thisUpdate < A + W'`: **provisional**, never final. A CRL
    archived inside the window listing the revocation: not verified.
12. **the expiry sequence** (H1): issue, decision at `A`, revoke at `R > A`, CRL 1 lists it before
    `notAfter`, certificate expires, CRL 2 after `notAfter` omits it. CRL 2 alone: "no information". CRL 1
    present: per `E` versus `A`. With `expiredCertsOnCRL` on CRL 2: usable. `W'` shrinks for a decision
    near `notAfter`.
13. `certificateHold` then release: the series still invalidates decisions with `A` in the hold; a gap in
    time coverage is "no information" for the interval, and a gap in `CRLNumber` values with coverage is
    not.
14. an intermediate revoked with `E_int <= A`: leaf decisions do not count; leaf status checked for every
    non-root certificate.
15. **time evidence** (H2): head times not non-decreasing in size beyond `ClockTolerance`: the run is
    `contradictory head times`; a smaller covering head with a late time and a larger one with an early
    time; a head in the kit that the anchor list does not show is ignored; the kit alone (no anchor
    list): "earlier covering heads cannot be excluded", top tier unreachable; top tier needs an RFC 3161
    or anchor receipt time.
16. downgrade: a `v3` signature from a certificate-bound approver does not count; a `v4` decision from a
    bare-key approver does not count; an old-binary simulation (a verifier that does not know `v4`)
    pauses without error.
17. certificate substitution: the same signature with another valid certificate for the same key.
18. one key under two certificates and two SANs: only one seat counts.
19. `ExcludePrincipal`: the requester's email equals a counted SAN under another approver id, and equals a
    bare-key approver's identity-map value: not counted.
20. rotation: decisions under C1 and C2 for one approver id: the first valid counts; C1 expired when the
    run resumes: still counts at `A(d)`.
21. receipts: chain carried; the anchoring proof is the smallest covering head; a stripped chain does
    not verify as identity.
22. intake: chain verified before write; status failure returns 503 and writes nothing; the intake
    anchors after the write; `GET /pending` shows the identity rule. A direct `bide approve` write with
    no head: the gate anchors before tallying (**no deadlock**, M2), and an unaudited store with a
    cert-bound approver is `ErrConfig`.
23. the `crypto/x509` behavior checks. **They exist in this PR**, in
    `internal/identitycheck/x509_assumptions_test.go`, run on Go 1.27.0, and fail naming the assumption if
    a release changes it: email name constraints (permitted and excluded, subdomains permitted by
    `bank.example`), URI SANs (host constrained, host-less refused), directory-name constraints (ignored
    or refused as unhandled), `CurrentTime`, the private EKU requiring `ExtKeyUsageAny` plus a by-hand
    check, unknown critical extensions, non-CA intermediates, path length, CRL entries and signature,
    raw invalidity date and delta indicator, `CRLNumber`, hold and remove reasons, the partition and
    `expiredCertsOnCRL` extensions, intermediates needing pins and their own status, several SANs
    returned, Ed25519 and ML-DSA leaves and roots, email SAN returned as issued. The build keeps them as
    the assumption checks.
24. forward compatibility, **also in this PR** (`agent/decision_forward_compat_test.go`): decision
    records tolerate unknown fields, the tally decoding is strict (so identity data has its own record),
    the pause note tolerates new fields.
25. `bide identity fetch-crl` against a local `httptest` server: atomic replace, stale alarm, archive,
    rollback refusal.
26. migration: observe mode logs would-be verdicts and counts bare; per-approver enforce; a switched
    approver's old bare decision does not count; a stripped `approval-identity` record on a bound gate is
    reported.
27. fuzzing the certificate and CRL wrappers (no panic, no accept of malformed input); mutation checks on
    the counting rule's identity clauses; pinned verdict vectors across Go versions.
28. **assessment gaps** (each a test): the C1 pruned-CRL sequence and an OCSP "good" after expiry; the
    execution-time re-check (a revocation between count and run stops the call; stale status at execution
    pauses); CRL number rollback and replay; head-time rollback, non-monotonic heads and omission of an
    earlier covering head; the live-versus-offline recount agreeing (no verification-time path); a CA
    certificate with its own EKU restricting the chain (the bide chain walk rejects what `Verify` under
    `ExtKeyUsageAny` accepts); required certificate policy OID; several chains and a cross-certificate (one
    chain required, carried certificates outside the pinned set ignored); a leaf with two SANs, mixed-case
    local parts, IDN and non-ASCII SANs; `approver_alg` that differs from the SPKI, a high-S ECDSA and a
    non-DER signature under `v4`; an issuing-distribution-point CRL, an indirect CRL, `onlySomeReasons`,
    a zero `nextUpdate`; the same serial from two issuers; `certificateHold` and release; the grace boundary
    (`min(A, notAfter)`) on both sides; toolchain drift (the `identitycheck` and verdict-vector tests run
    on every supported Go version in CI, not only 1.27.0); bundle substitution (the `approval-identity-kit`
    hash); the stale-replica downgrade and the stripped `approval-identity` record; the CRL fetcher
    refusing a URL not in the policy and a certificate CDP that disagrees, with size, time and redirect
    limits; the CA key rollover (a CRL signed by the old key still accepted for its certificates);
    `DistrustAfter` on both sides of the time; a cross-validator check of the name-constraint profile
    against OpenSSL for the same chains.
33. **RFC 3161 verifier** (decision 17): real-TSA vectors (OpenSSL `ts`, DigiCert) verify with the
    asserted `genTime`, imprint and signer, and the FreeTSA token is refused as `signing-certificate-v1-only`; differential tests against `openssl ts -verify` on valid and
    mutated tokens; one negative vector per profile refusal (BER or indefinite length, trailing bytes, two
    `SignerInfo`s, SHA-1 `ESSCertID` only, missing or duplicated `contentType`, `messageDigest` or
    `signingCertificateV2`, wrong `messageDigest`, wrong imprint, `genTime` earlier than the head it stamps,
    EKU not exactly `id-kp-timeStamping`, expired or revoked TSA certificate, unlisted TSA, SHA-1 or MD5
    anywhere, oversized input); a fuzz target with a CI budget and a checked-in corpus.
31. **confirmation-pass gaps** (each a test): `W'` near zero gives `provisional (short window)` below
    `MinW`; a producer omitting a listing CRL from the middle of `[A + W, L(C)]` cannot reach `final`
    (the series must have time coverage to the CRL at `L(C)`, and `L(C)` is the first CRL strictly after
    `notAfter`); startup refuses `CAPeriod > ExecStatusAge / 2`, and a 30 min CRL against a 15 min
    `ExecStatusAge` is refused rather than pausing half the time; the alarm is at 50% everywhere; the
    post-terminal hold: stale status holds without writing a tally, a revoked-before-execution decision
    writes `approval-exec-hold` and re-asks through `approval-tally:<id>:ident-reask`, and a crash
    recovery, a `RecoverLoop` resume and a retry all pass through the same check; a plain-name tally on an
    identity-bound gate is superseded and the gate re-asks; the auditor's `-expected-policy` is required
    for an identity-required gate and its absence is reported; the auditor's `-distrust` overlay turns a
    chain and a CRL signed by the distrusted issuer after the time into "not verified" and "no
    information"; one skewed or planted head implicates only the decisions it fixes, and the waiver is
    recorded; `Reanchor` is not invoked for a decision whose chain or signature fails, and is rate
    limited; a TSA token whose imprint mismatches, or whose `genTime` precedes the head it stamps, or from
    a TSA not on the auditor's list, does not count as independent; URI identifiers with a port, a
    trailing slash, percent-encoding, a query, an upper-case path or userinfo are refused; an RSA-3072 CA
    verifies and an RSA-2048 CA is refused unless `AllowRSA2048CA` is set, P-384 CAs verify, SHA-1 is
    refused; the kit archives the verifier hash and the archive timestamp; status bytes are stored once by
    hash and the identity record carries the roster digest and only the counted identifiers.
29. **delta CRLs** (decided 2026-10-15): base plus delta merges and the status time is the delta's
    `thisUpdate`; a delta with a base number above the base's CRL number, a delta whose number does not
    exceed the base's, a delta older than its base, a delta from another issuer key or with an issuing
    distribution point, a missing indicator, a non-critical indicator: each is "status unknown" and never a
    partial answer; `removeFromCRL` removes an entry from the merged view but the archived hold still
    invalidates decisions during it; a gap in time coverage between base and delta is "no information"; an AD
    CS-style cadence (weekly base, hourly delta, 8 h certificates) gives `final` for a decision whose
    window the deltas cover; a delta replayed from before a revocation is refused by the number rule.
30. model 1b: the configurations above, including the expected counterexamples (`WithholdCRL` and
    `RollbackCRL` without the freshness and number rules; `SkewHead` without the non-decreasing rule;
    `MisIssue` without name constraints; the direct-write deadlock without the gate's `Anchor` step), and
    the Go refinement test.

32. **delta and pilot-CA conformance** (against a real CA, not only the test CA): a pilot-CA conformance
    check run against the bank's actual CA (AD CS, and OpenSSL `ca -gencrl` and smallstep as independent
    implementations) records its `CRLNumber` scheme (consecutive, skipping, shared base and delta counter),
    its prune timing in the base-and-delta regime (does the merged view at the first CRL after `notAfter`
    still list a revoked, expired certificate?), and its behaviour on `certificateHold` and
    `removeFromCRL`; a critical entry extension (`certificateIssuer`) makes the CRL unusable; a week-old
    base with a fresh delta is stale by the earlier `nextUpdate`; a series with skipped numbers but time
    coverage reaches `final`, and a series with a time gap does not; the SHA-1 refusal holds for a
    hand-built CRL, OCSP response and RFC 3161 token (bide's own check, not Go's), and the OCSP library's
    SHA-1 behaviour is recorded by a test.

## Build plan (after the decisions)

1. Certificate verification, identity policy, canonicalization, `v4` decision bytes, new reasons,
   `TallyApprovals` and `EvaluateGate` with a static status snapshot, the head source and anchor-first
   step, the **execution-time status re-check** and its state transitions, the `:ident` tally suffix, `DistrustAfter`, the **strict stdlib RFC 3161 token verifier (decision 17) for
   gates that set `IndependentTime`**; test CA; model 1b configurations. An independent PKI review of
   this design, and of step 1 before it merges, gates the work; the critical and high findings of the
   assessment are exit criteria for step 1, with the pilot-CA conformance check (test plan item 32) and the RFC 3161 verifier's evidence (item 33).
2. CRL refresher, `bide identity`, live gate pause behavior, intake `cert` and 503, `bide approve -cert`,
   bide-hsm certificate pairing, the pause-note fields and the `approval-identity` record, and **delta CRL
   merging** (base and delta archive, indicator and number checks, `removeFromCRL`).
3. Identity bundle, covering-head proofs and time attestations in the kit, **the archive timestamp and
   the archived verifier hash**, the anchor-log time receipt,
   `bide audit verify-approvals -identity`, the verdict lines and tiers, receipts with chains.
4. OCSP module (optional), the archive-timestamp renewal tool for the kit.

Every step updates the guides it touches. The approvals guide states the post-quantum gap (decision 15: a
certificate-bound approver gives up the hybrid scheme's protection) where it first describes
certificate-bound approvers.

## Decisions

Numbers are stable across revisions. The assessment disagreed with the earlier recommendation on 2, 4, 9
and 13 and added caveats to 1 and 5; they are folded into the text below.

### Decided by the user, 2026-10-15

Decisions 1, 2, 5, 6 (delta CRLs), 12, 13, 15 and 17.

2. **Independent time above a value threshold.** `A(d)` is the earliest independent time, else the smallest
   covering head; head times must be non-decreasing; the auditor enumerates heads from the anchor log. A
   gate the bank marks above a value threshold sets `IndependentTime`: it **refuses to count a decision
   without an RFC 3161 token or another independent time source** (RFC 3161 verification is in build step
   1). Below the threshold the log signer's clock is accepted and the verdict is labeled `logclock-final`. A
   bank without a TSA or a time-bearing anchor can set no gate above the threshold.
5. **Revocation effect.** A decision stands iff `A(d) < E`, `E = min(invalidityDate, revocationTime)`.
   On `keyCompromise` (and `cACompromise`, `aACompromise`) with no invalidity date the revocation voids
   decisions **back to the certificate's `notBefore`**, and every already-counted decision it reaches is
   flagged **disputed** for the bank's process (the audit does not decide whether the action stands).
   Routine reasons get `RoutineGrace` (default 0). `certificateHold` counts as revoked during the hold.
   Entries are keyed by (issuer key id, serial).
6. **Delta CRLs are in the first build** (so an AD CS default works), with base plus delta merging, the
   delta CRL indicator and base CRL number checks, and the tests in plan item 29. Indirect, partitioned
   and `onlySomeReasons` CRLs stay refused. The rest of this decision is decided by recommendation
   below.
12. **The full chain is stored in the decision record by default** (the journal stays self-contained and
    the Merkle trail covers it); a **by-reference mode** (a salted commitment in the record, the
    certificate retained elsewhere) is a configured option for banks whose privacy review requires it.
    `GET /pending` requires authentication in either mode. A bank under GDPR chooses by-reference before
    go-live; the identity record keeps a roster digest and only the counted identifiers, and status bytes
    are stored once by hash.

1. **Identifier.** The seat key is an **opaque, immutable person id in a URI SAN with a host**; an email
   SAN is accepted as the identifier only with a bank-evidenced control that mailboxes are one per person
   and never reassigned; exactly one SAN; never the Subject DN; case-folded. This depends on the bank's
   directory (can it put an opaque person id in a certificate?). *Decided: yes.*
13. **Issuance assurance and EKU.** A certificate policy OID that the CA asserts only after MFA step-up
    and a hardware-bound or attested key is required; a bank-assigned private EKU OID by default;
    `id-kp-emailProtection` only on a dedicated issuing CA; bide enforces the EKU of every CA on the chain
    itself. This needs the bank's CA to assert a policy OID and to enforce MFA and key protection at
    issuance, which is the bank's capability, not bide's. *Decided: yes.*
15. **Post-quantum.** `ml-dsa-65` approvers can be certificate-bound (the standard library supports
    ML-DSA certificates); the hybrid scheme stays bare-key, so certificate-bound approvers give up hybrid
    protection (a recorded limitation). *Decided: accept the gap and state it in the guide.*

17. **RFC 3161 tokens are verified by a strict stdlib-only verifier** (user, 2026-10-15) on `encoding/asn1`,
    with the narrow profile in "RFC 3161 token verifier": exactly one `SignerInfo`, SHA-256 or better, the
    `contentType`, `messageDigest` and `signingCertificateV2` signed attributes checked, DER only, everything
    outside the profile refused. Real-TSA vectors (OpenSSL `ts` and DigiCert verify; FreeTSA is refused, see "RFC 3161 token verifier"), differential tests
    against `openssl ts -verify`, negative vectors and fuzzing are step-1 exit criteria. Rejected: a
    third-party CMS library in the core, and an optional module that the top tier would depend on.
    *Amended (user, 2026-10-15, after the build and its PKI review):* an **opt-in, off-by-default, per-TSA flag**
    (`Policy.AllowESSV1`, keyed by the TSA certificate's public key) accepts a token that binds its signer by the
    SHA-1 `signingCertificate` attribute alone. Public TSAs such as Sectigo, Entrust and FreeTSA produce only such
    tokens, and refusing them leaves a bank without them no top tier. What the attribute adds is a tie between the
    signature and one certificate among those the token carries; who signed is decided by the signature and the path
    to the auditor's anchors, which SHA-1 does not weaken. The weaker binding stays visible: a `Result` that relied
    on it has `ESSV1` set and the verdict prints it. SHA-1 as the CMS digest algorithm (Apple) stays refused
    whatever the policy says, and a SHA-1 attribute beside `signingCertificateV2` is checked for consistency only.

### Decided by recommendation (technical, 2026-10-15)

These are engineering choices the design settles; each can be reopened if the user objects.

3. **Anchor-first and the audited store.** A cert-bound decision counts only after a covering head; the
   gate anchors itself before tallying (no deadlock on a direct write); the application needs an audited
   store and a head source (an anchor read capability); no `t_verify`.
4. **Status rule.** Absence counts only from status with `thisUpdate` in `[A, L(C)]`, where `L(C)` is the
   first scheduled CRL at or after `notAfter` (unbounded with `expiredCertsOnCRL` or contractual
   retention); later CRLs only add rejections; `W = 24 h` cut to `W'`; the gate journals the status it
   relied on; `anchorGrace = 60 s` with the leaf verified at `min(A, notAfter)`; `skew` 2 min,
   `ClockTolerance` 60 s; a `final` verdict needs `W' >= MinW` (30 min) and a series complete to the CRL at
   `L(C)` (derived from the archived series, first CRL strictly after `notAfter`).
6. (rest) **Latency and CRL policy.** `MaxStatusAge` default 1 h; `ExecStatusAge` set from the CA's actual
   period (startup refuses `CAPeriod > ExecStatusAge / 2`) with a status re-check immediately before the
   call runs on every execution path, a published latency number, monotonic CRL numbers, alarm at 50%; CRLs fetched only from configured URLs by the refresher; `Status: NotRequired` only by explicit
   policy for short-lived certificates (default off).
7. **Stale or missing status.** The live gate pauses with a named needs-attention reason (at count time
   and at execution) and the intake returns 503; no deny, no break-glass in the core; staleness never
   uncounts a counted decision.
8. **CA compromise.** Status for every non-anchor certificate; per-anchor and per-intermediate
   `DistrustAfter` against `A(d)`, applied to chains **and to status signed by the distrusted issuer**, with
   an auditor-supplied `-distrust` overlay; a stated CA key rollover procedure; trust anchors per gate or policy,
   never the system roots.
9. **Seats and downgrade.** The seat is the canonical identifier, bare-key approvers included in the
   identity map, plus the dynamic same-leaf-key check; pinned intermediates and exactly one valid chain;
   the pause note records the approver kinds and a digest of the full policy; the auditor requires the
   `approval-identity` record on bound gates; observe mode is time-boxed (30 days), the old bare key is
   removed at the switch; the live protection is rolling the fleet first (no version floor an old binary
   would honor is claimed); identity-bound gates record their tally under `:ident` and a plain-name tally is
   superseded; the auditor's expected policy of record is a required input for identity-required gates.
10. **Evidence and kit.** A separate identity bundle (not evidence v7), bound to the journal by an
    anchored kit-hash record, carrying the full effective-dated policy; the kit records the verifier, Go
    version and `GODEBUG`; the bundle format is frozen with pinned vectors; an archive timestamp is
    renewed over the retention period.
11. **Decision format and records.** `bide.approval.v4` for certificate-bound decisions with the leaf hash
    signed and in the step name; the scheme is derived from the SPKI and a mismatch with `approver_alg` is
    refused; leaf schemes only ed25519, ecdsa-p256, ml-dsa-65; identity data in its own record, never the
    tally.
14. **Canonicalization and `ExcludePrincipal`.** One bank-defined `Canonical` for identifiers, SANs and
    principals; `ExcludePrincipal` also compares the verified identifier.
16. **Staging and review.** Four steps, each its own reviewed PR; an independent PKI review of the design
    before the build and of step 1 before it merges; the critical and high assessment findings are exit
    criteria for step 1, together with the confirmation pass's findings N2, N3, N5 and the downgrade
    residual, the conformance check of the pilot CA (test plan item 32: number scheme, prune timing in
    the delta regime, critical entry extensions) passing, and the RFC 3161 verifier's vectors, differential
    tests and fuzzing (test plan item 33) passing; commands under `bide identity` (`fetch-crl`, `check`, `kit`).

### Still open for the user

None. Every decision is settled.
