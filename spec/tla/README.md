# Formal models

TLA+ models of bide's coordination protocols, written in PlusCal and checked with the TLC model
checker. The plan and the reasoning behind it are in the design note
[formal models of the coordination protocols](../../docs/design/formal-models.md);
this directory holds models 1 (the claim protocol), 1b (the approval gate), 2 (the bide protocol's claim rules), 7 (flow semantics), 8 (spend accounting), 9 (the tool-call state machine), 10 (the run lifecycle and recovery) and 11 (delegation, sub-run authority and saga trees) of that plan.

What a model check establishes, stated narrowly: within the bounds a configuration states (drivers,
processes, faults, attempt numbers), TLC explores every interleaving of the modelled rules and every
placement of the faults, and checks each property in every reachable state. Nothing is proven
beyond the bounds. The model states the protocol's rules; it does not read the Go code. The map
below says which Go function each model step abstracts. Its meaning is checked by review, until
trace validation (milestone M3 of the plan) lands; its names are checked by a tool, and a change to
the code it names must change the model or say why not (see
[Keeping the code and the models in step](#keeping-the-code-and-the-models-in-step)).

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
`TLC_JOBS=<n>` runs n configurations at a time, one TLC worker and a 2 GB heap each (default 1:
one at a time, `TLC_WORKERS` workers), `TLC_MODELS="claims toolcall"` limits a group to those
model directories, `TLC_JAVA_OPTS` adds JVM options, and `TLC_KEEP_OUTPUT=<dir>` keeps each run's
full TLC output (counterexample traces included). Each concurrent run has its own TLC metadir and
`java.io.tmpdir` under the script's work directory, so runs never share TLC state.

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
`regress`, `finding` and `limit` configuration, four at a time (`TLC_JOBS=4`, one TLC worker each,
on the 4-vCPU runner). On a pull request it checks only the models whose directory under
`spec/tla/` the pull request changes, and every model when it changes `check.sh`, `tools.lock` or
`models.yml`; with none of those, the job reports success after printing that no model changed,
so it can be a required check without costing every other change several minutes. This loses no
check: TLC reads only the model's own directory, the pinned tools and `check.sh`, so an unchanged
model gives the result it gave on main, and a change to the Go code a model describes must change
the model or carry a `Protocol-Impact` override (the path rule below, in the Lint job). The merge
queue and main always run every model, and so does any doubt (a failed diff, an unexpected
event). **Models (nightly)** runs the `nightly` configurations
on a schedule and on demand (`workflow_dispatch`). The Go counterpart, the full-bound fault-schedule
explorations of the claim protocol and of flow lowering (`BIDE_EXPLORE=1`), runs nightly in
`.github/workflows/explore.yml`; see [verification](../../docs/testing/verification.md).

## Keeping the code and the models in step

Milestone M4 of the plan (section 6.3): the Go code each model describes is marked, and CI fails a
change to marked code that does not change the model.

**Region markers.** Each Go region a map row names is wrapped in line comments that name the model,
by its directory under `spec/tla`, and the model actions (PlusCal labels or TLA+ operators) the
region implements:

```go
// protocol:claims begin Claim ClaimRetry ClaimInsert ClaimNS
func (j *Journal) claim(ctx context.Context, runID, key string, rec Record) (bool, Record, error) {
	// ...
}
// protocol:claims end
```

A marker above a declaration is followed by a blank line, so it is not part of the doc comment.
Regions of one model do not nest; regions of different models may overlap (the run's Load is
`Open` in both model 1 and model 8). Model 10 (`lifecycle/`) marks the lease, recovery, drive and resolution code; the steps P14 has not built yet (`DStart`, `DAmend`, `DTurn`, `DPost`, `DVerdict`, `Cancel`'s `CGet`, `CIns`, `CRead`, `CReq`, `Status`'s `SPick`, `SStart`, `SGet`) are on its no-code list until they land, and P14's pull request adds their markers. Model 11 (`delegation/`) marks `audit`'s `AttenuatingSubAgent`, the sub-agent tool, the programmatic sub-run admission and links, the loop's hold and halt rules, and the saga's rollback walk. Model 2 (`protocol/`) is a design model with no Go code yet,
so it has no map and no markers.

**The checks.** `go run ./internal/tools/modelsync` (the Lint job, on every pull request, in the
merge queue and on main) fails when:

- a marker is malformed, unpaired, nested in its own model, or names a model with no directory;
- a marker names an action the model's spec (`spec/tla/<model>/<Model>.tla`) does not define, or
  its map below does not list (a rename in the spec, the map or the code);
- an action the map lists is not defined in the spec, or has no marker, unless the map's no-code
  list names it (a caller, a crash, a historical rule).

The maps are found by two comments on the lines before each table:
`<!-- modelsync: no-code <model> <Action> ... -->` (optional) and `<!-- modelsync: map <model> -->`.

**The path rule.** With `-base` (on a pull request, the base branch; in the merge queue, the
batch's base), modelsync diffs the merge base against the head. A change that touches a marked
region of a model (a line inside it, its markers, or lines deleted from it; a moved file counts as
deleted and added) and changes no file under `spec/tla/<model>/` fails, unless a commit message in
the range or the pull request's description holds an override line:

```text
Protocol-Impact: none (<reason>)
Protocol-Impact: claims,spend none (<reason>)
```

The first form covers every model, the second only the models it names. The reason is required,
and a line that does not parse, or names no model, fails the check. Use it for a change that leaves
the modelled behavior as it is (a rename, a comment, an error message); a change to a rule changes
the model. Every override used is printed in the log and as a warning annotation on the pull
request, so the reviewer sees it. The Lint job reads the description when it runs: after adding
the line to the description, re-run the job, or put the line in a commit message. In the merge
queue, the descriptions of every pull request in the batch are read.

```sh
go run ./internal/tools/modelsync                     # the consistency check
go run ./internal/tools/modelsync -base origin/main   # and the path rule against main
```

The Lint job also runs `TestProtocolVocabulary` (package `agent`) on every change, spec-only ones
included: the record kinds of the vocabulary block in `claims/Claims.tla` (`\* vocabulary: begin`
... `\* vocabulary: end`) must match the kinds the claim code's key constructors and record kinds
map to, in both directions, and every constructor's keys must parse back to their kind.

**A new model.** A model that describes Go code (model 9, tool calls, when it lands) comes
with its markers in the same pull request: its directory and `<Model>.tla`, a map section in this
README with the two anchor comments, and a `// protocol:<model> begin ...` region around every Go
region a map row names. modelsync then holds it to the same rules; a model with no map yet is not
checked against the code.

## Layout

```text
spec/tla/
  README.md          this file, with the model-to-code maps modelsync reads
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
  flows/, protocol/, spend/, toolcall/, lifecycle/, delegation/
                     models 7, 2, 8, 9, 10 and 11, laid out the same way
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

<!-- modelsync: no-code claims Start Hold LoserLead RNotStarted Finish Crash LateApply -->
<!-- modelsync: map claims -->
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
| `RNotStarted` | historical only: `resolveHalt`'s `recordNotStarted` of its own attempt after an errored write (`ResolveVoidOnError = TRUE`, #92 at `06408db`; finding F3) |
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
states; times are TLC's own, measured on a development machine (Apple M1 Pro, 8 workers; the `-a1`
rows on an Apple Silicon machine, 4 workers). The whole pull-request set of every model, vacuity
runs and JVM starts included, takes about 6 minutes on the CI runner (GitHub `ubuntu-latest`,
4 vCPUs, four configurations at a time; runner speed varies). It took 9 to 15.5 minutes before the
largest configurations of models 1, 2, 7 and 8 moved to nightly (#132).

| Config | Path | Drivers, processes | Faults (error replies, crashes, cancels) | Attempts | Property | States | Time |
|---|---|---|---|---|---|---|---|
| `step-same-a1` | Step | 2 in 1 | 1, 1, 1 | 0..2 | safety | 146,864 | 3 s |
| `step-cross-a1` | Step | 2 in 2 | 1, 1, 1 | 0..2 | safety | 152,375 | 2 s |
| `tool-same-a1` | tool | 2 in 1 | 1, 1, 1 | 0..2 | safety | 69,179 | 1 s |
| `tool-cross-a1` | tool | 2 in 2 | 1, 1, 1 | 0..2 | safety | 155,019 | 2 s |
| `resolve-lease` | tool, resolver (lease) | 2 leased in 2 | 2, 1, 1 | 0..3 | safety | 80,919 | 1 s |
| `resolve-minage-claim` | tool, resolver (min age, the F2 fix) | 2 in 2 | 2, 1, 0 | 0..4 | safety | 320,662 | 3 s |
| `intent` | two Steps that pause, the caller, resolver (lease) | 2 leased in 1 | 1, 1, 0 | 0..2 | safety, per intent | 1,370 | <1 s |
| `intent-minage-claim` | two tool calls, the caller, resolver (min age, the F2 fix) | 2 in 2 | 2, 0, 0 | 0..3 | safety, per intent | 662 | <1 s |
| `live-step-same` | Step | 2 in 1 | 1, 1, 1 | 0..2 | `Progress` | 291,262 | 9 s |
| `live-tool-cross` | tool | 2 in 2 | 1, 1, 1 | 0..2 | `Progress` | 144,243 | 5 s |
| `resolve-minage-claim-a3` | tool, resolver (min age, F2 and F3 fixed) | 2 in 2 | 3, 0, 0 | 0..4 | safety | 702,817 | 6 s |
| `resolve-lease-claim` | tool, resolver (lease, F4's fix), a leased driver and a plain Run | 2 in 1 | 3, 0, 0 | 0..4 | safety | 331,475 | 6 s |
| `resolve-in-proc-tool` | tool, resolver in the drivers' process (min age, fixes) | 2 in 1 | 2, 1, 0 | 0..4 | safety | 285,231 | 4 s |
| `flow-same` | one marker key (plan flow) | 2 in 1 | 2, 1, 1 | 0 | safety | 6,588 | <1 s |
| `flow-cross` | one marker key (plan flow) | 2 in 2 | 2, 1, 1 | 0 | safety | 14,357 | 1 s |
| `evict-tool-same-a1` | tool, 1 eviction | 2 in 1 | 1, 1, 1 | 0..2 | safety | 71,812 | 2 s |

Nightly (and on demand, `workflow_dispatch`):

| Config | Path | Drivers, processes | Faults | Attempts | Property | States | Time |
|---|---|---|---|---|---|---|---|
| `step-same` | Step | 2 in 1 | 2, 1, 1 | 0..3 | safety | 2,282,585 | 12 s |
| `step-cross` | Step | 2 in 2 | 2, 1, 1 | 0..3 | safety | 2,004,380 | 11 s |
| `tool-same` | tool | 2 in 1 | 2, 1, 1 | 0..3 | safety | 1,417,253 | 8 s |
| `tool-cross` | tool | 2 in 2 | 2, 1, 1 | 0..3 | safety | 2,760,599 | 17 s |
| `evict-tool-same` | tool, 1 eviction | 2 in 1 | 2, 1, 1 | 0..3 | safety | 1,553,944 | 15 s |
| `live-tool-same` | tool | 2 in 1 | 1, 1, 1 | 0..2 | `Progress` | 128,325 | 4 s |
| `live-evict-step-same` | Step, 1 eviction | 2 in 1 | 1, 1, 1 | 0..2 | `ProgressModuloEviction` | 304,754 | 10 s |
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

The nightly set takes about 21 minutes on the development machine. The five safety configurations
at two error replies and attempts 0..3 (`step-same`, `step-cross`, `tool-same`, `tool-cross`,
`evict-tool-same`, 1.4 to 2.8 million states) run nightly; each has a pull-request counterpart
(`-a1`) with the same invariants at one error reply and attempts 0..2, 70,000 to 155,000 states,
so every path keeps a passing safety configuration and its vacuity run on every pull request.
`Progress` runs on every pull request with one error reply on the Step path (`live-step-same`) and
the tool path (`live-tool-cross`); `live-tool-same`, `ProgressModuloEviction`
(`live-evict-step-same`) and every liveness check with two error replies run nightly, since liveness checking cannot use symmetry
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

<!-- modelsync: no-code claims Redeploy ResolverChange -->
<!-- modelsync: map claims -->
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
| `remote-no-loss` | ci | two workers, two attempts, three deliveries; a duplicated task, an ambiguous insert (`remote-restart` has the lost task) | 386,374 | 5 s |
| `remote-restart` | ci | the same with an engine crash (a new instance, an empty dispatch table), no duplicate | 480,039 | 3 s |
| `remote-resolve-no-restart` | ci | a worker crash and an operator resolving halts, a lost task, no engine crash (`remote-restart` has one) | 444,429 | 5 s |
| `remote-safe-d2` | ci | a retry-safe call: two deliveries (one re-dispatch), a lost and a duplicated task, an ambiguous insert | 73,657 | 1 s |
| `remote` | nightly | two workers, two attempts, three deliveries; a lost and a duplicated task, an ambiguous insert | 1,667,049 | 8 s |
| `remote-resolve` | nightly | a worker crash and an operator resolving halts, with an engine crash | 2,899,879 | 12 s |
| `remote-safe` | nightly | a retry-safe call: three deliveries, a lost and a duplicated task, an ambiguous insert | 1,689,461 | 9 s |
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

<!-- modelsync: map flows -->
| Label | Go |
|---|---|
| `Begin`, `BeginStart` | `journalhook.Begin`: `run:start`, or a recorded `run:complete` |
| `Node` (`NGet`, `NClaim`, `NBody`, `NNested`, `NRecord`) | `runNode` through `journalhook.Step`: the node key `node:[iter:<i>:]<name>`, its Step claim, the body, a Step the body runs (`node:iter:<i>:H:step:N`), the result |
| `Choose` (`CDo`, `CRoute`) | `chooseArmKeyed`: `switch:<over>` or `switch:iter:<i>:<over>` |
| `LoopH`, `AfterH`, `RunS`, `AfterS`, `ChooseS`, `AfterChooseS` | `runLoop`, with its `lp.max` bound (a runaway-loop error) |
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
| `flow-two-a0` | ci | two drivers; `E`, `H`, `T` side effects; 1 crash, no error reply (`flow-resolve` and `flow-all-side-resolve` have one) | 424,389 | 5 s |
| `flow-resolve` | ci | one driver and the resolver (this flow, or another); 1 error reply, 1 crash | 150,726 | 1 s |
| `flow-all-side-resolve` | ci | every node a side effect, one driver, the resolver | 253,402 | 2 s |
| `flow-live` | ci | `Completes`, one driver; 1 error reply, 1 crash | 6,886 | <1 s |
| `flow-two` | nightly | two drivers; `E`, `H`, `T` side effects; 1 error reply, 1 crash | 1,690,704 | 11 s |
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

<!-- modelsync: no-code spend Crash -->
<!-- modelsync: map spend -->
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
| `spend-two-no-crash` | ci | two drivers (no lease): both may answer a turn; one extra request, a failed call, an ambiguous write, no crash (`spend-one` has one) | 266,288 | 2 s |
| `spend-two` | nightly | the same with a crash | 1,789,459 | 6 s |
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

## Model 9: the tool-call state machine

`toolcall/ToolCall.tla` checks how one turn's tool calls are run, decided and recorded (redesign
P12, #117): the call state and began word, the base handler entered any number of times, the
middleware around it, the errgroup of sibling calls, and what the journal gets for each outcome.
It is a model of its own; the claim protocol under it is model 1's, so a claim here either
succeeds or its process crashes.

### What is modelled

- **One chain per call per drive.** The call state (`open`, `reached`, `refused`, `closed`,
  `refusedClosed`, the last three terminal) and the began word (`none`, `yes`, `sealed`), each
  changed only by CAS: `enterTool`, the refusals, `closeCall` and the loop's seal when the chain
  returns, `beginCall` immediately before `t.Call`.
- **The base handler**, entered by up to two invocations per call at once: a synchronous `next`,
  a retry, a renamed call (refused), and a `next` left running in a goroutine, which may outlive
  the chain and the drive (it keeps a frozen view of its closed chain; its context is done once
  `callTool` returned with a timeout, or the errgroup ended). Its steps: the checks and
  `enterTool` (`ctxDone`, `toolhook.CallGuard`), the saga's accepted-arguments write
  (`journalAcceptedArgs`, which can fail, commit after an error, or be cut off; for a retry-safe
  step that changes state it is also the "may have begun" record, T3), `beginCall` (which also
  answers a second invocation of a side effect "already ran", and refuses once the chain has
  returned, T5), and the tool, which fires its effect or not and returns a result, its own error, an unknown
  outcome, a context error, or `toolhook.Unrecorded` (a delegation under the wrong authority).
- **Three kinds of call** (`Kinds`): a side effect (claimed with an attempt marker, not
  retry-safe); a retry-safe tool that changes state (Idempotent, not ReadOnly, a Compensator;
  no attempt marker, and it may fire again on every re-run); and a delegation (retry-safe, no
  effect of its own, refuses Unrecorded under the wrong authority).
- **The middleware** (`MW`, per call): call `next` and return what it returned; turn its success
  into an error; retry it; rename the call; end the call with `ErrToolNotCalled`; answer from a
  cache; return an error of its own without the sentinel; leave `next` running and give up on a
  done context; call the tool itself (`direct`).
- **The loop**: the claim, the pre-call check, the chain under the tool's timeout, the decision
  of `notCalled`, the late rule, the unknown-outcome rules, the saga failure record, the result
  write (`recordFresh`), and `recordNotStarted` (a failed write is remembered, and the resume gate
  retries it: model 1's rule 4). The errgroup holds an Unrecorded refusal and a pause or halt until
  the siblings finish and joins them (`errors.Join`); every other error cancels the group.
- **The saga's rollback** (`rollbackRun`): the calls in reverse order. A failed step is skipped
  (an unknown outcome is listed); a side effect with a live marker and no result halts it; a
  result is compensated, as a memoized step whose record can fail; a retry-safe step with no
  result is re-run through its middleware chain, in a later `RunSaga`'s context, and its result
  compensated; only a re-run that reached the tool has a result. A re-run with an unknown
  outcome is listed and the walk goes on; any other error, or a failed write, stops the rollback,
  and a later `RunSaga` resumes it.
- **Faults**, each with a budget: a store write that errors (committed or not, A3), a process
  crash (every goroutine, leaked ones included, and `pendingClaims`), a run cancellation, a tool
  deadline, a guard refusal (a delegated grant expired), a tool that says its outcome is unknown,
  a tool's own error, and a delegation resumed under the wrong authority, which an operator puts
  right (fair). The run is driven again until it completes, aborts a saga, or halts for ever.

Abstracted away: the model turns (one turn of one or two calls, then completion), approvals
(model 1b), the claim's own faults and other drivers (model 1), the saga's `halted` flag, pauses
inside a tool (Interrupt, Sleep), and sub-runs (a delegation has no effect of its own here).

### Model-code map

Each label is one atomic step. Function names are those of #117 as merged (`132c187`); files are
`agent/toolexec.go` (T), `agent/loop.go` (L), `agent/saga.go` (S), `agent/tool_middleware.go`
(M) and `internal/toolhook/toolhook.go` (H). A change to any function named here changes the
model in the same pull request (CONTRIBUTING, "Formal models"); every row but the no-code list
(`IEnter2`, historical; the faults but `WrongAuth`) has a `// protocol:toolcall begin ... end`
region in the Go code, which modelsync checks.

<!-- modelsync: no-code toolcall IEnter2 Crash Cancel Deadline FixAuth -->
<!-- modelsync: map toolcall -->
| Label | Go |
|---|---|
| `IEnter` | T `Agent.toolHandler`'s base handler `h`: the `origID` check (`ErrConfig`, `ErrToolNotCalled`), `ctxDone`, `toolhook.CallGuard` (H), `enterTool` (from `callOpen`, `callRefused` or `callReached`; never from a closed state); the old `ran` mark under `RanEarly` and `RanBeforeArgs` |
| `IArgs`, `IIdem` | T `journalAcceptedArgs`: the saga's accepted arguments, written always for a compensable retry-safe write (`Safety.retrySafeWrite`), so the record is also its "may have begun" mark (T3); a failure returns `argsJournalError` before the tool |
| `IEnter2` | `RanEarly` only: the checks and `enterTool` after the arguments, as before round 4 |
| `IBegin` | T `countIn` (calls that are not ReadOnly, `tracked`), then `callIsClosed` (T5: no begin once the chain returned), then `beginCall` (`sealed`; `ErrToolReinvoked` for a side effect already begun); the `earlier` flag (T3) |
| `ICall` | T `t.Call` and the tool's own outcome (`toolRunning`, `toolSucceeded`, `toolFailed`, `toolUnknown` in `call.out`), through `callCounted`, whose `countOut` runs on return |
| `LStart` | L the goroutine's `gctx.Err()` check and `claimNextAttempt` (side effects only: `!retriableOnResume`) |
| `LPre` | L `recordFresh`'s pre-call check (`claimed && ctxDone(sctx)`); T `Agent.toolCallFor` and the fresh `st`, `began`, `out`, `earlier` of the chain |
| `LMw`, `LWait` | M the `ToolMiddleware` chain built by `Agent.UseTool`, as the `MW` sets allow |
| `LClose` | T `callTool` (the timeout and `late`), `closeCall`, the seal (`began` CAS to `beganSealed`), `running` (`tracked` and `countRunning`, whatever the chain's state, T6) and the unknown-outcome rules after `h` returns, with `Agent.unprovenFailure`; L `notCalled`, the `late` rule, `argsJournalError`, `sagaStepMayHaveBegun`, `StepSagaFail` with `OutcomeUnknown`; in a rollback re-run (`rbm`), S the `state != callReached` check |
| `LRec`, `LNS` | L `recordFresh`'s insert (S `a.store.Do` of `ToolResultStep` in a re-run), `recordNotStarted` |
| `LRet` | L the goroutine's deferred classification (held or cancelling the errgroup) |
| `DOpen`, `DGate`, `DWait` | L `Agent.run`'s `Load` (its `values`), the resume gate (`liveAttempts`, `toolHalt`), the errgroup's `Wait`, `errors.Join` and the drive's return |
| `DRollback`, `DRbStep` | S `Agent.rollback` and `Agent.rollbackRun`'s reverse walk: `failed` and `failedUnknown`, the `sagaArgsStep` listing of a failed retry-safe write (T3), the `started` halt (`toolHalt`), `!safety.RetrySafe()` skip |
| `DRbWait` | S the re-run of a retry-safe step with a compensator through `toolH` and `callTool`; `ErrToolOutcomeUnknown` lists it in `unknown` and the walk goes on, any other error stops it |
| `DRbComp`, `DRbNext` | S the memoized `sagaCompensateStep` (`Compensator.Compensate`, then its record) |
| `Crash`, `Cancel`, `Deadline`, `WrongAuth`, `FixAuth` | the faults; `WrongAuth` is H `Unrecorded` from a delegation under the wrong authority |

Not modelled: approvals (model 1b), sub-agent rollback recursion (`asSubAgent`, `bindRollback`),
the walk of a call's programmatic sub-runs (`walkSubRuns`, `rollbackSubRun`, `subRunLinks`),
pauses inside a tool, and an unregistered tool in the rollback (`tool == nil`).

### Properties

| Property | Kind | Statement |
|---|---|---|
| `NoDoubleFire` | invariant | a side effect fires at most once per call across drives; a call recorded as a known failure (or a saga failure) never fired, nor did an attempt recorded as not started; no call's effect (a retry-safe one included) fires after the rollback recorded its compensation |
| `TruthfulRecord` | invariant | a not-started record, or a failure recorded because the call was not called, means the tool never began; a failure whose text says the tool already ran means it began |
| `NoLostSibling` | invariant | a drive that returns only an Unrecorded refusal has recorded every side effect whose tool began |
| `SagaAccounted` | invariant | after a rollback, every effect still in place (a side effect's, or a retry-safe step's that changes state) is listed as an unknown outcome, or the rollback halted at or before it; the compensated ones are undone |
| `ArgsAfterReach` | invariant | the saga's accepted arguments are journaled only for a call that was reached |
| `BoundNotHit` | invariant | no claim is blocked by the attempt bound |
| `NeverBegunProgress` | liveness | a side effect whose live attempt never began does not halt for ever, unless a crash erased the process's record of the attempt |
| `UnrecordedContinues` | liveness | a run whose only issue is an Unrecorded refusal completes once driven under the right authority |
| `RollbackEnds` | liveness | a failed saga's rollback reaches its end (`SagaAborted`, or a halt for a human) |

Liveness assumes a middleware within the ToolMiddleware contract: one that leaves `next` running,
or ends a call without it and without `ErrToolNotCalled`, is answered with a halt by design.

### Configurations

Times are TLC's own on the development machine (Apple M1 Pro); each passing config is also run
for vacuity. The whole `toolcall/` pull-request set (ci, regress, finding and
limit configs) is about 2 minutes of TLC time on the CI runner, shared with the
other models' configurations four at a time. The model states #117's rules after its fifth review and the fixes of model 9's own
findings (below).

| Config | Group | What | States | Time |
|---|---|---|---|---|
| `contract-one` | ci | one side effect, middleware within the contract (a result check included), every fault | 33,884 | <1 s |
| `adv-one` | nightly | one side effect, any middleware but a direct call (retries, `next` left running, errors without the sentinel), every fault | 923,507 | 5 s |
| `saga-adv` | nightly | a saga Compensator with rewritten arguments (the accepted-arguments write), any middleware but a direct call, four attempts | 1,305,093 | 10 s |
| `hedge-one` | ci | a hedging wrapper: `next` left running, given up on a done context | 8,533 | <1 s |
| `direct-one` | ci | a middleware that calls the tool itself | 1,411 | <1 s |
| `siblings` | nightly | a side effect and a delegation that refuses Unrecorded; a cancellation, an error reply | 191,877 | 2 s |
| `live-contract` | nightly | `NeverBegunProgress`, contract middleware and retries without a cache answer, every fault | 83,596 | 3 s |
| `live-saga-args` | ci | `NeverBegunProgress` with the accepted-arguments write | 83,483 | 3 s |
| `live-unrec` | ci | `UnrecordedContinues`, a side effect and a delegation | 1,136 | 1 s |
| `saga-idem` | nightly | a retry-safe saga step that changes state, contract middleware, every fault (T3's shape) | 532,471 | 3 s |
| `rollback-rerun` | nightly | the rollback re-runs a retry-safe step cut off by a sibling's saga failure: `next` left running, a cache answer, a cancellation (T4, T5) | 245,482 | 3 s |
| `live-rollback` | ci | `RollbackEnds`: a result check that rejects every success of the re-run step | 900 | 1 s |
| `deep-adv-a2` | nightly | one side effect, any middleware, two extra invocations and two error replies, four attempts | 46,315,249 | 8 min* |
| `deep-adv-two` | nightly | two side effects, any middleware but a direct call, a cancellation and an error reply | 103,675,470 | 16 min* |
| `deep-two-side` | nightly | two side effects, contract middleware, every fault | 5,913,713 | 38 s* |
| `deep-siblings` | nightly | a side effect and a delegation, contract middleware on both, every fault | 65,561,449 | 24 min* |
| `deep-live-siblings` | nightly | `NeverBegunProgress` over the siblings with faults, without a cache answer | 3,338,804 | 4 min* |
| `deep-saga-idem-adv` | nightly | a retry-safe saga step that changes state, any middleware but a direct call, every fault | 6,651,541 | 48 s* |
| `deep-rollback-rerun` | nightly | the rollback's re-run with an error reply and a deadline beside the cancellation, four attempts | 29,286,833 | 5 min* |

\* The nightly times are TLC's own on the development machine while it ran other jobs (a load
average of about 30 on 10 cores), so they are upper bounds; on an idle machine `deep-siblings`
took about 5 minutes and `deep-adv-a2` about 3.

Regressions (each must fail with its property, and passes with its `Bugs` flag removed):

| Config | The historical rule | Expected | Trace |
|---|---|---|---|
| `regress/abandoned-next` | R117-6 (round 1): "not called" was the flag the base handler sets before `t.Call`; a middleware left `next` running and returned first (`FlagRule`) | `NoDoubleFire` | 22 states |
| `regress/direct-call` | round 2, item 2: under the flag rule, a middleware calling the tool itself left the flag unset (`FlagRule`) | `NoDoubleFire` | 11 states |
| `regress/refused-leak` | round 4, finding 1: `refused` was not terminal, so a leaked `next` moved it to `reached` after the call was recorded (`RefusedNotTerminal`; with T5's rule, no begin after the chain returned, as `NoClosedBegin`, it is stopped there too) | `NoDoubleFire` | 26 states |
| `regress/ran-early` | round 4 (c): the `ran` mark came before the refusal checks, so a retry of a refused call was recorded as "already ran" (`RanEarly`) | `TruthfulRecord` | 26 states |
| `regress/sealed-args` | round 5, F4: no began word, so a saga call whose accepted-arguments write a cancellation cut off was "called" and halted for ever (`NoBeganWord`) | `NeverBegunProgress` | 19 states |
| `regress/sibling-cutoff` | round 5, F1: an Unrecorded refusal cancelled the errgroup and cut off a sibling side effect in flight (`UnrecCancels`) | `NoLostSibling` | 62 states |
| `regress/sibling-cutoff-live` | the same, as liveness: the cut-off sibling halts the re-drive the refusal asked for | `UnrecordedContinues` | 41 states |
| `regress/t1-maperr` | model 9, T1: a middleware turned the tool's success into an error, recorded as a known failure (`NoToolOutcome`) | `NoDoubleFire` | 17 states |
| `regress/t1-saga-maperr` | T1 in a saga: a saga failure the rollback skipped and listed nowhere (`NoToolOutcome`) | `SagaAccounted` | 23 states |
| `regress/t1-retry` | T1: a retry refused after the tool fired, recorded as the call's known failure (`NoToolOutcome`) | `NoDoubleFire` | 25 states |
| `regress/t1-leak` | T1: `next` left running; the middleware's own error recorded while the tool fires (`NoToolOutcome`) | `NoDoubleFire` | 20 states |
| `regress/idem-saga-skip` | the P12 final review, bug 2: a retry-safe saga step that changes state, whose success a middleware turned into an error, was a known saga failure the rollback skipped (`IdemSagaSkip`, with `NoIdemBegan` as the code then was; fixed in #117 `be367d7`) | `SagaAccounted` | 23 states |
| `regress/t3-idem-earlier-attempt` | model 9, T3: a retry-safe saga step that changes state fired, was cut off with nothing recorded, and its re-run failed: the rollback skipped it (`NoIdemBegan`) | `SagaAccounted` | 29 states |
| `regress/t4-cache-while-running` | model 9, T4: a cache answer while `next`, left running, was in the tool; recorded and compensated before the effect landed (`NoRunningCheck`) | `NoDoubleFire` | 42 states |
| `regress/t5-begin-after-return` | model 9, T5: a `next` left running began a retry-safe tool after its chain returned and its compensation was recorded (`NoClosedBegin`) | `NoDoubleFire` | 67 states |
| `regress/t6-cache-while-earlier-runs` | model 9, T6: a re-drive's cache answer while an earlier drive's invocation was in the tool; recorded and compensated before it landed (`RunningReachedOnly`) | `NoDoubleFire` | 55 states |
| `regress/rollback-no-end` | model 9: the rollback's re-run stopped on any error, so a result check rejecting every success left it with no end (`NoRbUnknown`) | `RollbackEnds` | 71 states |
| `regress/t2-ran-before-args` | model 9, T2: the `ran` mark preceded the accepted-arguments write, so a retry after a failed write recorded "already ran" (`RanBeforeArgs`) | `TruthfulRecord` | 41 states |

Accepted limits:

- `limits/direct-then-next`: a middleware that calls the tool itself and also gets a refusal
  from `next` ends `refusedClosed`, indistinguishable from a call that never ran
  (`NoDoubleFire`). Calling the tool itself is outside the contract; alone it is answered with a
  halt (`direct-one`).
- `limits/cache-write-fails`: a middleware answers without `next` (a cache) and the result write
  fails. A closed call counts as possibly called (it could have called the tool itself), so the
  re-drive halts on a tool that never began (`NeverBegunProgress`).

### Findings

Found by this model in #117 at `0e0efc3`, each with a failing Go test, and fixed in #117 as the
model checked them (`a976104`); their counterexamples are the `t1-*` and `t2-*` regressions.

- **T1: a known failure was recorded for a side effect whose tool began and did not itself
  fail.** The loop decided "known failure" from the chain's error alone. The tool may have
  succeeded and a middleware turned that into an error (a result check, which the contract
  allows), or the chain returned a later invocation's refusal (a retry answered
  `ErrToolReinvoked`, or refused by the guard), or the middleware returned while `next` still ran
  the tool. The journal then said the call failed though its effect fired; in a saga the step was
  a `StepSagaFail` without `OutcomeUnknown`, which the rollback skips as "made no change" and
  `SagaAborted` listed nowhere. The fix: the base handler records the tool's own outcome
  (running, succeeded, failed, unknown); when the chain returns an error for a side effect whose
  tool began and did not itself fail, while its context is not done, the error is an unknown
  outcome: nothing is recorded and the run halts.
- **T2: "already ran" was recorded for a tool that never began.** Round 4's fix (c) moved the
  `ran` mark after `enterTool`, but it still preceded the accepted-arguments write. When that
  write failed, the tool never began (the seal made the call not called, rightly), yet a retry of
  `next` was answered "already ran and is not retry-safe", and that text was journaled. The fix:
  no `ran` map; an invocation that finds the began word `yes` is the reinvocation.

Found by the model's extension to retry-safe steps and the rollback's re-run, in #117 at
`10e889b`, each with a failing Go test, and fixed in #117 as the model checks them (`be4241f`,
`257596f`, `bb3dd20`); their counterexamples are regressions:

- **T3: an earlier attempt of a retry-safe saga step was listed nowhere**
  (`regress/t3-idem-earlier-attempt`, `NoIdemBegan`, `SagaAccounted`). A retry-safe call has no
  attempt marker. A step that changes state fired, the run was cut off (a cancellation, a crash)
  with nothing recorded, and the re-run failed with a known failure (its own error, a denial):
  the rollback skipped it as one that made no change, and the first attempt's effect stayed. The
  fix: the saga-args record, written before the call, is the "may have begun" mark of such a
  step, and a later known failure of it is an unknown outcome, listed.
- **T4: a success recorded while the tool still ran** (`regress/t4-cache-while-running`,
  `NoRunningCheck`, `NoDoubleFire`). A middleware left `next` running and answered from a cache;
  the call was recorded, and in a saga compensated before the tool's effect landed. The fix: a
  nil chain return while any invocation of the call is in the tool (of this chain, or one left
  running past an earlier drive) is an unknown outcome. One outcome word per chain is not enough:
  a sibling invocation overwrites it, and it does not see an earlier chain's.
- **T5: a retry-safe tool began after its chain returned** (`regress/t5-begin-after-return`,
  `NoClosedBegin`, `NoDoubleFire`). `closeCall` leaves a reached call `reached`, so a `next` left
  running entered the tool again after the result, and the compensation, were recorded. The fix:
  no invocation begins the tool once its chain has returned.
- **The rollback's re-run had no end** (`regress/rollback-no-end`, `NoRbUnknown`,
  `RollbackEnds`). `rollbackRun` stopped on any error of the re-run, so a result check that
  rejects every success left the rollback returning an error on every `RunSaga`, with no
  `SagaAborted` and no halt to resolve. The fix: a re-run whose outcome is unknown is listed, and
  the rollback goes on; a re-run answered without reaching the tool (a cache) is such an outcome.

Found after T4's fix, at `7c0ffd9`, with a failing Go test, and fixed in #117 (`38c1e69`):

- **T6: a cache answer while an earlier drive's invocation is in the tool**
  (`regress/t6-cache-while-earlier-runs`, `RunningReachedOnly`, `NoDoubleFire`). T4's fix reads
  the in-flight count only for a chain that reached the tool. A drive was cancelled while a
  `next` left running was in a retry-safe tool (nothing recorded); the re-drive's middleware
  answered from a cache without reaching the tool; the answer was recorded and, when a later
  step failed the saga, compensated, and the first invocation's effect landed after. The fix: a
  result while the count (of calls that are not ReadOnly, keyed by store, run and call) is above
  zero is an unknown outcome, whatever the chain's state.

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
- **Cancel (P14, D1).** `Cancel` of a run that is not over writes `run:cancelled`. The drive's
  checks follow `CancelRule`: `"turn"` is D1's original text (a `Get` when a drive starts and at
  every turn boundary); `"claim"` is the adopted L2 rule, a check after the claim is won and
  before the call that records the attempt as not started. `VerdictRule = "first"` is the adopted
  L3 rule: the first end marker in journal order is the run's end, and a drive or `Cancel` that
  wrote a marker reads the markers again before it reports. On a saga (`Api.sagaCancel`):
  `"marker"` is D1 as written (`run:cancelled`, and the drive that sees it rolls back and writes
  `run:aborted`); `"request"` is the rule this model proposes (L4): `Cancel` reads `run:start`,
  refuses a run with none (`ErrNotStarted`), and on a saga writes a rollback request, which is not
  an end marker; the drive that sees it rolls the run back and writes `run:cancelled`.
- **The run header and per-run options (P14, B1, D3).** The primary's first drive writes
  `run:start` with its caller's options (first writer wins); every later drive (a `Resume`, a
  recovery drive) loads them. An option set is a tool filter (the calls whose tools the run may
  use), a turn limit (the calls the whole run may make), and one value standing for every other
  journaled setting (system prompt, sampling, tool choice, output mode, typed schema,
  principal). A later caller (`Res`, a `Resume` of the primary's run) passes options of its own:
  a different limit is journaled as an amendment `run:limits:<n>` and takes effect, any other
  difference is `ErrConfig`. `Api.opt = "caller"` is the code before P14 (each drive runs its own
  caller's options, or the agent's defaults). A call outside the journaled filter is refused at
  dispatch, its error result recorded (`Api.filter = "dispatch"`); `"request"` only narrows the
  tools the model is offered. The model's calls are fixed, so a call to a filtered-out tool stands
  for a model that names a tool it was not offered, or a turn replayed from the journal.
- **Status (P14, D8).** Reads one run at any time: `run:start`, then the end markers, by one
  `Load` (`"load"`), by one `Get` per marker (`"gets"`), or by the `Get`s and, once any marker
  was found, the `Get`s again (`"regets"`). A paused, halted, running or limit-stopped run has no
  end marker and reports `Started` (D8: pauses are not journaled).
- **Not-started runs (P14's dispatch).** Recovery reads `run:start` under the lease; a run with
  none is skipped and reported (`ErrNotStarted`) once per worker process (`"report"`); the
  variants report it every pass (`"every"`) or skip it for good once reported (`"remember"`).
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
with the halted runs divided by C), the compensations of a rollback (one step here; models 5
and 9), token budgets as distinct from turn limits (both are limits under rule 2), typed runs
and their resumers (`ResumeTyped`), and image input.

### Model-code map

<!-- modelsync: no-code lifecycle DStart DAmend DTurn DPost DVerdict CGet CIns CRead CReq SPick SStart SGet Stall Wake Crash -->
<!-- modelsync: map lifecycle -->
| Label | Go |
|---|---|
| `DIdle` | `Lease` (`AcquireLease` under `<holder>#<token>`, `leaseToken`); for the primary, the caller's `Lease` around `Agent.Run`, or a plain `Agent.Run` |
| `DCheck` | `recoverRun`'s `runEnded` under the lease: `Store.Get` of each name in `endOfRunMarkers`; P14's dispatch: `RecordedStart`, a run with none skipped and reported once per process (`ErrNotStarted` through `WithRecoverErrors`) |
| `DResume` | `recoverRun` calling `resume(ctx, runID)` |
| `DOpen` | `Agent.run`: `openRun`, `completedAnswer` (a finished run returns its answer), `holdToStart`, the resume gate (`toolHalt`, `HaltCrashed`), the approval pre-pass (`ApprovalPending`); P14: the start check of `run:cancelled` and of a saga's rollback request, `run:start`'s options and the limit amendments from the same `Load`, the `ErrConfig` comparison, the turn limit |
| `DStart` | P14: the first drive's `run:start` insert with the caller's options (first writer wins; the stored entry is used) |
| `DAmend` | P14 (rule 2): the `run:limits:<n>` insert of a later drive's different limit |
| `DTurn` | P14 (D1): the `run:cancelled` (or rollback request) `Get` at a turn boundary, and the turn limit |
| `DClaim` | `claimNextAttempt` / `Journal.claim` (model 1), under the drive's context; P14 (D3): a call outside the journaled tool filter refused at dispatch, its error result recorded |
| `DPost` | P14 (D1, L2): `run:cancelled` (or the rollback request) read after the claim is won, `recordNotStarted` |
| `DCall` | `recordFresh`: the `sctx.Err()` check, `t.Call` |
| `DRecord` | `recordFresh`'s insert of the result (`putRecord` under `context.WithoutCancel`) |
| `DRollback`, `DAbort` | `Agent.rollback`: `rollbackRun`, then `store.Do` of `run:aborted`; P14 (L4, proposed): `run:cancelled` after a rollback a cancellation asked for |
| `DComplete` | the loop's terminal: `putRecord` of `run:complete` |
| `DVerdict` | P14 (D1, L3): the end markers read again; the first in journal order is reported |
| `DRel` | `Lease`'s deferred `ReleaseLease` |
| `PList`, `PNext`, `PSlot`, `PWait` | `RecoverLoop`'s `pass` and `every`: `lister.Runs(ctx, recoverFilter)` in the full pass (process `"pass"`), `lister.Runs(ctx, lapsedFilter)` in the lapsed loop (process `"tkp"`, `PassRule = "split"`; its slots are `WithRecoverLapsedConcurrency`'s, the `"tko"` driver); `recoverable`; `PNext`'s `InFlight` is the `inFlight` check before the slot wait; `PSlot` is the slot wait, then the in-flight re-check and mark under the lock (the model does not re-check: a run the other loop took meanwhile reaches `DIdle` and is refused by the lease, where the code skips it before acquiring); the ticker (`Recover`: one pass) |
| `OPick`, `OLease`, `OWrite`, `ORel` | `ResolveHaltRef` / `resolveHalt` with `checkNoLiveDriver`'s lease; `Approve` |
| `CGet`, `CIns`, `CRead`, `CReq` | P14's `Cancel` (D1): the end-marker check (and, under L4's rule, `run:start`), the `run:cancelled` insert, the read-back (L3), and a saga's rollback request (L4, proposed) |
| `SPick`, `SStart`, `SGet` | P14's `Status` (D8): `run:start`, then the end markers |
| `Tick` | wall-clock time: `driveWithRenew`'s renewal, lease expiry, `RecoverLoop`'s `time.Ticker` |
| `Stall`, `Wake`, `LeaseNotice` | a process pause; `renewLoop` returning `ErrLeaseLost` and cancelling the drive |
| `Crash` | a process dies (a worker restarts) |

`Leaser.ReapLeases`, which each lapsed pass of `RecoverLoop` calls first, is not modeled: it deletes the
lapsed leases of ended runs and of runs with no entry, checking the expiry in the same statement,
and deleting a lapsed lease changes nothing a holder can observe, since any holder may take it.

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
| `CancelFinal` | invariant | no effect fires under a claim won after `run:cancelled` (or a saga's rollback request) is in the journal, and a drive that read it claims nothing (P14, under every rule below) |
| `NoFireAfterCancel` | invariant | no effect is called once `run:cancelled` is in the journal (the literal form; not achievable, see the limits) |
| `AtMostOnce` | invariant | each call fires at most once |
| `EndMarked` | invariant | a drive that reports a run complete or aborted leaves the marker |
| `OutcomeRecorded` | invariant | an effect whose driver returned, did not crash and got no error on its record write is recorded |
| `BoundedPickup` | invariant (Timed) | a run whose lease holder died is taken over within `Bound` ticks of its lease lapsing |
| `PickedUp` | liveness | a run whose lease holder died is eventually taken over, or ends |
| `StatusTruthful` | invariant | `Status` reports a run completed, aborted or cancelled only if that is its first end marker in journal order, and started only if, at an instant during the call, it held `run:start` and no end marker (P14, D8) |
| `FilterHonoured` | invariant | no effect fires for a call whose tool is outside the run's journaled filter, whichever drive runs it (P14, D3) |
| `RunOptionsDurable` | invariant | every drive, a `Resume` and a recovery drive included, runs under the options journaled when it loaded the run (`run:start`'s filter and settings, its limit or the last amendment's), and fires no effect past that limit (P14, B1) |
| `NoFireOverLimit` | invariant | no effect fires past the limit journaled at that moment (the literal form; see the limits) |
| `CancelRollsBack` | invariant | a cancelled saga with an effect in place, no finished rollback, and no `run:complete` first, is still listed by recovery (no end marker) or being driven (P14, D1 on a saga) |
| `NotStartedOnce` | invariant | recovery reports a run with no `run:start` at most once per worker process (P14's dispatch) |
| `StartedRunSettles` | liveness | a started run ends, or halts for an operator (configurations with no pauses or limits): recovery never skips a run for good because it once had no `run:start` |
| `EffectNotReachable`, `PickupNotReachable` | vacuity | must be violated |

`BoundedPickup` is a bounded-response property: under `Timed`, time cannot pass while a process
can step, so a tick is an upper bound on a round trip, and a takeover within `Bound` ticks is a
safety property over the ghost clock `age`. `PickedUp` is its unbounded form, which holds under
both rules; only the bound shows the cost of a pass.

### Recovery cost: the pickup bound (L1, fixed by #126)

`RecoverLoop`'s godoc, since v0.9.0, says a dead holder's run is picked up within about one
interval of its lease expiring only while a pass is short: a pass visits every unfinished run it
lists, halted ones included, at about five store round trips each, and the next pass starts only
once this one has handed out every run. The model makes that cost visible. Under `Timed`, a
halted run's visit is five steps of the worker's slot (`DIdle` with the lease, `DCheck`,
`DResume`, `DOpen`'s halt, `DRel`), and the pass hands one run to the slot at a time, so a pass
over H halted runs takes about 5H ticks.

**L1 (`regress/pickup-halted-pile`, `BoundedPickup`).** Runs are listed in id order, so older
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
| `split` (since #126) | 3 | 3 | 4 | 4 |

**The rule (`PassRule = "split"`, `pickup-split`), `RecoverLoop` since #126.** L1 is now a regression. Each worker runs a second loop,
on the same interval, that lists only the unfinished runs whose lease row has lapsed (the stores
delete a row on release, so a lapsed row means its holder died or stalled) and drives them with a
slot of its own. The full pass is unchanged and still re-drives the halted and never-leased runs.
A dead holder's run is then taken over within 3 or 4 ticks of the lapse with 0 to 3 halted runs
(the interval is 2), against 4, 5, 10 and 15 under v0.9.0's rule: about one halted run's visit
per halted run listed before it. Ordering each pass's list with the lapsed runs first is not enough
(`PassRule = "lapsedFirst"`, `findings/pickup-lapsed-first`): a lease that lapses while a pass
walks the halted runs still waits for the next pass. The rule needs a store query for runs with
a lapsed lease (`RunFilter.LeaseLapsed`) and its own concurrency (`WithRecoverLapsedConcurrency`).

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
| `pickup-split` | ci | The recovery rule since #126 (PassRule "split"): a second loop per worker visits only the runs whose lease lapsed, every interval, with a slot of its own. Two halted runs listed before a leased primary's run; the primary dies. Takeover within Bound ticks of the lapse. | 4,859 | 1 s |
| `pickup-reach` | ci | Vacuity of the pickup configurations: a dead holder's run waits after its lease lapsed (PickupNotReachable must be violated), under the rule since #126. | 189 | <1 s |
| `life-resolve` | ci | One run: a leased primary Run, one Recover pass (not RecoverLoop) and an operator resolving halts; an error reply and a crash. | 32,982 | 1 s |
| `live-pickup` | ci | PickedUp under v0.9.0's rule: a halted run listed before a leased primary's run, the primary dies; every step and the clock weakly fair. | 4,699 | 2 s |
| `deep-two-workers` | nightly | One run: a leased primary Run and two RecoverLoop workers; an error reply and a crash. | 3,620,876 | 2 min |
| `deep-cancel-plain` | nightly | Cancel (P14) under the proposed rules, a plain Run of two calls and a worker; an error reply and a crash. | 2,290,004 | 1 min |
| `deep-leased-resolve` | nightly | One run of two calls, a saga: a leased primary Run, a RecoverLoop worker and an operator resolving halts; an error reply and a crash. | 1,269,395 | 34 s |
| `deep-paused` | nightly | Three runs (one waiting for an approval, one halted, a leased primary's), a worker, the operator, a crash. | 3,440,717 | 2 min |
| `deep-stall` | nightly | One run: a leased primary Run and two workers; a lease holder stalls past its TTL. | 2,183,810 | 1 min |

### Regressions and limits

Each regression restores a historical rule behind `Bug` and must fail with its property; each
finding fails under the rule as it stands and flips to a regression once the fix is adopted; each
limit states behavior the design accepts. L1 was v0.9.0's rule (`RecoverLoop`); #126 fixed
it, and it is a regression. L2 and L3 are against D1's original text; both rules are now in D1
(`docs/design/api-v1.md`), and the findings stay open until P14 implements them.

| Config | Group | The rule or behavior | Expected | Trace |
|---|---|---|---|---|
| `regress/no-recheck` | regress | #114: recovery checked the end markers when it listed a run, not again under the lease; a run the primary finished in between was handed to resume (Bug = "NoRecheck"). | `NoResumeOfFinished` | 14 states |
| `regress/no-recheck-stall` | regress | #114 as TestHA_MultiProcessStallPastTTL caught it: the primary stalls past its TTL, the worker takes the run over and finishes it, and the worker's next pass, which listed the run before, resumes it (Bug = "NoRecheck"; no crash). | `NoResumeOfFinished` | 16 states |
| `regress/shared-holder` | regress | #58 finding 4: Lease claimed under the bare WithLeaseHolder name, which AcquireLease takes as a renewal: a worker's recoverer and its primary under one name both drive the run (Bug = "SharedHolder"). | `OneDriverPerEpoch` | 6 states |
| `regress/shared-holder-stall` | regress | #58 finding 4 in the HA harness: a worker stalls holding a run, a restarted worker under the same name renews the stalled worker's live lease and drives the run (Bug = "SharedHolder"). | `OneDriverPerEpoch` | 10 states |
| `regress/no-abort-marker` | regress | #31: a saga whose rollback finished recorded no run:aborted, so every pass re-drove it (Bug = "NoAbortMarker"). | `EndMarked` | 8 states |
| `regress/record-under-ctx` | regress | #58 finding 1: the SQL stores recorded a step's result under the drive's context, so a drive whose lease was lost while the effect ran dropped the result (Bug = "RecordUnderCtx"). | `OutcomeRecorded` | 10 states |
| `regress/skip-when-busy` | regress | #58, RecoverLoop's first version: a pass that found every slot busy stopped, and the next began from the top, so runs listed after halted ones starved (Bug = "SkipWhenBusy"). | `PickedUp` (liveness) | 30 states |
| `regress/pickup-halted-pile` | regress | L1, fixed by #126: v0.9.0's RecoverLoop (PassRule "all") visits every unfinished run in id order, halted ones included, so a dead holder's run listed after two halted runs is taken over more than Bound ticks after its lease lapsed. | `BoundedPickup` | 20 states |
| `regress/replay-finished` | regress | c6deb766: a drive of a finished run asked the model for another turn, whose calls have new tool-use ids (Bug = "ReplayFinished"). | `FinishedFinal` | 16 states |
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
- **The path rule.** The map's rows are marked regions (in `agent/recovery.go`, `agent/lease.go`,
  `agent/loop.go`, `agent/saga.go`, `agent/halt.go` and `agent/journal.go`), so a pull request
  that changes one changes `spec/tla/lifecycle/` or carries a `Protocol-Impact` override, and the
  Models job checks this model whenever it changes. Code outside the marked regions that the
  model depends on (`agent/runstart.go`, the stores' `Leaser` and `Lister`) is held by review.
- **Built (M4, #125):** the region markers, the path rule and `TestProtocolVocabulary`; see
  [Keeping the code and the models in step](#keeping-the-code-and-the-models-in-step).
- **Not built yet** (the plan's M3, for every model): trace validation (`bidetrace` hooks,
  `tracestore`, a trace spec per model) and the counterexample-to-test helper
  (`agent/internal/interleave`). For this model, the hooks would be `recoverRun` (acquire, `runEnded`, `resume`, release), the end-marker
  writes, `renewLoop`'s `ErrLeaseLost`, and P14's `Cancel` and `run:cancelled` checks; the
  multi-process HA harness is the natural producer.
- **Findings need Go tests before their fixes**, as for every model: L1's is
  `TestRecoverLoop_TakesOverALapsedLeaseWithinAnIntervalBehindHaltedRuns` (`agent/recover_lapsed_test.go`,
  committed failing before #126's fix), which measures a takeover behind halted runs against the
  interval, and L2 and L3 become tests of P14.

## Model 11: delegation, sub-run authority and saga trees

`delegation/Delegation.tla` checks what model 9 treats as a black box: a call that runs another
agent's run. It covers `audit.AttenuatingSubAgent` and the sub-agent tool it wraps, programmatic
sub-runs (`RunInfo.SubRunFor`, #127), and a saga's rollback through the whole tree. The claim
protocol under each call is model 1's, and one turn's sibling calls are model 9's. Here the
calls of a run go one after another, as in an errgroup with a limit of one, and a sub-run is a
nested drive.

### What is modelled

- **A tree of runs** (`Tree`, from `DelegationMC.tla`). Each run is one turn of calls of four
  kinds:
  - a compensable side effect with an attempt marker;
  - a tool that fails;
  - a delegation (`AttenuatingSubAgent` over a sub-agent's run);
  - a tool that starts a programmatic sub-run. It may itself be a retry-safe compensable write,
    and it declares the agent the rollback uses (`WithSubRuns`: the same store, none, or
    unusable).
- **Grants.** A grant is an id that names its chain, a subject, its parent, and an expiry on a
  discrete clock. A drive binds a root grant (`P`), none, or the wrong one (`W`). After `P`
  expires, the operator binds a live one (`P2`). A child grant is minted from the bound grant,
  checked, and journaled (`RecordGrant`). On resume it is reused only after its subject, its
  parent (the signature and narrowing checks), and its expiry are checked. A delegation with no
  grant bound journals the ungranted marker. `CallGuard` refuses every call of a delegated
  sub-run once its grant has expired.
- **Refusals.** Unrecorded refusals are a storage error, a resume under other authority, minting
  from an expired bound grant, and minting onto a sub-run with records but no authority. Recorded
  failures are a journaled grant for another subject, an expired journaled grant (F2), and a
  child grant the `AttenuateFunc` gives another subject or a past expiry.
- **The loop's rules.** An Unrecorded refusal does not cut off later calls. A halt or a lost
  outcome sets the saga's `halted` flag, which also holds when it is joined with an Unrecorded
  refusal (bug 8). A sub-run that stopped short of a verdict records nothing
  (`subRunUnfinished`). A saga step's failure is a `StepSagaFail`, and the drive returns the
  held refusals and halts joined.
- **Programmatic sub-runs.** In a saga's tree (inherited through plain runs), the link is
  written before the sub-run records anything. A sub-run on another store is refused, and so is
  one started after its call returned. An ID from another call's scope is refused.
- **The rollback** (`rollbackRun`). Failed calls are handled first, then every call in reverse.
  It recurses into a delegation through `bindRollback` and `BindRollback`, which rebinds the
  journaled grant and identity or no grant. It walks a call's linked sub-runs latest first, with
  the declared agent, or with no tools, which lists their writes. It compensates as a memoized
  step. It re-runs a retry-safe compensable call that has no result, and reloads the links
  afterward. A stopped rollback is resumed by a later drive.
- **Faults**, each with a budget:
  - a crash at every step: the journal stays, and the stack, held errors and flags are lost;
  - an error reply on the wrapper's leaves, a link or a compensation record (A3: committed or
    not);
  - a transient read error of a sub-run's journal;
  - a lost outcome;
  - a drive bound to the wrong authority;
  - the clock.

  The operator binds the authority the last refusal asked for. It is strongly fair in the
  liveness configurations.

Abstracted away:

- approvals (model 1b);
- the claim's races (model 1);
- middleware and sibling concurrency (model 9);
- model turns beyond one;
- the token budget;
- sessions.

### Model-code map

Each label is one atomic step. Files are `audit/delegate.go` (A), `agent/subagent.go` (U),
`agent/loop.go` (L), `agent/saga.go` (S), `agent/runctx.go` (R), `agent/keys.go` (K),
`agent/budget_tree.go` (B), `agent/toolexec.go` (T) and `internal/toolhook/toolhook.go` (H).
Every row but the no-code list has a `// protocol:delegation begin ... end` region. The no-code
list holds the driver, the faults, the tool's own code, and the side effect's claim and call,
which are models 1 and 9.

<!-- modelsync: no-code delegation Idle Back EMark EFire ERes SRun SRet SWrite SLateRet Tick WrongAuth FixAuth Crash -->
<!-- modelsync: map delegation -->
| Label | Go |
|---|---|
| `DOpen` | S `runSagaWithTelemetry`: `sagaFailure` (an aborting saga goes to its rollback); L `Agent.run`'s open and resume gate (model 9's `DOpen`, `DGate`) |
| `DNext` | L the goroutine's `gctx.Err()` and `halted.Load()` checks: a call after a halt does not start |
| `DGuard` | T the base handler's `toolhook.CallGuard` (H); A `init`'s guard: a call under an expired delegated grant is refused, recorded |
| `DgRead` | A `attenuatingSubAgent.Call`'s run scope and `journaledAuthority`; `storageFailure`, `authorityErr`, `unrecorded`; the checks of an ungranted, a legacy, a foreign-subject, a wrong-parent or an expired journaled grant |
| `DgUng` | A `Call` with no grant bound: the `audit:delegation:ungranted` marker |
| `DgMint` | A `Call`'s mint: the bound grant's expiry, `t.narrow`, the subject, `CheckAttenuation`, the child's expiry |
| `DgRec` | A `SignGrant`, `RecordGrant` (a failure is Unrecorded) |
| `DgRun`, `DgRet` | A the rebound identity and `grantCarrier` (`delegated`); U `subAgentTool.Call` (`RunSaga` in a saga, `Run` otherwise; `subRunUnfinished`) |
| `SStart` | K `checkRunID`; R `derivedRunID`, `stepRunName`, `withRunContext`'s `sagaTree`; `linkSubRun`'s other-store refusal (`sameStore`) |
| `SLink` | R `linkSubRun`'s `subRunLinkStep` write; L `Agent.run`'s call of it |
| `DClass` | L the goroutine's deferred hold (Unrecorded, `halted` for a joined halt or lost outcome) and the classification through `subRunUnfinished`; U `subRunUnfinished`; H `Unrecorded` |
| `SLate` | L `started.callReturned()`; B `callUsage.callReturned`; R `linkSubRun`'s `returned` check |
| `DRb`, `DRbEnd` | S `runSagaWithTelemetry`'s rollback, `Agent.rollback` (`run:aborted`) |
| `DEnd` | L the drive's return: the refusal joined with a held pause or halt |
| `RbOpen`, `RbLoop` | S `rollbackRun`: its `History`, `subRunLinks`, the failed calls first, then every call in reverse |
| `RbSub`, `RbSubRet` | S `walkSubRuns`, `rollbackSubRun`, `declaredSubRunAgent`, `subRunLinks`; U `subRunAgentFor` |
| `RbBind`, `RbRec`, `RbBindRet` | U `bindRollback`, `asSubAgent`; H `RollbackBinder`; A `BindRollback`, `checkChild`, `withoutGrant`; S the recursion into the sub-agent's run |
| `RbComp` | S the memoized `sagaCompensateStep` |
| `RbRe`, `RbReRun`, `RbReRet`, `RbReW` | S the re-run of a retry-safe compensable call through `toolH` and `callTool`, and the links reloaded after it |
| `Idle`, `Back`, `Tick`, `WrongAuth`, `FixAuth`, `Crash` | the root's drives and the environment |
| `EMark`, `EFire`, `ERes` | a side effect's claim, call and outcome (models 1 and 9) |
| `SRun`, `SRet`, `SWrite`, `SLateRet` | the tool's own code: `Run` or `RunSaga` of its `SubRunFor` ID, and its own write |

### Properties

| Property | Kind | Statement |
|---|---|---|
| `AuthorityNarrows` | invariant | A child never acts (fires a write, or compensates one) with authority its delegation did not grant. No call of a delegated sub-run reaches its tool after the grant expired. |
| `RollbackSound` | invariant | Once the root's rollback finished (`run:aborted`), every write still in place anywhere in the tree is listed. It is listed itself in `Uncompensated` or `UnknownOutcome`, or a call above it is in `Uncompensated`. |
| `RollbackUnderGrant` | invariant | A compensation in a delegated sub-run runs under the authority the sub-run journaled, which is the one its write fired under. |
| `HaltPropagates` | invariant | In a saga, no call starts after a call of the same drive returned a halt or a lost outcome, alone or joined with an Unrecorded refusal, from anywhere below it. |
| `NoForgedSubRun` | invariant | A sub-run runs only under an ID derived from its own call, while the call is open. |
| `NoFalseFailure` | invariant | A delegation is recorded as failed only for a permanent cause, never for a storage error or a resume under other authority. |
| `UnrecordedContinues` | liveness | A run stopped by Unrecorded refusals completes, aborts or halts for a human once the operator binds the authority they ask for. |
| `RollbackEnds` | liveness | A failed saga's rollback reaches its end. |
| `NoFireAfterExpiry` | invariant (limit) | The literal form: no write fires after its grant expired. |

### Configurations

Times are TLC's own on the development machine (Apple M1 Pro). Each passing configuration is
also run for vacuity. The model states the rules of `main` after #127. The `ci` set (regress,
finding and limit configurations included, with vacuity runs and JVM starts) takes about
45 seconds (36 for the `ci` group with its vacuity runs, 9 for the 18 `regress`, `finding` and `limit` configurations at four at a time) on the development machine.

| Config | Group | What | States | Time |
|---|---|---|---|---|
| `deleg-faults` | ci | A saga delegates under `P`, then fails. It covers mint, reuse on resume and `BindRollback`, with a crash, an error reply, a read error, a wrong binding, and `P` expiring. | 53,760 | 1 s |
| `deleg-ungranted` | ci | The same with no grant bound: the ungranted marker, and a resume under a grant refused. | 10,022 | <1 s |
| `deleg-expiry` | ci | A child grant that expires: `CallGuard`, the expired journaled grant (F2), and `BindRollback` with no expiry check, with a crash. | 1,228 | <1 s |
| `nested` | ci | A delegation inside a delegation, with a crash, a read error, and `P` expiring. | 17,854 | <1 s |
| `halt` | ci | The child's turn holds an Unrecorded refusal joined with a lost outcome, with a crash. | 2,787 | <1 s |
| `subruns` | ci | Programmatic sub-runs that are declared, undeclared, and declared on another store, with a crash and an error reply. | 2,389 | <1 s |
| `sub-long` | ci | A tool-use ID longer than `encodeID`'s limit (B1). | 207 | <1 s |
| `sub-store` | ci | A saga's sub-run on another store is refused (B2). | 119 | <1 s |
| `sub-late` | ci | A sub-run started after its call returned is refused. | 128 | <1 s |
| `plain-tree` | ci | A plain sub-run's sub-run, three levels (B3), with a crash and an error reply. | 1,580 | <1 s |
| `rerun` | ci | The rollback re-runs a call that never ran, which starts a sub-run, and its outcome may be lost (B4). | 580 | <1 s |
| `live-unrec` | ci | `UnrecordedContinues` and `RollbackEnds` with a read error, an error reply, a wrong binding, and `P` expiring. | 5,501 | 1 s |
| `fix-d1-chain` | ci | D1's proposed fix, with both liveness properties. | 647 | <1 s |
| `fix-d2-guard` | ci | D2's proposed fix, with a crash. | 1,118 | <1 s |
| `fix-d3-recurse` | ci | D3's proposed fix, with a crash. | 341 | <1 s |
| `deep-deleg` | nightly | The saga delegation under every fault, two of each, with a child grant expiring at tick 2. | 1,211,214 | 33 s |
| `deep-nested` | nightly | Nested delegations under two crashes and every other fault. | 1,287,276 | 43 s |
| `deep-halt` | nightly | Halt propagation with three crashes, two error replies and two read errors. | 141,806 | 2 s |
| `deep-two` | nightly | Two delegations with D1's fix, `P` expiring between them, and every fault. | 8,487,708 | 2 min 29 s |
| `deep-subruns` | nightly | Programmatic sub-runs under three crashes and three error replies. | 40,197 | 1 s |
| `deep-plain-tree` | nightly | The three-level plain tree with a sub-agent at its bottom, D3's fix, crashes and error replies. | 48,249 | 1 s |
| `deep-rerun` | nightly | The rollback's re-run in a delegated sub-run that starts a sub-run, with D2's fix, the grant expiring, lost outcomes and crashes. | 143,739 | 1 s |
| `deep-live` | nightly | Both liveness properties over nested delegations, with every fault. | 443,210 | 1 min 1 s |

Regressions. Each must fail with its property, and passes with its `Bug` value set to `"none"`:

| Config | The historical rule | Expected | Trace |
|---|---|---|---|
| `regress/hidden-halt` | #117 final review, bug 8 (rev117e audit 1): an Unrecorded refusal joined with a lost outcome did not set `halted` (`HiddenHalt`). | `HaltPropagates` | 66 states |
| `regress/storage-recorded` | rev117e audit 2: a store error on the delegation's authority was recorded as its failure (`StorageRecorded`). | `NoFalseFailure` | 38 states |
| `regress/mint-on-records` | rev117e audit 3: a grant was minted onto a sub-run with records but no journaled authority, and the rollback compensated those records under it (`MintOnRecords`). | `RollbackUnderGrant` | 32 states |
| `regress/foreign-bind` | rev117e audit 4: `BindRollback` did not check the subject, and compensated under another delegation's grant (`ForeignBind`). | `AuthorityNarrows` | 18 states |
| `regress/no-bind` | R117-4: the rollback walked a wrapped sub-run under the parent's authority (`NoBind`). | `RollbackUnderGrant` | 37 states |
| `regress/expired-unrecorded` | round 5, F2: an expired journaled grant was Unrecorded, which no binding puts right, so the saga stopped on every drive (`ExpiredUnrecorded`). | `UnrecordedContinues` | 28 states |
| `regress/no-call-guard` | round 5, F3: expiry was checked only at mint and reuse, so the sub-run's calls ran past it (`NoCallGuard`). | `AuthorityNarrows` | 45 states |
| `regress/wrong-auth-recorded` | round 3, item 3: a resume under other authority was recorded as a failure (`WrongAuthRecorded`). | `NoFalseFailure` | 28 states |
| `regress/mint-foreign` | round 3 (b): an `AttenuateFunc` could set another subject (`MintForeign`). | `AuthorityNarrows` | 14 states |
| `regress/b1-long-id` | #127 S1 review, B1: links looked up by the raw tool-use ID missed a long ID's digest (`B1`). | `RollbackSound` | 33 states |
| `regress/b2-other-store` | B2: a saga's sub-run journaled to another store, which the rollback did not read (`B2`). | `RollbackSound` | 35 states |
| `regress/b3-plain-link` | B3: links were written only when the call's own run was a saga (`B3`). | `RollbackSound` | 50 states |
| `regress/b4-rerun-links` | B4: after a re-run with an unknown outcome, the links were not reloaded (`B4`). | `RollbackSound` | 39 states |
| `regress/late-start` | #127 review (c): a sub-run started after its call returned was accepted (`NoReturnedCheck`). | `NoForgedSubRun` | 9 states |

Accepted limit: `limits/fire-in-flight`. `CallGuard` is asked when a call reaches its tool, so a
tool entered before the grant expired may fire after it (`NoFireAfterExpiry`).
`AuthorityNarrows` states the guarded form.

### Findings

Found by this model on `main` at `635e7c0` (#127 merged). Each has a failing Go test in a
scratch directory (`audit/zz_model11_test.go`, not in this pull request). Each fix is checked in
the model (`Fix`) as a `ci` configuration. The findings stay open (`findings/`) until the code
adopts a fix.

- **D1: a saga whose delegations were minted under two bound grants can never finish its
  rollback** (`findings/d1-rotation`, `RollbackEnds`).
  - `BindRollback` verifies each journaled grant against the one grant bound now.
  - Minting from an expired bound grant is refused with "bind a live one and drive again". A
    saga that delegated under `P`, and then under the live `P2`, has two delegations, and no
    single binding verifies both. The walk stops at the first that does not match, latest
    first, whichever grant is bound.
  - The test is `TestModel11_D1_RollbackAcrossRotatedGrants`. It rotates the root grant between
    drives and re-drives under each grant in turn; the rollback never finishes.
  - Proposed fix (`ChainBind`): the caller binds every grant the saga minted under, beside the
    current one, and `BindRollback` verifies a journaled grant against the one it was minted
    from.
  - A grant never bound stays refused, which keeps `TestR117_BindRollbackRefusesAForeignParent`.
    A fix that only checks the signature would break that test.
  - The maintainers adopted this rule (multi-grant binding). A separate change fixes D1 to D3 in
    the code; the findings stay open until it lands.
- **D2: the rollback's re-run calls a tool after the delegation's grant expired**
  (`findings/d2-rerun-expired`, `AuthorityNarrows`).
  - `BindRollback` rebinds the journaled grant with `WithGrant`, which drops the `delegated`
    mark, so `CallGuard` passes every call under it.
  - A compensation needs that, because F2 lets a rollback compensate after expiry. But the
    re-run of a retry-safe compensable call with no result goes through the base handler and
    calls the tool, which is a forward act.
  - The test is `TestModel11_D2_RollbackRerunAfterGrantExpiry`. The sub-run's own rollback,
    under the delegated grant, is refused for the same call, and the parent's walk then calls
    the tool.
  - Proposed fix (`GuardRerun`): `BindRollback` keeps the `delegated` mark, which `Compensate`
    does not consult. A re-run that `CallGuard` refuses is listed in `UnknownOutcome`, and the
    walk goes on. Without that listing the rollback would stop for ever.
- **D3: a sub-agent's writes in a plain sub-run of a saga are skipped**
  (`findings/d3-plain-deleg`, `RollbackSound`).
  - A plain run (`Run` of a `SubRunFor` ID, which #127 allows in a saga's tree) records a
    sub-agent call that failed as an error result.
  - `rollbackRun` skips a call with an error result before it recurses into a sub-agent. The
    sub-agent's run had written and then failed (a model error, or an expired journaled grant),
    so its write is neither compensated nor listed.
  - The test is `TestModel11_D3_FailedSubAgentInPlainSubRunSkipped`.
  - Proposed fix (`RecurseFailed`): recurse into a sub-agent call whatever its result, as the
    function's comment already says.

### Keeping model 11 and the code in step

- **The map** above, with its `// protocol:delegation` regions in `agent/`, `audit/` and
  `internal/toolhook/`, checked by `modelsync`. A pull request that changes one changes
  `spec/tla/delegation/` (the path rule), so the Models job checks this model on it.
- **Regressions** for every historical bug of #117's delegation rounds and #127's S1 review
  that the model can state, each a `Bug` value.
- **No vocabulary block.** The plan asks for one for the claim records only (section 6.2).
  This model's records (the grant leaf `audit:grant:`, the ungranted marker, the `@subrun/`
  link and the saga records) are named in the map.
- **Not built yet** (M3): trace validation. The hooks would be `Call`'s refusals and leaves,
  `linkSubRun`, `bindRollback` and the walk's steps.

## What the bounds do not cover

Model 11: one turn per run, calls one at a time, trees of up to four runs and three levels, two
root grants and a clock of three ticks, and up to three of each fault nightly. A bug that needs
two concurrent sibling delegations, a second model turn, or more than two rotations of the
bound grant is outside the check.

Model 9: one turn of one or two calls, two invocations of the base handler per call at a time,
one extra invocation on pull requests, one of each fault (two error replies nightly). A bug that
needs three concurrent invocations of one call, or a second turn, is outside the check.

Model 10 adds: one or two recovery workers at concurrency 1, up to three runs (four in the pickup
measurements), one or two calls per run, one error reply, one crash and one stall per run of the
checker. The pickup bound is measured for a TTL and an interval of 2 ticks and up to three halted
runs; the growth it shows (about one halted run's visit per halted run) is an observation at those
bounds, not a proof for larger ones.

Model 1b adds: three approvers and a policy of 2 of 3 (tightened to 3 or loosened to 1 by a
redeploy), up to three decisions and two `Approve` calls, one call. A bug that needs more
decisions than that, or two gated calls whose approvals interact, is outside the check.

Two drivers (three in `deep-drivers`, nightly, with one error reply), one crash, one
cancellation, up to two error replies per run on pull requests (three in the F3 configs) and
four nightly; one call except in the intent configurations.
Calls interact only through the fault budgets and the id pool, so two independent calls add no
behavior the one-call configurations miss. A bug that needs more than these is outside the check.
