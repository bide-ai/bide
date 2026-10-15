# Design: approver identity and revocation

Status: proposed. Design only; nothing here is built. The decisions at the end are the user's; the
build waits for them. This is bank readiness item 1.

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
needed status is missing.

Not claimed:

- That the person was at the keyboard. Proof of presence is the bank's SSO and its MFA when it issues
  the certificate (and, later, a hardware or passkey signer). bide sees a certificate and a signature.
- That the CA, the CRL publisher or the identity provider behaved. A compromised CA can issue a
  certificate for anyone; this design limits that with name constraints and per-policy trust anchors,
  and says exactly what it cannot detect.
- A trusted clock. Time is bounded by signed tree heads, which rest on the log signer's clock. An
  external timestamp is an optional hardening (decision 2).
- Compliance with any regime. The core uses only the Go standard library for certificate and CRL
  handling, so it runs on Go's FIPS 140-3 module where the deployment enables it. `crypto/x509` is
  not itself inside the validated module boundary (it calls into it), and this note makes no claim
  about validation. Whether that satisfies an assessor is the bank's call.

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
| **Stolen approver key** (disk, backup, malware) | Short-lived certificates (hours) bound the use window; revocation ends it earlier; the effective revocation time `E` invalidates every decision first provable at or after `E` (see Time); keys in an HSM or KMS (bide-hsm) make theft harder | Decisions provable before `E` stand, including ones the thief made before the theft was noticed but after the real compromise. `keyCompromise` with no invalidity date is treated as retroactive to the certificate's `notBefore` (decision 4) |
| **Departed employee** | Certificates are not renewed after SSO access ends; revocation on departure; the approver id's identity rule no longer matches any live certificate | A decision made before departure stays valid by design. A bank that wants waiting gates to drop a leaver's approvals re-asks (an operational step; see Operations) |
| **Compromised CA** (or a mis-issuing RA) | Trust anchors are per gate or policy, not global; name constraints on the CA bound the identities it can mint (email domain, URI host); the identity rule fixes the exact SAN per approver id; EKU is required | A CA inside its name constraints can still issue a certificate for any approver in its domain. bide cannot detect that; a certificate transparency style log for the bank's internal CA is the bank's control. The offline kit lets an auditor see exactly which issuer each decision rests on |
| **Backdated decision**: the thief signs with a revoked or expired key and sets `decided_at` inside the validity window | `decided_at` is signed by the approver and so is the approver's claim only. The time that decides validity is `A`, the earliest time the decision provably existed: the timestamp of the first signed tree head that covers it. A decision whose `A` is at or after `E`, or after `notAfter` plus a small grace, does not count | If the decision was written but no head covers it for a long time (a direct `bide approve` write, no application write since), `A` is unknown or late and the decision cannot be shown valid yet. Fail closed, not guess |
| **Revocation list withheld from the verifier** (an attacker or a failure keeps a CRL that lists the key away from the gate or the auditor) | Live gate: a CRL older than `MaxStatusAge`, or past its `nextUpdate`, is "status unknown" and the decision does not count (the gate pauses). Offline: the verifier needs a CRL (or OCSP response) issued at or after `A + W` where `W` is the bank's compromise-reporting window; a CRL that predates that does not prove anything and the verdict says so | A revocation the CA never published, or published after the evidence was sealed and after `W`, is invisible. The evidence is as strong as the status data the producer chose to archive; the auditor's own fetch of the CA's current CRL history is the check |
| **Someone enrolls themselves** (adds a key or an identity for their own id, or a second id for themselves) | An approver is no longer enrolled by a key anyone with the file can add. The identity map (approver id to expected SAN) and the trust anchors are policy configuration, change-controlled by the bank, and `bide check -diff` flags a weakened gate. A person cannot get a certificate for another SAN without the IdP; one SAN under two approver ids is refused (identity seat); `ExcludePrincipal` keeps the requester out by identity as well as by id | Whoever can change the resolver configuration can still enroll. That is the same trust boundary as the code and the policy. The note only moves the question from "who may add a key" to "who may issue a certificate and edit the identity map" |

Two assumptions are explicit. The log signer's key and clock are trusted to sign tree heads (this is
already the root of every audit claim in bide). The bank's CRL publication point is trusted to publish
correctly when reachable, and failures of availability must stop approvals, not weaken them.

## How an approver id binds to a certificate

Today: approver id `ops` resolves to a key. New: approver id `alice` resolves to an **identity rule**
and a **trust set**; the key is whatever key the presented certificate carries.

**What the certificate must say.**

