# CRL refresher for certificate-bound approvers

Status: design, for review before building (2026-10-16). Step 2 of the
[approver identity](approver-identity.md) work promised "the refresher fetches and archives" CRLs;
this note fixes how. Today the application fetches CRLs itself and calls `StatusSnapshot.AddCRL`;
the operator guide says "bide does not fetch CRLs". A bank's platform team should not have to write
that loop, and a hand-written loop is where rollback, stale-on-failure and silent-death bugs live.

## What it is, and what it is not

`audit.CRLRefresher` is a long-running component that keeps the `StatusSnapshot` of a `CertApprover`
supplied with the newest verified CRLs of the issuers the policy pins, and keeps a dated archive of
every CRL it accepts.

- It fetches **CRLs only** (full and delta). OCSP is a separate module and out of scope.
- It fetches **only from URLs the operator configured**. It never fetches a URL read from a
  client-supplied chain, never in the request path (the intake and the gate never wait on it).
- It does **not** decide anything about a certificate. Verdicts stay in `EvaluateStatus`. The
  refresher can make status staler than the CA's own series (by failing), never fresher.
- It is **not** part of the verifier. `CertApprover` stays free of I/O so audit tools and
  `bide approve -verify-receipt` keep working offline.

## Decisions in this note

### 1. Where URLs come from

Configured per issuer, keyed by the SPKI hash of the issuing key (the same key the `CAPeriodFor`,
`ExecStatusAgeFor` maps use), in the certificate policy file:

```json
"crl": {
  "<issuer spki sha-256 hex>": {
    "urls": ["http://pki.bank.example/issuing.crl"],
    "delta_urls": ["http://pki.bank.example/issuing+.crl"],
    "from_pinned_cdp": false
  }
}
```

and as `audit.CRLSources` (a map with the same shape) for a program that does not use the file.

- **Issuer certificates.** The policy pins intermediates by SPKI hash only (`IdentityPolicy.Inter`);
  only anchors are certificates. A hash cannot verify a signature, and the only place an
  intermediate's certificate exists today is a client-supplied chain, which is never a source. So the
  refresher takes the **intermediate certificates themselves** (`intermediate_certs`, PEM files
  relative to the policy file like the anchors, or `Config.Issuers` in Go). At startup each is
  checked: its SPKI hash is a pinned one, it chains to an anchor, and it is not distrusted. A pinned
  intermediate with no certificate supplied is `ErrConfig`. These certificates, and only these and
  the anchors, are the issuers for signature checks, for `WithIssuers` and for `from_pinned_cdp`.
  Out of scope, and refused: a CRL signed by a delegated CRL signer (not the issuing key), an
  indirect CRL, a partitioned (issuing distribution point) CRL. `AddCRL` already refuses the last two;
  the refresher reports that refusal as `EventUnsupportedCRL`, never as "fetched".
- Every key must be a pinned issuer (an anchor or a pinned intermediate); an unknown key is
  `ErrConfig`. Every issuer the policy can reach through an accepted chain needs an entry or the
  refresher refuses to start (an issuer with no source would be a permanent pause that looks like a
  network problem). `ValidateSources(approver)` names the missing ones.
- URL rules, checked at load: scheme `http` or `https` only; no userinfo (credentials in a config
  file and in logs); no fragment; an explicit non-empty host. `ldap`, `ldaps`, `file` and `ftp` are
  refused with a message saying LDAP is not supported (a bank publishing only to LDAP mirrors it to
  HTTP, as every public CA profile expects).
- **Reading CRL distribution points from certificates.** The CDP extension of a certificate names
  the CRL of that certificate's *issuer*. So a pinned intermediate's own CDP names its issuer's CRL
  (the root's), and that is the only case where a certificate bide pinned can name a URL. The CRL of
  the leaf's issuing CA is named only in leaves, which arrive from clients: never fetched. Therefore:
  `from_pinned_cdp: true` is an opt-in per entry that adds the CDP URLs (and freshestCRL URLs for
  deltas) of the **supplied certificate whose issuer is that entry's issuer** (for the root's entry,
  the intermediate certificate the operator supplied; the root itself has no CDP); they are subject to the
  same URL rules and the same transport limits. Default false, so the safe default is "exactly the
  configured list". The effective URL list is printed at startup and reported in `Health()`.
