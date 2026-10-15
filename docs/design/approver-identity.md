# Design: approver identity and revocation

Status: proposed, revision 2. Design only; nothing here is built. Revision 2 folds in an adversarial
review (two high and eight medium findings, which changed the time rule, the status rule, the
anchoring order and the record placement) and the `crypto/x509` behaviors checked by running code.
The decisions at the end are the user's; the build waits for them. This is bank readiness item 1.

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
  (`golang.org/x/crypto/ocsp`) may live only in an optional module.
- SSO is the bank's. Its SSO issues short-lived certificates through its existing PKI (smallstep,
  Vault PKI, ADCS, or another). bide verifies certificates. It never speaks OIDC.

## Threat model

Adversaries and what the design does about each. "Residual" says what remains after the mitigation.

| Threat | Mitigation | Residual |
|---|---|---|
| **Stolen approver key** (disk, backup, malware) | Short-lived certificates (hours) bound the use window; revocation ends it earlier; the effective revocation time `E` invalidates every decision first provable at or after `E`; keys in an HSM or KMS (bide-hsm) make theft harder | Decisions provable before `E` stand, including ones the thief made before the theft was noticed. A late-reported compromise is caught only if the CA dates it back (invalidity date) and the status evidence is late enough (window `W'`). `keyCompromise` with no invalidity date is retroactive to `notBefore` |
| **Departed employee** | Certificates are not renewed after SSO access ends; revocation on departure; the approver id's identity rule no longer matches any live certificate | A decision made before departure stays valid by design. A bank that wants waiting gates to drop a leaver's approvals re-asks the gate (an operational step) |
| **Compromised CA** (or a mis-issuing RA) | Trust anchors per gate or policy, not global; name constraints on the CA; the identity rule fixes the exact SAN per approver id; permitted intermediates are pinned; exactly one SAN; EKU required | A CA inside its name constraints can still issue a certificate for any approver in its domain. bide cannot detect that. The offline kit shows which issuer each decision rests on |
| **Backdated decision** (a thief signs with a revoked or expired key and sets `decided_at` inside the window) | `decided_at` is the approver's claim. The time that decides validity is `A`, the time of the smallest-size signed head that covers the record, anchored before the gate counts the decision. A decision whose `A` is at or after `E`, or after `notAfter` plus a grace, does not count | The kit's time is only as independent as its source: with the log signer's clock alone the top tier is not reached (see Time). A late covering head means a late `A`, which fails closed |
| **Operator as adversary** (an insider with journal and evidence-producer access, but without the log key, the CA key or an approver key) | `A` is the timestamp of the smallest covering head and head times must be non-decreasing in size, so one slow or edited clock cannot pull `A` earlier; the auditor enumerates heads from the anchor log, not from the producer; the top tier needs an independent time source | An operator who also holds the log signer key can mint heads. That is outside every audit claim in bide today (the log key is the root of trust) and needs the independent time source to bound |
| **Revocation list withheld, replayed or rolled back** | Live: a CRL older than `MaxStatusAge`, past `nextUpdate`, or with a lower `CRLNumber` than the cached one is refused; stale status pauses the gate. Offline: only status issued at or after `A` and within the certificate's life (plus the CA's declared retention) counts; a gap in the CRL number series is "no information" | A revocation the CA never published, or published only after the retention ended, is invisible |
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
  (pinned by a test). Recommended kind: **rfc822Name** (`alice@bank.example`), because Go's chain
  verification enforces name constraints on email SANs and a human identity is naturally an email
  address. A **URI SAN with a host** (`spiffe://bank.example/approver/alice`) is the alternative. A
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
  cannot approve.
- Not a CA (`basicConstraints` CA false). Leaf key schemes: `ed25519`, `ecdsa-p256` (and P-384 where
  the decision scheme exists), RSA 2048 or larger, and `ml-dsa-65` under a CA that issues it. The leaf
  key scheme is the decision's `alg`. A hybrid approver stays bare-key (see Scope).
- Unknown critical extensions on the leaf or an intermediate make the chain fail (`Verify` does this
  itself, pinned by a test).

**Canonicalization.** One bank-defined function, `Canonical`, maps an identity string to its
canonical form, and it is used in **every** place an identity is compared: the identity map, the SAN,
the `run:principal` requester (`ExcludePrincipal`), and the approver-id mapping for bare-key
approvers. The default: Unicode NFC, then lowercase the whole email (local part included; the bank
states that its directory is case-insensitive), ASCII only (internationalized addresses are refused
until a bank asks), no trimming or alias folding (`a.smith` and `asmith` are different identities
unless the identity map says otherwise). URI: scheme and host lowercase, path exact. The comparison
is bytes after this, never substring or suffix. A bank with a different directory rule supplies its
own `Canonical`; the verdict records its name and version.