- Identity: a Subject Alternative Name, matched exactly (after the normalization below) against the
  value the policy's identity map gives for that approver id. Recommended field: **rfc822Name**
  (`alice@bank.example`), because Go's chain verification enforces name constraints on email SANs
  and a human identity is naturally an email address. A **URI SAN with a host**
  (`spiffe://bank.example/approver/alice`) is the alternative for non-mailbox identities. A URI with
  no host (`urn:...`) is not usable: with URI name constraints in the chain, Go refuses it ("URI with
  empty host cannot be matched against constraints"). The Subject DN (`CN`) is **not** used: a CN is
  free text, and `crypto/x509` does not enforce directory-name constraints (a non-critical one is
  ignored; a critical one makes the chain fail with "unhandled critical extension", which is safe
  but protects nothing). These behaviors were run against Go 1.27.0 and are pinned by the tests in
  `internal/identitycheck` (test plan item 23).

  Two details from those runs shape the rules. A permitted email constraint written `bank.example`
  also permits subdomains (`alice@evil.bank.example` verifies), unlike RFC 5280's reading, so a CA
  that must exclude subdomains adds an excluded subtree `.bank.example`, and the design never relies
  on a permitted subtree alone to pin a domain: the identity map matches the SAN exactly, and the
  constraints are defense in depth against a CA issuing outside its remit. And `Certificate.Verify`
  requires the ServerAuth extended key usage when `VerifyOptions.KeyUsages` is empty, so a leaf
  carrying only the bank's private OID fails; the verifier passes `ExtKeyUsageAny` and checks the
  required OID itself against `Certificate.UnknownExtKeyUsage` (a leaf with no EKU at all passes
  `ExtKeyUsageAny`, so the by-hand check requires the OID to be present).
- Key usage: digital signature. Extended key usage: a bide approver EKU (a private OID the bank
  assigns, documented in the guide), checked by hand as above, so a certificate issued for TLS or
  mail cannot approve. A bank
  that cannot add an EKU may pin `id-kp-emailProtection` (decision left in the open questions of
  the build; the verifier takes the required EKU as a policy value).
- Not a CA (`basicConstraints` CA false), RSA 2048 or larger, ECDSA P-256 or P-384, or Ed25519 for
  the leaf key. The leaf key scheme is the decision's `alg` (today `ed25519`, `ecdsa-p256`,
  `ml-dsa-65`, hybrid; X.509 support for ML-DSA depends on the bank's CA and is out of scope here).

**Normalization.** Email: lowercase the domain, keep the local part as issued, reject if the
local part contains characters the identity map does not contain. URI: scheme and host lowercase,
path exact. The comparison is bytes after this, never substring or suffix.

**Where the rules live.** Not in the journal's `ApprovalPolicy` (which records `need` and approver
ids and stays as it is). A new value `audit.IdentityPolicy`, handed to the resolver, holds:

```go
type IdentityPolicy struct {
    Anchors   *x509.CertPool          // trust anchors for THIS gate (not the system roots)
    Inter     *x509.CertPool          // optional fixed intermediates; the chain may also be carried
    Identity  map[string]string       // approver id -> expected SAN value
    SANKind   SANKind                 // Email or URIWithHost
    EKU       asn1.ObjectIdentifier   // required extended key usage
    Status    StatusPolicy            // CRL required, OCSP optional, MaxStatusAge, W
    MaxChain  int                     // path length bound
}
```

A resolver returns an `ApproverVerifier` for an id as it does today; for a certificate-bound id it
returns a verifier that also implements the new decision-level interface (below). One process can
serve some gates with bare keys and some with identity policies, and one policy can mix approvers
of both kinds.

**Trust anchors per gate or policy.** A refund gate may trust the "treasury approvers" issuing CA and
a deploy gate the "release approvers" CA. Name constraints on each CA (permitted email domain or
URI host, excluded subtrees) are checked by `Certificate.Verify`. The anchors a decision was counted
under are named in the gate's recorded tally (see Journal changes), so an auditor can tell which
trust set the gate used and compare it with the one they hold.

## Time: what makes a decision valid

This is the hard part, so it is stated as definitions and one rule.

Facts a verifier can check, and who controls them:

| Fact | Source | Controlled by |
|---|---|---|
| `decided_at` | signed by the approver | the approver (or a thief): a claim only |
| `recorded_at` | journal record, unsigned | the process that wrote the record (a `bide approve` machine for a direct write): not trusted for validity |
| `T_head(k)` | `timestamp_nanos` of the signed tree head `k` | the log signer; a head whose size exceeds the decision's journal position **provably covers** the record (the receipt machinery already checks this) |
| `A(d)` | the smallest `T_head` over signed heads that cover the decision record | derived: the earliest time the decision provably existed |
| `notBefore`, `notAfter` | certificate | the CA |
| `E` | revocation: the CRL entry's invalidity date if present, else its revocation time; for reason `keyCompromise` with no invalidity date, the certificate's `notBefore` | the CA |
| `thisUpdate`, `nextUpdate` | CRL | the CA (or its CRL issuer) |

**Rule.** A decision `d` by approver `a` counts iff all of:

1. the certificate chain verifies to a trust anchor of the gate's trust set, at time `A(d)`, with the
   required EKU and name constraints, and the leaf's SAN matches `a`'s identity rule;
2. `notBefore <= decided_at <= notAfter` and `decided_at <= A(d) + skew` (the signed time is
   plausible and not from the future of its own proof);
3. `A(d) <= notAfter + grace` (the decision was already provably in existence while the certificate
   was alive, give or take the anchoring delay; `grace` is a policy value, default 0);
