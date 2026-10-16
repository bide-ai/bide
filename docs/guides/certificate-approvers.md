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
2. **The approver makes a key.** A key file: `bide keygen -out ops.key -as ops` (Ed25519, or `-alg
   ecdsa-p256`; both have a certificate scheme; ML-DSA-65 does too, a hybrid does not). A key in an HSM or a
   KMS: `bide-hsm pubkey -as ops -key <key reference>`. Either prints the public key. The private key never
   leaves its file, token or KMS.
3. **The CA issues a certificate for that public key**, after the bank's own identification of the person
   (single sign-on with step-up, say), with one subject alternative name that is the person's identifier,
   the bank's EKU and the bank's policy OID. How the request is made is the CA's procedure: bide asks the CA
   for nothing, and it cannot know how the person was identified. For a KMS key the request's proof of
   possession is signed by the KMS key with the KMS vendor's tooling.
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
   check and says so loudly; the gate will still refuse a chain it does not accept.
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
the "A person with two identifiers fills two seats" entry in [known limitations](../KNOWN-LIMITATIONS.md).

### Large CRLs

The journal stores a CRL it relied on by hash, with the bytes inline up to `agent.MaxStatusInline`
(256 KiB). **Configure `CertApprover.StatusArchive` (an `audit.StatusArchive`: `PutStatus(ctx, hash, der)`,
idempotent and durable) whenever any issuer's CRL can exceed 256 KiB.** Without one the gate refuses to
count, with `ErrConfig` naming the issuer, the size and this fix. Build the status snapshot with
`audit.WithLimits` to refuse an oversize CRL at intake instead.

## When you revoke a certificate

**Publish the CRL right after revoking.** AD CS dates a revocation at its publication, not at the moment
the operator pressed revoke, so a revocation left unpublished is not in any CRL and no gate can know of it
(`certutil -crl` on AD CS, or the CA's equivalent). bide refreshes nothing by itself in this build: the
application adds the CRLs it fetches from the configured distribution points to the `StatusSnapshot`
(`AddCRL`), and your own monitoring raises an alarm when the newest CRL of any issuer is older than half of
the age the policy allows it. A root CRL that lapses holds every gate that depends on it, so give the root
CRL a validity of at least two publication periods.

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
  nothing in the gate retries by itself, so add the fresh CRL to the `StatusSnapshot` and re-drive the run
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