- Policy digest: the URLs are **not** part of `PolicyDigest`. They change for operational reasons
  (a CDP moves) and do not affect any verdict, since every CRL is verified against the pinned key.
  Each archived CRL records the URL it came from instead. Moving a URL cannot change a verdict: the
  signature (against the supplied issuer certificate), the number rules and the freshness rules do not
  depend on where the bytes came from.
  Conditions for that (pki-auditor-2): (1) the URL is provenance only and never evidence; the
  evidence of a verdict is the CRL itself (hash and bytes: inline, `StatusArchive`, or the dated
  archive), which the gate records in the identity record independent of any URL. (2) Each archived
  CRL records its source URL and fetch time, and the effective URL list (after `from_pinned_cdp`) is
  logged at startup and reported through the hook as a security event on every change, because a URL
  moved to a mirror that replays old CRLs is a withholding path (it can only make status staler,
  never fresher). (3) Everything that changes a verdict stays in the digest or is pinned: the
  supplied intermediate certificates must match the pinned SPKI hashes (refused otherwise), and
  `CAPeriodFor`, `ExecStatusAgeFor`, `MaxStatusAgeFor`, `CAOverlapFor`, `ThisUpdateSkew` and
  `DistrustAfter` are already in it. The refresher's interval, retry and alarm tuning are outside it
  and affect timeliness only. (4) The skew in the future-dating refusal is the declared
  `ThisUpdateSkew`, not a separate refresher value.

### 2. Transport

- GET only, no body, no cookies, `Accept: application/pkix-crl`, a fixed `User-Agent`
  (`bide-crl-refresher/<version>`). The response is DER or PEM (`X509 CRL`); PEM is normalised to
  DER; the content type is not trusted.