4. the signature verifies under the leaf key over decision bytes that include the leaf's hash (see
   Journal changes);
5. the certificate's status is known and good: no CRL (or OCSP response) in the status data lists the
   serial with `E <= A(d)`, and the status data is fresh enough (live gate) or late enough (offline
   audit; see below).

Why `A(d)` and not `decided_at`: a stolen key can set `decided_at` to anything inside the
validity window. It cannot make a signed tree head that does not cover its record exist earlier than
it was signed. So the thief's best case is `A(d)` equal to the true time of the forged write. If
that time is after `E`, rule 5 rejects it. If the thief wrote before `E` and the CA later records an
earlier compromise date, `E` moves earlier and rule 5 rejects it too. This is why `invalidityDate`
and the `keyCompromise` default matter.

The cost of the rule is the **anchoring window**: a decision whose covering head is late has a late
`A`. With the application's `AuditedStore` and the approvals intake, a head covers the decision at the
next write, in milliseconds. With a direct `bide approve` journal write, nothing signs a head until
the application next writes to the run; `A` is then late, and a decision signed at 17:59 under a
certificate that expired at 18:00 cannot be shown valid if the head is stamped 19:00 and `grace` is
0. Three ways to close it, in order of strength:

- route approvals through the intake (it writes through the application's audited store);
- `grace` sized to the longest expected anchoring delay (a policy value, weakens the bound by
  exactly that much);
- an RFC 3161 timestamp token over the decision record's hash, requested by the intake or carried
  by the approver (decision 2). It gives a time that does not depend on the log signer, and the
  token is archived with the decision.

**Live gate versus offline audit.** Both run one function, `TallyApprovals` extended to take an
**identity environment**: the trust sets, a status snapshot (CRL and OCSP bytes with their fetch
times), and a function giving `A(d)` for a record. The inputs differ:

- Live: `A(d)` is, in order, the covering head's time if the journal already holds one, else the
  intake's own verification time `t_verify` when the intake wrote it (the intake verified the chain
  and fresh status itself at `t_verify`, inside the application's trusted process), else
  "unknown", which means the decision does not count yet and the gate keeps waiting (and
  re-evaluates when a head covers it). The status snapshot is the gate's CRL cache, fresh within
  `MaxStatusAge`.
- Offline: `A(d)` comes from the evidence (the covering head and the proof that it covers the
  record); the status snapshot is the archive in the evidence kit.

Because the function is the same, the gate cannot count what the auditor would refuse for a reason
the auditor can see. The terminal tally records a digest of the identity environment it used, so a
difference between the gate's snapshot and the auditor's is reported rather than silently
different.

**What `bide audit` can prove offline, years later**, given the evidence kit and an auditor-held
trust anchor:

- the leaf certificate chained to the auditor's anchor, with the constraints, EKU and SAN rule,
  **at `A(d)`**, using `Certificate.Verify(CurrentTime: A(d))`;
- the decision's signature verifies under that leaf, over bytes that include the leaf hash;
- the decision existed by `A(d)`: the inclusion proof of the record under the first covering head,
  and that head's signature under the log key;
- no listed revocation with `E <= A(d)` in a CRL issued at or after `A(d) + W`, signed by the issuer
  chaining to the anchor; or an OCSP response with `thisUpdate >= A(d) + W` saying good. Both are in
  the kit.

What it cannot prove, and says so in the verdict: the real moment the person signed (only that it
was no later than `A(d)`); that nothing was revoked after the status data's own `thisUpdate`; that
the CA, the CRL publisher and the IdP behaved correctly; presence of the person; anything about a decision
whose covering head is missing.

**The verdict tiers.** `verified (status as of S)` when status evidence at or after `A(d) + W` is in
the kit; `provisional` when the chain and head check out but the status evidence is earlier than
`A(d) + W` (not an error: the decision may be final once a later CRL is archived); `not verified`
otherwise, with the reason (chain, SAN, EKU, expired, revoked at `E`, no covering head, status
missing, status predates the window).

**Why a window `W`.** A CRL issued just after `A(d)` cannot list a compromise the CA learns about
later and dates earlier (an invalidity date before `A(d)`). `W` is how long the bank allows for
compromises to be reported and dated back. The auditor insists on status issued at least `W` after
the decision. A bank that sets `W = 0` accepts that a later-reported compromise dated earlier is not
caught by the archive; it can still be caught if the auditor fetches the CA's current CRL history.

**CRLs drop expired entries.** A CA may remove a revoked certificate from later CRLs once it has
expired (unless the `expiredCertsOnCRL` extension is set). A CRL fetched years later is therefore not
a substitute for the one archived at the time. The kit archives the CRL issued in `[A + W, A + W +
Delta]` for each CA in the chain, at decision time, and the verifier uses only archived status data
whose `thisUpdate` falls in that range or later.

## Revocation

- **CRL** is required and enough. Parsed with `x509.ParseRevocationList`; the CRL signature is
  checked against the issuing CA certificate of the chain (`CheckSignatureFrom`, which fails for
  any other CA); entries carry serial, revocation time and reason code as fields, and the invalidity
  date extension (OID 2.5.29.24, a GeneralizedTime) is visible only as a raw entry extension that
  bide parses. `Certificate.Verify` never consults a CRL, so every revocation check is bide's. The
  delta CRL indicator and the critical flag are visible in `RevocationList.Extensions`, so refusing
  delta CRLs is possible. Unrecognized critical extensions,
  **delta CRLs** (OID 2.5.29.27) and **partitioned CRLs** (an issuing distribution point that does
  not cover the certificate) are refused in the first version: "status unknown", never "good".
- **OCSP** is optional, in a separate module so the core does not import
  `golang.org/x/crypto/ocsp`. An OCSP response is archived whole and verified like a CRL (signed by
  the CA or a delegated responder with the OCSP-signing EKU, `thisUpdate`, `nextUpdate`). `unknown`
  and any parse failure are "status unknown". OCSP gives fresher status to the live gate and the
  intake; the offline kit may hold either.
- **Fail closed.** No status for a certificate that requires it, a stale one, a signature that does
  not verify, a CRL that does not cover the certificate: the decision does not count. For the live
  gate this is a **pause**, not a deny: the gate stays waiting, the run's needs-attention state
  names "approver identity status unavailable", and the notifier says so (decision 7). A deny is
  reserved for cases where the gate can already never pass.
- **Freshness and caching.** The live process holds a CRL cache directory (or in-memory cache) fed by
  a refresher that fetches from the certificate's CRL distribution point, verifies, and replaces
  atomically. A CRL is usable while `now < nextUpdate` and `now - thisUpdate <= MaxStatusAge`
  (policy; default 24h). The refresher runs at half of that. A failed refresh keeps the previous CRL
  until it goes stale, and raises an operator alarm (an `OnStatusError` hook, like
  `AuditedStore.OnError`).
- **A revocation after a decision.** Under the rule, a revocation with `E > A(d)` does not change
  that decision: the approver was authorized when it became provable. The gate that already
  recorded a terminal tally keeps it (terminal tallies are never recounted, as today). A gate still
  waiting counts the decision at its next evaluation under the new snapshot, which includes the
  revocation; since `E > A(d)`, the decision still counts. If the bank wants a leaver's earlier
  approvals on a waiting gate dropped, that is a separate, explicit operation (re-ask the gate), not
  an implicit property of revocation. A revocation with `E <= A(d)` (a key compromised, an
  invalidity date set earlier) removes the decision from the count and, for an already
  recorded terminal tally, makes the audit report the tally as resting on a revoked decision (the
  call may have run; the audit says so, loudly, which is its job).

## Rotation, renewal and continuity of identity

Certificates are short-lived (hours to a day, the bank's choice). The approver's key may be:

- ephemeral: a fresh key per certificate (smallstep, Vault PKI after SSO), held in a 0600 key file
  (`bide keygen` already writes one) for the certificate's life; or
- long-lived in an HSM or KMS (bide-hsm), with the certificate renewed for the same key.

Continuity of identity across certificates is the approver id plus the identity rule, not the key.
Consequences:

- A new certificate for the same SAN is the same approver. Decisions under different certificates
  by the same approver id still obey "an approver's first valid decision counts".
- The seat is the **identity**. The static policy check (`ValidateKeys`, one seat per key) cannot
  see a key that is not known until a decision arrives, so a certificate-bound verifier reports
  `KeyIDs() = ["ident:" + canonical SAN]` and two approver ids that resolve to one SAN are refused
  with `ErrConfig`, as two ids on one key are today. A **dynamic** check closes the rest: two counted
  decisions by different approver ids whose leaf public keys hash equal do not both count
  (`ReasonSharedKey`), so a CA that issued one key under two SANs cannot give one holder two seats.
- Renewal in flight: a decision signed under certificate C1 counts under C1's rules at `A(d)` even if
  C1 has expired by the time the run resumes. The live gate does not demand the certificate be
  valid now, only at `A(d)` (rule 1, 3).
- Rotation of the **trust anchor** (a new root): the old root stays in the gate's trust set for
  decisions made under it; the offline kit holds both, and the tally records which anchor each
  counted decision chained to.

## Interplay with what exists

**k-of-n and one seat per key.** Unchanged for bare-key approvers. For certificate-bound approvers
the seat is the identity (above), plus the dynamic key check. `ApproverVerifier.KeyIDs` keeps its
contract (a list of identities, compared as exact strings); the new `ident:` entries are one more
kind, and the existing guard that refuses two approvers sharing an entry applies unchanged.

**`ExcludePrincipal` and `run:principal`.** Today the requester recorded in `run:principal` is
compared with approver ids under normalization. With certificates the check also compares the
requester principal with the counted decision's **verified SAN**. This closes "the requester
approves under a second approver id": the second id maps to the same person's SAN or a different one,
and either way a requester email equal to a counted SAN never counts (`ReasonRequester`). The
principal format a bank records should be the same email or URI the certificates carry (a guide
item). When the principal is not an email or URI, the existing id comparison is all there is; the
verdict says "identity match not possible".

**`EvaluateGate` and the intake.** `EvaluateGate(recs, subject, policy, resolver)` gains the
identity environment by way of the resolver (the environment is part of what the resolver returns),
so its signature stays; its `GateStanding` gains per-approver reasons (below). The approvals intake
takes the certificate chain in the request (`cert`, base64 DER chain, leaf first), verifies it with
the **same** verifier before writing (`WithDecisionCheck`), records it in the decision record, and
stamps `t_verify`. A status failure returns 503 `status_unavailable` (fail closed, retry), not 422.
`GET /pending` shows the gate's identity rule per approver (expected SAN) so a client can check its
own certificate before signing.

**`bide approve`.** `-cert chain.pem` names the leaf-first chain; `-key` is the key or an HSM
reference. `bide approve` checks before signing: the chain verifies at "now" against `-trust-anchors`
(optional; when absent it asks the intake/journal for the gate's identity rule from `GET /pending`
or the recorded policy, but never trusts the endpoint's anchors), the leaf key equals the signing
key, the SAN matches the `-as` id's identity rule, and the certificate has at least a minimum time
left (default 60s). The signature includes the leaf hash. The receipt gains the chain.

**bide-hsm.** The certificate sits next to the HSM or KMS key: a PEM chain file, or a PKCS#11
certificate object on the token (matched by `CKA_ID`). `bide-hsm` refuses to open a key whose public
key does not equal the certificate's (a mismatch would sign decisions that never verify). The
signer interface does not change: the `audit.Signer` signs the v4 decision bytes (below) and
the chain travels with the decision. For a KMS key the bank's CA issues the certificate against the
KMS public key (the CSR proof of possession is signed by the KMS key).

**Decision receipts.** A receipt carries the certificate chain and, once a head covers the decision,
the anchoring proof (`audit.AnchorReceipt`, unchanged: it already picks the smallest covering head,
which is `A(d)`). `Verify` for an identity receipt also checks chain and SAN against trust anchors the
caller supplies. The receipt's "approver's claim" fields are as before.

**Evidence v6.** The identity data is **not** put into the evidence package. Before 1.0 a verifier
reads only the current format of each artifact (see the audit guide), so a v7 package would orphan
every v6 package. Instead an **identity bundle** (`bide.audit.identity.v1`) accompanies the package:
per counted decision, the chain, the first covering head with its inclusion proof, and the status
data. `bide audit verify-approvals` takes `-identity bundle.json -trust-anchors roots.pem` and
produces the tiered verdict. A package with bare-key approvers verifies exactly as today (decision 5).

## Journal and format changes

**The decision record** (`ApproverSignature`) gains `approver_cert` (the leaf-first DER chain,
base64 in JSON) beside `approver_alg`, `signature`, `decided_at`, `recorded_at`. It is additive and
optional. The chain is stored, not just its hash, so the journal is self-contained and its Merkle
trail covers it. A chain is about 1 to 4 KB; the cost is one such record per decision.

**The signed decision bytes** get a new version tag, `bide.approval.v4`, **only for
certificate-bound decisions**. Layout = v3 plus `len32(SHA-256(leaf DER)) leaf-hash` before the
decision time. Reasons:

- the signature is only valid in the context of the certificate, so a valid certificate for the same
  key but another identity, or a swapped-in certificate, cannot be attached to someone else's
  signature;
- an old binary that does not know v4 cannot verify it, so it does not count it: no downgrade where
  an old process counts a certificate-bound approver's decision as a bare-key decision, skipping the
  identity checks;
- bare-key decisions stay `v3`, unchanged, so existing policies, journals, receipts and evidence
  keep verifying.

A certificate-bound approver's verifier accepts `v4` only; a `v3` signature by that approver is not
counted (`ReasonNoCert`). A bare-key approver's verifier accepts `v3` only. The gate decides by the
resolver's kind for the approver id, never by what the record contains.

**Reasons** (additions to the `Reason*` set, each tested): `ReasonNoCert`, `ReasonCertChain`,
`ReasonCertIdentity`, `ReasonCertUsage` (EKU or key usage), `ReasonCertWindow` (decided_at or `A(d)`
outside validity), `ReasonRevoked`, `ReasonStatusUnknown`, `ReasonStatusStale`, `ReasonNoCoveringHead`
(the live "not provable yet").

**The decision step name** is unchanged (identity-keyed, from #288: call, approver, verdict, scheme,
decision time). A re-signing under another certificate at the same `decided_at` is the same
decision by name and the check picks the valid one, as for a randomized scheme today.

**The terminal tally** gains `identity_env` (a digest of the trust sets and the status snapshot the
count used) and per-counted-decision `anchor` (the trust anchor's key id the chain ended at). The
tally record stays one record; the fields are additive and omitted for bare-key gates.

**Backward compatibility.** Journals without `approver_cert` read and verify as before. A policy
whose resolver returns only bare-key verifiers behaves exactly as today. A mixed fleet is safe in the
direction that matters: an old binary cannot count a `v4` decision. It would also not know a gate was
identity-bound; deploying identity enforcement requires every process that evaluates the gate to run
the new version, and the guide says to roll the fleet before enabling a certificate-bound approver
(the first v4 decision is the first thing an old process refuses to count, which pauses the gate
safely).

## Formal model impact (model 1b)

The approval-gate model checks the counting rule over signed, possibly invalid records. It needs the
following, as a new configuration set and with the existing configurations unchanged:

State: a set of certificates, each with a holder (person), an identity (SAN), a validity interval
`[nb, na]`, and a revocation time `E` (or none); the CA's constraint set (identities it may issue);
trust anchors per gate; a status snapshot (which serials are listed, as of which time); a clock;
decisions carry a certificate and a bound `A`.

Actions: `IssueCert(person, identity)` (constrained by the CA's permitted set, plus an adversarial
`MisIssue` that ignores it, to show what name constraints buy), `Revoke(cert, E)`, `CRLPublish`
(the snapshot advances), `WithholdCRL` (the verifier's snapshot lags), `ClockTick`, `SubmitCert`
(a decision with a certificate and a stolen or genuine key), `Backdate` (a submission whose
`decided_at` is earlier than its true time, with `A` set by the covering head).

Properties that must hold (invariants unless noted):

- **IdentityBound**: a counted decision's certificate chains to a trust anchor of the gate, with the
  approver's SAN, valid at `A(d)`.
- **RevocationSound**: a counted decision's certificate has no `E <= A(d)` in the snapshot the count
  used. With `WithholdCRL` enabled and no freshness rule the property must **fail** (a documented
  counterexample), and hold once staleness makes the status "unknown".
- **FailClosed**: unknown or stale status never counts.
- **NoBackdate**: a decision signed after `E` and first covered after `E` never counts, however its
  `decided_at` reads.
- **SeatPerPerson**: counted decisions come from distinct people (the holders), also when two
  approver ids map to one SAN or one key sits under two certificates. With `MisIssue` and no name
  constraints this fails; with them it holds.
- **TerminalStable**: a terminal tally is not changed by a later revocation or snapshot (the existing
  property, extended).
- **NoStuckPause**: a gate with enough genuinely valid decisions and fresh status is not stuck.

A Go refinement test runs the same scenarios through `TallyApprovals` with the stdlib CA (below) and
compares the counted set with the model's, trace by trace, as the existing models do. The change to
`spec/tla` and its README map is part of the build, and `modelsync` gates the PR.

## Operations for a bank

**Setup.**

1. The CA issues approver certificates after SSO with MFA: SAN email, the approver EKU, a short
   lifetime, name constraints on the issuing CA, a CRL distribution point.
2. The platform team writes the gate's `IdentityPolicy`: trust anchors for this gate, the identity
   map (approver id to email), EKU, `MaxStatusAge`, `W`, `grace`. It lives with the code and the
   flow config, reviewed like a policy. `bide check` learns `assert every approver of gate X is
   identity-bound`, and `-diff` flags a gate loosened from identity-bound to bare keys.
3. Approvers run `bide approve -cert`, or the browser page once it can hold a certificate (out of
   scope here), against the intake.
4. Run the **observe mode** first (migration below).

**CRL distribution.** A `bide identity` command group (core, stdlib `net/http` only, no new
module):

- `bide identity fetch-crl -cache dir -policy file` fetches, verifies and stores CRLs atomically,
  and keeps a dated archive (`crl/<issuer-key-id>/<thisUpdate>.crl`) for the evidence kit;
- `bide identity check -cert chain.pem -policy file` tells an approver why their certificate would
  not count (chain, SAN, EKU, window, status), the same function the gate uses;
- `bide identity kit -evidence pkg.json -out dir` assembles the identity bundle for a package.

The application embeds the same refresher (`identity.Refresher`) when it does not run a cron.

**The offline auditor's kit** is a directory: the evidence package, the identity bundle (chains,
covering-head proofs, archived CRLs/OCSP responses), and a README listing what the auditor must
bring: the trust anchors, from the bank's PKI team over a channel not shared with the producer, and
the log public key. `bide audit verify-approvals -identity ... -trust-anchors ...` needs nothing
else and no network. Keep the archive for the retention period, including the CRLs: the archived CRL
is the evidence, because a CA may later drop expired entries.

**Stale CRL in production.** The refresher alarms before staleness (at 75% of `MaxStatusAge`). At
staleness gates pause with a named needs-attention reason; the notifier's `needs-attention`
notification says which CA and since when. There is **no break-glass in the core**: weakening the
policy is a deploy of a changed `IdentityPolicy`, which is a visible, reviewable diff. A bank that
needs an emergency path defines it in its change process, not in bide.

**Migration from bare keys.** (1) Deploy the new version everywhere (the fleet rule above). (2)
**Observe**: configure the identity policy in `Observe` mode: the gate counts bare keys as before and
logs, for each decision, what the identity verdict would have been. (3) Issue certificates to
approvers; fix SAN mismatches the observe log shows. (4) **Enforce per approver**: switch an approver
id from a bare key to an identity rule; an approver is one or the other, never both, so there is no
fallback to bypass. (5) Decisions recorded under the old bare key by an approver who has been
switched do not count after the switch (a waiting gate needs them re-signed). The guide says to switch
approvers when no gate is waiting on them, or to accept the re-sign.

## Alternatives considered and rejected

- **Verify OIDC tokens directly (the bank's SSO as the identity source).** Rejected. The costs:
  JWT/JWS and JWKS handling outside the standard library (a new dependency in the verification
  path, and a larger FIPS and audit surface); the live gate and the verifier need network access to
  the IdP's JWKS at verification time, and an offline auditor years later needs the archived key
  history of the IdP; a bearer token proves a login, not that this person signed this exact call
  (it needs a holder-of-key binding such as DPoP, or a signature on top, which is the certificate
  design again); token lifetimes and claims are IdP-specific, so the identity rule is a per-IdP
  parser; revocation is IdP session state with no standard offline-checkable list; and bide would
  speak OIDC, which the user ruled out. Certificates issued by the bank's PKI from its SSO keep the
  SSO on the bank's side and give bide a signature to check.
- **Check the certificate once at enrollment.** Rejected: it gives enrollment-time binding and no
  per-decision validity or revocation, which is what the audit needs.
- **Subject CN as the identity.** Rejected: free text, and `crypto/x509` does not enforce
  directory-name constraints.
- **Trust `decided_at` for validity.** Rejected: it is signed by the party being checked.
- **Trust `recorded_at`.** Rejected: unsigned, set by the submitter's clock.
- **Put identity data in evidence v7.** Rejected for now (decision 5): it would orphan v6.
- **A global revocation service bide runs.** Rejected: a new always-on component in the trust path.
  The bank's CA already publishes CRLs.
- **WebAuthn attestation as identity.** Not an identity system; it is a separate roadmap item
  (a new signature scheme) and composes with this one later, as the signing key under a certificate.

## Test plan

A **test CA** helper in the test packages, standard library only: a root and an intermediate
(`x509.CreateCertificate`), name constraints, leaf certificates with chosen SAN, EKU, validity and
key (ECDSA P-256 and Ed25519), and CRLs (`x509.CreateRevocationList`) with chosen entries,
revocation times, reason codes and an invalidity date extension (added as a raw extension). It runs
in-process, deterministic with injected clocks. Each scenario is a test of `TallyApprovals` (live
environment) and of `bide audit verify-approvals` (offline, from a kit), asserting the same counted
set and the same reason:

1. valid chain, matching SAN, in window, good status: counts.
2. expired at `A(d)`; not yet valid at `decided_at`; `decided_at` in the future of `A(d)`.
3. wrong SAN; SAN under a different approver id; two approver ids on one SAN (`ErrConfig`).
4. name-constraint violation (issuing CA permits only `@bank.example`, leaf `@other.example`).
5. wrong EKU; key usage without digital signature; leaf marked CA.
6. unknown CA; right CA but not this gate's anchors; chain longer than `MaxChain`.
7. revoked before the decision (`E <= A`): does not count. Revoked after (`E > A`): counts.
8. revoked with an invalidity date earlier than `A`: does not count. `keyCompromise` with no
   invalidity date: retroactive to `notBefore`.
9. backdated `decided_at` after revocation, with a late covering head: does not count.
10. CRL stale (`nextUpdate` passed; `thisUpdate` older than `MaxStatusAge`): live gate pauses with
    `ReasonStatusStale`; no deny. CRL signed by the wrong key; issued by another CA; delta CRL;
    partitioned CRL; critical unknown extension: all "status unknown".
11. withheld CRL: the offline kit has only a CRL with `thisUpdate < A + W`: verdict `provisional`,
    never `verified`. A CRL archived after `A + W` listing the revocation: `not verified`.
12. a CRL that later dropped the expired revoked certificate: verification uses the archived one and
    still rejects.
13. downgrade: a `v3` signature from a certificate-bound approver does not count; a `v4` decision
    from a bare-key approver does not count.
14. certificate substitution: the same signature with another valid certificate for the same key.
15. one key under two certificates and two SANs: only one seat counts (`ReasonSharedKey`).
16. `ExcludePrincipal`: the requester's email equals a counted SAN under another approver id: not
    counted.
17. rotation: decisions under C1 and C2 for one approver id: the first valid counts; C1 expired when
    the run resumes: still counts at `A(d)`.
18. receipts: chain carried; `AnchorReceipt` bundle is the covering head; a stripped chain does not
    verify as identity.
19. intake: chain verified before write; status failure returns 503 and writes nothing; `GET
    /pending` shows the identity rule.
20. bide-hsm: certificate/key mismatch refused at open (software fake token; the real-HSM tests stay
    where they live).
21. `bide identity fetch-crl` against a local `httptest` server: atomic replace, stale alarm, archive.
22. migration: observe mode logs would-be verdicts and counts bare; per-approver enforce; switched
    approver's old bare decision does not count.
23. a stdlib `x509` behavior check, as a test, for each assumption this note relies on. **They exist
    in this PR**, in `internal/identitycheck/x509_assumptions_test.go`, run on Go 1.27.0, and fail
    naming the assumption if a release changes it:
    `TestEmailNameConstraintsAreEnforced` (permitted and excluded email subtrees, including that a
    permitted `bank.example` also permits subdomains), `TestURISANConstraints` (a URI host is
    constrained, a host-less URI is refused), `TestDirectoryNameConstraintsAreNotEnforced`
    (non-critical ignored, critical refused as unhandled), `TestVerifyAtAPastTime`
    (`CurrentTime` honoured on both sides of the window), `TestPrivateEKUMustBeCheckedByHand`
    (the default requires ServerAuth; a private OID needs `ExtKeyUsageAny` plus a by-hand check),
    `TestUnknownCriticalExtensionIsRefused`, `TestNonCAIntermediateIsRefused`,
    `TestPathLenIsEnforced`, `TestRevocationListParsing` (entries, reason, times, signature check,
    raw invalidity date and delta indicator, Verify ignores revocation), `TestCRLIssuerNeedsCRLSign`,
    `TestEd25519Leaf`, `TestEmailSANIsReturnedAsIssued` (normalization is the verifier's job). The
    build keeps them as the assumption checks beside the identity verifier's own tests.
24. fuzzing the certificate and CRL parsers' wrappers (no panic, no accept of malformed input), and
    mutation checks on the counting rule's identity clauses.
25. model 1b: the configurations above, including the two expected counterexamples (`WithholdCRL`
    without freshness; `MisIssue` without name constraints), and the Go refinement test.

## Build plan (after the decisions)

1. Certificate verification, identity policy, `v4` decision bytes, new reasons, `TallyApprovals` and
   `EvaluateGate` with a static status snapshot; test CA; model 1b configurations.
2. CRL refresher, `bide identity`, live gate pause behavior, intake `cert` and 503, `bide approve
   -cert`, bide-hsm certificate pairing.
3. Identity bundle, covering-head proofs in the kit, `bide audit verify-approvals -identity`, tiered
   verdicts, receipts with chains.
4. OCSP module (optional), RFC 3161 token carriage (if decision 2 says so).

## Decisions for the user

1. **Identity field.** SAN **rfc822Name (email)** as the primary human identity, with **URI SAN with
   a host** as the alternative; never the Subject DN. *Recommend: yes.*
2. **Time bound `A(d)`.** The first covering signed tree head by default, with an optional RFC 3161
   token later for a time independent of the log signer. *Recommend: heads now; the token as phase 4
   if the bank's assessor asks; and treat routing approvals through the intake as the supported path
   (a direct `bide approve` journal write has a late `A`).*
3. **Compromise window `W` and `grace`.** Per-policy values with defaults `W = 24h`, `grace = 0`,
   and the tiered `verified` / `provisional` verdict. *Recommend: yes; the bank sets `W` from its
   incident process.*
4. **Retroactive revocation.** A decision stands iff `A(d) < E`, where `E` is the invalidity date,
   else the revocation time, and `keyCompromise` without an invalidity date means `E = notBefore`.
   *Recommend: yes.*
5. **Evidence shape.** A separate **identity bundle** artifact, not evidence v7. *Recommend: separate
   bundle (v6 packages and bare-key verification untouched).*
6. **Decision format.** `bide.approval.v4` for certificate-bound decisions only, with the leaf hash
   signed; bare-key stays `v3`. *Recommend: yes (closes certificate substitution and the downgrade
   to an old binary).*
7. **Stale or missing status.** The live gate **pauses** with a named needs-attention reason and the
   intake returns 503; no deny, no break-glass in the core. *Recommend: yes.*
8. **Revocation sources.** Full CRLs in the core; delta and partitioned CRLs refused as "status
   unknown"; OCSP in an optional module only. *Recommend: yes; revisit delta CRLs if a bank's CA
   issues only those.*
9. **Seats.** The seat is the identity (`ident:` entries in `KeyIDs`), plus the dynamic
   same-leaf-key check. *Recommend: yes.*
10. **`ExcludePrincipal`.** Also compare the requester principal with the verified SAN of a counted
    decision. *Recommend: yes; the guide tells banks to record the principal as the same email or URI
    the certificate carries.*
11. **Migration.** Observe mode, then per-approver enforcement, an approver being bare-key **or**
    certificate-bound, never both. *Recommend: yes.*
12. **Trust anchors.** Per gate or policy, never the system roots. *Recommend: yes.*
13. **Required EKU.** A bank-assigned private OID by default, with `id-kp-emailProtection`
    accepted as a configured alternative. *Recommend: yes.*
14. **Command name.** `bide identity` (`fetch-crl`, `check`, `kit`). *Recommend: yes.*
15. **Staging.** Build in the four steps above, each its own reviewed PR, the first behind the model
    1b configurations. *Recommend: yes; none of it is released before the model and the adversarial
    review of step 1.*