**Where the rules live.** Not in the journal's `ApprovalPolicy` (which records `need` and approver
ids and stays as it is). A new value `audit.IdentityPolicy`, handed to the resolver, holds:

```go
type IdentityPolicy struct {
    Anchors   *x509.CertPool          // trust anchors for THIS gate (not the system roots)
    Inter     []SPKIHash              // PINNED intermediates: the chain may only pass through these
    Identity  map[string]string       // approver id -> canonical identity, for EVERY approver of the gate
    SANKind   SANKind                 // Email or URIWithHost
    EKU       asn1.ObjectIdentifier   // required extended key usage
    Canonical Canonicalizer           // one function for identities, principals and SANs
    Status    StatusPolicy            // CRL required, OCSP optional, MaxStatusAge, W, grace values
    MaxChain  int                     // path length bound
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
policy by public key hash, the verifier builds the chain from the pinned set and the carried chain,
and records which chain it accepted. Name constraints on each CA are checked by `Verify`. The anchors a
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
  covering two writers with skewed clocks), makes the run's time evidence **contradictory**: every
  cert-bound decision of the run is `not verified: contradictory head times`. The kit is rejected, not
  repaired.
- **The auditor enumerates heads from the anchor log, not from the producer.** How an auditor knows
  there is no earlier covering head than the one in the kit: they list the run's heads in the anchor
  log (an independent party) and compute the smallest covering size over that list. A head that the
  producer put in the kit but the anchor log does not show is not used. If the auditor has only the
  producer's kit, the verdict says "earlier covering heads cannot be excluded" and the decision cannot
  reach the top tier.

**The time source is part of the verdict.** A head's timestamp comes from the log signer's clock, which
is the same party the operator-adversary may influence. Therefore:

- The **top tier requires an independent time source**: an RFC 3161 token over the covering head (or
  its hash) from a TSA the auditor trusts, or an anchor-log receipt carrying the log operator's own
  signed time (today `audit.AnchorEntry` carries a sequence number and a head, no time; a time-bearing
  anchor receipt is added in the build). The verdict prints the source.
- With only the log signer's clock the best tier is lower and says so.

**The rule.** A decision `d` by approver `a` counts iff all of:

1. the certificate chain, built from the pinned intermediates, verifies to a trust anchor of the gate's
   trust set, at time `A(d)`, with the required EKU, name constraints and key usage, **exactly one**
   SAN of the configured kind, and the canonical SAN equals `a`'s identity rule;
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

Revocation data is only evidence about a window, and CAs drop entries for expired certificates from
later CRLs unless the CRL carries `expiredCertsOnCRL` (OID 2.5.29.60). A CRL issued after a
certificate expired is therefore **no information** about it: it cannot show the certificate was
never revoked. For a certificate `C` and decision `d`:

- `L(C) = notAfter(C) + epsilon`, where `epsilon` is the retention the CA declares for expired entries
  (default 0). With `expiredCertsOnCRL` present on the CRL, any later CRL is as good as one inside the
  window. Short-lived certificates are the common case, so `epsilon = 0` is the safe default.
- A CRL is **usable** for `d` iff `A(d) <= thisUpdate <= L(C)` and it is the right issuer with a valid
  signature and `CRLNumber` consistent with its series. An OCSP response is usable on the same terms
  with `thisUpdate`.
- The effective window `W' = min(W, L(C) - A(d))`: the bank's compromise-reporting window `W`
  (default 24 h), cut to what the certificate's own lifetime leaves. A decision made near the end of a
  certificate's life has little status window left; that is a property of short-lived certificates,
  stated rather than hidden.
- Verdicts: **final** iff there is a usable status `S` with `S >= A(d) + W'`; **provisional** iff the
  best usable status has `S < A(d) + W'` and a later one could still exist (`now < L(C)`); **no
  information** iff every status is outside `[A(d), L(C)]`, which is then `not verified: status
  missing`.
- `E` is the earlier of the invalidity date (entry extension 2.5.29.24, when present) and the
  revocation time: `E = min(invalidityDate, revocationTime)`. For reasons `keyCompromise`,
  `cACompromise` and `aACompromise` with no invalidity date, `E = notBefore` of the revoked
  certificate (the compromise time is unknown, so every decision under it is suspect).