- **Size**: the body is read through a limit of the snapshot's `maxBytes` plus one; more is a
  failure, never truncated. (The snapshot's own limits, `WithLimits`, remain the single source.)
- **Timeouts** (defaults, configurable): connect 5s, TLS 5s, response header 10s, whole request 60s.
  A slow-drip body hits the whole-request deadline. Contexts are honoured everywhere.
- **Redirects**: at most 3, and only to the **same host and port** as the configured URL, and never
  from https to http. (CRL servers commonly redirect http to https or add a trailing path; a redirect
  to another host is the open-redirect/SSRF route.) A refused redirect is a failure naming the
  target host.
- **Proxy**: the library's default transport has `Proxy: nil`, so library code never reads
  `HTTP_PROXY`. `cmd/bide` builds its client with `http.ProxyFromEnvironment`. An application passes
  its own `*http.Client` (`Config.Client`) for mTLS, a corporate proxy or a custom resolver; the
  refresher then still enforces size, per-request deadline, and the redirect rule (it wraps
  `CheckRedirect`, not the transport). https verifies the server certificate with the system roots
  (`Config.Client` may override); a CRL is signed, so an http URL is the normal profile and a
  man-in-the-middle can at worst withhold or replay, which the rules below handle.
- `DisableCompression: true` on the library transport (no transparent gzip); the size limit applies to
  the decoded stream anyway and is tested with a gzip bomb. A `Content-Length` above the limit is
  refused before any body is read.
- An application-supplied `Config.Client` is the application's: the library cannot enforce proxy or
  DNS rules on a transport it did not build (it can still bound the request context, the body size and
  the redirect rule through `CheckRedirect`), and it logs once that a client was supplied.
- Conditional GET (`If-None-Match`, `If-Modified-Since`) is used when the server gave a validator.
  A `304` counts as "checked, nothing new": it never refreshes anything, because freshness is read
  from the CRL's own `thisUpdate` and `nextUpdate`, never from a transport header.
- No address filtering by default (a bank's CRL host is often in a private range). An application
  that wants a deny-list sets a `Dialer.Control` in its own client.

### 3. Scheduling

One goroutine per source issuer; the schedule is a pure function of (state, declared parameters,
clock, random source), unit-tested without a network or a real clock.

- **Interval** `I = min(CAPeriod/2, min(MaxStatusAge, ExecStatusAge)/4)` with the issuer's own
  values (`CAPeriodFor`, `MaxStatusAgeFor`, `ExecStatusAgeFor`; the leaf's issuing CA uses
  `ExecStatusAge`), floor 30s. Startup validation (`CAPeriod <= ExecStatusAge/2`) makes
  `CAPeriod/2` the smaller term, so a new CRL is picked up within half a period of its issue. This
  tightens the row in the identity note, which gave only the `/4` term; that table row is updated.
- **Catch-up**: the next CRL is expected at `thisUpdate + skew + CAPeriod` (skew is the declared
  `ThisUpdateSkew`). From that moment until a CRL with a higher number arrives, poll every
  `clamp(CAPeriod/16, 30s, 5m)` instead of `I`. A CA that is late is found quickly; one that is early
  is found by the regular interval.
- **Jitter**: each wait is multiplied by a uniform factor in `[0.9, 1.1]`, drawn from an injectable
  source, so a fleet does not synchronise on the CA.
- **Backoff** on failure: `30s, 60s, ...` doubling (with full jitter over the upper half) up to `I`,
  reset by a success. **Every configured URL is queried in each attempt** and the highest-numbered
  valid CRL is added (all valid ones are archived, so a mirror that lags is on record); the attempt
  fails only if none answers with a valid CRL. Taking the first answer would let a stale mirror, or a
  replay over plain http of a pre-revocation CRL still inside its validity, win on a cold start with no
  high-water mark. A single-URL deployment cannot do better, and the guide says so: an http-only CRL
  source can be replayed until the CRL's `nextUpdate`. The backoff applies per attempt, not per URL. A server `Retry-After` longer than the backoff is
  honoured up to `I`.
- **Never replace a newer CRL with an older one.** Per issuer, the refresher tracks the highest
  `CRLNumber` seen of full CRLs and of deltas separately (one number sequence across both is allowed;
  the comparison is against the highest number of the same kind). A fetched CRL with a **lower**
  number is a rollback (a stale mirror, or a replay): not added, reported by `OnEvent` naming the
  issuer and both numbers. The **same** number with a different **TBSCertList** is an equivocation:
  not added, reported loudly (`EventConflict`, with both TBS hashes). The comparison is on the TBS,
  not the whole DER: ECDSA and RSA-PSS signatures are randomised, so a CA that re-signs one CRL
  (HA publishers, EJBCA clusters, two AD CS CDPs) is the same CRL, not a conflict. CRL numbers are
  compared as big integers (up to 20 octets). The mark is per issuer (partitioned CRLs are refused, so
  there is one scope). It is rebuilt at startup from the archive (see 5), so a restart does not forget
  it; an empty snapshot after a restart would otherwise accept any CRL still inside its validity.
  **The mark has an exit.** A CA that issues one CRL with a mistaken huge number, or is restored from
  backup and restarts its numbering, would otherwise leave every later real CRL a "rollback" and the
  gates for that issuer paused. `ResetHighWater(issuer, reason)` (and `bide identity fetch-crl
  -reset-high-water <issuer> -reason ...`) drops the mark; it reports `EventHighWaterReset` with the old
  mark, the reason and the operator, and the CLI writes it to the archive directory's log as a security
  event. It never removes a CRL from the snapshot or the archive.
- **Verify before adding.** Order: size, parse, algorithm allow-list (the snapshot's, never SHA-1),
  the signature verified against **the entry's own supplied issuer certificate** (not "any issuer in
  the snapshot": a CRL of issuer B served from A's URL must fail), issuer name and authority key id
  match, `DistrustAfter` for that key, then the future-dating check (`thisUpdate` more than
  `skew + 1 minute` ahead of the clock is refused). Only then `AddCRL`. A parse or signature failure
  never touches what is held.
- **Expired on arrival** (`nextUpdate` already past): archived and added (an old CRL fetched late
  fills a gap in the series an offline audit reads), not counted as a success for the health figures,
  reported as `EventStaleOnArrival`.
- **Delta CRLs** are fetched from `delta_urls` on their own schedule (`I` computed from the delta
  period if declared in `CAPeriodFor`-style `DeltaPeriodFor`, else `I`). A delta names the base number
  it is cumulative since; the status rule pairs it with a held base numbered in
  `[baseNumber, deltaNumber)` (big integers). A delta whose number is not above a newer held base's
  number is obsolete: archived, not paired. If no such base is held, the refresher fetches the base immediately and
  retries the delta once; a delta whose base cannot be obtained is still archived and added (it never
  gives evidence of absence alone) but reported as `Paired: false`, and the issuer's freshness is the
  base's. When both are held, freshness is the merged view's (earlier `nextUpdate`, delta's
  `thisUpdate`), as the status rule uses it.

### 4. Concurrency with the gate

`StatusSnapshot` is already mutex-guarded; `AddCRL` parses and verifies outside the lock and takes it
only to insert. The refresher does all network I/O, parsing and archiving outside any snapshot lock,
and calls `AddCRL` once per accepted CRL; the gate reads through `viewsFor` under the same mutex, so a
gate sees either the snapshot before or after an insert, never a partial CRL. A CA that stops publishing deltas must not leave a stale delta looking current: freshness is the merged
view's earlier `nextUpdate`, and a test covers it. Required race tests:
many gate evaluations against a snapshot while the refresher rotates CRLs (race detector), and the
refresher stopping (context cancel) mid-fetch and mid-archive with no goroutine left behind
(`goleak`-style check).

Eviction: the snapshot bounds the series (`WithMaxCRLs`, default 1024). The refresher reads the
bound and refuses to start when it would hold fewer than the CRLs of the longest validity window the
policy needs (`MaxStatusAge / CAPeriod + 2`, bases and deltas), because evicting a CRL the live rule
needs makes status staler. The long series for offline evidence lives in the archive, not the
snapshot.

### 5. Archive and restart

`CRLArchive` is an interface (`PutCRL(ctx, issuer SPKIHash, info CRLInfo, source string, der []byte)`
and `Load(ctx, yield)`), with `audit.DirCRLArchive` the directory implementation using the layout
the identity note already fixed (`crl/<issuer-key-id>/<number>-<thisUpdate>.crl`): write to a
temporary file, fsync, rename, fsync the directory; idempotent by hash.

- Order: **archive, then add to the snapshot.** If the archive write fails, the CRL is **still added**
  (a CRL listing a revoked certificate must reach the gate; withholding it would make status look
  better, and the journal's own evidence of what a gate relied on is separate: inline or through
  `CertApprover.StatusArchive`). The failure is reported (`EventArchiveError`), the CRL is kept in a
  bounded in-memory retry queue and retried with the backoff, and `Health()` shows `ArchiveBacklog`;
  a series with a hole makes offline verdicts non-final, so the operator must hear of it.
- The retry queue is bounded by bytes, drops the oldest first, and reports `EventArchiveDropped` for
  each drop, so a hole in the series is known and never silent. `PutCRL` is safe on a directory shared
  by several replicas: names are content-derived and the rename is atomic (file fsync, rename,
  directory fsync, in that order). The directory is created `0700`, an existing one that is
  world-writable or not owned by the process user is refused (it is the rollback memory), and files
  are written with explicit modes.
- At start, `Load` seeds the high-water marks (from file names, cheap) and the snapshot through the
  same verification path. The archive is never pruned (a 30-minute CA is about 17,500 CRLs a year per
  issuer), so seeding goes **newest first** and loads only what the live rule needs: the newest
  CRLs within `MaxStatusAge` plus the base of any delta, bounded by `WithMaxCRLs` and a startup time
  budget. Seeding does **not** count toward the snapshot's eviction bookkeeping (`evictedNew`): it
  uses an internal insert that never evicts, since loading oldest first and evicting would raise
  `evictedNew` and make live status non-final for no reason. A seeded CRL is judged by its own dates,
  so seeding cannot make anything fresher than it was. Without an archive the refresher runs, but a restart forgets rollback protection and says so at
  startup.

### 6. Observability

No metrics library in the core. Two surfaces:

- `Hooks` (callbacks, like `AuditedStore.OnError`): `OnEvent(Event)` for every attempt and outcome
  (`EventFetched`, `EventUnchanged`, `EventRollback`, `EventConflict`, `EventBadSignature`,
  `EventOversize`, `EventTimeout`, `EventRedirectRefused`, `EventFutureDated`, `EventStaleOnArrival`,
  `EventArchiveError`, `EventClockSkew`) and `OnAlarm(Alarm)` on entering and leaving an alarm state.
  Errors, the startup printout of effective URLs and `Health()` carry the issuer, the URL with its query
  and fragment removed (a token in a query string is never logged), and a reason; never a body. The
  startup lines (effective URLs, per-issuer limit and margin) are also one machine-readable JSON line
  each, so a platform team can alert on a margin under one period.
- `Health()` returns, per issuer: `LastAttempt`, `LastSuccess` (a fresh CRL accepted), `NewestNumber`,
  `NewestThisUpdate`, `NewestNextUpdate`, `Age` (`now - thisUpdate`, with no skew credit and never from the fetch
  time: the gate judges the same way, so the alarm cannot lag the pause), `NextExpected` (`thisUpdate + skew + CAPeriod`), `NextAttempt`, `ConsecutiveFailures`,
  `LastError`, `ArchiveBacklog`, `Paired`, `Alarm`, `Loop` (last time its goroutine ran, a watchdog
  for a dead loop), and the effective URLs. `cmd` exposes it as a JSON line per change and an
  optional listen address for a scrape; the library never opens a port.
- **Alarm.** The promise in the guide is "alarm at 50% of validity". Taken literally it is wrong for
  a weekly root: its status limit is about one period, so half of it is inside the normal cycle and
  would alarm every week. Defined once: `limit = min(applicable MaxStatusAge, applicable
  ExecStatusAge)`, taken as the **minimum over the issuer's roles** (it can be the leaf's issuing CA
  for one approver and a CA tier for another), computed by the same function `EvaluateStatus` uses,
  with a test that Age and limit agree with it at the boundaries; alarm when `Age >
  min(max(limit/2, CAPeriod + CAOverlap + tolerance), limit - I)`; `Critical` when `Age > limit`
  (gates are pausing now). The `limit - I` cap keeps the alarm ahead of Critical when
  `ExecStatusAgeFor` is only just above period plus overlap (which validation allows); startup refuses
  a configuration whose `limit - (CAPeriod + CAOverlap)` is under `tolerance + I` for a CA tier, and
  warns for the leaf's CA. For a fast CA the first term governs (a missed
  CRL is flagged long before the gate pauses); for a root the second does, which is why the guide
  tells the operator to give the root CRL a validity of two periods and set `ExecStatusAgeFor` to it:
  then the alarm is one full period before any gate pauses. The refresher logs the margin it computed
  per issuer at startup, and warns when the margin is under one period.

### 7. Failure behaviour

- A failed fetch changes nothing held. The snapshot has no expiry of its own; staleness is judged by
  `EvaluateStatus` from `Now`, `nextUpdate` and `MaxStatusAge`, so a dead refresher, a dead CA or a
  blocked network ends in the existing outcome: gates pause (`agent.PendingIdentityStatus`), the
  intake answers 503 with `Retry-After`. The refresher adds no new pause or deny path.
- Nothing it does can raise freshness: no touch-up of dates, no use of `Date`, `Last-Modified` or
  `304`, no re-adding of a held CRL as new.
- A panic in one issuer's loop is recovered, reported (`EventPanic`) and the loop restarts with
  backoff; a stuck loop shows in `Health().Loop`.
- `Run(ctx)` returns on cancel after in-flight requests end (they carry the context). `RefreshOnce`
  runs one pass for every issuer and returns the per-issuer outcome (for cron use and tests).
  `Ready()` closes once every issuer has a fresh CRL; the program may wait on it before serving, but
  nothing requires it: until then the gates pause, as they do today.
- Local clock error: the HTTP `Date` header is compared with the local clock and a difference over 5
  minutes raises `EventClockSkew` as a warning only; so does a future-dated `thisUpdate` beyond
  skew plus a minute when several issuers show it at once (a slow local clock). Neither is used for
  a decision. `NewCRLRefresher` refuses an approver with a nil `Now`.

### 8. How a program enables it, and the CLI

```go
approver := &audit.CertApprover{ /* as today, Status: audit.NewStatusSnapshot(audit.WithIssuers(...)) */ }
r, err := audit.NewCRLRefresher(approver, sources, audit.RefresherOptions{
    Archive: audit.DirCRLArchive("/var/lib/bide/crl"),
    Hooks:   audit.RefresherHooks{OnEvent: log, OnAlarm: page},
})
go r.Run(ctx)
```

- A **separate type**, not an option on `CertApprover`, built from the approver so the interval,
  the alarm and the issuer set come from the same declared `StatusParams`, `ExecStatusAge*` and
  `CAOverlapFor` the gate uses (no second copy to drift). `NewCRLRefresher` returns `ErrConfig` when
  `approver.Status` is nil, when a reachable issuer has no source, or when the declared parameters
  leave no positive interval.
- **`bide identity fetch-crl -policy file -cache dir [-once | -every]`**: the cron/daemon form of the
  same code (the identity note already names this command), writing the archive layout and
  exiting non-zero on any issuer that is not fresh with `-once`. Issuer certificates come from the
  policy's `intermediate_certs`. Durations for a daemon come from
  flags (`-period`, per-issuer overrides in a `-params` JSON), since the policy file carries no
  status parameters today.
- **`bide approve -cert`** does **not** fetch. Its local check is a convenience before signing (it
  already says it judges no status); fetching from an approver's workstation would need egress to
  the bank's CRL points and would suggest a verdict only the gate gives. A later `bide identity check`
  may read the cache dir the refresher keeps.

## Tests (step 2)

An `httptest` CRL server driven by the test CA already in `audit` (rotation by number; stale CRL;
oversize; slow-drip and header timeout; wrong signature; a CRL for another issuer on this issuer's
URL; rollback; same-number conflict; delta with its base, delta whose base is missing, base rotation
under a delta; future-dated `thisUpdate` and declared skew; expired on arrival; redirect to the same
host, to another host, https to http; PEM body; 304; Retry-After); a fake clock and timer so no test
sleeps; the schedule function table-tested (interval, catch-up, jitter bounds, backoff cap and reset);
restart seeding and high-water mark from the archive; archive failure and retry; alarm thresholds for
a 30-minute CA and a weekly root; race tests against concurrent gate evaluation; goroutine-leak and
cancel tests. Mutation-check the rollback, the issuer binding of the signature check, the
future-dating check, the size limit, the redirect rule and the alarm threshold.

## Documentation to change with the build

Operator guide (replace "bide does not fetch CRLs", the alarm text, the Large CRLs and monitoring
paragraphs), KNOWN-LIMITATIONS (items about fetching and the alarm), CLI reference (`identity
fetch-crl`), API reference (godoc, regenerated), identity note (the refresher row and the step-2
text), CONCEPTS, changelog fragment.

## Open for the lead (recommendations in brackets)

1. Separate `audit.CRLRefresher` built from the `CertApprover`, not a field on it [yes].
2. URLs in the policy file under `crl`, not in `PolicyDigest`; `from_pinned_cdp` opt-in only for the
   CA tiers, default off [yes].
3. Alarm redefinition (`max(limit/2, period + overlap + tolerance)`, plus `Critical` at the limit)
   instead of a literal 50% [yes: the literal figure alarms weekly on a healthy root; the guide's
   advice becomes "root CRL validity of two periods, `ExecStatusAgeFor` equal to it"].
4. On archive failure, still add to the snapshot, alarm, and retry [yes: withholding a CRL would
   make status look better].
5. `bide identity fetch-crl` in this PR (with `-once` and a daemon mode); `approve -cert` stays
   fetch-free; `bide identity check` is a separate later PR [yes].
6. HTTP/HTTPS only; LDAP stated as unsupported [yes]. OCSP out of scope [yes].
7. Interval default tightened to `min(CAPeriod/2, min(MaxStatusAge, ExecStatusAge)/4)` [yes].
8. Review by pki-auditor-2 (2026-10-16) folded in: supplied intermediate certificates, TBS comparison,
   high-water reset, query-all-URLs, alarm cap and age definition, seeding rules, transport additions.
