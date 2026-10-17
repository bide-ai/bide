# Operating certificate-bound approvers

An approver of an m-of-n gate can be a person with a certificate from the bank's own CA instead of a bare
key. The decision then carries the certificate chain, and the gate counts it only when the chain is
accepted for that person, the revocation status allows it, and the status is still current when the call
is about to run. This guide is for the people who run the CA and the application: what to declare from
your CA, what to do when you revoke, what the gate does before a call runs, and the numbers to publish. The
design and its reasons are in the [approver identity design](../design/approver-identity.md); the
gate's records and re-asks are in the [human approval guide](hitl-approval.md#a-gate-with-a-certificate-bound-approver-its-tally-its-holds-and-its-re-asks).

What this gives, and what it does not. A counted decision is tied to a person your CA identified when it
issued the certificate, and an audit can recompute the verdict from the journal, the certificates and the
CRLs. bide cannot see how your CA identified the person, whether the key is in hardware, or whether the CRL
it is given is the CA's newest. Those are your CA's controls; bide requires that the certificate says so
(`IdentityPolicy.Policy`, the certificate policy OID) and records what it relied on. Nothing here is a
compliance certification.

## Before go-live: capture your own CA

The status rule is checked against real CAs, not only against the CRLs bide's tests build. The repository
has a manually dispatched workflow, `crl-fixtures`, that captures a two-tier PKI, its certificates and its
CRLs (delta CRLs included) from EJBCA Community and from Microsoft AD CS, and tests that run the rule on
the captures. **Pilot checklist item: capture the bank's own enterprise CA, with its certificate templates,
with that workflow, and run the conformance tests on the capture before any gate uses the CA.** The
captures show what that CA stamps (the back-dating of `thisUpdate` and `notBefore`, the real CRL period,
the overlap, how a revoked certificate is dated) and the values below are declared from them, never assumed.
Before a gate goes live, run [`bide identity check`](#pre-flight-bide-identity-check) over the same files.

## Walkthrough: from the CA's certificate to an approved call

This is the path of one approver, `ops`, on a gate whose policy names `ops` as certificate-bound. Four
people are involved: the **CA operator** (issues certificates, one identifier per person, with the bank's
EKU and policy OID), the **platform team** (writes the certificate policy and runs the application), the
**approver** (holds a key and a certificate), and the **application** (runs the gate).

1. **The platform team writes the certificate policy file**, the same file the application's
   `audit.IdentityPolicy` is built from and `bide approve` reads with `-identity-policy`: the trust anchors
   (PEM files next to it), the pinned intermediates, the approver-to-identity map (one identifier per
   approver, `ops` included) and the EKU and policy OID the CA asserts. Its format is in
   [Human approval](hitl-approval.md#deciding-from-the-terminal). The application's side (an
   `audit.CertApprover` for `ops`, with the CA's facts declared as above) is Go configuration: see the
   sections below.
2. **The approver makes a key and a certificate request.** A key file: `bide keygen -out ops.key -as ops
   -csr ops.csr.pem` (Ed25519, or `-alg ecdsa-p256`; both have a certificate scheme. ML-DSA-65 does not
   get a request from bide, and a hybrid has no certificate scheme). A key in an HSM or a KMS: `bide-hsm csr`
   makes the request, signed by the key where it is held, see [the key and the request](#the-key-and-the-certificate-request).
   The private key never leaves its file, token or KMS.
3. **The CA issues a certificate for that public key**, after the bank's own identification of the person
   (single sign-on with step-up, say), with one subject alternative name that is the person's identifier,
   the bank's EKU and the bank's policy OID. The request names nothing, so the CA or the identity provider
   sets all of that; bide asks the CA for nothing, and it cannot know how the person was identified.

4. **The approver assembles the chain**: a PEM file with the leaf first, then the intermediates up to but
   not including the trust anchor (the policy holds the anchor), at most 8 certificates:
   `cat ops-leaf.pem issuing-ca.pem > chain.pem`. Only CERTIFICATE blocks: a file with a private key block is
   refused unread.
5. **The approver decides.** The command shows the call, then the certificate it will use and the identity
   the policy derives from it, and asks for confirmation; answering no records nothing, which is also how to
   try a certificate:

   ```
   bide approve -db app.db -run refund-1049 -call c2 -as ops -key ops.key \
       -cert chain.pem -identity-policy policy.json
   ```

   Through the application's approval intake instead of a database, add `-to https://approvals.bank.example`
   (and `-tls-ca`, `-client-cert`, `-token-env` as for any `-to`); with a key in an HSM or KMS, `bide-hsm
   approve` takes the same flags with `-key` a key reference. Before it signs anything the command refuses
   if: the file is not a clean chain; the key is not the leaf's key (the keys themselves are compared, not
   only a signature); the certificate is not valid now or has less than `-cert-min-left` (60 seconds) left;
   or the policy does not accept the chain for `ops` (wrong CA, wrong EKU or policy, a name that is not
   `ops`'s identifier). The HSM or KMS is opened only after the file, the policy and the validity pass. There
   is no fallback: a failure never produces a bare-key decision. `-unverified` signs without the policy
   check and says so loudly; the gate will still refuse a chain it does not accept, and the decision is then
   recorded and never counts. The command tells a certificate-bound approver from a bare-key one by the gate's
   pause note in the journal: if that note was written by a process that does not have the certificate-bound
   approvers configured, the gate reads as bare, and a bare decision for a bound approver would be recorded and
   never counted (the gate itself stays safe), so run the application with the same approver configuration on
   every process that drives the gate.
6. **The decision is recorded** with the chain, signed over the `bide.approval.v4` bytes, which bind the
   SHA-256 of the leaf, and a receipt is written (with the chain). `bide approve` does not say that the gate
   counted it, and cannot: counting needs a signed tree head over the decision (the application's audited
   store makes one with the write; the gate anchors before it counts) and current revocation status. Through
   the intake, status the application cannot judge yet answers HTTP 503 and records nothing: run the same
   command again.
7. **The gate counts and the call runs.** When enough approvers have decided, the application re-runs the run
   (or `RecoverLoop` does): the gate counts the decision, and immediately before the call runs the status of
   the counted certificates is checked again (below). The run viewer and `bide approve -list` show the gate's
   phase and each counted decision's identity, tier and status time.
   For a pending approver who is certificate-bound, both print the `bide approve -cert` command with
   placeholders for the key, the chain and the policy file: `-list` for each such approver, and the Needs
   attention list once, as the only command when every pending approver is certificate-bound, or next to the
   bare-key command (marked for the bare-key approvers only) when some are not. A bare-key command is never
   offered for them, because their decision would be recorded and not counted.
8. **Anyone can check the receipt later**: `bide approve -verify-receipt ops.receipt.json -identity-policy
   policy.json -log-key <log public key>` checks the chain and identity at the time the log's head made the
   decision provable (and, as it says, not revocation status); `bide audit verify-receipts` checks it against
   the journal and its signed heads.

When it refuses, the message says why. The common ones: "is not the key of the certificate's leaf" (the key
file is not the one the certificate was issued for); "the certificate has expired" or "less than the 1m0s a
decision needs left" (ask the CA for a new certificate); "not accepted for ops by the policy" with the
reason (the chain does not end at a pinned anchor, the identifier is not the one the policy maps to `ops`,
EKU or policy OID missing); "is a certificate-bound approver of this gate" (give `-cert`) or "is a bare-key
approver of this gate" (drop it). The approve page in the browser cannot hold a certificate: it tells a
certificate-bound approver to use `bide approve -to ... -cert`.

## The key and the certificate request

The request proves the approver holds the key. By default it names nothing: a subject-less PKCS#10 request with no
extensions, so the CA or identity provider decides the name (the identifier in the SAN), the EKU and the
policy OID. A CA that insists on a subject in the request is a CA setting (most enterprise CAs let a
template or the issuing application override it); bide puts a subject there only when asked, with `-cn <name>`
(`bide keygen -csr` and `bide-hsm csr`): one common name of printable ASCII text, at most 64 characters. The gate
reads no subject, so `-cn` changes nothing about who may approve; it exists for tools such as `step ca sign`, which
refuse a request with an empty subject and ignore the name when the CA's template builds the certificate.
`bide-hsm csr` can also ask for a SAN and an EKU (below), which a CA may honor or replace.

**A key file.** `bide keygen -out ops.key -as ops -csr ops.csr.pem` writes the key (mode 0600, never
printed), `ops.key.pub`, and the request. It refuses to overwrite any of the three files and writes
nothing when it refuses, and with `-alg ml-dsa-65` or the hybrid it refuses `-csr`. Check the request
before sending it: `openssl req -in ops.csr.pem -noout -verify -text` prints `verify OK`, an empty
subject, and the public key bide printed.

**A key in an HSM or a KMS: `bide-hsm csr`.** It signs the request with the key where it is held, through the same
key references `bide-hsm approve` takes, so no vendor tooling is needed and the key never leaves the token or the KMS.
The PIN or the credentials come from where they do for `bide-hsm approve` (a file, the environment or a prompt; never
the command line).

```
bide-hsm csr -key 'pkcs11:token=bide-approvers;object=approver-p256?module-path=/usr/lib/softhsm/libsofthsm2.so&pin-source=file:/run/secrets/hsm-pin' \
    -out ops.csr.pem
bide-hsm csr -key 'awskms:arn:aws:kms:eu-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab' -out ops.csr.pem
```

The request goes to the file named by `-out`, or to standard output without it, and nothing else goes to standard
output. An existing file is not overwritten unless you give `-force` (which replaces only a regular file, never a
directory or a symbolic link). A KMS key signs once for the request, billed as one signing request, beside the one
the key's self-test makes when it is opened. By default the request names nothing, as for a key file. The key and its
scheme per source:

| Source | ECDSA P-256 (ecdsa-with-SHA256) | Ed25519 |
|---|---|---|
| PKCS#11 | yes | yes, if the token has `CKM_EDDSA` |
| AWS KMS | yes (a digest is signed) | yes |
| Google Cloud KMS | yes | only a SOFTWARE key version, with `-allow-software-key` |
| Azure Key Vault | yes | no (Key Vault has no Ed25519) |

There is no RSA request: bide-hsm keys, and the certificates the gate accepts, are Ed25519 or ECDSA P-256. Before it
writes anything, `bide-hsm csr` parses the request it built, checks its signature, checks that its public key is the
one the key source reports (so a token that signs with another key, or a KMS key replaced behind the reference, writes
no file), and checks that it carries exactly the names asked for.

*Asking for names.* The CA decides what the certificate says, and many ignore or replace what a request asks for; the
certificate it issues is what `bide approve -cert` checks. A request can still ask, with `-san-email <address>` or
`-san-uri <uri>` (one subject alternative name, never both) and `-eku clientAuth,emailProtection` (names or dotted
OIDs). To ask for exactly what the gate looks for, give the policy file `bide-hsm approve -identity-policy` takes and the
approver's id:

```
bide-hsm csr -key "$KEY" -policy policy.json -as ops -out ops.csr.pem
```

The request then asks for the policy's identifier for `ops` (an email address or a person URI, in the policy's
canonical form) as its one SAN, and for the policy's EKU; `-san-email`, `-san-uri` and `-eku` are refused beside
`-policy`. It cannot ask for the policy OID, which a CA asserts after the bank's own sign-on, so it prints on standard
error what the CA must issue: the one SAN, the EKU (and emailProtection if the policy accepts it), the policy OID and
the longest lifetime. It reads the policy file and checks no certificate.

Check the request before sending it, and the key it is for:

```
openssl req -in ops.csr.pem -noout -verify -text
bide-hsm pubkey -as ops -key "$KEY"
```

The first prints `verify OK`, and the public key in the second is the one in the request. The tests run `bide-hsm csr`
for each scheme on SoftHSM 2 (PKCS#11), an in-process AWS KMS fake (which implements the same digest-then-sign API as
AWS KMS; there is no AWS emulator test for this command), the Google Cloud KMS emulator and a Key Vault fake, and
have OpenSSL verify each request; it has not been run against a hardware HSM or a real cloud KMS.

**With the vendor's tooling.** If your CA or process requires a request made by the vendor's own tooling, the key can
be used that way too; check the result as above. The PKCS#11 procedures below were run on SoftHSM, not on each
vendor's product, and whether they work on your HSM depends on the mechanisms it supports; the KMS procedures were
not run.

*PKCS#11.* Have the token sign the request, so the key never leaves it. The PIN comes
from the environment or a file, never from the command line (a command line is visible to other users and to
process listings). With GnuTLS (`certtool` on Linux; Homebrew installs it as `gnutls-certtool`), which signs
with the single-part mechanism every token has and works for ECDSA P-256 and Ed25519:

```
: > empty.tmpl     # an empty template: no subject, no extensions
GNUTLS_PIN="$(cat /run/secrets/hsm-pin)" certtool --generate-request \
    --provider /usr/lib/softhsm/libsofthsm2.so \
    --load-privkey 'pkcs11:token=bide-approvers;object=approver-p256;type=private' \
    --load-pubkey  'pkcs11:token=bide-approvers;object=approver-p256;type=public' \
    --template empty.tmpl --outfile ops.csr.pem
```

(Use your HSM's PKCS#11 library for `--provider`.) Check that the request was made by the key `bide-hsm approve
-key` will sign with before sending it:

```
openssl req -in ops.csr.pem -noout -verify
bide-hsm pubkey -as ops -key 'pkcs11:token=bide-approvers;object=approver-p256?module-path=/usr/lib/softhsm/libsofthsm2.so'
```

The first prints `verify OK` (and `-text` shows the empty subject); the public key in the second must be the one
in the request (`openssl req -in ops.csr.pem -pubkey -noout | openssl pkey -pubin -outform DER`, whose last 65
bytes for P-256, or 32 for Ed25519, are the hex bide prints after `ecdsa-p256:` or `ed25519:`).
`bide-hsm approve -cert` checks the same before it signs, but only after the CA has issued a certificate, so check
here. What was run: SoftHSM 2.7.0 with GnuTLS 3.8.13, for one ECDSA P-256 key and one Ed25519 key
(`pkcs11-tool --keypairgen --key-type EC:secp256r1` and `EC:edwards25519`): both requests verified, had an empty
subject and carried the key `bide-hsm pubkey` printed.

OpenSSL's PKCS#11 provider ([pkcs11-provider](https://github.com/latchset/pkcs11-provider),
`openssl req -new -provider pkcs11 -provider default -key '<pkcs11 URI with pin-source=file:...>' -subj /
-out ops.csr.pem`) also worked for the Ed25519 key. It did not for P-256 on SoftHSM: the provider signs a request
with a multi-part digest-and-sign mechanism and SoftHSM 2.7.0 failed it with `CKR_OPERATION_NOT_INITIALIZED` (the
cause was not pursued), so use GnuTLS there. Whether either works on your HSM depends on the mechanisms it
supports; try it with a test key first.

*A KMS.* The request for a KMS key is made with the KMS vendor's documented procedure.
These procedures were not run for this guide (the repository's KMS emulators implement the key and signing calls,
not the vendors' request tooling), so none is reproduced here:

- AWS KMS has no operation that builds a certificate request, and no AWS procedure or PKCS#11 library for one
  was found when this guide was written. The request is the key's signature (the [Sign](https://docs.aws.amazon.com/kms/latest/APIReference/API_Sign.html)
  operation) over the request's bytes, which a tool you trust must assemble; third-party PKCS#11 libraries for
  KMS exist, and this guide neither tested nor endorses them.
- Google Cloud KMS: the
  [Cloud KMS PKCS #11 library with OpenSSL](https://cloud.google.com/kms/docs/reference/pkcs11-openssl) page has
  a "Create a new certificate signing request" section for a Cloud HSM signing key (`openssl req -new` with the
  library's engine). It shows a subject in the request; bide needs none, and the CA replaces it.
- Azure Key Vault and Managed HSM: [Create and merge a CSR in Key Vault](https://learn.microsoft.com/azure/key-vault/certificates/create-certificate-signing-request)
  generates the key pair in the vault and a request for it. The page has you enter a subject; the CA replaces it.

The links were fetched and their content read on 2026-10-16, and the procedures were not run.

After any of these, run the `bide-hsm pubkey` comparison above with the key reference.

## Declare the CA's facts

Every value is a statement about your CA. A wrong one is a wrong verdict, so each is read from the capture
and recorded in the policy (the policy digest and the identity record carry all of them).

| Value | Where | Declare it as | If it is wrong |
|---|---|---|---|
| `CAPeriod`, `CAPeriodFor` | `StatusParams` | how often the CA publishes a CRL, overall and per issuing CA (by the SPKI hash of its key) | too small: a gap in the series is read as a missed CRL. Too large: a CRL the CA did not publish could hide in the gap, and a decision is never `final` |
| `ThisUpdateSkew`, `ThisUpdateSkewFor` | `StatusParams` | how much earlier than its real issue the CA stamps `thisUpdate` (AD CS back-dates by about ten minutes; its setting is `ClockSkewMinutes`), 0 to 30 minutes | a CRL looks fresher or staler than it is by that much |
| `NotBeforeBackdate` | `IdentityPolicy` | how much earlier than the real issue the CA stamps `notBefore`, 0 to one hour, policy-wide; it widens `MaxLeafLifetime` by that much | a certificate that spans its life plus the back-dating is refused as too long-lived |
| `MaxStatusAge` | `StatusParams` | the oldest status accepted when a decision is counted; choose from your risk and the certificates' lifetime, never above a quarter of the shortest lifetime | see below |
| `MaxStatusAgeFor` | `StatusParams` | for a CA tier above the leaf's issuing CA (an offline root), the age its CRL may have when a decision is counted, set to that CRL's period | the root's weekly CRL makes every decision stale, or a stale root CRL is accepted |
| `ExecStatusAge` | `CertApprover` | the oldest status of the leaf's issuing CA accepted immediately before the call runs, **set from the CA's actual CRL period, with no default** | startup refuses `CAPeriod > ExecStatusAge/2` |
| `ExecStatusAgeFor`, `CAOverlapFor` | `CertApprover` | for each pinned CA and each trust anchor (below): the age its status may have before the call runs, and how long after its period the next CRL may still be coming (from the CA's CRL overlap or ceremony setting; **declared, zero if none**, never left out) | startup refuses a pinned CA without them |
| `W`, `MinW` | `StatusParams` | the compromise-reporting window the bank commits to (default 24 hours) and the floor on the window of a `final` verdict (default 30 minutes) | a decision is `provisional` when its window is short |
| `RoutineGrace` | `StatusParams` | for revocations that are not compromises (affiliation changed, superseded, ceased, privilege withdrawn), how long after the revocation a decision still counts; minutes, never hours | a leaver's decision a few minutes before their offboarding is refused, or accepted too long |
| `PolicyEffectiveFrom` | `CertApprover` | the date of the change record that put this policy in force | the identity record cannot say which policy applied years later |
| `GoDebug` | `CertApprover` | the x509 settings of `GODEBUG` the process runs under (they change how certificates verify) | the record names settings the process did not have |

Call `audit.CheckDeclaredGoDebug(declared, os.Getenv("GODEBUG"))` at startup: it compares the declared
x509 settings with the environment and returns `ErrConfig` on a mismatch. The engine itself never reads the
environment, so the declaration is yours to keep true. The identity record carries `godebug_declared` and
`godebug_build`.

### Two tiers of CA

A certificate's status comes from the CRL of its issuer, and a chain has several issuers. The tier is the
position in the **accepted** chain (the one the gate validated and the identity record names), never the
issuer's key, and the same intermediate can be a leaf's issuing CA for one approver and a CA above it for
another:

- **The leaf's issuing CA** (an online CA that issues short-lived certificates) publishes often. Its CRL age
  is bounded by `ExecStatusAge`, and its period must be at most `ExecStatusAge / 2`: a CA that publishes
  every 30 minutes gives a newest CRL up to 30 minutes old, so an age of 15 minutes would hold about half of
  all calls. A 15 minute age needs a CA period of 7 minutes or less; a 30 minute period needs one hour.
  Startup checks the default `CAPeriod` and any `CAPeriodFor` of a CA that declares no tier; the check
  before a call checks the period of the leaf's actual issuing CA again and holds the call if it does not fit.
- **Every CA above it** (an offline root, a policy CA) publishes rarely: AD CS defaults to a weekly base
  CRL. It is judged by its own `ExecStatusAgeFor`, and startup requires, for each,
  `CAPeriodFor + CAOverlapFor <= ExecStatusAgeFor <= MaxStatusAgeFor` and `0 <= CAOverlapFor <= CAPeriodFor`.

The policy cannot tell which pinned CA will sit above a leaf's issuer, so **every pinned intermediate
(`IdentityPolicy.Inter`) declares `ExecStatusAgeFor`, `CAPeriodFor`, `CAOverlapFor` and `MaxStatusAgeFor`,
and so does every trust anchor once intermediates are pinned** (the anchor's CRL covers them); startup
refuses a policy where one is missing. A leaf issued directly by an anchor, with no intermediate pinned,
makes the anchor the leaf's issuing CA, which declares none. The declarations of a CA that is only ever a
leaf's issuer are not used for it (it is judged by `ExecStatusAge`), but they must be there. A CA's CRL
that the check relies on must be no older than its value at the moment the call is about to run.

`CertApprover.ValidateConfig` runs when the gate is built and on every evaluation, so a bad or missing
value fails at configuration, not at the first decision. A decision is only as fresh as the oldest status in
its chain. Note also that a CRL issued after a certificate's `notAfter` is not evidence about it (only the
first one after it is), so a certificate that has expired is judged by the CRLs from its life and that first
one.

### Who is excluded as a requester, and the identity map

A gate has **one identity map**: the certificate-bound approvers of a gate must share one roster, and
approvers with different roster digests are refused. Separation of duties excludes the requester by
approver id and, for a certificate-bound approver, by identity. The default mapping from a requester
principal to an identity only repeats the exclusion by approver id. **A bank that wants a requester who holds
no approver id (a login that maps to a person) excluded by identity must supply
`CertApprover.RequesterIdentity`.** A requester it cannot map is excluded by approver id only, and the
finding carries a warning.

### One person, one identifier

bide cannot see that two different identifiers belong to one person, so such a person fills two seats of a
certificate-bound gate: it refuses only two approver ids that map to one identifier, and two certificates
or keys of one identifier. The control is yours: **the CA's issuance policy gives one person one
identifier, and the identity map lists one identifier per approver.** See
the "A person with two identifiers fills two seats" entry in [known limitations](../KNOWN-LIMITATIONS.md#approval-and-quorum).

### Large CRLs

The journal stores a CRL it relied on by hash, with the bytes inline up to `agent.MaxStatusInline`
(256 KiB). **Configure `CertApprover.StatusArchive` (an `audit.StatusArchive`: `PutStatus(ctx, hash, der)`,
idempotent and durable) whenever any issuer's CRL can exceed 256 KiB.** Without one the gate refuses to
count, with `ErrConfig` naming the issuer, the size and this fix. Build the status snapshot with
`audit.WithLimits` to refuse an oversize CRL at intake instead.

## Keeping the CRLs current

`crlrefresh.Refresher` (package `github.com/bide-ai/bide/audit/crlrefresh`) keeps the `StatusSnapshot` of a `CertApprover` supplied with the newest verified CRLs of
the issuers the policy pins, so your platform team does not write a fetching loop. It is a separate component
and the verifier stays free of I/O: audit tools and `bide approve -verify-receipt` work offline as before.

<!-- docsnip: setup sources audit.CRLSources; issuingCA *x509.Certificate; logEvent func(crlrefresh.Event); page func(crlrefresh.Alarm); ctx context.Context -->
```go
approver := &audit.CertApprover{ /* as above; Status: audit.NewStatusSnapshot(...) and Now are required */ }
r, err := crlrefresh.New(approver, sources, crlrefresh.Options{
    Issuers: []*x509.Certificate{issuingCA},            // the pinned intermediates' certificates
    Archive: crlrefresh.DirArchive("/var/lib/bide/crl"),   // the dated series and the rollback memory
    Hooks:   crlrefresh.Hooks{OnEvent: logEvent, OnAlarm: page},
})
go r.Run(ctx)   // or r.RefreshOnce(ctx) from a cron job
```

The interval, the alarm and the set of issuers come from the values the approver already declares
(`CAPeriod`, `MaxStatusAge`, `ExecStatusAge`, the `...For` maps, the overlap, the skew), so there is no second
copy to drift. `crlrefresh.New` refuses (`agent.ErrConfig`) a configuration that cannot work: an issuer with no
source, a pinned intermediate with no certificate, no declared period, a snapshot too small for the series the
status age needs, or a CA tier whose limit leaves no room for an alarm before the gates pause.

**Where CRLs come from.** Only from URLs you configure, per issuing CA (keyed by the SHA-256 of its key), in
the policy file or as `audit.CRLSources`:

```json
"intermediate_certs": ["issuing-ca.pem"],
"crl": {
  "<issuing CA spki sha-256 hex>": {"urls": ["http://pki.bank.example/issuing.crl"],
                                    "delta_urls": ["http://pki.bank.example/issuing+.crl"]},
  "<root spki sha-256 hex>":       {"urls": ["http://pki.bank.example/root.crl"]}
}
```

The policy pins intermediates by key, and a key cannot verify a signature, so the refresher takes the
intermediates' certificates (`intermediate_certs`, relative to the policy file like the anchors); each must
match a pinned key and chain to an anchor. A URL is never read from a certificate a client supplied. The one
use of a distribution point is the opt-in `"from_pinned_cdp": true` on an entry, which adds the CRL
distribution point of the supplied certificate issued by that CA, so it helps only for a CA above the leaf's
issuing CA: a leaf names its issuer's CRL, and leaves come from clients, so the leaf CA's CRL URL is always
configured. The URLs are not part of the policy digest: moving one cannot change a verdict, because every
CRL is verified against the pinned key and judged by the same number and freshness rules whatever URL
delivered it; each archived CRL records its source URL and fetch time as provenance, never as evidence.

**Transport.** HTTP and HTTPS only; `ldap://` is refused with a message saying LDAP is not supported
(publish the CRL over HTTP). GET only, a response size limit (the snapshot's, 16 MiB by default), timeouts on
every stage, no compression, and redirects only to the same host and port, at most three, never from https to
http; the one exception is the usual move to TLS, an http URL on port 80 redirected to the same host over https on
port 443. The library reads no proxy from the environment; `bide identity fetch-crl` does, and an application passes
its own `*http.Client` for mTLS or a proxy (then the proxy and DNS rules are the application's). A CRL is signed,
so plain HTTP is the normal profile; a man in the middle can withhold or replay a CRL, which the rules below
bound, but not forge one. **With a single URL and plain HTTP a replayed CRL stays acceptable until its
`nextUpdate`**: list every mirror of the same CRL and the refresher asks all of them on each attempt and keeps
the highest-numbered valid one.

**What it checks before a CRL reaches the snapshot.** The signature algorithm profile (never SHA-1), the
signature against the issuing CA's own certificate (not any certificate the snapshot knows: another CA's CRL
served at this CA's URL is refused), the issuer name, the key's `Distrust` date, a `thisUpdate` not ahead of the
clock beyond the declared skew and a minute, and the structure rules of `AddCRL` (indirect and partitioned CRLs
are refused and reported as unsupported). A CRL with a **lower number** than the highest accepted is a rollback
and is not added; the **same number with a different TBSCertList** is an equivocation and is not added (a CA that
re-signs one CRL, whose signature bytes differ, is the same CRL). Both are archived with a `rollback` or
`conflict` marker and reported. A delta CRL is paired with a base numbered in `[base indicator, delta number)`;
the refresher fetches the base at once when none is held, and a delta whose base cannot be had is still added
and archived and reported unpaired. Freshness is the merged view's earlier `nextUpdate`, so a CA that stops
publishing deltas does not leave a stale delta looking current.

**Failure behaviour.** A failed fetch changes nothing that is held. Freshness is read from each CRL's own
`thisUpdate` and `nextUpdate`, never from `Date`, `Last-Modified` or a `304`, so the refresher cannot make status
look fresher than the CA's series. A dead refresher, a dead CA or a blocked network therefore ends where it did
before: the gates pause (`agent.PendingIdentityStatus`) and the intake answers 503 with `Retry-After`. A CRL that
arrives already past its `nextUpdate` is archived and added, because it fills the series, but is not counted as
fresh. Attempts back off from 30 seconds to the regular interval with jitter, honouring `Retry-After`. The
regular interval is `min(CAPeriod/2, min(MaxStatusAge, ExecStatusAge)/4)`, at least 30 seconds; from the moment
the next CRL is due it polls every `CAPeriod/16` (30 seconds to 5 minutes) until one with a higher number
arrives.

**The alarm, and why it is not "50 percent".** Half of a CRL's validity is inside the normal cycle of a root
whose status limit is about one period, so a literal 50 percent alarms every week on a healthy root. The
refresher alarms when the newest CRL's age (`now - thisUpdate`, exactly the age a gate judges) exceeds
`min(max(limit/2, CAPeriod + overlap + tolerance), limit - interval)`, where `limit` is the smallest status age
the issuer is judged by in any of its roles (counting a decision; the leaf's issuing CA before a call runs; a CA
tier before a call runs). For a fast CA that is half the limit, long before the pause; for a root it is when the
next CRL is overdue. `Critical` is set when a gate no longer accepts the newest CRL (the status rule's own
freshness test, shared in code, with a test at the boundary). Startup refuses a CA tier whose limit leaves less
than the tolerance plus one interval after its CRL cycle, so give the root CRL a validity of two periods and set
its `ExecStatusAgeFor` (and `MaxStatusAgeFor`) to it: then the alarm comes a full period before any gate
pauses.

**Archive and restart.** `crlrefresh.DirArchive` writes `crl/<issuer key hash>/<number>-<thisUpdate>.crl` with a
`.meta.json` beside it, through a temporary file, a sync, a rename and a directory sync; the directory is created
`0700` and an existing one that others can write to (or, on Unix, another user owns) is refused, because the
archive is the rollback memory: **write access to it is write access to the refresher's memory**. Several
replicas may share one directory. The CRL is archived before it is added; if the archive refuses, the CRL is
**still added** (withholding it would make status look better), queued for retry and reported
(`archive-error`, `ArchiveBacklog`), and the queue drops its oldest entries only beyond a byte bound, saying so
(`archive-dropped`). At start the refresher takes the rollback mark from archived CRLs that verify (a file name
alone is never a mark; a file that does not verify is reported `archive-corrupt`) and seeds the snapshot with
what the live rule needs, newest first, without touching the snapshot's eviction count. Without an archive a
restart forgets rollback protection, and the refresher says so at start.

**A poisoned mark.** A CA that issues one CRL with a mistaken huge number, or is restored from a backup and
restarts its numbering, would make every later real CRL a rollback. `bide identity reset-high-water` (or
`crlrefresh.Refresher.ResetHighWater`) sets that CRL aside: it appends a `RESET-<time>.json` record to the archive (what
is set aside, the reason, the operator, the time), removes the CRL from the live snapshot and recomputes the
mark; the CRL stays in the archive, and a restart honours the record. A reset made with the command reaches a
running daemon (or a gate's embedded refresher on a shared archive) at its next attempt, with no restart, and a
source that keeps serving the set-aside CRL has it archived with a `setaside` marker and never added
(`set-aside-served`). The CRL set aside is normally the issuer's newest real one, so a reset that would drop a
revocation no remaining CRL lists, and so make status look better until the CA publishes it again, is refused
unless you pass `-accept-lost-listings` (`crlrefresh.ResetRequest.AcceptLostListings`); the serials it dropped are written
into the record's `lost_serials`, which is the audit trail. A listing on a CRL set aside no longer
reaches the gate: the record is your statement that the CRL was wrong. A forged record could lower a mark, so
every start reports an unacknowledged reset (`high-water-reset`, `Health().UnackedResets`) until an operator
runs `bide identity ack-reset`.

**Observability.** `crlrefresh.Hooks.OnEvent` receives every attempt and refusal (`fetched`, `unchanged`,
`rollback`, `conflict`, `bad-signature`, `distrusted`, `unsupported-crl`, `future-dated`, `stale-on-arrival`,
`oversize`, `redirect-refused`, `fetch-failed`, `archive-error`, `clock-skew`, ...), with the issuer, the URL
without its query or fragment, and no response body; `OnAlarm` receives alarm changes; `Health()` returns for
each issuer the last attempt and last success, the newest number and dates, the age, the next expected CRL, the
failure count, the archive backlog, `Paired`, `Alarm` and `Critical`, and a `Loop` time that stops moving if a
loop dies. A server's `Date` header that differs from your clock by more than five minutes is reported as a
warning only. Start-up events print the effective URLs and each issuer's limit, alarm age and margin.

**The command.** `bide identity fetch-crl -policy policy.json -params ca.json -cache dir` is the same code for
cron (`-once` exits 1 unless every issuer holds a fresh CRL) or as a daemon, printing one JSON line per event
and a health line per issuer. `ca.json` is a copy of the timings the gate declares (`ca_period`,
`max_status_age`, `exec_status_age`, `this_update_skew`, and per issuer `ca_period`, `max_status_age`,
`exec_status_age`, `ca_overlap`, `this_update_skew`, `delta_period`); it must match the gate's declaration. Do not compare the policy digest the command prints with the digest the gate
records: today the two cannot be equal. The digest covers `effective_from` (the command's stand-in approver
takes its start time) and values the file cannot hold (`w`, `min_w`, `routine_grace`, `clock_tolerance`,
holds, retention, consecutive numbers), which the command fills with defaults. Compare instead the
`declared values` start-up event with the policy record the gate journals (the `ApprovalIdentity` record):
the timing members (`ca_period*`, `max_status_age*`, `exec_status_age*`, `ca_overlap_for`,
`this_update_skew*`, `coverage_tolerance`) and the anchors and intermediates must be the same. A
mismatch means the schedule and the alarm follow other numbers than the gate. A delta outage is reported as an event and does not fail `-once`; a base that
is not fresh does. A
gate process embeds the refresher itself; the command is for the archive an audit reads and for monitoring.
`bide approve -cert` does not fetch CRLs: its local check is a convenience before signing, and the gate decides.

## Pre-flight: `bide identity check`

Run `bide identity check` before go-live, after any change to the policy file, the CA's timings or the CRL
sources, and when an approver's decision is refused. It answers one question: will a gate built from this
setup count a certificate-bound approver and let the call run, and if not, exactly why. It runs the gate's
own code for each answer (the policy loader, the gate's configuration check, the CRL refresher's fetch and
health model, and the function the gate's counting and execution checks call for a certificate chain), so a
verdict here is the gate's verdict, not a second opinion.

```text
bide identity check -policy policy.json -params ca.json \
    -expect gate-policy.json  -crl-dir ./crl-archive  [-cert chain.pem] [-json]
```

It never reports ready while a part the gate relies on was left out, so two choices are required:

| You choose | Flag | If you decline |
|---|---|---|
| Where the CRLs come from | `-crl-dir <dir>` offline: a `bide identity fetch-crl` archive (read, never written) or a flat directory of `.crl`, `.der` or `.pem` files; no network. `-fetch` online: the policy's `crl` sources over HTTP or HTTPS with the limits of `fetch-crl` (size, timeouts, same-host redirects), nothing stored. Together, the archive seeds the rollback memory and the fetch is judged. | `-no-status`: the result is **partial** |
| What the gate runs with | `-expect <file>`: the gate's recorded policy, the `policy` member of its identity record (or the whole record) | `-no-expect`: the result is **partial** |

Optional: `-cert chain.pem` (the approver's chain, leaf first, as `bide approve -cert` takes it; `-chain` adds
more certificates behind it, `-as` names the approver id when the policy has several) asks whether that
certificate would be accepted, as a decision made now (`-at <RFC 3339>` judges at another time, offline only).
Run `-fetch` on the gate's host: reachability is proved from where the check runs. Without `-cert` the report
says so (`cert.none`): a ready result then covers the deployment, not any approver, so run it with `-cert` and
`-as` for each approver before go-live. The report prints the subject of CA certificates only, never an
approver's, and every URL without its userinfo, query and fragment.

**Exit status.** 0 ready: every check passed. 1 will not work as configured: at least one check failed (a
failure outranks everything). 2 usage error. 3 a file could not be read or the tool could not run. 4
partial: nothing failed, but `-no-status` or `-no-expect` left a part unchecked; a partial result is never a
go-live result. `-json` prints `{"ready", "partial", "checked_at", "checks": [{"id", "status", "message",
"fix"}], "not_checkable"}`; no CRL bytes, URL queries, keys or private-key blocks are ever printed.

**Reading the output.** One line per check: `OK`, `WARN` (works, but will not for long), `FAIL` (a gate would
refuse, pause or miscount), `SKIP` (not run, and the summary says whether that makes the result partial). A
`WARN` or `FAIL` has a `fix:` line. The check ids are stable:

| Check id | What it asks |
|---|---|
| `policy.load` | the policy file parses strictly and compiles (`audit.ParseIdentityPolicy`) |
| `policy.anchor`, `policy.intermediate` | each pinned CA certificate is valid now (WARN inside 90 days of expiry) |
| `policy.sources`, `policy.intermediates` | the `crl` and `intermediate_certs` members parse; every pinned intermediate key has its certificate, and every certificate supplied is pinned |
| `params.load`, `params.config` | the `-params` file parses, and passes the gate's own start-up check (`CertApprover.ValidateConfig`) |
| `params.expect`, `params.expect.unverified` | the policy file and `-params` agree with the gate's recorded policy, member by member; the members they cannot declare are listed, not claimed |
| `crl.setup` | every pinned issuer has a CRL source and a certificate, and the timings leave the refresher a margin before gates pause (`crlrefresh.New`) |
| `crl.load`, `crl.file`, `crl.fetch`, `crl.event.*` | the CRLs were read or fetched, verified against the pinned issuers; files and events that were refused (rollback, equivocation, bad signature, oversize, a refused redirect, ...) |
| `crl.fresh.<issuer>` | the newest CRL of each issuer is fresh by the status rule's own test and the refresher's health model (`FAIL` when a gate would not accept it now, `WARN` past the alarm age) |
| `cert.none` | no `-cert` was given: the deployment was checked, and no approver's certificate was (a `SKIP` that does not make the result partial) |
| `cert.parse`, `cert.chain` | the file is a certificate chain; the gate would accept it for the approver (`VerifyChain`), with the gate's reason if not |
| `cert.status.count`, `cert.status.exec` | the revocation status rule, as when a decision is counted and as immediately before the call runs |

**Why the comparison with the gate is member by member.** The digest `fetch-crl` prints cannot equal the
gate's (see [the command](#keeping-the-crls-current)): the digest covers `effective_from` and values the
`-params` file cannot carry. `-expect` compares the members both sides can declare (anchors, intermediates,
the identity profile, the lifetime and algorithm limits, and every CA timing) and names each difference with
both values. `params.expect.unverified` lists the rest (`w`, `min_w`, `routine_grace`, `clock_tolerance`,
holds, retention, consecutive numbers, `effective_from`) with the recorded values: compare those with the
gate's declaration by hand.

**Troubleshooting: the messages, and what to do.**

| You see | It means | Do this |
|---|---|---|
| `policy.load` FAIL, `"x" is not a field` | a member of the policy file is misspelled or unknown | correct the member; unknown members are refused so a typo cannot weaken a policy |
| `policy.anchor` FAIL, `expired` | every chain through that CA is refused | pin a new CA certificate before it lapses; plan with the WARN at 90 days |
| `policy.intermediates` FAIL, `holds no certificate for it` | the policy pins a key; a key cannot verify a CRL | add the intermediate's PEM to `intermediate_certs` |
| `policy.intermediates` FAIL, `does not pin` | a certificate is supplied for a CA the policy does not pin | pin its key under `intermediates`, or remove the file |
| `params.config` FAIL, `ExecStatusAge/2` | a CA publishes less often than twice per execution age | raise `exec_status_age` or declare the CA's real period |
| `params.expect` FAIL, `<member>: policy and -params say A, the gate recorded B` | the files differ from what the gate runs with | make the files carry the gate's values (the schedule and alarm follow them) |
| `crl.setup` FAIL, `no CRL source` | a pinned issuer has no `crl` entry | add `"crl": {"<issuer key hash>": {"urls": [...]}}` |
| `crl.fetch` FAIL, `http-status: 404`, `error`, `not-a-crl` | the URL does not serve the CA's CRL | fix the URL, firewall, proxy, DNS or TLS trust from the gate's host; only http and https are fetched |
| `crl.fresh` FAIL, `no CRL is held` | none read or fetched for that issuer | run `fetch-crl`, or put the CRL in `-crl-dir` |
| `crl.fresh` FAIL, `a gate would not accept it` | the newest CRL is older than the declared ages, or past `nextUpdate` | the CA must publish within its declared period and the fetch job must run |
| `cert.chain` FAIL, `does not verify` | the chain does not reach a pinned CA, or lacks a pinned intermediate | the file given to `bide approve -cert` must carry the leaf and the pinned intermediates, leaf first |
| `cert.chain` FAIL, `is not the identity of approver` / `no subject alternative name` / `exactly one is required` | the certificate's one SAN is not the identifier the policy gives this approver id | reissue the certificate, or correct the identity map through the change process |
| `cert.chain` FAIL, `not usable for approving` | the extended key usage, certificate policy, key usage, key type or CA flag is wrong | compare with the policy's `eku` and `policy_oid`; reissue from the right template |
| `cert.chain` FAIL, `outside the certificate's validity` | a certificate is expired or not yet valid at the check time, or the leaf outlives `max_leaf_lifetime` | renew; check the host's clock |
| `cert.status.*` FAIL, `revoked` | a certificate of the chain is revoked, or refused by the status policy (a hold) | the approver needs a new certificate |
| `cert.status.exec` FAIL, `older than the policy allows`, while `cert.status.count` is OK | the CRL is old enough to count a decision but not to run its call | the CA is publishing too slowly for `exec_status_age`, or the fetch job has stopped |
| `cert.status.*` FAIL, `no usable revocation status` | no CRL covers a certificate of the chain, or a CA above the leaf's issuer has no `ExecStatusAgeFor` | fetch the CRLs for every issuer in the chain; declare the tier |

**What it cannot check ahead of time**, and says so in every report: the signature of a real decision and
that the key is the certificate's; when a real decision becomes provable (the covering head and the log
clock) and, with no independent time source shipped, nothing about it; whether a future decision will be
provisional or final; revocations published after the check; the application's own HTTP client, proxy and
DNS rules; the gate's other settings (separation of duties, the roster, requester mapping, holds, the
`GODEBUG` of the process) unless `-expect` supplies them; and that the gate embeds the refresher or that a
`fetch-crl` job is running. A ready result means the declared setup works as far as these checks reach, not
that a bank's controls are met; nothing here is a compliance certification.

## When you revoke a certificate

**Publish the CRL right after revoking.** AD CS dates a revocation at its publication, not at the moment
the operator pressed revoke, so a revocation left unpublished is not in any CRL and no gate can know of it
(`certutil -crl` on AD CS, or the CA's equivalent). The [CRL refresher](#keeping-the-crls-current) picks the
new CRL up within half the CA's declared period of its issue. A root CRL that lapses holds every gate that
depends on it, so give the root CRL a validity of at least two publication periods.

What a revocation does to decisions:

- A decision that became provable **before** the effective revocation time `E` stands. A decision first
  provable at or after `E` does not count.
- A compromise reason (`keyCompromise`, `cACompromise`, `aACompromise`) with no invalidity date is
  retroactive to the certificate's `notBefore`, so it reaches every decision the key ever made. A late
  report is caught only if the CA dates it back (invalidity date) and the status evidence is late enough
  for the window.
- A decision already counted in a terminal tally stays counted in that tally. What stops the action is the
  check before the call runs (next section), and an audit reports the decision as disputed.

## Counting: provisional and final

A gate counts a decision as soon as it is provable (a signed head of the run covers it, which fixes
`A(d)`, the time it became provable) and its chain is accepted, with `CountLive` set and a status that is
fresh. A CRL issued before the decision is enough to count it, because waiting for the next CRL would hold
every decision for a CRL period. Such a count is **provisional**: a revocation effective before the
decision may be reported after that CRL, and the identity record says so (`basis_kind`, the status time
`S` and its age). Only a decision whose status reaches `A + W'` over a window of at least `MinW`, with the
series complete up to the first CRL beyond the certificate's `notAfter`, is **final**, and a final verdict
still names its time source (`independent-final` with an RFC 3161 token or another independent time,
`logclock-final` when the log signer's clock is all there is). The record carries the tier, so a reader can
tell; the word "verified" is reserved for an audit result, and a viewer must not present a provisional
count as verified.

## Taking the decision over HTTP

A certificate-bound approver decides through the approvals intake with the chain in the request (`cert`), or
directly with `agent.SubmitDecision` and `agent.WithDecisionCheck`. Both verify the chain **before they write**
and write nothing on a failure: the chain is accepted for the approver at the time of the write, the signature
is over the `bide.approval.v4` bytes under the leaf's key, and status is usable by the execution ages
(`ExecStatusAge`, `ExecStatusAgeFor`). A decision whose status cannot be judged is `agent.ErrStatusUnavailable`
(the intake answers 503 with `Retry-After`; retry once the refresher has current CRLs), not a refusal. A
revoked chain, a bare signature from a certificate-bound approver and a chain from a bare-key approver are
refused (`agent.ErrInvalidApproval`, 422). The write is then covered by a signed head (the application's
`AuditedStore` signs one with it, and the intake asks again), and the gate counts it; the first count is
usually **provisional** (status that predates the decision), which the response shows (`tier`, `provisional`)
and which never reads as verified. See [taking decisions over HTTP](hitl-approval.md#taking-decisions-over-http).
The identity rule per approver is on `GET /pending` (`gate.identity`): restrict who can read a gate, since it
lists person identifiers.

## Before the call runs

A terminal tally is never recounted, so a decision counted at 10:00 would otherwise still authorize an
action at 11:30 after a 10:30 revocation. Immediately before a gated call executes, one function
(`agent.preExecute`) re-verifies every counted certificate-bound decision of the gate's outcome with the
execution ages above. Every path that can run the tool passes it: the first execution, a resume, crash
recovery and `RecoverLoop`, a retry of a retry-safe tool by a middleware, and a plan flow node before its
body runs (not when its recorded result replays). A call is never begun before it, and a gate with no
certificate-bound approver pays nothing.

What it does with a decision that does not hold:

- **Revoked, or no longer verifying for good** (the certificate shows `E <= A(d)`; another approver now
  shares its key; the signature no longer verifies): the call does not run. The gate writes an exec hold
  (`approval-exec-hold:<id>:<generation>`, with the certificate's serial, `E`, the reason code, the CRL's
  hash and `A(d)`), counts the next generation without the decisions that no longer hold, and asks again
  (`agent.PendingExecHoldReapproval`). Decisions under other certificates count, so replacement decisions
  can run the call. A revoked decision is a dispute for your process, not an automatic denial.
- **Status missing or stale, or a decision no head covers right now:** the call does not run and nothing
  authoritative is written. `Run` returns `*agent.ApprovalPending` with `agent.PendingIdentityStatus`, a
  phase note says `exec-hold` with one sentence of detail, and the same approvals run the call when a fresh
  check passes. Nobody is asked to decide, so no approval notification is sent for it; watch the gate's phase
  (`agent.LatestPausePhase`) and the CRL alarm above. **A drive has to run again for the call to run:**
  nothing in the gate retries by itself, so make sure the refresher has added the fresh CRL to the `StatusSnapshot` and re-drive the run
  (your own `Resume`, or `RecoverLoop`, which re-drives runs that are still waiting on each pass and so runs
  the call on the first pass after status is current).
- **The recount ends in a denial or an expiry** (the approvers who decided again denied it, or the gate's
  deadline passed): the call does not run, the drive stops with an error that says so, and the next drive
  records the denial as the call's result.

## Published revocation latency

Publish both numbers for your CA. They are the longest time from the CA recording a revocation to a gate
refusing to run on it.

- **Leaf tier:** CA publication delay + the leaf CA's CRL period (or delta period) + `ExecStatusAge`
  (+ `ThisUpdateSkew`). `audit.ExecLatencyBound(publishDelay, period, 0, execAge)` computes it. With a 30
  minute period, a 5 minute publication delay and a one hour `ExecStatusAge` it is 95 minutes.
- **CA tier, through the offline root's CRL:** the time to hold the root ceremony and publish + the root's
  CRL period + `CAOverlapFor` + `ExecStatusAgeFor`. `audit.ExecLatencyBound(ceremonyDelay, rootPeriod,
  overlap, execAge)` computes it. For a root that publishes weekly it is **days to a week or more**, and is
  never to be read as minutes. The fast path for a compromised issuing CA is not the root's CRL: it is a
  change of policy, removing the intermediate from `IdentityPolicy.Inter` or setting `IdentityPolicy.Distrust`
  for it (`DistrustAfter`), and redeploying. Rehearse it before you need it.

The two numbers bound when the gate can *know*. They do not bound the instant between the check and the
call: a revocation the CA records after the check passed is not seen by that call. The check is the last
step before the call is begun (after any middleware that waits, such as a rate limiter), so the interval is
the time the process takes to begin the tool, milliseconds in process, and not the tool's own duration.

## Rolling it out

Roll the whole fleet before enabling a certificate-bound approver, and remove the old bare key from every
resolver at the switch: an old process without that change cannot refuse to count a stolen bare signature.
A gate that is certificate-bound is never counted with bare keys afterwards: a policy that has lost the
certificate-bound approvers is refused with `ErrConfig`. The auditor needs the expected policy of record for
a gate the bank classifies as identity-required.