- **Series, not snapshots.** The verifier reads the whole archived series of CRLs for each issuer in
  `[A, L]`. A listing in any CRL of the series is a revocation from its time on; a later CRL that no
  longer lists the serial does not undo it (`certificateHold` (6) and `removeFromCRL` (8) are visible
  as reason codes, pinned by a test). A gap in `CRLNumber` between archived CRLs is "no information"
  for the interval. A cache refuses a CRL whose `CRLNumber` is lower than the cached one (a rollback).
- **Intermediates.** The same rule applies to every non-root certificate of the chain: an intermediate
  revoked with `E_int <= A(d)` invalidates the decision.
- The exact sequence that must be tested (test plan 12): a certificate is issued; a decision is made at
  `A`; the certificate is revoked at `R > A` and listed in CRL 1 (issued before `notAfter`); the
  certificate expires; CRL 2 (issued after `notAfter`) no longer lists it. The verifier given CRL 2
  alone must answer "no information", never "not revoked"; given CRL 1 it answers per `E` versus `A`.

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
(<issuer> CRL #<n>), window W'=<d>, rules v<n>, verifier go<version>`

and a tier: **confirmed** (independent time source, status final), **log-clock** (status final, time
from the log signer only, earlier heads excluded by the anchor list), **provisional** (a later status
could change it), **disputed** (below), **not verified** (with the reason: chain, SAN, EKU, expired,
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
| `anchorGrace` | `A <= notAfter + anchorGrace` | 60 s | recommend 60 s with synchronous anchoring (anchor-first makes `A` follow the write by seconds); a bank that anchors slowly sets it to its worst case, which weakens the bound by that much |
| `RoutineGrace` | for revocation reasons other than compromise (affiliationChanged 3, superseded 4, cessationOfOperation 5, privilegeWithdrawn 9), a decision with `A < E + RoutineGrace` still counts | 0 | covers a CRL published a few minutes after an offboarding; a bank sets minutes, never hours; compromise reasons and unspecified never get grace |
| `W` | compromise-reporting window | 24 h | from the bank's incident process |
| `MaxStatusAge` | live: oldest usable CRL | 2 x CRL period | with the CRL period at most a quarter of the shortest certificate lifetime |

### CRL cadence and the CA that cannot meet it

A short-lived certificate (say 8 h) needs status inside its life: a CRL every 2 h, or OCSP responses
archived at decision time. Mapping CA behavior to what this design needs:

| CA behavior | Works? | What to do |
|---|---|---|
| Full CRLs every 1 to 4 h | yes | set `MaxStatusAge` to twice the period |
| Weekly base CRL plus daily or hourly **delta** CRLs | no, in the core | delta CRLs are refused ("status unknown"). Ask the CA for full CRLs at the short period (step-ca, Vault PKI and ADCS can publish full CRLs frequently), or use OCSP (optional module) |
| Daily full CRL, certificates of 8 h | not final | a decision usually has no usable CRL inside its certificate's life: provisional or "status missing". Shorten the CRL period, set `expiredCertsOnCRL`, or use OCSP |
| No revocation for short-lived certificates (expiry is the control) | only by explicit policy | `Status: NotRequired` is accepted only for a certificate whose lifetime is at most `ShortLived` (default off, bank-set, hours); revocation latency then equals the lifetime, and the verdict says status was not required (decision 8) |

## Revocation

- **CRL** is required and enough (unless the policy above says otherwise). Parsed with
  `x509.ParseRevocationList`; the CRL signature is checked against the issuing CA certificate of the
  chain (`CheckSignatureFrom`, which fails for any other CA); entries carry serial, revocation time
  and reason code as fields, and the invalidity date extension (a GeneralizedTime) is visible only as
  a raw entry extension that bide parses. `Certificate.Verify` never consults a CRL, an intermediate's
  included, so every revocation check is bide's (pinned by a test). The delta CRL indicator
  (2.5.29.27), the issuing distribution point (2.5.29.28, partitioned CRLs) and any unrecognized
  critical extension are visible in `RevocationList.Extensions` with their critical flag, so
  **delta, partitioned and unknown-critical CRLs are refused** in the first version: "status unknown",
  never "good".
- **OCSP** is optional, in a separate module so the core does not import
  `golang.org/x/crypto/ocsp`. An OCSP response is archived whole and verified like a CRL (signed by the
  CA or a delegated responder with the OCSP-signing EKU). `unknown` and any parse failure are "status
  unknown". It gives the intake and the gate fresher status, and the kit may hold either.
- **Fail closed.** No status for a certificate that requires it, a stale one, a signature that does not
  verify, a CRL that does not cover the certificate: the decision does not count. For the live gate
  this is a **pause**, not a deny: the gate stays waiting, the needs-attention state names "approver
  identity status unavailable", and the notifier says so (decision 7). A deny is reserved for cases
  where the gate can already never pass.
- **Freshness and caching.** A refresher fetches from the certificate's CRL distribution point,
  verifies, and replaces the cache atomically, keeping every CRL in a dated archive. A CRL is usable
  live while `now < nextUpdate`, `now - thisUpdate <= MaxStatusAge`, and its `CRLNumber` is not lower
  than the cached one. A failed refresh keeps the previous CRL until it goes stale and raises an
  `OnStatusError` hook (like `AuditedStore.OnError`).

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
would make an older binary fail the read, not pause. The identity environment of a terminal count
(trust anchors by key id, the accepted chain per counted decision, the status series digests, the time
source, the rules version) is written as a separate insert-once record, `approval-identity:<encoded
call id>`, beside the tally. Older binaries ignore it. The pause note (`ApprovalWait`, decoded
leniently, pinned by a test) gains two additive fields, `identity_bound` and `identity_kinds` (a digest
of the sorted `approver=bare|cert` pairs), so the **auditor requires the `approval-identity` record
whenever the pause note says the gate had a certificate-bound approver** and reports a missing one
instead of treating a tally with the record stripped as a bare-key tally.

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
- **NoStuckPause** (extended): a gate with enough genuinely valid cert-bound decisions, fresh status
  and an audited store is not stuck, **including when a decision was written directly with no covering
  head**: the gate's `Anchor` step supplies it. Without the `Anchor` step the model must show the
  deadlock, and with it not.

A Go refinement test runs the same scenarios through `TallyApprovals` with the stdlib CA (below) and
compares the counted set with the model's, trace by trace, as the existing models do. The change to
`spec/tla` and its README map is part of the build, and `modelsync` gates the PR.

## Operations for a bank

**Setup.**

1. The CA issues approver certificates after SSO with MFA: exactly one SAN (email), the approver EKU, a
   short lifetime, name constraints on the issuing CA (with an excluded subtree for subdomains if it must
   exclude them), a CRL distribution point, and a CRL period within the table above.
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

**The offline auditor's kit** is a directory: the evidence package, the identity bundle (chains,
covering-head proofs, time attestations, the archived CRL/OCSP series), and a README listing what the
auditor must bring: the trust anchors, from the bank's PKI team over a channel not shared with the
producer; the log public key; and the anchor log's list of heads for the run (so the smallest covering
head can be computed independently). `bide audit verify-approvals -identity ... -trust-anchors ...
-anchor-log ...` needs nothing else and no network. Keep the archive for the retention period,
including the CRLs: the archived series is the evidence, because a CA drops expired entries.

