# Formal models

TLA+ models of bide's coordination protocols, written in PlusCal and checked with the TLC model
checker. The plan and the reasoning behind it are in the design note
[formal models of the coordination protocols](../../docs/design/formal-models.md);
this directory holds model 1 of that plan, the claim protocol.

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
\* GROUP: ci | nightly | regress | finding
\* EXPECT: pass | invariant <Name> | liveness
```

A `pass` configuration must finish with no error, and is then run a second time with the single
invariant `EffectNotReachable`, which TLC must report violated: a model in which the effect can never
fire would satisfy every safety property, so this vacuity check is part of every passing run. An
`invariant <Name>` configuration must stop with exactly that invariant violated (TLC exit status
12), and a `liveness` configuration with a temporal property violated (exit status 13); anything
else fails the check.

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
    findings/        open findings, which fail until they are fixed
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
(`durableStep`, `claimAttempt`), which the DST and reference-model suites drive; retry-safe Steps
and tools, which write no marker; an operator in the same process as a driver (the resolver never
shares a flight with a driver); the `maxPendingClaims` eviction (it loses knowledge like a crash
does, which the liveness property excuses only for a crash).

Claim ids are allocated as the smallest id nothing in the state refers to, so re-drives do not grow
the state space; the protocol compares ids only for equality, which makes this sound.

### The rules

The model states the rules of P6a (#92) after its third review:

1. Every claim inserts its marker under a fresh claim id, so a won claim's not-started key is
   empty (`WonKeyEmpty`), and only a not-started record voids an attempt.
2. If the marker's Insert errors, the claim records a not-started record under its own id. Any
   failed not-started write (`Journal.notStarted`, wherever it is called from) leaves the id
   remembered in the process, in `pendingClaims`, which holds a set of ids per marker key.
3. A remembered id is never used to run. The next claim of that marker key in the process first
   retries each remembered id's not-started record (voiding its marker if that committed), then
   claims with a fresh id; a voided marker moves the claim to the next numbered attempt.
4. The tool resume gate, before it halts on a live marker, retries the not-started record of that
   marker's claim if the process remembers it; if the retry succeeds, the call claims its next
   attempt instead of halting.
5. A Step that loses the claim joins the process's in-flight call of the step, or reads the
   result, or halts; it never starts a flight of its own. A claim winner joins a flight already in
   the process and, if it fails, records its own attempt as not started.
6. A tool that loses the claim halts (`HaltContended`).
7. Halt resolution refuses while a live driver may be running: under a store that leases runs, it
   holds the root run's lease while it resolves, and only leased drivers are seen; otherwise it
   needs `WithMinHaltAge`.

### Model-code map

Each label is one atomic step: one store round trip or one local decision. Function names are
those of #92 (P6a); where #92 has not yet adopted a rule, the step names the rule.

| Label | Go |
|---|---|
| `Start` | the caller issuing the call: a model turn naming the tool-use id, or code calling `Step` |
| `Open` | the drive's one `Load` (`Journal.open` in `Agent.run`; `journalStep`'s `j.Get`); on the tool path, `liveAttempts` and the resume gate's loop in `Agent.run` |
| `GateTake`, `GateWrite` | the resume gate's retry of a remembered claim (rule 4): `pendingClaims` membership of the live marker's id, then `Journal.notStarted` |
| `Claim` | one iteration of `Journal.claimNext`: `pendingClaims.take` in `Journal.claim` |
| `ClaimRetry` | `Journal.claim` retrying a remembered id's not-started record (rule 3) |
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
| `RClaim` | not in the code: the proposed fix of finding F2 below (`ResolveClaim = TRUE`) |
| `RWrite` | `resolveHalt`'s write of the result (`store.Do` on the result key) |
| `Crash(p)` | a process dies: its drives restart, its `pendingClaims` and flights are gone, its lease lapses |
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

On every pull request and in the merge queue (`ci`, `regress`, `finding`). States are distinct
states; times are TLC's own, measured on a development machine (Apple M1 Pro, 8 workers). The
whole pull-request set, vacuity runs and JVM starts included, takes CITOTAL on the CI runner
(GitHub `ubuntu-latest`, 4 cores).

| Config | Path | Drivers, processes | Faults (error replies, crashes, cancels) | Attempts | Property | States | Time |
|---|---|---|---|---|---|---|---|
| `step-same` | Step | 2 in 1 | 2, 1, 1 | 0..3 | safety | 2,234,519 | 10 s |
| `step-cross` | Step | 2 in 2 | 2, 1, 1 | 0..3 | safety | 2,004,380 | 12 s |
| `tool-same` | tool | 2 in 1 | 2, 1, 1 | 0..3 | safety | 1,417,253 | 7 s |
| `tool-cross` | tool | 2 in 2 | 2, 1, 1 | 0..3 | safety | 2,760,599 | 14 s |
| `late-step-same` | Step, weak A3 | 2 in 1 | 2, 1, 1 | 0..3 | safety | 6,490,247 | 32 s |
| `late-tool-cross` | tool, weak A3 | 2 in 2 | 2, 1, 1 | 0..3 | safety | 5,522,592 | 30 s |
| `resolve-lease` | tool, resolver (lease) | 2 leased in 2 | 2, 1, 1 | 0..3 | safety | 80,919 | 1 s |
| `resolve-minage-claim` | tool, resolver (min age, the F2 fix) | 2 in 2 | 2, 1, 1 | 0..4 | safety | 5,886,132 | 32 s |
| `intent` | two Steps that pause, the caller, resolver (lease) | 2 leased in 1 | 1, 1, 0 | 0..2 | safety, per intent | 1,370 | <1 s |
| `intent-minage-claim` | two tool calls, the caller, resolver (min age, the F2 fix) | 2 in 2 | 2, 0, 0 | 0..3 | safety, per intent | 662 | <1 s |
| `live-step-same` | Step | 2 in 1 | 1, 1, 1 | 0..2 | `Progress` | 286,942 | 8 s |
| `live-tool-same` | tool | 2 in 1 | 1, 1, 1 | 0..2 | `Progress` | 128,325 | 3 s |
| `live-tool-cross` | tool | 2 in 2 | 1, 1, 1 | 0..2 | `Progress` | 144,243 | 5 s |

Nightly (and on demand, `workflow_dispatch`):

| Config | Path | Drivers, processes | Faults | Attempts | Property | States | Time |
|---|---|---|---|---|---|---|---|
| `live-step-same-a2` | Step | 2 in 1 | 2, 1, 1 | 0..3 | `Progress` | N1 | NT1 |
| `live-tool-same-a2` | tool | 2 in 1 | 2, 1, 1 | 0..3 | `Progress` | N2 | NT2 |
| `live-tool-cross-a2` | tool | 2 in 2 | 2, 1, 1 | 0..3 | `Progress` | N3 | NT3 |
| `live-memo-a3` | Step | 2 in 1 | 3, 0, 1 | 0..4 | `Progress` | N4 | NT4 |
| `deep-faults` | Step | 2 in 1 | 3, 1, 1 | 0..4 | safety | N5 | NT5 |
| `deep-drivers` | Step | 3 in 2 (2 + 1) | DDF | DDG | safety | N6 | NT6 |
| `deep-late-tool-same` | tool, weak A3 | 2 in 1 | 2, 1, 1 | 0..3 | safety | N7 | NT7 |
| `deep-late-step-cross` | Step, weak A3 | 2 in 2 | 2, 1, 1 | 0..3 | safety | N8 | NT8 |
| `deep-resolve-lease-same` | Step, resolver (lease) | 2 leased in 1 | 2, 1, 1 | 0..3 | safety | N9 | NT9 |

The liveness checks run on every pull request with one error reply; with two they run nightly,
since liveness checking cannot use symmetry and costs several times a safety check of the same
states.

### Regression configurations

Each is a rule an earlier review found wrong, restored behind the `Bug` constant. CI requires each
to fail with its named property; one that stops failing means the model lost the behavior or the
property weakened.

| Config | The historical rule | Expected | Trace |
|---|---|---|---|
| `regress/reuse-nohold` | #92 second review: a remembered claim id is reused for the claim and runs (`Bug = "ReuseNoHold"`) | `NotStartedExclusive` | 14 states |
| `regress/reuse-nohold-fire` | the same, checked for the double fire itself | `AtMostOnce` | 24 states |
| `regress/held-pin` | #92 third review, finding 1: the claim-held pin absorbs the reused claim's not-started record after a cancellation (`"HeldPin"`) | `Progress` | 22 states |
| `regress/gate-no-retry` | #92 third review, finding 2: the tool gate halts on a claim its process remembers (`"GateNoRetry"`) | `Progress` | 10 states |
| `regress/no-memo-after-call` | #92 third review, finding 3: a failed not-started write after a cancelled call is not remembered (`"NoMemoAfterCall"`) | `Progress` | 18 states |
| `regress/memo-overwrite` | finding F1 below: `pendingClaims` holds one id per marker key (`"MemoOverwrite"`) | `Progress` | 34 states |
| `regress/loser-leads` | #92 first review, finding 3: the Step loser starts the flight and the winner takes its halt (`"LoserLeads"`) | `WinnerNeverHalts` | 21 states |
| `regress/pause-as-failure` | #92 first review, fix 1: a Step's pause guard recorded as its tool's failure (`"PauseAsFailure"`) | `AtMostOncePerIntent` | 16 states |
| `regress/resolve-no-check` | #90 F2: resolution with no live-driver check (`WithoutLiveDriverCheck`) | `NoLiveOverride` | 13 states |
| `regress/resolve-no-check-intent` | the same through the caller: "not charged", a new call, a second fire | `AtMostOncePerIntent` | 17 states |
| `regress/resolve-unleased-driver` | #90's documented limit: the lease check does not see a driver that holds no lease | `NoLiveOverride` | 16 states |

### Findings

Found by this model in the rules #92 is adopting; each is a counterexample, and each needs a
deterministic Go test before its fix (M2 of the plan).

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
  (`findings/minage-revoid`, `findings/minage-intent`: two error replies, no crash). The lease
  check is not affected, since leased drivers cannot drive while the resolver holds the lease. A
  fix the model checks (`ResolveClaim`, configs `resolve-minage-claim` and `intent-minage-claim`):
  before it writes, the resolver claims the attempt after the live one under its own id; a driver
  that voids the live attempt later loses that claim and reads the resolution, and if a driver
  already holds it, the resolution refuses. The `findings/` configs flip to `pass` when the code
  is fixed.

### What the bounds do not cover

Two drivers (three in `deep-drivers`, nightly), one crash, one cancellation, up to two error
replies per run on pull requests and three nightly; one call except in the intent configurations.
Calls interact only through the fault budgets and the id pool, so two independent calls add no
behavior the one-call configurations miss. A bug that needs more than these is outside the check.
