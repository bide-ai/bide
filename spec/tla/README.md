# Formal models

TLA+ models of bide's coordination protocols, written in PlusCal and checked with the TLC model
checker. The plan and the reasoning behind it are in the design note
[formal models of the coordination protocols](../../docs/design/formal-models.md);
this directory holds models 1 (the claim protocol), 1b (the approval gate), 2 (the bide protocol's claim rules), 7 (flow semantics), 8 (spend accounting) and 10 (the run lifecycle and recovery) of that plan.

What a model check establishes, stated narrowly: within the bounds a configuration states (drivers,
processes, faults, attempt numbers), TLC explores every interleaving of the modelled rules and every
placement of the faults, and checks each property in every reachable state. Nothing is proven
beyond the bounds. The model states the protocol's rules; it does not read the Go code. The map
below says which Go function each model step abstracts, and it is checked by review, not by a tool,
until trace validation (milestone M3 of the plan) lands.

## Running it

```sh
spec/tla/check.sh                 # translation check, then every ci, regress and finding config
spec/tla/check.sh nightly         # the larger configs the nightly job runs
spec/tla/check.sh run spec/tla/claims/step-same.cfg
spec/tla/check.sh translate       # re-translate the PlusCal after editing it
spec/tla/check.sh self-test       # prove a stale translation and a wrong jar checksum both fail
```

The script needs Java 11 or later (`JAVA_HOME` or `java` on `PATH`), `curl`, and `sha256sum` or
`shasum`. It downloads the TLA+ tools pinned in [tools.lock](tools.lock) (`tla2tools.jar` from the
v1.7.4 release) into `~/.cache/bide-tla` (or `$BIDE_TLA_CACHE`) and refuses a jar whose SHA-256
differs from the lock file. `TLC_WORKERS` sets TLC's worker count (default `auto`),
`TLC_JAVA_OPTS` adds JVM options, and `TLC_KEEP_OUTPUT=<dir>` keeps each run's full TLC output
(counterexample traces included).

The PlusCal algorithm and its TLA+ translation live in one file, between the
`BEGIN TRANSLATION` and `END TRANSLATION` markers. CI translates a copy and fails if the committed
translation differs, so a PlusCal edit cannot merge without its translation.

Each configuration declares its group and its expected result in two comment lines:

```text
\* GROUP: ci | nightly | regress | finding | limit
\* EXPECT: pass | invariant <Name> | liveness
```

A `pass` configuration must finish with no error, and is then run a second time with the single
invariant `EffectNotReachable`, which TLC must report violated: a model in which the effect can never
fire would satisfy every safety property, so this vacuity check is part of every passing run. An
`invariant <Name>` configuration must stop with exactly that invariant violated (TLC exit status
12), and a `liveness` configuration with a temporal property violated (exit status 13); anything
else fails the check.

## In CI

The workflow `.github/workflows/models.yml` has two jobs. **Models** runs on every pull request, in
the merge queue and on pushes to main: the self-test, the translation check, and every `ci`,
`regress` and `finding` configuration. On a pull request, its steps run only when the pull request
touches the models or the Go code they describe (`spec/tla/`, `agent/`, `store/`, `plan/`,
`internal/journalhook/`, or `models.yml` itself); otherwise the job reports success after printing
that no modelled code changed, so it can be a required check without costing every documentation
change six minutes. The merge queue and main always run in full, and so does any doubt (a failed
diff, an unexpected event). A pull request that changes the claim code outside those paths must
widen the filter in the same pull request. **Models (nightly)** runs the `nightly` configurations
on a schedule and on demand (`workflow_dispatch`). The Go counterpart, the full-bound fault-schedule
explorations of the claim protocol and of flow lowering (`BIDE_EXPLORE=1`), runs nightly in
`.github/workflows/explore.yml`; see [verification](../../docs/testing/verification.md).

## Layout

```text
spec/tla/
  README.md          this file
  tools.lock         pinned tool versions and SHA-256 checksums
  check.sh           fetch, translation check, TLC runs, expected-result checks
  claims/
    Claims.tla       model 1: the PlusCal algorithm, its translation, crashes, late commits,
                     and the properties
    ClaimsMC.tla     named placements of drivers, processes and calls, and symmetry sets, which
                     the configurations select with <-
    *.cfg            the configurations (group ci or nightly)
    regress/         historical rules, each of which must still produce its counterexample
    findings/        open findings, which fail until they are fixed (none open at present)
    limits/          accepted behavior, stated as an expected violation
  flows/, protocol/, spend/, lifecycle/
                     models 7, 2, 8 and 10, laid out the same way
```

## Model 1: claims and attempts

### What is modelled

One run; one or two logical calls (a tool call that is not retry-safe, or a Step that is not); for
each call, its attempt markers (`attempt:tool:<id>` or `attempt:step:<name>`, then
`attempt:retry:<n>:...`), the not-started keys `attempt:not-started:<claim>:<marker>`, and its
result key. Drivers belong to processes: two drivers of one process share `pendingClaims` and the
in-flight step calls (`shareFlight`), as two Journals over one store value do; drivers of different
processes share only the store. Optionally, an operator resolving halts (`ResolveHaltRef`), and a
caller that issues a new call when it reads a failure (the model's own conversation).

Every `Store.Insert` is one atomic step with three replies: `ok` (the first writer of the key wins,
A1), `err_nc` (error, not committed) and `err_c` (error, committed). The caller sees only "error".
Under `LateCommit` (weak A3, open question 2 of the plan) a fourth reply, `err_late`, returns an
error and commits at any later step, or never. A process crash may happen at any step of any of its
drivers, which is also every placement of "crash before" and "crash after" a write. A cancellation
may land between a won claim and the call.

Abstracted away: record encoding, salts, `Seq` and pagination (A2 is assumed); the journal header;
failed reads (a failed read fails the drive and changes nothing, so it is a re-drive); lease
internals (a leased driver holds the run's lease for its whole drive, and a crash releases it);
wall-clock time (`WithMinHaltAge` is an assumption, below); a lease that expires under a live
holder (two leased drivers exclude each other here; a holder that stalls past its TTL is model 3); the transitional `Durable` path
(`durableStep`, `claimAttempt`); retry-safe Steps and tools, which write no marker.

Also modelled, each in its own configurations:

- **One marker key (`Kind = "flow"`).** `ClaimAttempt` on a single key, as a plan flow node that is
  not retry-safe uses it (`plan/flow.go`): the drive halts on any marker of the step, voided or
  not; the claim retries remembered ids and takes a fresh id as `claimNext` does, but a lost claim
  halts, and a body error records nothing.
- **The `maxPendingClaims` eviction (`MaxEvict`).** At any step, a process may forget every id it
  remembers for one marker key.
- **An operator in a driver's process (`ResolverProc`).** The resolution's `ClaimAttempt` shares
  that process's `pendingClaims`, and its `store.Do` on the result key shares the process's
  in-flight calls (`shareFlight`): a driver's winning call can join the resolution's write and the
  resolution can join a driver's call. The process's crash kills the resolution.

Claim ids are allocated as the smallest id nothing in the state refers to, so re-drives do not grow
the state space; the protocol compares ids only for equality, which makes this sound.

### The rules

The model states the rules of P6a (#92) after its third review:

1. Every claim inserts its marker under a fresh claim id, so a won claim's not-started key is
   empty (`WonKeyEmpty`), and only a not-started record voids an attempt.
2. If the marker's Insert errors, the claim records a not-started record under its own id. Any
   failed not-started write (`Journal.notStarted`, wherever it is called from) leaves the id
   remembered in the process, in `pendingClaims`, which holds a set of ids per marker key.
3. A remembered id is never used to run. The next claim of that marker key in the process takes
   back every remembered id and writes each one's not-started record again (voiding its marker if
   that committed; a failed write remembers the id again), then claims with a fresh id; a voided
   marker moves the claim to the next numbered attempt.
4. The tool resume gate, before it halts on a live marker, retries the not-started record of that
   marker's claim if the process remembers it; if the retry succeeds, the call claims its next
   attempt instead of halting.
5. A Step that loses the claim joins the process's in-flight call of the step (a failed call is a
   `HaltContended` halt), or reads the result, or halts (`HaltCrashed`); it never starts a flight
   of its own. A claim winner joins a flight already in
   the process and, if it fails, records its own attempt as not started.
6. A tool that loses the claim halts (`HaltContended`).
7. Halt resolution refuses while a live driver may be running: under a store that leases runs, it
   holds the root run's lease while it resolves (MemStore, `store/postgres`, and `store/sqlite` as
   of #92), and only leased drivers are seen; otherwise it needs `WithMinHaltAge`, and then (F2's
   fix) it first claims the attempt after the live one with `ClaimAttempt`, refusing if a driver
   holds it; if its result write errors, that attempt stays live (F3's fix). It claims on the
   lease path too (F4's fix); `WithoutLiveDriverCheck` skips both the check and the claim.

### Model-code map

Each label is one atomic step: one store round trip or one local decision. Function names are
those of #92 (P6a); where #92 has not yet adopted a rule, the step names the rule.

| Label | Go |
|---|---|
| `Start` | the caller issuing the call: a model turn naming the tool-use id, or code calling `Step` |
| `Open` | the drive's one `Load` (`Journal.open` in `Agent.run`; `journalStep`'s `j.Get`); on the tool path, `liveAttempts` and the resume gate's loop in `Agent.run` |
| `GateTake`, `GateWrite` | the resume gate's retry of a remembered claim (rule 4): `pendingClaims` membership of the live marker's id, then `Journal.notStarted` |
| `Claim` | one iteration of `Journal.claimNext`: `pendingClaims.take` in `Journal.claim` |
| `ClaimRetry` | `Journal.claim`: `pendingClaims.takeAll`, then `Journal.notStarted` for each taken id (rule 3) |
| `ClaimInsert` | `newClaimID`, `Journal.insert` of the marker (`Store.Insert`), and the `got.claim == id` check |
| `ClaimNS` | `Journal.claim`'s error path: `Journal.notStarted`, which remembers the id on failure |
| `Hold` | historical only (`Bug = "HeldPin"`): `Journal.holdClaim` and `StepClaimHeld` |
| `Lost` | `Journal.claimNext` and `Journal.voided`; on the tool path the `!won` halt of the tool call |
| `Join`, `LoserRead`, `LoserWait` | `journalStep`'s loser branch: `joinFlight`, then `j.Get`, else the halt |
| `LoserLead` | historical only (`Bug = "LoserLeads"`): the loser's probe through `Do` starting the flight |
| `Win`, `WinnerWait` | `Journal.doFresh` (`recordFresh` on the tool path): `shareFlight` on the result key |
| `Call` | the `doFresh` closure: `journalStep`'s `ctx.Err()` check and `started.Store(true)`, the tool call's `sctx.Err()` check and `called.Store(true)`; a Step body that pauses returns `stepPauseError` |
| `Record` | `Journal.insert` of the result inside `doFresh` |
| `NotStarted` | `recordNotStarted` / `Journal.notStarted` after `!started` or `!called` |
| `Finish` | the drive's end; any outcome but the call's result is driven again (`Recover`, `RecoverLoop`, a re-run) |
| `RCheck` | `resolveHalt`: `checkNoLiveDriver` (the lease), the `WithMinHaltAge` check, `liveAttempts` |
| `RClaim`, `RRetry`, `RInsert`, `RClaimNS` | F2's fix, `resolveHalt`'s `ClaimAttempt` on `nextAttemptStep` of the live attempt (`ResolveClaim = TRUE`): `Journal.claim`'s retry of remembered ids, its fresh-id Insert, and its error path |
| `RWrite`, `RRecord`, `RWait` | `resolveHalt`'s `store.Do` on the result key: `Journal.Do` through `shareFlight` when the resolver runs in a driver's process |
| `RNotStarted` | `resolveHalt`'s `recordNotStarted` of its own attempt after an errored write (`ResolveVoidOnError = TRUE`, #92 at `06408db`; finding F3) |
| `RRelease` | the lease's release |
| `Crash(p)` | a process dies: its drives restart, its `pendingClaims` and flights are gone, its lease lapses, a resolution running in it stops |
| `Evict(p)` | `claimMemo.remember` dropping the oldest marker keys past `maxPendingClaims` |
| `LateApply` | weak A3 only: a write that returned an error commits now |

`WithMinHaltAge` cannot be checked in an untimed model. `LiveCheck = "minAge"` encodes it as the
assumption it rests on: a drive that holds the live attempt's claim (`Window`: from the claim's
error path or its win until its result or not-started record) is over before the minimum age has
passed. A claim remembered in `pendingClaims` is not a drive, and its process may retry its
not-started record at any later time; finding F2 is about exactly that.

### Properties

| Property | Kind | Statement |
|---|---|---|
| `AtMostOnce` | invariant | the effect of each call fires at most once |
| `AtMostOncePerIntent` | invariant | across a call and the call the caller issued after reading it failed, at most one fire |
| `NotStartedExclusive` | invariant | no not-started record exists for a claim whose effect fired |
| `WonKeyEmpty` | invariant | a won claim's not-started key is empty (rule 1) |
| `NoLiveOverride` | invariant | a driver that called the effect never finds a resolution in place when it records |
| `WinnerNeverHalts` | invariant | a driver that won a claim never returns a halt |
| `AtMostOneLive` | invariant | at most one driver-claimed attempt of a call is live |
| `BoundNotHit` | invariant | no driver is ever blocked by the attempt bound or the id pool |
| `ResultStable` | action | a recorded result is never replaced |
| `Progress` | liveness | a provably unstarted effect does not halt for ever |
| `ProgressModuloEviction` | liveness | `Progress`, also excusing a live attempt whose remembered claim an eviction forgot |
| `EffectNotReachable` | vacuity | must be violated in every passing configuration |

`Progress` says that each issued call eventually has a recorded outcome for ever, unless the effect
fired or a crash erased the only knowledge that a live attempt never called it:

```tla
Excused(c) == fired[c] > 0 \/ \E x \in Gens : Live(c, x) /\ marker[c][x] \in lost
Progress   == \A c \in Calls : <>[](~Issued(c) \/ Recorded(c) \/ Excused(c))
```

Fairness: every driver step is weakly fair, so a driver that can re-drive does. Crashes, error
replies, cancellations and late commits are faults with finite budgets and no fairness, so TLC
checks every order in which they happen and then stop. The operator is never assumed to resolve
(its check step is not fair; once it has checked, it writes).

Bound sufficiency, so a bound can never pass for a liveness failure or hide one: every way to void
an attempt consumes an error reply or a cancellation, and `MaxGen` is at least the sum of those
budgets (one more with a resolver that claims); the claim-id pool is larger than the ids the state
can hold at once; and `BoundNotHit` fails if a driver or the resolver is ever blocked on either
bound, so a bound that is too small is reported as a configuration error, not as a result.

Symmetry: in the configurations whose drivers share a process and a call, the drivers are a
symmetry set (`DriverSym`, `PairSym`). TLC does not combine symmetry with liveness, so the liveness
configurations run without it, and so do the configurations with drivers in different processes.

### Configurations

On every pull request and in the merge queue (`ci`, `regress`, `finding`, `limit`). States are distinct
states; times are TLC's own, measured on a development machine (Apple M1 Pro, 8 workers). The
whole pull-request set, vacuity runs and JVM starts included, takes about 6.5 minutes on the CI runner
(GitHub `ubuntu-latest`, 4 cores).

| Config | Path | Drivers, processes | Faults (error replies, crashes, cancels) | Attempts | Property | States | Time |
|---|---|---|---|---|---|---|---|
| `step-same` | Step | 2 in 1 | 2, 1, 1 | 0..3 | safety | 2,282,585 | 12 s |
| `step-cross` | Step | 2 in 2 | 2, 1, 1 | 0..3 | safety | 2,004,380 | 11 s |
| `tool-same` | tool | 2 in 1 | 2, 1, 1 | 0..3 | safety | 1,417,253 | 8 s |
| `tool-cross` | tool | 2 in 2 | 2, 1, 1 | 0..3 | safety | 2,760,599 | 17 s |
| `resolve-lease` | tool, resolver (lease) | 2 leased in 2 | 2, 1, 1 | 0..3 | safety | 80,919 | 1 s |
| `resolve-minage-claim` | tool, resolver (min age, the F2 fix) | 2 in 2 | 2, 1, 0 | 0..4 | safety | 320,662 | 3 s |
| `intent` | two Steps that pause, the caller, resolver (lease) | 2 leased in 1 | 1, 1, 0 | 0..2 | safety, per intent | 1,370 | <1 s |
| `intent-minage-claim` | two tool calls, the caller, resolver (min age, the F2 fix) | 2 in 2 | 2, 0, 0 | 0..3 | safety, per intent | 662 | <1 s |
| `live-step-same` | Step | 2 in 1 | 1, 1, 1 | 0..2 | `Progress` | 291,262 | 9 s |
| `live-tool-same` | tool | 2 in 1 | 1, 1, 1 | 0..2 | `Progress` | 128,325 | 4 s |
| `live-tool-cross` | tool | 2 in 2 | 1, 1, 1 | 0..2 | `Progress` | 144,243 | 5 s |
| `resolve-minage-claim-a3` | tool, resolver (min age, F2 and F3 fixed) | 2 in 2 | 3, 0, 0 | 0..4 | safety | 702,817 | 6 s |
| `resolve-lease-claim` | tool, resolver (lease, F4's fix), a leased driver and a plain Run | 2 in 1 | 3, 0, 0 | 0..4 | safety | 331,475 | 6 s |
| `resolve-in-proc-tool` | tool, resolver in the drivers' process (min age, fixes) | 2 in 1 | 2, 1, 0 | 0..4 | safety | 285,231 | 4 s |
| `flow-same` | one marker key (plan flow) | 2 in 1 | 2, 1, 1 | 0 | safety | 6,588 | <1 s |
| `flow-cross` | one marker key (plan flow) | 2 in 2 | 2, 1, 1 | 0 | safety | 14,357 | 1 s |
| `evict-tool-same` | tool, 1 eviction | 2 in 1 | 2, 1, 1 | 0..3 | safety | 1,553,944 | 15 s |
| `live-evict-step-same` | Step, 1 eviction | 2 in 1 | 1, 1, 1 | 0..2 | `ProgressModuloEviction` | 304,754 | 10 s |

Nightly (and on demand, `workflow_dispatch`):

| Config | Path | Drivers, processes | Faults | Attempts | Property | States | Time |
|---|---|---|---|---|---|---|---|
| `live-step-same-a2` | Step | 2 in 1 | 2, 1, 1 | 0..3 | `Progress` | 4,541,292 | 2 min |
| `live-tool-same-a2` | tool | 2 in 1 | 2, 1, 1 | 0..3 | `Progress` | 2,817,680 | 1 min |
| `live-tool-cross-a2` | tool | 2 in 2 | 2, 1, 1 | 0..3 | `Progress` | 2,760,599 | 1 min |
| `live-memo-a3` | Step | 2 in 1 | 3, 0, 1 | 0..4 | `Progress` | 1,772,048 | 49 s |
| `deep-faults` | Step | 2 in 1 | 3, 1, 1 | 0..4 | safety | 31,402,708 | 2 min |
| `deep-drivers` | Step | 3 in 2 (2 + 1) | 1, 1, 1 | 0..2 | safety | 25,004,792 | 2 min 51 s |
| `deep-late-step-same` | Step, weak A3 | 2 in 1 | 2, 1, 1 | 0..3 | safety | 6,639,013 | 38 s |
| `deep-late-tool-same` | tool, weak A3 | 2 in 1 | 2, 1, 1 | 0..3 | safety | 3,040,107 | 17 s |
| `deep-late-step-cross` | Step, weak A3 | 2 in 2 | 2, 1, 1 | 0..3 | safety | 5,649,510 | 43 s |
| `deep-late-tool-cross` | tool, weak A3 | 2 in 2 | 2, 1, 1 | 0..3 | safety | 5,522,592 | 31 s |
| `deep-resolve-lease-same` | Step, resolver (lease) | 2 leased in 1 | 2, 1, 1 | 0..3 | safety | 79,460 | 1 s |
| `deep-evict-step-same` | Step, 1 eviction | 2 in 1 | 2, 1, 1 | 0..3 | safety | 2,602,491 | 31 s |
| `deep-resolve-in-proc-step` | Step, resolver in the drivers' process | 2 in 1 | 2, 1, 0 | 0..4 | safety | 5,715,586 | 41 s |
| `deep-resolve-in-proc-cross` | Step, resolver in d1's process | 2 in 2 | 2, 1, 0 | 0..4 | safety | 2,067,129 | 17 s |
| `deep-resolve-in-proc-a4` | Step, resolver in the drivers' process | 2 in 1 | 4, 0, 0 | 0..5 | safety | 15,988,971 | 2 min |
| `deep-resolve-lease-claim` | tool, resolver (lease, F4's fix), a leased driver and a plain Run | 2 in 1 | 2, 1, 1 | 0..4 | safety | 3,663,147 | 1 min |

The nightly set takes about 20 minutes on the development machine. The liveness checks run on every pull
request with one error reply; with two they run nightly, since liveness checking cannot use symmetry
and costs several times a safety check of the same states. `deep-late-tool-cross` is nightly only
to keep the pull-request job short, and so is `deep-late-step-same`; weak A3 runs nightly.

### Regression configurations

Each is a rule an earlier review found wrong, restored behind the `Bug` constant. CI requires each
to fail with its named property; one that stops failing means the model lost the behavior or the
property weakened.

| Config | The historical rule | Expected | Trace |
|---|---|---|---|
| `regress/reuse-nohold` | #92 second review: a remembered claim id is reused for the claim and runs (`Bug = "ReuseNoHold"`) | `NotStartedExclusive` | 15 states |
| `regress/reuse-nohold-fire` | the same, checked for the double fire itself | `AtMostOnce` | 24 states |
| `regress/held-pin` | #92 third review, finding 1: the claim-held pin absorbs the reused claim's not-started record after a cancellation (`"HeldPin"`) | `Progress` | 22 states |
| `regress/gate-no-retry` | #92 third review, finding 2: the tool gate halts on a claim its process remembers (`"GateNoRetry"`) | `Progress` | 10 states |
| `regress/no-memo-after-call` | #92 third review, finding 3: a failed not-started write after a cancelled call is not remembered (`"NoMemoAfterCall"`) | `Progress` | 18 states |
| `regress/memo-overwrite` | finding F1 below: `pendingClaims` holds one id per marker key (`"MemoOverwrite"`) | `Progress` | 37 states |
| `regress/loser-leads` | #92 first review, finding 3: the Step loser starts the flight and the winner takes its halt (`"LoserLeads"`) | `WinnerNeverHalts` | 21 states |
| `regress/pause-as-failure` | #92 first review, fix 1: a Step's pause guard recorded as its tool's failure (`"PauseAsFailure"`) | `AtMostOncePerIntent` | 16 states |
| `regress/resolve-no-check` | #90 F2: resolution with no live-driver check (`WithoutLiveDriverCheck`) | `NoLiveOverride` | 11 states |
| `regress/resolve-no-check-intent` | the same through the caller: "not charged", a new call, a second fire | `AtMostOncePerIntent` | 17 states |

### Accepted limits

Configurations in `limits/` state behavior the design accepts, as an expected violation, so it
cannot change silently:

- `limits/flow-progress`: a plan flow node whose claim errored after its marker committed halts
  for ever, since flows have no numbered re-attempts until P5b lowers them onto `claimNext`
  (`Progress`; 1 error reply, 9 states).
- `limits/evict-progress`: an eviction forgets the only record that a live attempt never started
  (`Progress`; 2 error replies, a cancellation and an eviction). `live-evict-step-same` shows the
  eviction costs nothing else, and `evict-tool-same` and `deep-evict-step-same` that it never
  costs safety.
- `limits/resolve-unleased-driver`: #90's documented limit. The lease check sees only drivers
  that hold the run's lease; a plain `Run` holds none, and a resolution overrides it
  (`NoLiveOverride`).
- `limits/lease-plain-run-claims-first`: the same limit through the caller, with the claim of the
  next attempt in place (suspicion (a) of the #103 re-review). A plain `Run` that holds the live
  attempt when the leased resolution reads the history, whether it claimed that attempt directly
  or after voiding the one before it through a remembered claim, is overridden, and a "not
  charged" verdict makes the caller's new call fire again (`AtMostOncePerIntent`, 19 states).
  `PlainRunIdleAtCheck` excludes exactly this case (a plain run holding the live attempt at the
  check) and nothing else, and with it `resolve-lease-claim` passes, so the suspicion is this
  limit, not a separate double fire.
- `limits/resolve-over-voided-attempt`: reachable and harmless (suspicion (b) of the #103
  re-review). Between the resolution's history read and its claim, a process retries a remembered
  claim's not-started record and voids the live attempt; the resolution still claims the next
  attempt and records its verdict for an effect that never ran (the probe
  `ResolveOnlyLiveAttempt` is violated, 14 states). The verdict the model's operator gives is
  true ("not charged"), no effect fires, and every safety property holds in the same model
  (`resolve-minage-claim-a3`, `resolve-lease-claim`): the driver that voided the attempt loses
  the next one to the resolution and reads its verdict.

### Findings

Found by this model in the rules #92 adopted; each is a counterexample, and each needs a
deterministic Go test before its fix (M2 of the plan). F1 to F3 are fixed in #92 at `42f7419`
and F4 at `2c8d2db`, and their counterexamples are regression configurations now.

- **F1: `pendingClaims` holds one id per marker key.** `claimMemo.remember` replaces the id stored
  for a key. Two claims of one marker key in one process can each fail a not-started write: a
  winner cancelled before the call whose record fails (its marker is live), and a concurrent
  claimant whose marker Insert and not-started record both error. The second `remember` replaces
  the first id, the retry then voids the wrong claim, and the live marker, whose effect never ran,
  halts for ever with no crash. Needs three error replies and a cancellation
  (`regress/memo-overwrite`). The model's rule 2 holds a set of ids per key, and every retry drains
  it; `live-memo-a3` checks that nightly.
- **F2: `WithMinHaltAge` does not cover a remembered claim.** The age is measured from the live
  attempt's marker, and the model assumes every drive holding that claim is over by then. But the
  claim's process remembers the id and may retry its not-started record at any later time (rules
  3 and 4): the retry voids the attempt, the process claims the next one and calls the effect, and
  a resolution checked just before writes its verdict first. The live driver's result is then
  lost, and if the verdict was "not charged", the caller asks again and the effect fires twice
  (`regress/minage-revoid`, `regress/minage-intent`: two error replies, no crash). The lease
  check is not affected, since leased drivers cannot drive while the resolver holds the lease. A
  fix the model checks (`ResolveClaim`, configs `resolve-minage-claim` and `intent-minage-claim`):
  before it writes, the resolver claims the attempt after the live one under its own id; a driver
  that voids the live attempt later loses that claim and reads the resolution, and if a driver
  already holds it, the resolution refuses. #92 adopted this fix at `06408db`.
- **F3: the F2 fix voids its own attempt after an errored write.** `resolveHalt` records its
  claimed attempt as not started when `store.Do` on the result key returns an error, but the
  write may have committed. A driver in its claim loop (it voided the live attempt through a
  remembered claim, and lost the next one to the resolution) then sees that attempt voided,
  claims the one after, and calls the effect under the recorded resolution; with a "not charged"
  verdict the caller asks again and the effect fires twice (`regress/resolve-void-on-error`,
  `regress/resolve-void-on-error-intent`: three error replies, no crash, 28- and 33-state
  traces). The model's fix (`ResolveVoidOnError = FALSE`, every other resolver config): after an
  errored write, the resolution's attempt stays live, and the call halts until it is resolved
  again. `resolve-minage-claim-a3` checks it with three error replies, and
  `deep-resolve-in-proc-a4` with four and the resolver in the drivers' process.
- **F4: on the lease path, a plain `Run` revives a leased driver's remembered claim.** The
  lease check sees only leased drivers (#90's documented limit), and the resolution does not claim
  the next attempt on that path. Even when no unleased driver holds the live claim at the check
  (`PlainRunIdleAtCheck`), a plain `Run` in the process of the leased driver whose claim errored
  takes that remembered claim, voids the live attempt, and claims the next one while the
  resolution records its verdict (`regress/lease-revival`: two error replies, no crash, 22
  states). The fix, adopted by #92 at `2c8d2db`: claim the next attempt on the lease path too
  (`resolve-lease-claim`, three error replies; `deep-resolve-lease-claim` nightly, with a crash
  and a cancellation). A plain `Run` that holds the live claim at the check stays #90's limit
  (`limits/resolve-unleased-driver`).

The `findings/` configs flip to `pass` when the code is fixed.

## Model 1b: the approval gate

An extension of model 1 in the same spec: before a tool call claims its attempt, the loop's
pre-pass applies the call's approval gate, and the claim protocol, crashes, faults and halt
resolution all run around it.

### What is modelled

- **1-of-1 (`Approve`).** `approval:<id>` holds the first decision recorded (`Approve1`, either
  verdict, at any time).
- **m-of-n (`SubmitDecision`).** Each decision is its own record, in journal order: the approver
  id it names, the person who signed, the verdict, the key it was signed with, and whether it was
  signed over this exact call or another call or other arguments (`Subjects`). People sign as an
  approver with a key they hold that a resolver maps to it (`KeyOf`, `KeyOf2`, `Holder`); an
  actor holding no key (`hx`) forges. An identical resubmission adds nothing.
- **Keys (#109).** Each approver's verifier accepts a set of keys (`KeyIDs`: several during a
  rotation, both components of a hybrid). The resolver (`keys`) may change once (`KeyOf2`), at any
  time, a count already under way included. The gate's check (`ValidateKeys`, every evaluation)
  refuses a policy with an approver the resolver knows with no key, or two approvers whose key sets
  meet; the count (`TallyApprovals`) never seats such an approver under the resolver as it is at
  the count, and leaves them out of the unreachable test, so a change can make the gate deny.
- **The counting rule (`TallyApprovals`).** An approver's decision is their first record whose key
  the approver's verifier accepts at the count and that was signed over this call; the tally passes with `need` approvals and is final once passed or
  unreachable. `quorumTally` reads the history afresh, uses a recorded tally if there is one,
  pauses on a count that is not final, and otherwise records the tally (first writer wins) and
  goes by the record the journal holds.
- **Deployments.** Each process has its own deployment of the tool's gate (`policy[p][c]`), which
  a redeploy may remove, loosen, tighten or change between that process's drives, while another
  process may still run the old one. `ApprovalPolicy.Validate`'s fold check (`FoldSame`) refuses
  a policy that lists one approver under two spellings.
- **A recorded denial is final.** The pre-pass denies a call whose Load read an `Approve(false)`
  or a failing recorded tally, whatever the gate is now (#70), and records the denial as the
  call's result.

| Label | Go |
|---|---|
| `ApGate` | `Agent.run`'s pre-pass: `decided`/`approvals` and `values` from the Load, then `t.Safety()` |
| `QTally` | `Agent.quorumTally`: `ApprovalPolicy.Validate` and the key check, `History`, a recorded `ApprovalTallyStep` |
| `QCount` | `TallyApprovals` with the resolver as it is now: shared and keyless seats excluded (`ReasonSharedKey`, `ReasonNoKeyID`) |
| `QRecord` | `quorumTally`'s `step` recording the tally (retry-safe, first writer wins) |
| `Deny` | the pre-pass's `putRecord` of the denied result |
| `Approve1` | `Approve` |
| `Submit` | `SubmitDecision` (without `WithDecisionCheck`: the gate never depends on it) |
| `Redeploy` | a new deployment of a process's tools |
| `ResolverChange` | the verifier resolver answering differently (a rotation, a new key file) |

### Properties

| Property | Kind | Statement |
|---|---|---|
| `NoUnapprovedFire` | invariant | an effect fires under a gate only with a recorded sufficient approval (an `Approve(true)` or a passing recorded tally) |
| `DenialFinal` | invariant | a drive whose Load read a recorded denial never fires the effect, whatever the gate is now |
| `TallySound` | invariant | a passing recorded tally rests on `need` valid approvals, signed over this exact call, by distinct people (the holders of the signing keys) |
| `DenialSound` | invariant | a failing recorded tally rests on valid denials and seats the count excluded: an invalid record cannot force a denial |
| `NoStuckPause` | invariant | the gate never pauses for approvals that are already in (no lockout) |

`AtMostOnce`, `NoLiveOverride` and the rest of model 1's properties are checked in the same runs.
A denial recorded after a drive's Load, on a process whose deployment has no gate, does not stop
that drive: the gate is gone, and no approval semantics applies to it; `DenialFinal` holds for
every drive that read the denial.

### Configurations

| Config | Group | What | States | Time |
|---|---|---|---|---|
| `ap-one-cross` | ci | 1-of-1, two drivers in two processes with their own deployments; a redeploy removes the gate or makes it m-of-n; two `Approve` calls; 1 error reply, 1 crash | 172,801 | 4 s |
| `ap-m-cross` | ci | m-of-n (2 of 3), two drivers in two processes recording the tally concurrently; two decisions from the approvers or the adversary; a redeploy tightens or loosens one process's policy; 1 error reply | 415,936 | 8 s |
| `ap-m-bound` | ci | m-of-n with decisions signed over another call; one driver, three decisions | 2,790 | <1 s |
| `ap-one-resolve` | ci | 1-of-1 with an operator resolving halts (lease check, claim of the next attempt); 1 error reply, 1 crash | 3,857 | <1 s |
| `ap-one-resolve-minage` | ci | 1-of-1 with an operator resolving halts under `WithMinHaltAge` and the claim of the next attempt; 2 error replies | 48,356 | 2 s |
| `ap-fold-check` | ci | the #68 fix: a policy listing one approver twice is refused | 222 | <1 s |
| `ap-shared-key-check` | ci | F5's fix (#109): a policy whose approvers share a key is refused | 102 | <1 s |
| `ap-keysets-check` | ci | key sets that meet without being equal ({k1, k4} and {k2, k4}) are refused | 2,406 | <1 s |
| `ap-resolver-change` | ci | the resolver changes between the check and the count (a2 now accepts a1's key); two drivers, three decisions | 75,730 | 2 s |
| `deep-ap-m-same` | nightly | m-of-n, two drivers in one process, three decisions, a redeploy; 1 error reply | 5,252,508 | 6 min |

Regressions: `regress/ap-denial-not-final` (#70: a recorded denial honoured only while the tool
had a gate; `DenialFinal`, 12 states), `regress/ap-unbound-subject` (approval v2: a signature over
another call counted; `TallySound`, 14 states), `regress/ap-slot-per-approver` (approval v2: one
record per approver, so a forged record took the approver's place and the gate waited for
approvals already in; `NoStuckPause`, 8 states), `regress/ap-fold-dup` (#68: one person filled
two seats under two spellings of one id; `TallySound`, 14 states), `regress/ap-shared-key` (F5,
fixed in #109: two approvers on one key with neither the check nor the count looking at keys;
`TallySound`, 9 states), `regress/ap-set-equality` (key sets compared for equality, not overlap;
`TallySound`, 9 states), `regress/ap-no-count-exclusion` (the count seating what the check passed
after the resolver changed; `TallySound`, 10 states). Accepted: `limits/ap-resolver-change-denies`
(a change that makes two approvers share a key excludes both seats, and the gate can deny with
no denial recorded; `NoDenyWithoutDenials`, 8 states).

### Findings

- **F5 (fixed in #109): two approvers whose verifiers resolve to one key were two seats for one
  person.** `ApprovalPolicy.Validate` refused one approver under two spellings, but nothing
  compared keys: if `verifierFor` resolved a1 and a2 to the same key, its holder signed both
  decisions, and the tally passed with one person's approval (`regress/ap-shared-key`,
  `TallySound`). #109 gives `ApproverVerifier` its key identities; the gate refuses a policy whose
  approvers' key sets meet, and the count never seats a shared or keyless approver.

## Model 2: the bide protocol's claim rules

`protocol/Protocol.tla` checks the claim rules of a remote side-effect tool call under the bide
protocol (`docs/design/protocol.md`, #95, revision 2, section 18). It is a model of its own rather
than an addition inside model 1, so model 1's cost is unchanged; it restates the claim rules the
protocol needs (a fresh-id marker, first writer wins, numbered attempts, a not-started record voids
an attempt, ambiguous inserts).

- **Claim at assignment (I1).** The drive claims the call's next attempt when it assigns a
  delivery to a polling worker: a remote marker naming the delivery and the worker. An errored
  claim records its own not-started record.
- **Transport.** A task may be lost, delivered late, or duplicated to a second worker presenting
  the same delivery id.
- **Begin (I2).** A worker runs its handler only after `BeginTask` answered begun: the engine's RPC
  handler (beside the drive, not on its scheduler slot) checks the dispatch table and the claim
  id, inserts `attempt:begin:<marker>` naming the delivery and the worker's nonce, and answers
  from the stored record. A lost answer is retried with the same nonce.
- **Abandon (I3).** The drive abandons a delivery that lapsed or that its engine instance does not
  know: it inserts an abandon under the begin key, and writes the not-started record only when the
  stored begin record is an abandon (also when a crashed instance left the abandon without it).
  After `MaxAttempts` voided attempts it records `DELIVERY_EXHAUSTED`.
- **Outcomes.** Only the begun delivery completes; `unknown` is journaled; `not_started` from a
  delivery that has not begun abandons it and is rejected from one that has.
- **Engine and worker crashes.** An engine crash empties the dispatch table and cuts off the RPC
  in progress (the worker retries); a worker crash forgets its task (no SDK-side durability, I6).
- **Retry-safe calls (10.9).** No marker and no begin (pull): the drive dispatches a delivery,
  and again under the same once-key scope after a lapse, an unknown report or a refusal, up to
  `max_deliveries`; any delivery may complete. When the window closes with no outcome, a
  `read_only` call records `DELIVERY_EXHAUSTED` unless an unknown report is journaled, and an
  `idempotent` call halts (P2's rule). An idempotent tool's downstream deduplicates by once key, so
  one scope applies its effect once.
- **Resolution.** The halt's cause comes from the journal: an unknown report is `crashed`; a begun
  attempt with no report is `worker_lost`, resolvable only after the lost-worker floor, encoded as
  its assumption (the worker holding the begun delivery is done with it).

| Property | Kind | Statement |
|---|---|---|
| `AtMostOnce` | invariant | the effect fires at most once |
| `NotStartedExclusive` | invariant | no not-started record for a claim whose effect fired, with a voider other than the holder (the abandon) |
| `BeginExclusive` | invariant | at most one worker ever runs under one marker |
| `BeginIdempotent` | invariant | a live worker whose nonce the stored begin record holds is never answered false |
| `NoRunAfterAbandon` | invariant | no worker runs under a marker whose begin key holds an abandon |
| `NoLiveOverride` | invariant | a resolution never lands while a begun worker's handler runs |
| `ExhaustedTruthful` | invariant | `DELIVERY_EXHAUSTED` is recorded only when no effect ran (for a retry-safe call, no downstream effect) |
| `DownstreamOnce` | invariant | a retry-safe call's deliveries share one once-key scope, so its downstream effect applies once |
| `ResultStable` | action | the recorded outcome is never replaced |

| Config | Group | What | States | Time |
|---|---|---|---|---|
| `remote` | ci | two workers, two attempts, three deliveries; a lost and a duplicated task, an ambiguous insert | 1,667,049 | 8 s |
| `remote-restart` | ci | the same with an engine crash (a new instance, an empty dispatch table), no duplicate | 480,039 | 3 s |
| `remote-resolve` | ci | a worker crash and an operator resolving halts | 2,899,879 | 12 s |
| `remote-safe` | ci | a retry-safe call: three deliveries, a lost and a duplicated task, an ambiguous insert | 1,689,461 | 9 s |
| `deep-remote-safe` | nightly | a retry-safe call with an engine crash and a worker crash | 3,294,639 | 26 s |
| `deep-remote-safe-read-only` | nightly | a `read_only` retry-safe call (it may record `DELIVERY_EXHAUSTED`) | 3,555,285 | 1 min |
| `deep-remote-two` | nightly | every fault at the pull-request bounds together | 12,112,395 | 55 s |
| `deep-remote` | nightly | three attempts, four deliveries; a lost task, an ambiguous insert, an engine crash | 15,126,088 | 1 min |
| `deep-remote-a2` | nightly | two ambiguous replies and an engine crash | 1,473,772 | 7 s |

Section 18's `ci-remote` bounds (four attempts and two ambiguous replies with every other fault)
do not fit: three attempts with two ambiguous replies passed 215 million states without
converging.

Section 18's required failures, each a regression configuration: `inserted-flag-won` and
`inserted-flag-won-fire` (`BeginExclusive`, `AtMostOnce`), `delivery-id-only` and
`delivery-id-only-fire` (`BeginExclusive`, `AtMostOnce`), `not-started-from-begun`
(`AtMostOnce`), `byte-equal-won` (`BeginIdempotent`), `abandon-without-key`
(`NotStartedExclusive`), `caller-cause` (`NoLiveOverride`), all in `protocol/regress/`, with
`new-scope-per-delivery` (the maintainer decision behind 10.9: a new scope per re-dispatch lets a
deduplicating downstream apply the effect twice; `DownstreamOnce`).

Finding P1 (`protocol/regress/begin-check-before-read`, fixed in #95's text as 10.4 step 0):
revision 2's `BeginTask` checked the dispatch table before it read the stored begin record. A
worker whose begin landed and whose answer was lost retried with its nonce after the delivery's
lease lapsed or the engine restarted, was refused (`unknown_delivery`), and never ran: the call
halted `worker_lost` for an effect that never started (`BeginIdempotent`). The fix: a begin the
stored record already holds for this delivery and nonce is answered true before the dispatch-table
checks, which then apply only to a first begin.

Finding P2 (`protocol/regress/lost-is-exhausted`, fixed in #95's text, 10.9 and 6.3): revision 2
recorded `DELIVERY_EXHAUSTED` for a retry-safe call whose deliveries were only lost. A lost
delivery may have run its handler (its lease lapsed while the worker ran, or the worker died or its
report was lost after the downstream effect), and a refusal proves nothing either, since a
duplicate of the refused delivery may run. The error told the model the effect did not happen, its
next call ran under a new once-key scope, and a deduplicating downstream applied the effect twice
(`ExhaustedTruthful`, 10 states). The maintainer's rule: `DELIVERY_EXHAUSTED` only for `read_only`
tools (`limits/read-only-exhausted` shows it is recorded), and an `idempotent` call halts.

Not modelled yet: the call deadline as a clock (the window is `max_deliveries` here), push
delivery's signature and endpoint rules, and approvals over the protocol (model 1b checks the
gate).

## Model 7: flow semantics

`flows/Flows.tla` checks a lowered plan flow (P5b, #103): every node runs as an `agent.Step`
under its node key, so this model abstracts the Step's claim to one marker (first writer wins; a
drive that finds a marker with no result halts until the halt is resolved) and leaves the claim
protocol itself to model 1. Nodes are Steps since P5b: flows no longer claim through
`ClaimAttempt` on one key (the `Kind = "flow"` configurations of model 1 keep the old rule as a
record of it).

The flow is one small graph with every routing shape: an entry `E`; a Switch over `E` to a
bounded loop (head `H`, switched node `S`, whose Switch loops back to `H` or exits to `T`) or to
`Q`; `H`'s body runs a nested Step `N`. Node outputs are 0 or 1, chosen by the body; the
predicates are pure over the recorded value. Two drivers, crashes, ambiguous replies, and an
operator resolving node halts through `Flow.ResolveHalt` (of this flow, or of another flow or
digest) run around it.

| Label | Go |
|---|---|
| `Begin`, `BeginStart` | `journalhook.Begin`: `run:start`, or a recorded `run:complete` |
| `Node` (`NGet`, `NClaim`, `NBody`, `NNested`, `NRecord`) | `runNode` through `journalhook.Step`: the node key `node:[iter:<i>:]<name>`, its Step claim, the body, a Step the body runs (`node:iter:<i>:H:step:N`), the result |
| `Choose` (`CDo`, `CRoute`) | `chooseArmKeyed`: `switch:<over>` or `switch:iter:<i>:<over>` |
| `LoopH` .. `AfterChooseS` | `runLoop`, with its `lp.max` bound (a runaway-loop error) |
| `Complete` | `journalhook.Complete`: `run:complete` with the terminal's output |
| `RPick`, `RWrite` | `Flow.ResolveHalt`: a node of this flow with a live attempt and no result, in a run of this flow and digest (`checkRunOfFlow`), then `ResolveHaltRef` |

| Property | Kind | Statement |
|---|---|---|
| `AtMostOncePerIteration` | invariant | a node that is not retry-safe fires at most once per iteration key |
| `NestedOncePerIteration` | invariant | the nested Step fires at most once per iteration, and every iteration a driver recorded ran it |
| `Conform` | invariant | the journal holds a declared path: every recorded result and choice is on the path the recorded choices take, each choice follows its predicate over the recorded value, and `run:complete` holds the reached terminal's output |
| `RoutesFollowDeclared` | invariant | every route a driver took followed the predicate over its own iteration's recorded value |
| `ResultsTyped` | invariant | every recorded node output is one this flow's node types read |
| `Completes` | liveness | a run whose drives do not halt completes, or stops on the loop bound |

`Conform` here is the property the journal must have, not `plan.Conform` itself: the model shows
every journal the drivers and the resolver can produce is a declared path, which is what
`plan.Conform` checks in Go.

| Config | Group | What | States | Time |
|---|---|---|---|---|
| `flow-two` | ci | two drivers; `E`, `H`, `T` side effects; 1 error reply, 1 crash | 1,690,704 | 11 s |
| `flow-resolve` | ci | one driver and the resolver (this flow, or another); 1 error reply, 1 crash | 150,726 | 1 s |
| `flow-all-side-resolve` | ci | every node a side effect, one driver, the resolver | 253,402 | 2 s |
| `flow-live` | ci | `Completes`, one driver; 1 error reply, 1 crash | 6,886 | <1 s |
| `deep-flow-all-side` | nightly | every node a side effect, two drivers | 5,592,438 | 34 s |
| `deep-flow-resolve` | nightly | two drivers and the resolver, 1 crash | 15,882,920 | 1 min 42 s |
| `deep-flow-live` | nightly | `Completes`, two drivers | 3,381,384 | 2 min |
| `deep-flow-iter3` | nightly | three loop iterations, two drivers | 10,758,528 | 1 min |

Regressions (each must fail with its property): `flows/regress/nested-unscoped` (#103 review F7:
a Step a loop body ran had one key for every iteration, so later iterations replayed the first;
`NestedOncePerIteration`, 38 states), `flows/regress/loop-switch-unscoped` (a guard: the loop
Switch's choice journaled once for all iterations; `RoutesFollowDeclared`, 46 states),
`flows/regress/resolve-any-node` (#103 review F3: a resolution recorded a node that never halted,
off the taken path; `Conform`, 6 states), `flows/regress/resolve-any-flow` (#103 re-review F4: a
resolution through another flow or digest recorded a value this flow cannot read;
`ResultsTyped`, 13 states).

Not modelled here: the flow input's canonical comparison (#103 F2, a Go encoding question), the
run ID check (F6), and joins (a Join is ordinary sequential Step under topological order).

## Model 8: spend accounting

`spend/Spend.tla` checks that the journal holds every billed model request exactly once (P9,
#104). Every request bills one unit. A turn's primary request is its answer; an extra request (a
hedge loser, a retried attempt) ends before the turn's record is built (its usage is the
record's discarded usage), or stays in flight and ends later (the next turn's record takes it, or
the drive's end journals it as late spend), or ignores its cancellation past the drive's bounded
wait. Drivers are in separate processes; faults are failed model calls, ambiguous writes, a failed
read of a turn's record after its write errored, crashes, and requests outliving the wait.

| Label | Go |
|---|---|
| `Open`, `SettlePending` | `Agent.run`'s Load and `settlePending` (spend a drive of this process could not journal, decided from the journal) |
| `Turn`, `Call` | a model turn: `chain.call` through the spend meter; `meter.take()` into the record's `DiscardedUsage` |
| `Insert`, `Recorded` | `store.Do` of `@llm/<n>` and `recorded` (another driver's record: this drive's spend is late) |
| `FailPath`, `FailLookup`, `FailSpend` | the failed turn: `waitEnd`, `lookup` of the record, `keepSpend`, `@spend/<id>` |
| `Leave`, `LeaveLate`, `End`, `EndLate` | `settle`: the bounded wait and `@spend-late/<id>` |
| `Complete` | `run:complete` |
| `RequestEnds`, `EndsAfterReturn` | a request in flight ends; one that outlived a drive that did not wait for it |
| `Crash(d)` | the process dies: its meter, requests in flight and kept spend are gone |

| Property | Kind | Statement |
|---|---|---|
| `NoDoubleCount` | invariant | the journal's spend never exceeds the billed spend |
| `SpendExact` | invariant | once every drive is done and nothing is in flight, the journal holds every billed unit, except what a documented limit lost (a crash, a request that outlived the wait) and what a process still keeps for a drive that will not come |
| `ResultSpend` | invariant | a drive's `Result.Spend` never exceeds the journal's spend, and with one driver it equals it |

`Replay`'s spend is the journal's (it reads the same records, and carries late spend to the turn
before it or the first turn), so `SpendExact` is its property too.

| Config | Group | What | States | Time |
|---|---|---|---|---|
| `spend-one` | ci | one driver, two turns, every fault | 38,921 | <1 s |
| `spend-two` | ci | two drivers (no lease): both may answer a turn; one extra request, a failed call, an ambiguous write, a crash | 1,789,459 | 6 s |
| `deep-spend-two` | nightly | two drivers, every fault | 18,110,591 | 1 min |

Regressions: `spend/regress/shared-late-key` (#104 re-review suspicion (a): late spend keyed by a
sequence number each drive counted itself, so two drivers wrote one key and the second's spend
was lost; `SpendExact`, 25 states), `spend/regress/lost-turn-not-late` (the same review: a driver
whose `@llm/<n>` insert lost to another driver's record dropped its requests' spend;
`SpendExact`, 29 states), `spend/regress/no-landed-lookup` (#104 review F4: a turn whose record
write errored journaled its spend as a failed call's though the record had landed;
`NoDoubleCount`, 26 states), `spend/regress/no-settle` (#104 review F3: a run ended without
waiting for requests in flight; `SpendExact`, 17 states).

Not modelled: two drivers' records equal in every journaled field (KNOWN-LIMITATIONS: taken as
each driver's own), `OnAnswer` and `Cost`, and the token budget's stop (it reads the same
totals).

## Model 10: the run lifecycle and recovery

`lifecycle/Lifecycle.tla` checks how a run reaches its end and how recovery finds the runs that
have not: the end markers, the drives that write them, the recovery passes that re-drive what is
left, and how long a dead holder's run waits. It is a model of its own; the claim protocol under
the calls is model 1's, reduced here to one marker per call (first writer wins), with a lost or
errored claim leaving the run halted.

### What is modelled

- **End markers.** `run:complete` (the loop's terminal turn, `putRecord`), `run:aborted` (a saga
  whose rollback finished, `Agent.rollback`) and `run:cancelled` (reserved; written by P14's
  `Cancel`, D1 in `docs/design/api-v1.md`). Each is its own key, first writer wins, and the model
  keeps them in journal order (A2).
- **Drives.** A leased `Run` (`Lease` around `Agent.Run`, the primary), a plain `Run` (no lease),
  and the `resume` a recovery pass calls (the model's resume is `Run` or `RunSaga`). A drive loads
  the run, returns a finished run's end, halts on a live attempt with no result (the resume gate),
  pauses on a pending approval, claims and calls up to two side effects, records each result, and
  writes `run:complete`, or, for a saga whose call failed, rolls back and writes `run:aborted`.
- **Recovery.** `Recover` (one pass) and `RecoverLoop` (a pass every interval) per worker: the
  `Lister` filter (no end marker), the in-flight set, a slot that waits for a free slot (the
  model's concurrency is 1), and `recoverRun`: `AcquireLease`, `runEnded` under the lease (#114),
  `resume`, the deferred `ReleaseLease`. Runs are listed in id order, as the SQL stores list them.
- **Leases.** Owner and time left. A holder's renewer keeps its lease live; a stalled or dead
  holder's lapses after `TTL` ticks, and any holder may then take it. A holder that wakes from a
  stall keeps driving until its renewer notices the loss and cancels its context (`ErrLeaseLost`):
  a claim and a not-yet-called effect stop there, a result is still recorded
  (`context.WithoutCancel`).
- **The operator.** `ResolveHalt` (`checkNoLiveDriver` takes the run's lease, then the result is
  written, first writer wins) and `Approve`. Never assumed to act.
- **Cancel (P14).** `Cancel` of a run that is not over writes `run:cancelled`. The drive's checks
  follow `CancelRule`: `"turn"` is D1 as written (a `Get` when a drive starts and at every turn
  boundary); `"claim"` adds the proposed check after the claim is won, before the call, recording
  the attempt as not started. `VerdictRule = "first"` is the proposed reading rule: the first end
  marker in journal order is the run's end, and a drive or `Cancel` that wrote a marker reads the
  markers again before it reports.
- **Faults.** Error replies on every write (A3: committed or not), crashes (a worker restarts with
  new lease tokens and an empty in-flight set; the primary does not restart), and a lease holder
  that stalls past its TTL.
- **Time.** A discrete clock (`Tick`) runs the leases and each loop's ticker (a buffered tick, as
  `time.Ticker`). Under `Timed`, every process takes at most one step per tick and none lets a
  tick pass while it can step (`Settled`), so a step is one store round trip and a pass's length
  is visible. Without `Timed` (the safety configurations) time and processes interleave freely,
  which covers every relative speed.

Abstracted away: the model's conversation (the calls of a run are fixed), the claim protocol's
numbered attempts and remembered claims (model 1), compensation (models 5 and 9), sub-runs and
sessions (`recoverable`), renewal errors short of a lapse, `Recover` without a `Leaser`, a pass's
concurrency above 1 (a pass with C slots walks C halted runs at a time; the pickup delay scales
with the halted runs divided by C), and `Cancel` on a saga (an abort, model 5).

### Model-code map

| Label | Go |
|---|---|
| `DIdle` | `Lease` (`AcquireLease` under `<holder>#<token>`, `leaseToken`); for the primary, the caller's `Lease` around `Agent.Run`, or a plain `Agent.Run` |
| `DCheck` | `recoverRun`'s `runEnded` under the lease: `Store.Get` of each name in `endOfRunMarkers` |
| `DResume` | `recoverRun` calling `resume(ctx, runID)` |
| `DOpen` | `Agent.run`: `openRun`, `completedAnswer` (a finished run returns its answer), `holdToStart`, the resume gate (`toolHalt`, `HaltCrashed`), the approval pre-pass (`ApprovalPending`); P14's start check of `run:cancelled` |
| `DTurn` | P14 (D1): the `run:cancelled` `Get` at a turn boundary |
| `DClaim` | `claimNextAttempt` / `Journal.claim` (model 1), under the drive's context |
| `DPost` | proposed for P14: `run:cancelled` read after the claim is won, `recordNotStarted` |
| `DCall` | `recordFresh`: the `sctx.Err()` check, `t.Call` |
| `DRecord` | `recordFresh`'s insert of the result (`putRecord` under `context.WithoutCancel`) |
| `DRollback`, `DAbort` | `Agent.rollback`: `rollbackRun`, then `store.Do` of `run:aborted` |
| `DComplete` | the loop's terminal: `putRecord` of `run:complete` |
| `DVerdict` | proposed for P14: the end markers read again; the first in journal order is reported |
| `DRel` | `Lease`'s deferred `ReleaseLease` |
| `PList`, `PNext`, `PSlot`, `PWait` | `RecoverLoop`'s `pass`: `lister.Runs(ctx, recoverFilter)`, `recoverable`, `inFlight`, `slots`, the ticker (`Recover`: one pass) |
| `OPick`, `OLease`, `OWrite`, `ORel` | `ResolveHaltRef` / `resolveHalt` with `checkNoLiveDriver`'s lease; `Approve` |
| `CGet`, `CIns`, `CRead` | P14's `Cancel` (D1): the end-marker check, the `run:cancelled` insert, and the proposed read-back |
| `Tick` | wall-clock time: `driveWithRenew`'s renewal, lease expiry, `RecoverLoop`'s `time.Ticker` |
| `Stall`, `Wake`, `LeaseNotice` | a process pause; `renewLoop` returning `ErrLeaseLost` and cancelling the drive |
| `Crash` | a process dies (a worker restarts) |

### Properties

| Property | Kind | Statement |
|---|---|---|
| `NoResumeOfFinished` | invariant | a recovery pass never calls `resume` for a run holding an end marker |
| `OneDriverPerEpoch` | invariant | no two drives hold a live lease on one run at once (each acquisition of a free or lapsed lease is an epoch, owned by one drive's token) |
| `OneLiveDriver` | invariant | no two drives of one run run at once with live contexts |
| `NoDoubleCompletion` | invariant | a run is never both completed and aborted, and each end marker is written once |
| `OneEnd` | invariant | a run holds at most one end marker |
| `VerdictAgreement` | invariant | a drive or `Cancel` that reports how a run ended agrees with the run's first end marker |
| `FinishedFinal` | invariant | no effect fires under a claim won after `run:complete` or `run:aborted` |
| `CancelFinal` | invariant | no effect fires under a claim won after `run:cancelled` is in the journal (the property P14 must satisfy) |
| `NoFireAfterCancel` | invariant | no effect is called once `run:cancelled` is in the journal (the literal form; not achievable, see the limits) |
| `AtMostOnce` | invariant | each call fires at most once |
| `EndMarked` | invariant | a drive that reports a run complete or aborted leaves the marker |
| `OutcomeRecorded` | invariant | an effect whose driver returned, did not crash and got no error on its record write is recorded |
| `BoundedPickup` | invariant (Timed) | a run whose lease holder died is taken over within `Bound` ticks of its lease lapsing |
| `PickedUp` | liveness | a run whose lease holder died is eventually taken over, or ends |
| `EffectNotReachable`, `PickupNotReachable` | vacuity | must be violated |

`BoundedPickup` is a bounded-response property: under `Timed`, time cannot pass while a process
can step, so a tick is an upper bound on a round trip, and a takeover within `Bound` ticks is a
safety property over the ghost clock `age`. `PickedUp` is its unbounded form, which holds under
both rules; only the bound shows the cost of a pass.

### Recovery cost: the pickup bound (finding L1)

`RecoverLoop`'s godoc, since v0.9.0, says a dead holder's run is picked up within about one
interval of its lease expiring only while a pass is short: a pass visits every unfinished run it
lists, halted ones included, at about five store round trips each, and the next pass starts only
once this one has handed out every run. The model makes that cost visible. Under `Timed`, a
halted run's visit is five steps of the worker's slot (`DIdle` with the lease, `DCheck`,
`DResume`, `DOpen`'s halt, `DRel`), and the pass hands one run to the slot at a time, so a pass
over H halted runs takes about 5H ticks.

**L1 (`findings/pickup-halted-pile`, `BoundedPickup`).** Runs are listed in id order, so older
halted runs come first. A leased primary's run is listed last and the primary dies. In the
shortest counterexample (two halted runs) the pass listed all three runs, its slot is visiting
halted run 1 when run 3's lease lapses, and run 3 waits for the visits to runs 1 and 2, past a
bound of 4 (where the trace stops). When the lapse comes just after the pass tried run 3 (its
lease still live), the wait is the whole next pass: 10 ticks with two halted runs. The smallest bound that holds grows with the
halted runs (TTL 2, interval 2, one worker, concurrency 1; the smallest `Bound` at which
`BoundedPickup` holds, found by checking each bound in turn):

| Rule | 0 halted runs | 1 | 2 | 3 |
|---|---|---|---|---|
| v0.9.0 (`all`) | 4 | 5 | 10 | 15 |
| lapsed runs first (`lapsedFirst`) | 4 | 5 | 10 | 15 |
| proposed (`split`) | 3 | 3 | 4 | 4 |

**The proposed rule (`PassRule = "split"`, `pickup-split`).** Each worker runs a second loop,
on the same interval, that lists only the unfinished runs whose lease row has lapsed (the stores
delete a row on release, so a lapsed row means its holder died or stalled) and drives them with a
slot of its own. The full pass is unchanged and still re-drives the halted and never-leased runs.
A dead holder's run is then taken over within 3 or 4 ticks of the lapse with 0 to 3 halted runs
(the interval is 2), against 4, 5, 10 and 15 under v0.9.0's rule: about one halted run's visit
per halted run listed before it. Ordering each pass's list with the lapsed runs first is not enough
(`PassRule = "lapsedFirst"`, `findings/pickup-lapsed-first`): a lease that lapses while a pass
walks the halted runs still waits for the next pass. The rule needs a store query for runs with
a lapsed lease (a `RunFilter` field, say), and its own concurrency.

`PickedUp` holds under every rule but the historical one (`regress/skip-when-busy`): without a
bound, a slow pass is not a liveness failure, which is why the docs could only be narrowed and the
bound needs the clock.

### Cancel (P14): the property P14 must satisfy

D1 (`docs/design/api-v1.md`) says: `Cancel` writes `run:cancelled`; a driver checks for it with a
`Get` when a drive starts and at every turn boundary, never starts a new claim after seeing it,
and lets calls already in flight finish; `Recover` excludes cancelled runs. The model states the
property P14 must satisfy, `CancelFinal`: no effect fires under a claim won after `run:cancelled`
is in the journal, and a drive that read it claims nothing. Two findings against D1 as written,
each with the proposed rule the model checks (`life-cancel`, `life-cancel-plain`):

- **L2: the turn-boundary check races the claim** (`findings/cancel-turn-check`, `CancelFinal`).
  A drive checks `run:cancelled` (not there), `Cancel` writes it, and the drive claims and calls
  the effect: a claim won after the cancellation fires, with no fault. Proposed rule
  (`CancelRule = "claim"`): read `run:cancelled` again once the claim is won and before the call,
  and if it is there record the attempt as not started. The claim then orders the two: a claim won
  before the cancellation landed is a call in flight, one won after it never fires.
- **L3: `Cancel` and completion race on two keys** (`findings/cancel-verdict`,
  `VerdictAgreement`). `Cancel` reads no end marker, the run writes `run:complete`, `Cancel` writes
  `run:cancelled` and reports the run cancelled; or the run reports its answer after `Cancel`
  landed first. Two keys cannot be written atomically (A1 is per key), so `OneEnd` cannot hold
  (`limits/cancel-two-ends`). Proposed rule (`VerdictRule = "first"`): the first end marker in
  journal order (A2) is the run's end for every reader (`Run`, `Status`, the drive itself), and a
  drive or `Cancel` that wrote a marker reads the end markers again before it reports. It costs one
  `Get` at the end of a run and one in `Cancel`.

What no rule gives, stated as limits: an effect whose claim was won just before `run:cancelled`
landed is called just after it (`limits/cancel-in-flight`, `NoFireAfterCancel`), which is D1's
"calls already in flight finish"; and `Cancel` takes no lease, so it can land between a recovery
pass's re-check and `resume` (`limits/cancel-resume`, `NoResumeOfFinished`), where the drive's
start check returns the run cancelled without a write.

### Configurations

States are distinct states. The `ci` rows were measured on the CI runner (GitHub
`ubuntu-latest`, TLC's own times); the `nightly` rows on the development machine (Apple M1 Pro,
under load). Each passing configuration is also run for vacuity, and the safety configurations
run without `Timed`, so time and the processes interleave freely. The `nightly` rows raise one
budget each: two workers with an error reply and a crash, two calls with `Cancel`, the operator
with two calls, three runs with a crash, two workers with a stall. Model 10 adds about 50 seconds
to the pull-request job.

| Config | Group | What | States | Time |
|---|---|---|---|---|
| `life-leased` | ci | One run of two calls, a saga (a failing call rolls it back to run:aborted): a leased primary Run and a RecoverLoop worker; an error reply and a crash of either process. | 102,124 | 2 s |
| `life-stall` | ci | One run: a leased primary Run and a worker; a lease holder stalls past its TTL and wakes; a crash. A stalled holder may resume a run that ended (limits/stall-resume-finished) and may drive beside the run's new holder (limits/stall-two-drivers); every other property holds. | 85,718 | 2 s |
| `life-paused` | ci | Two runs, one waiting for an approval and one halted, a leased primary on a third that a worker recovers; the operator approves and resolves. | 136,880 | 3 s |
| `life-cancel` | ci | Cancel (P14) under the proposed rules: run:cancelled read again once a claim is won, before the call, and the first end marker in journal order is the run's verdict. A leased primary, a worker, two calls, a crash. | 101,555 | 2 s |
| `life-cancel-plain` | ci | Cancel (P14) under the proposed rules, with a plain Run (no lease) and a worker; a crash. | 157,820 | 3 s |
| `pickup-split` | ci | The proposed recovery rule (PassRule "split"): a second loop per worker visits only the runs whose lease lapsed, every interval, with a slot of its own. Two halted runs listed before a leased primary's run; the primary dies. Takeover within Bound ticks of the lapse. | 4,859 | 1 s |
| `pickup-reach` | ci | Vacuity of the pickup configurations: a dead holder's run waits after its lease lapsed (PickupNotReachable must be violated), under the proposed rule. | 189 | <1 s |
| `life-resolve` | ci | One run: a leased primary Run, one Recover pass (not RecoverLoop) and an operator resolving halts; an error reply and a crash. | 32,982 | 1 s |
| `live-pickup` | ci | PickedUp under v0.9.0's rule: a halted run listed before a leased primary's run, the primary dies; every step and the clock weakly fair. | 4,699 | 2 s |
| `deep-two-workers` | nightly | One run: a leased primary Run and two RecoverLoop workers; an error reply and a crash. | ? | ? |
| `deep-cancel-plain` | nightly | Cancel (P14) under the proposed rules, a plain Run of two calls and a worker; an error reply and a crash. | 2,290,004 | 1 min |
| `deep-leased-resolve` | nightly | One run of two calls, a saga: a leased primary Run, a RecoverLoop worker and an operator resolving halts; an error reply and a crash. | 1,269,395 | 34 s |
| `deep-paused` | nightly | Three runs (one waiting for an approval, one halted, a leased primary's), a worker, the operator, a crash. | 3,440,717 | 2 min |
| `deep-stall` | nightly | One run: a leased primary Run and two workers; a lease holder stalls past its TTL. | ? | ? |

### Regressions and limits

Each regression restores a historical rule behind `Bug` and must fail with its property; each
finding fails under the rule as it stands and flips to a regression once the fix is adopted; each
limit states behavior the design accepts. L1 is v0.9.0's rule (`RecoverLoop`); L2 and L3 are
against D1's text, before P14 is written.

| Config | Group | The rule or behavior | Expected | Trace |
|---|---|---|---|---|
| `regress/no-recheck` | regress | #114: recovery checked the end markers when it listed a run, not again under the lease; a run the primary finished in between was handed to resume (Bug = "NoRecheck"). | `NoResumeOfFinished` | 14 states |
| `regress/no-recheck-stall` | regress | #114 as TestHA_MultiProcessStallPastTTL caught it: the primary stalls past its TTL, the worker takes the run over and finishes it, and the worker's next pass, which listed the run before, resumes it (Bug = "NoRecheck"; no crash). | `NoResumeOfFinished` | 16 states |
| `regress/shared-holder` | regress | #58 finding 4: Lease claimed under the bare WithLeaseHolder name, which AcquireLease takes as a renewal: a worker's recoverer and its primary under one name both drive the run (Bug = "SharedHolder"). | `OneDriverPerEpoch` | 6 states |
| `regress/shared-holder-stall` | regress | #58 finding 4 in the HA harness: a worker stalls holding a run, a restarted worker under the same name renews the stalled worker's live lease and drives the run (Bug = "SharedHolder"). | `OneDriverPerEpoch` | 10 states |
| `regress/no-abort-marker` | regress | #31: a saga whose rollback finished recorded no run:aborted, so every pass re-drove it (Bug = "NoAbortMarker"). | `EndMarked` | 8 states |
| `regress/record-under-ctx` | regress | #58 finding 1: the SQL stores recorded a step's result under the drive's context, so a drive whose lease was lost while the effect ran dropped the result (Bug = "RecordUnderCtx"). | `OutcomeRecorded` | 10 states |
| `regress/skip-when-busy` | regress | #58, RecoverLoop's first version: a pass that found every slot busy stopped, and the next began from the top, so runs listed after halted ones starved (Bug = "SkipWhenBusy"). | `PickedUp` (liveness) | 30 states |
| `regress/replay-finished` | regress | c6deb766: a drive of a finished run asked the model for another turn, whose calls have new tool-use ids (Bug = "ReplayFinished"). | `FinishedFinal` | 16 states |
| `findings/pickup-halted-pile` | finding | L1: v0.9.0's RecoverLoop visits every unfinished run in id order, halted ones included, so a dead holder's run listed after two halted runs is taken over more than Bound ticks after its lease lapsed. | `BoundedPickup` | 20 states |
| `findings/pickup-lapsed-first` | finding | L1, a rejected fix: each pass takes the runs whose lease lapsed first. A lease that lapses while a pass walks the halted runs still waits for the next pass. | `BoundedPickup` | 20 states |
| `findings/cancel-turn-check` | finding | L2: D1 checks run:cancelled when a drive starts and at turn boundaries; a drive past its check claims and fires after Cancel landed (CancelRule "turn"). | `CancelFinal` | 7 states |
| `findings/cancel-verdict` | finding | L3: Cancel and completion race on two keys: Cancel reads no end marker, the run completes, Cancel writes run:cancelled and reports the run cancelled (VerdictRule "none"). | `VerdictAgreement` | 10 states |
| `limits/stall-resume-finished` | limit | #114's documented residual: a worker stalls past its TTL between the re-check and resume; the primary takes the lapsed lease and finishes the run; the worker wakes and resumes it. | `NoResumeOfFinished` | 16 states |
| `limits/plain-run-resume-finished` | limit | #114's other residual: a plain Run holds no lease, so it can finish the run between a worker's re-check and resume. | `NoResumeOfFinished` | 13 states |
| `limits/cancel-resume` | limit | Cancel takes no lease, so it can land between a worker's re-check and resume; the drive's start check then returns the run cancelled without a write. | `NoResumeOfFinished` | 9 states |
| `limits/stall-two-drivers` | limit | Leases are not fenced: a holder that stalls past its TTL wakes still driving, beside the run's new holder, until its renewer notices. | `OneLiveDriver` | 9 states |
| `limits/cancel-in-flight` | limit | Cancel cannot stop a call already past its check: under the proposed rules a claim won just before run:cancelled lands fires just after it (NoFireAfterCancel is the literal form). | `NoFireAfterCancel` | 8 states |
| `limits/cancel-two-ends` | limit | Two keys cannot be written atomically (A1): Cancel and completion can both land; the first in journal order is the verdict (VerdictAgreement holds in life-cancel). | `OneEnd` | 10 states |

### Keeping model 10 and the code in step

The mechanisms of section 6 of the plan, as they apply here:

- **The model-code map** above names the Go function behind every label, checked by review (the
  plan's adversarial review gate) until trace validation lands.
- **Regressions.** Every historical bug of this layer that the model can state is a `Bug` value
  and a configuration in `regress/` that must keep failing; a change that makes one pass means
  the model lost the behavior. The two scenarios of the HA harness (`no-recheck-stall`,
  `shared-holder-stall`) are steered to their shape with a `CONSTRAINT` in `LifecycleMC.tla`.
- **The path filter.** The Models job runs on every pull request that touches `agent/` or
  `store/`, which hold every function in the map (`agent/recovery.go`, `agent/lease.go`,
  `agent/loop.go`, `agent/runstart.go`, `agent/saga.go`, `agent/halt.go`, and the stores'
  `Leaser` and `Lister`).
- **Not built yet** (the plan's M3 and M4, for every model): trace validation (`bidetrace`
  hooks, `tracestore`, a trace spec per model), `TestProtocolVocabulary`, the region markers and
  the path-rule job, and the counterexample-to-test helper (`agent/internal/interleave`). For this
  model, the hooks would be `recoverRun` (acquire, `runEnded`, `resume`, release), the end-marker
  writes, `renewLoop`'s `ErrLeaseLost`, and P14's `Cancel` and `run:cancelled` checks; the
  multi-process HA harness is the natural producer.
- **Findings need Go tests before their fixes**, as for every model: L1 needs a test that
  measures a takeover behind halted runs against the interval (the HA harness's
  `takeoverBound` with halted runs listed first), and L2 and L3 become tests of P14.

## What the bounds do not cover

Model 1b adds: three approvers and a policy of 2 of 3 (tightened to 3 or loosened to 1 by a
redeploy), up to three decisions and two `Approve` calls, one call. A bug that needs more
decisions than that, or two gated calls whose approvals interact, is outside the check.

Two drivers (three in `deep-drivers`, nightly, with one error reply), one crash, one
cancellation, up to two error replies per run on pull requests (three in the F3 configs) and
four nightly; one call except in the intent configurations.
Calls interact only through the fault budgets and the id pool, so two independent calls add no
behavior the one-call configurations miss. A bug that needs more than these is outside the check.