**Stale CRL in production.** The refresher alarms before staleness (at 75% of `MaxStatusAge`). At
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
    key; issued by another CA; delta CRL; partitioned CRL; critical unknown extension: all "status
    unknown", live gate pauses with `ReasonStatusStale`, no deny.
11. withheld CRL: the kit has only a CRL with `thisUpdate < A + W'`: **provisional**, never final. A CRL
    archived inside the window listing the revocation: not verified.
12. **the expiry sequence** (H1): issue, decision at `A`, revoke at `R > A`, CRL 1 lists it before
    `notAfter`, certificate expires, CRL 2 after `notAfter` omits it. CRL 2 alone: "no information". CRL 1
    present: per `E` versus `A`. With `expiredCertsOnCRL` on CRL 2: usable. `W'` shrinks for a decision
    near `notAfter`.
13. `certificateHold` then release: the series still invalidates decisions with `A` in the hold; a gap in
    `CRLNumber` is "no information" for the interval.
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
28. model 1b: the configurations above, including the expected counterexamples (`WithholdCRL` and
    `RollbackCRL` without the freshness and number rules; `SkewHead` without the non-decreasing rule;
    `MisIssue` without name constraints; the direct-write deadlock without the gate's `Anchor` step), and
    the Go refinement test.

## Build plan (after the decisions)

1. Certificate verification, identity policy, canonicalization, `v4` decision bytes, new reasons,
   `TallyApprovals` and `EvaluateGate` with a static status snapshot, the head source and anchor-first
   step; test CA; model 1b configurations.
2. CRL refresher, `bide identity`, live gate pause behavior, intake `cert` and 503, `bide approve -cert`,
   bide-hsm certificate pairing, the pause-note fields and the `approval-identity` record.
3. Identity bundle, covering-head proofs and time attestations in the kit, the anchor-log time receipt,
   `bide audit verify-approvals -identity`, the verdict lines and tiers, receipts with chains.
4. OCSP module (optional), RFC 3161 token carriage and verification.

## Decisions for the user

1. **Identity field.** SAN **rfc822Name (email)** primary, **URI SAN with a host** the alternative; exactly
   one SAN; never the Subject DN. *Recommend: yes.*
2. **Time source.** `A(d)` is the smallest-size covering head; head times must be non-decreasing; the
   auditor enumerates heads from the anchor log; the **top tier requires an independent time source**
   (an RFC 3161 token or a time-bearing anchor receipt) and the verdict prints the source. The cost: a
   TSA or a time-bearing anchor is a real dependency, and a bank without one reaches only the
   `log-clock` tier. *Recommend: yes; build the head rules first, the token and receipt in step 4.*
3. **Anchor-first and the audited store.** A cert-bound decision counts only after a covering head, the
   gate anchors itself before tallying (no deadlock), the application needs an audited store and a head
   source (an anchor read capability), and a cert-bound approver on an unaudited store is `ErrConfig`.
   *Recommend: yes.*
4. **Status windows.** The `L(C)`/`W'` rule and the verdict tiers in this note, defaults `W = 24 h`,
   `epsilon = 0`, `anchorGrace = 60 s`, `skew = 2 min`, `ClockTolerance = 60 s`. *Recommend: yes; the
   bank sets `W` from its incident process.*
5. **Revocation effect.** A decision stands iff `A(d) < E`, `E = min(invalidityDate, revocationTime)`,
   `keyCompromise` family without an invalidity date means `E = notBefore`, routine reasons get a
   `RoutineGrace` (default 0), and a revocation that retroactively reaches an already-counted decision
   is reported as **disputed** for the bank's process, not decided by the audit. *Recommend: yes.*
6. **CRL cadence policy.** Full CRLs only in the core; delta and partitioned CRLs refused; the CRL period
   at most a quarter of the shortest certificate lifetime; `Status: NotRequired` for certificates of at
   most a bank-set short lifetime is allowed only by explicit policy (default off). *Recommend: yes; ask
   the CA owner now whether it can publish full CRLs at the short period.*
7. **Stale or missing status.** The live gate pauses with a named needs-attention reason, the intake
   returns 503; no deny, no break-glass in the core. *Recommend: yes.*
8. **Evidence shape.** A separate **identity bundle** artifact (plus the anchor-log list as an auditor
   input), not evidence v7. *Recommend: yes.*
9. **Decision format and names.** `bide.approval.v4` for certificate-bound decisions with the leaf hash
   signed and in the step name; bare-key stays `v3`. *Recommend: yes.*
10. **Where identity data lives.** Its own `approval-identity:<call>` record and additive pause-note
    fields; never the tally (strict decoding); the auditor requires the record on bound gates.
    *Recommend: yes.*
11. **Seats and identity.** The seat is the canonical identity, bare-key approvers included in the
    identity map, plus the dynamic same-leaf-key check; pinned intermediates; status for every non-root
    certificate. *Recommend: yes.*
12. **Canonicalization and `ExcludePrincipal`.** One bank-defined `Canonical` (default NFC, lowercase,
    ASCII) for identities, SANs and principals; `ExcludePrincipal` also compares the verified identity.
    *Recommend: yes; the guide tells banks to record the principal as the same email or URI.*
13. **Migration.** Observe mode, then per-approver enforcement, an approver bare-key **or**
    certificate-bound, never both. *Recommend: yes.*
14. **Trust anchors and EKU.** Per gate or policy, never the system roots; a bank-assigned private EKU OID
    by default with `id-kp-emailProtection` as a configured alternative. *Recommend: yes.*
15. **Post-quantum.** `ml-dsa-65` approvers can be certificate-bound (the standard library supports ML-DSA
    certificates); the hybrid scheme stays bare-key until a certificate form exists. *Recommend: accept
    the gap and say so in the guide.*
16. **Commands and staging.** `bide identity` (`fetch-crl`, `check`, `kit`); build in the four steps, each
    its own reviewed PR, the first behind the model 1b configurations; nothing released before the model
    and the adversarial review of step 1. *Recommend: yes.*
