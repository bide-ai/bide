# Formal models of the coordination protocols (design proposal and plan)

Status: accepted, in progress. It expands the
[roadmap item](../ROADMAP.md#formal-models-of-the-coordination-protocols) into a plan. Models 1,
1b, 2, 7, 8, 9 and 10 (numbered as in `spec/tla/README.md`) are implemented in
[spec/tla](../../spec/tla/README.md) and checked on every pull request. M0, M1 and M4 (#125) are
done; of M2, the regression configurations and the expected-violation check are done and the
counterexample-to-test helper is not built yet; M3 and M5 are not started. The overview is
[Formal verification](../formal-verification.md). Where this plan and `spec/tla/README.md` differ, the README
states what is checked. Sections 4 and 5 below describe #92's final rules, in which the claim-held
pin and the reuse of a remembered claim id are gone (the prototype of section 4.8 predates them);
the script is `spec/tla/check.sh`, and the configurations have other names and bounds.

Grounded in: draft PR [#92](https://github.com/bide-ai/bide/pull/92) (P6a, head `9ace7c6`: `Store`,
`Journal`, the claim protocol after both reviews) and draft PR
[#90](https://github.com/bide-ai/bide/pull/90) (P10, head `d430525`: `ResolveHaltRef` with the
live-driver check). Function names below are those of these heads. Where they move before merge,
the model-code map (section 4.4) moves with them.

## 1. Summary

bide promises that a side effect fires at most once, across crashes, overlapping drivers and a
store that can report an error for a write it committed. That promise rests on a small protocol:
an exclusive attempt claim, a not-started record bound to the claim, numbered re-attempts, an
in-process memory of claims whose insert was ambiguous, a resume gate, and halt resolution. The
reviews of #92 and #90 found three double fires and two further errors in and around that protocol
after the existing crash sweeps, reference model and multi-process harness had passed. One double
fire needed two ambiguous store replies in a specific order; the other two needed no fault at all,
only a rule that let a later drive or a new logical call repeat an effect; one of the other errors
needed a specific interleaving of two drivers in one process.

This proposal adds TLA+ models of the coordination protocols, written in PlusCal, checked with
TLC in CI, and kept in step with the Go code by trace validation: the Go test suites emit protocol
events, and TLC checks that every emitted trace is a behavior of the model.

What this buys, stated narrowly:

- **Within the bounds checked, every interleaving of the protocol rules is explored**, including
  every placement of ambiguous replies and crashes. The existing tests explore the schedules they
  are written or randomized to reach.
- **Trace validation checks that the code implements the model** for the executions the test
  suites produce. It is not a proof that the Go code refines the spec; it catches drift on the
  paths the tests exercise.
- **Nothing is proven beyond the bounds.** Section 8 treats this as a risk, not a footnote.

A throwaway prototype of model 1 (about 120 lines of PlusCal, in section 4.8) was written to size
this plan. The numbers quoted for state counts and runtimes come from it, run on the development
machine (Apple M1 Pro, 8 TLC workers, TLC 1.7.4, JDK 21). CI numbers must be re-measured on the CI
runner, as for the benchmarks.

## 2. Why models, given the existing tests

bide already has strong tests for this layer: the DST crash sweep (`agent/dst_test.go`), the
reference model with crash sweeps and randomized schedules (`agent/refmodel_*_test.go`), the chaos
sweep at the storage port (`chaos/bide.go`, seven writes including `@journal`), the storetest
ambiguous-claim cases, the commit-then-fail sweep, and the multi-process HA harness against real
Postgres (`store/postgres/ha_multiproc_test.go`). They check the implementation. They do not check
the design exhaustively, for three reasons:

1. **Fault placement is combinatorial.** A double fire in #92's second review needed a marker
   write that committed and errored, then a not-started write that committed and errored, then a
   reused claim in the same process, then a lost result write, then a new process. A sweep that
   injects one fault per run, or random faults, reaches such a schedule by luck.
2. **Interleaving is combinatorial.** The shared-flight bug in #92's first review needed the
   loser's probe to start the in-flight entry between the winner's claim and the winner's call.
3. **Reply ambiguity is invisible to the caller.** A3 lets an erroring `Insert` have committed.
   The code cannot branch on which happened; a test must inject both, at every write. A model
   gets this by construction: every write has three outcomes.

## 3. Scope and order

| # | Model | When | Trigger to start |
|---|---|---|---|
| 1 | **Claims and attempts:** `Journal.claim`, `claimNext`, numbered retries, not-started records, remembered claims and their retried not-started records, the resume gate, the Step loser path through shared flights, `ResolveHaltRef`'s live-driver check. | Now, as part of finishing #92 (see open question 1). | None: P6a's rules are final after the second review. |
| 2 | **The bide protocol:** the wire protocol of [#95](https://github.com/bide-ai/bide/pull/95), a remote worker running a tool under the engine's claim, redelivery, a worker that dies or answers late. | Done: `spec/tla/protocol` (#112), before #95 was accepted, as its section 18 required. | #95 is accepted and its message set is frozen, and before the first SDK is built on it. |
| 3 | **Leases and recovery:** acquire, renew, release, takeover, a holder that stalls past its TTL, `Recover`/`RecoverLoop` dispatch, and the lease that `checkNoLiveDriver` takes. Invariant: at-most-once holds with leases failing arbitrarily. | Deferred. | P13 (the `LeaseControl` option) and P14 (recovery dispatch, not-started runs skipped) are merged. |
| 4 | **The store contract and journal header:** A1 to A8 as an abstract store with concurrent readers, prefix-closed visibility (A2), first-writer races, the `@journal` header rules, refusal of other formats, redaction. | Deferred. | The first change to A1 to A8 or to the header rules after #92 merges, the runs-table follow-up, or P16 setting the final `JournalFormat`, whichever comes first. |
| 5 | **Saga rollback:** parallel siblings, sub-agents, calls that never started, compensation by recorded safety, `Cancel` on a saga. | Deferred. | P12 (rollback by recorded safety, `compensate.go`) and P14 (`Cancel` on a saga) are merged. |
| 1b | **Approval and halt resolution,** an extension of model 1: 1-of-1 `Approve` and m-of-n signed decisions with their recorded tally (approver sets, duplicate approvers or keys, a decision arriving while a resume runs), a denial that stays final when the gate later changes, approval bound to the exact call, contended and crashed halts, and resolution while a driver may be live in all three live-driver modes. Invariants: no action runs without a recorded sufficient approval; a denial is never overridden; resolution never overrides a live driver; at most one fire. | Done: `spec/tla/claims` (model 1b in `spec/tla/README.md`, #108); it found F5, fixed in #109. | None: the rules are those of #90 and #92. |
| 7 | **Flow semantics:** switch and loop replay, `run:complete` for flows, per-iteration step scoping. | Done: `spec/tla/flows` (model 7, #110). | #103 (which fixes these) is merged. |
| 8 | **Spend accounting:** `@llm/<n>`, `@spend/<id>` and `@spend-late/<id>` under A3, crashes, hedged losers and two drivers; `Result.Spend` and `Replay` equal the billed spend exactly once. | Done: `spec/tla/spend` (model 8 in `spec/tla/README.md`, #111). | P9 (#104). |
| 10 | **The run lifecycle and recovery:** `run:start`, `run:complete`, `run:aborted` and P14's `run:cancelled`; a leased `Run`, a plain `Run`, `resume`, `Recover` and `RecoverLoop` passes (list, lease, the #114 re-check, resume, release), halts, pauses and `ResolveHalt`, under lease expiry, a stalled holder, ambiguous writes and crashes, with a clock that makes a pass's cost visible (bounded pickup). It takes the lease and recovery part of model 3. | Done: `spec/tla/lifecycle` (model 10, #124, extended for P14 in #129); it found L1, fixed in #126, L2 and L3, adopted into D1, and L4 to L7, adopted into P14's design; L2 to L7 are open until P14. | #114, and P14's Run API (D1 `Cancel`, on a saga too; D3 the per-run tool filter; D8 `Status`; the journaling rule; not-started runs in recovery), whose rules it states before P14 is built. |
| 9 | **The tool-call state machine:** one turn's tool calls in an errgroup, the call state and began word, the base handler entered by several invocations, the tool middleware, the loop's record decision, retry-safe steps that change state, and `rollbackRun`'s re-run and compensation. | Done: `spec/tla/toolcall` (#123); it found T1 to T6 in #117, all fixed there. | P12 (#117). |
| 11 | **Delegation, sub-run authority and saga trees:** `AttenuatingSubAgent`'s grants (mint, reuse on resume, expiry, the subject check, the ungranted marker, a wrong-authority resume as Unrecorded), `CallGuard`, programmatic sub-runs (`SubRunFor`, links, one store per saga tree), the rollback's recursion through `BindRollback` and the sub-run walk, and halt propagation through the tree. | Started: `spec/tla/delegation`; it found D1 to D3 (see `spec/tla/README.md`). | P12 (#117) and P13 (#127). |
| 12 | **Sessions:** concurrent `Send` and `SendOnce` on handles, workers and processes, turn ordering, `from/` starting points, crashes between and within turns, the per-turn budget, run-ID collisions, and P14's `Cancel` of a turn's run. | Started: `spec/tla/sessions`; it found S1 to S4 (see `spec/tla/README.md`): S1, S2 and S4 are fixed in #137, S3 lands with P14. | None: the session rules are those of #22, #56 and #86. |
| 13 | **The whole-tree budget:** the bound on how far concurrent sub-agents can overshoot a shared token budget. | Candidate, low priority. | None. |

Models 2 to 5 wait because modelling a design that is still moving costs the model twice. Each
trigger is the merge that fixes the rules that model would check. The order of work: the approval
and halt-resolution extension of model 1 first, then model 2 (it gates the acceptance of #95),
then model 4, then models 3 and 5 with the waves that fix their rules, then sessions and flow
semantics, and the budget bound last. Plan flows (`plan/flow.go`) keep
their own markers until P5b lowers them onto claims; after P5b, a flow node becomes one more
driver path in model 1 rather than a model of its own.

## 4. Model 1: claims and attempts

### 4.1 What is modelled and what is not

Modelled: one run; a set of logical calls (tool calls and Steps); for each call, the attempt
markers `attempt:tool:<id>` or `attempt:step:<name>` and their numbered re-attempts
`attempt:retry:<n>:...`, the not-started keys `attempt:not-started:<claim>:<marker>` holding
a not-started record, and the result key (`tool:<id>` or the step
name); drivers grouped into processes; the process-wide `pendingClaims` memo and in-flight steps;
crashes; ambiguous store replies; context cancellation before the effect; the resume gate; the
Step loser path; halt resolution.

Abstracted away: record encoding, salts, `Seq` values and `Load` pagination (A2 is assumed here and
modelled in model 4); the header (one guard, section 4.9); the model's conversation (the caller
appears only as "may issue a new call after reading a failure", section 5.2); wall-clock time (the
minimum halt age is an assumption, section 4.4); lease internals (a flag per driver; model 3).

### 4.2 State variables

| Variable | Meaning | Go counterpart |
|---|---|---|
| `marker[c][g]` | claim id stored under call `c`'s attempt `g`, or 0 | `retryAttemptStep(base, g)` entries |
| `nsSet` | the `<<c, g, i>>` whose not-started key holds a `StepNotStarted` record | `notStartedStep(key, i)` entries |
| `result[c]` | `None`, or who recorded it and what it says (`driver`/`resolver`, `ok`/`error`) | `ToolResultStep(id)` or the step name |
| `pending[p][c][g]` | the claim ids remembered by process `p` for that marker key (a set; see finding F1 in `spec/tla/README.md`) | `pendingClaims` (`claimMemo`) |
| `flight[p][c]` | `None` or the driver leading process `p`'s in-flight call of `c`'s result key | `flights` (`shareFlight`, `joinFlight`) |
| `pc[d]`, `g[d]`, `cid[d]`, `oldId[d]`, `won[d]`, `reply[d]`, `outcome[d]` | driver `d`'s position and locals | the call stack of one drive |
| `lease[d]` | whether `d` holds the run's lease (leased drivers only) | `Leaser` |
| `ambig`, `crashes`, `cancels` | fault budgets used so far | none (bounds) |
| `fired[c]`, `firedAt[c][g]` | ghost: effect calls, and the claim each fired under | the counters in the tests |

Claim ids are allocated as the smallest id nothing in the state refers to (canonical fresh names).
A counter would make every re-drive a new state forever; with canonical allocation, unbounded
re-drives keep the state space finite, which liveness checking needs (section 4.6). This is sound
because the protocol compares ids only for equality and never orders them. It is not a full
canonical form: two states that differ only by a renaming of ids are still distinct, so a larger
id pool still multiplies the state count (section 4.7).

### 4.3 Store writes: three replies

Every `Store.Insert` in the model is one atomic step with three possible replies:

- `ok`: the entry is stored if the name was free (A1: the first writer wins), and the caller sees
  the stored bytes, its own or the winner's.
- `err_nc`: error, not committed. Nothing changes.
- `err_c`: error, committed (A3: on an error the entry "is either absent or complete"). The entry
  is stored if the name was free, but the caller sees only the error.

The caller cannot tell `err_nc` from `err_c`; the driver's next step reads only "error". Each error
reply consumes one unit of the `MaxAmbig` budget, so TLC places every combination of ambiguous
replies on every write, up to the bound.

A configuration flag adds a fourth behavior, `err_late`: the error is returned and the write
commits at any later step, or never. This is the realistic reading of a client-side timeout on
Postgres, where the server may commit after the client gave up. A3 as written ("absent or complete"
at return) excludes it. The model should be checked under both readings: if an invariant holds only
under strict A3, the store contract must say so and each store must be shown to meet it (open
question 2).

In PlusCal the reply is a macro, and each write site applies its effect when the reply is not
`err_nc`. This is the shape used by the prototype:

```tla
macro Reply() begin
  either reply := "ok";
  or await ambig < MaxAmbig; ambig := ambig + 1; reply := "err_nc";   \* error, not committed
  or await ambig < MaxAmbig; ambig := ambig + 1; reply := "err_c";    \* error, committed (A3)
  end either;
end macro;
```

### 4.4 Actions

Each label is one atomic step: one store round trip, or one local decision. The Go function each
step abstracts is named, so the model-code map is explicit.

| Label | Go | Step |
|---|---|---|
| `Open` | `Journal.open`, `liveAttempts`, the resume gate in `loop.go` (the `for id := range attempted` halt) | One `Load`. If the result is recorded, the drive returns it. On the tool path, a marker that is not voided and has no result halts (`HaltCrashed`), unless the process remembers that marker's own claim id: then `GateTake`/`GateWrite` retry its not-started record (`Journal.retryNotStarted`), and on success the call claims its next attempt. The Step path has no gate: `journalStep` goes to `Get` and then `claimNext`. |
| `Claim`, `ClaimRetry` | `Journal.claim` (first half) | Take an id the process remembers for this marker key (`pendingClaims.take`) and write its not-started record again (`Journal.notStarted`); on error the id is remembered again and the drive fails. A remembered id is never used to claim. |
| `ClaimInsert` | `Journal.claim` → `newClaimID`, `Journal.insert` → `Store.Insert` | Insert the marker under a fresh id (three replies). On error: `ClaimNS`. If the stored marker carries another id: `Lost`. Else the claim is won, and its not-started key is empty. |
| `ClaimNS` | `Journal.claim` → `Journal.notStarted` | Insert the not-started record under the claim's own id (three replies). On error: `pendingClaims.remember`. The drive fails. |
| `Lost` | `Journal.claimNext`, `Journal.voided` | If the stored marker is voided by its claimant's not-started record: next `g`, back to `Claim`. Else the tool path halts (`HaltContended`); the Step path goes to `JoinOrRead`. |
| `JoinOrRead` | `journalStep` loser branch: `joinFlight`, then `Journal.Get` | Join the process's in-flight call if there is one and take its outcome (if it fails, halt `HaltContended`); else read the result; else halt (`HaltCrashed`). Never lead a flight. |
| `Call` | `journalStep`'s `doFresh` closure; `loop.go`'s `recordFresh` closure (`sctx.Err()` check, `called.Store(true)`) | Either the context is cancelled (the effect is not called: `NotStarted`), or the effect is called: `fired[c]` increments and `firedAt[c][g]` is set. |
| `Record` | `Journal.doFresh` → `Journal.insert` | Insert the result (three replies). An error leaves the marker without a result: the next drive halts. |
| `NotStarted` | `recordNotStarted` → `Journal.notStarted` | Insert the not-started record (three replies). On error: `pendingClaims.remember`, as for every failed not-started write. |
| `Finish` | the caller, `Recover`/`RecoverLoop` | A drive that did not end with a result is driven again. |
| `Crash(p)` | process death | Every driver of `p` restarts at `Open` with empty locals; `pending[p]` and `flight[p]` are emptied. A TLA+ action added beside the translated `Next`, since PlusCal cannot reset another process's `pc`. |
| `Resolve` | `resolveHalt`, `checkNoLiveDriver` | If the call has no result and a live marker: under `LiveCheck = "lease"`, take the lease if no leased driver holds it; under `"minAge"`, the assumption below; under `"none"` (`WithoutLiveDriverCheck`), nothing. The
lease check covers every store that leases runs (MemStore, `store/postgres`, and `store/sqlite`
after #92). Then insert the result through the result key (first writer wins). |

`WithMinHaltAge` cannot be checked in an untimed model. Model 1 treats it as an assumption that a
driver which called the effect records its outcome (or dies) before the age has passed, encoded as
a guard: under `"minAge"`, `Resolve` is enabled only when no driver is between `Call` and the end
of `Record`. That makes explicit what the Go code relies on. Model 3 adds a discrete clock and
checks it.

### 4.5 In-process and cross-process drivers

Each driver belongs to a process (`ProcOf[d]`). Two drivers of one process share `pendingClaims`
and `flights`, as two Journals over one store value do (`storeIdentity`), and a crash of that
process kills both. Two drivers of different processes share only the store. The CI
configurations cover both placements, since the bugs differ: the pending-claim double fire needs
two drivers of one process, and the #90 live-driver override needs two processes (in one process
the flight would join them).

### 4.6 Properties

Safety invariants, checked on every reachable state:

```tla
\* 1. At most one fire per logical call.
AtMostOnce == \A c \in Calls : fired[c] <= 1

\* 2. A not-started record never coexists with a fired effect for that claim.
NotStartedExclusive ==
  \A c \in Calls, gg \in Gens :
    firedAt[c][gg] # 0 => ns[c][gg][firedAt[c][gg]] # "ns"

\* 3. A recorded result is never replaced (an action property).
ResultStable == [][\A c \in Calls : result[c] # None => result'[c] = result[c]]_vars

\* 4. Resolution never overrides a live driver's outcome: no driver that called the effect
\*    finds a resolver's result in place when it records its own.
NoLiveOverride ==
  \A d \in Drivers : pc[d] = "Record" => result[CallOf[d]].by # "resolver"
```

Invariant 2 is the invariant `Journal.claim`'s comment states ("an effect runs only under a marker
that is not, and can never become, voided"); it fails earlier and with a shorter trace than
invariant 1 when the claim rules are wrong, so it is the better regression target. Invariant 3
holds by construction of `Insert` today; it is kept because a future redaction action (A6) must
not break it for a live run.

Two further invariants state review findings as properties:

```tla
\* 5. A driver that won a claim never returns a halt for an effect nobody started
\*    (#92 first review, finding 3).
WinnerNeverHalts ==
  \A d \in Drivers : outcome[d] \in {"halt_crashed", "halt_contended"} => ~won[d]

\* 6. Sanity (must be violated): the effect is reachable at all.
EffectReachable == \A c \in Calls : fired[c] = 0
```

Invariant 6 is a vacuity check: CI expects TLC to report it violated. A model in which the effect
can never fire satisfies every safety property.

Liveness: a provably unstarted effect does not halt forever. In journal terms, the system never
stays forever in a state where every attempt of a call is voided and no result is recorded:

```tla
Progress == \A c \in Calls : <>[](result[c] # None \/ LiveMarker(c))
```

A call left with a live marker is a real halt (a process died between claim and record, and the
journal cannot say whether the effect fired). `Progress` says that is the only way to be stuck.

Fairness: weak fairness of each driver's steps (`fair process`), so a driver that can re-drive
does. No fairness on `Crash`, on ambiguous replies, on cancellations before the call or on
`Resolve`: each of these is a fault with a budget, so faults are finite, and resolution is never
assumed. TLC then checks `Progress` over every behavior in which the faults happen, in any order,
and then stop.

Bounds must not masquerade as liveness failures. The prototype showed this twice: with a claim-id
counter and a re-drive budget, TLC reported a `Progress` violation that was only the re-drive
budget running out; with canonical ids but a pool of four ids, it reported one that was only the
pool running out (both drivers blocked in `Claim`). Unbudgeted cancellations would give a third
(a driver cancelled before every call never progresses, which is correct behavior, not a bug).
The rules for the liveness configurations:

- every way to void an attempt (a failed claim insert, a cancellation before the call) has a
  budget, and `MaxGen` is above the sum of those budgets, so attempt numbers never run out;
- the claim-id pool is larger than the number of ids the state can hold at once (markers, not-started
  keys, pending entries, drivers' current ids);
- an invariant `BoundNotHit` fails if any driver is ever blocked on `g > MaxGen` or on an empty
  id pool, so a bound that is too small is reported as a configuration error, never as a liveness
  result.

### 4.7 Bounds, runtime and symmetry

Prototype measurements. All rows: one call, two drivers, attempt numbers 0 to 3, a pool of six
claim ids, one crash, one cancellation before the call; exhaustive, 8 workers.

| Configuration | Property | Distinct states | Depth | Time |
|---|---|---|---|---|
| Step path, drivers in one process, 2 ambiguous replies | safety | 16.6 M | 116 | 1 min 19 s |
| Step path, drivers in two processes, 2 ambiguous replies | safety | 16.8 M | 112 | 1 min 18 s |
| Tool path (resume gate), one process, 2 ambiguous replies | safety | 6.7 M | 86 | 32 s |
| Step path, one process, 1 ambiguous reply | `Progress` | 1.9 M | 92 | 1 min 11 s |
| Step path, one process, 2 ambiguous replies | `Progress` | 16.6 M | 116 | 15 min 8 s |
| `Bug = "NoHold"`, one process | `NotStartedExclusive` violated | | 12 | under 1 s |
| `Bug = "NoHold"`, one process, `AtMostOnce` only | `AtMostOnce` violated | | 22 | 1 s |

With three attempt numbers, four ids and unbudgeted cancellations, the first row was 2.8 M states
in 15 s: the bounds, not the rules, dominate the cost. The full model 1 adds the resolver, the Step
flight, leases and the tool path in one spec, so the CI numbers will be larger. Proposed
configurations:

| Config | Drivers and processes | Calls | Attempts | Ambiguous replies | Crashes | Where | Budget |
|---|---|---|---|---|---|---|---|
| `ci-same` | 2 in 1 process | 1 Step | 4 | 2 | 1 | every PR | 3 min |
| `ci-cross` | 2 in 2 processes | 1 tool | 4 | 2 | 1 | every PR | 3 min |
| `ci-resolve` | 2 in 2 processes, 1 resolver | 1 tool | 3 | 1 | 1 | every PR | 2 min |
| `deep-*` | 3 in 2 processes; or 2 calls (1 tool, 1 Step); or 3 ambiguous replies and 2 crashes | | 5 | | | nightly | 60 min each |
| `live` | 2 in 1 process and 2 in 2 processes | 1 | 4 | 2 | 1 | nightly | 60 min |
| `late` | as `ci-cross`, with `err_late` | 1 | 4 | 2 | 1 | nightly | 30 min |
| regression configs | section 5 | | | | | every PR | 10 s each |

The `ci-*` configurations run as parallel jobs. `deep` is split by dimension because the product of
three drivers, two calls and three ambiguous replies will not fit an hour; each `deep-*` config
raises one dimension above the `ci-*` bounds. If one still does not fit, it runs in TLC's
simulation mode (`-simulate`) with a fixed seed, and the README says so.

Two ambiguous replies is the minimum for the pending-claim double fire, and three writes with an
error (marker, not-started record, result) is the most one attempt performs, so the `deep-faults`
config covers every placement of a fault on every write of one attempt.

Symmetry: drivers of the same process and path are interchangeable, and so are processes with the
same driver count. `Permutations` over each such group is sound for the safety configurations and
should cut the `ci-*` state counts by up to the group's factorial (to be measured in M1). Claim ids
can be made model values under a symmetry set too, which removes the id renamings that canonical
allocation leaves. TLC does not support symmetry with liveness checking, so the `live`
configuration runs without either. Calls are not symmetric once one is a tool and one a Step.

### 4.8 The prototype

The prototype that produced these numbers is not proposed for commit as is, and it models the
rules of `9ace7c6`, with the claim-held pin (`Hold`) that #92 has since dropped. It models one call,
with the tool path reduced to the resume gate, and omits the Step flight, result values, nested
calls and the caller; its resolver has only the `none` and `lease` checks. Its claim
section shows the intended density: one label per round trip.

```tla
Claim:
  await g <= MaxGen;
  if pending[<<ProcOf[self], g>>] # 0 then
    cid[self] := pending[<<ProcOf[self], g>>]; pending[<<ProcOf[self], g>>] := 0; reused := TRUE;
  else
    cid[self] := CHOOSE i \in Free : \A j \in Free : i <= j; reused := FALSE;
  end if;
ClaimInsert:
  Reply();
  if reply # "err_nc" /\ marker[g] = 0 then marker[g] := cid[self]; end if;
ClaimCheck:
  if reply # "ok" then goto ClaimNotStarted;
  elsif marker[g] # cid[self] then goto Lost;
  elsif reused /\ Bug # "NoHold" then goto Hold;
  else goto Call;
  end if;
Hold:
  Reply();
  if reply # "err_nc" /\ ns[<<g, cid[self]>>] = None then ns[<<g, cid[self]>>] := "held"; end if;
HoldCheck:
  if reply # "ok" then pending[<<ProcOf[self], g>>] := cid[self]; outcome := "error"; goto Finish;
  elsif ns[<<g, cid[self]>>] = "ns" then goto Lost;
  else goto Call;
  end if;
```

### 4.9 One guard for the journal format

`Journal.open` and `ensureHeader` refuse a run whose first entry is not a header in a supported
format. Model 1 keeps one boolean per run, `legacy`, set in some initial states together with a
recorded result and `fired = 1` (a run completed by v0.7.0). `Open` refuses such a run. That is
enough to reproduce the third historical double fire (section 5.3); the header's own races belong
to model 4.

## 5. Proof that the model is useful: the historical double fires

Each double fire found in review becomes a regression configuration: the model with one rule
reverted to its buggy form, behind a `Bug` constant, and a CI check that TLC reports the named
invariant violated. A regression configuration that stops failing means either the model lost the
behavior or the invariant weakened; both must be investigated. If the model cannot reproduce a
historical bug, it is missing something, and that gap is fixed before the model is trusted.

### 5.1 Double fire 1: a remembered claim runs under a voided marker

Found by #92's second review (tests `TestRememberedClaimRunsUnderAVoidedMarker_Tool` and `_Step`,
now in `agent/claim_reuse_test.go`). First fixed by pinning a reused id's not-started key with a
claim-held record, which the third review showed can halt an effect that never started for ever
(its not-started record lands on the pin); fixed for good by never claiming with a remembered id:
the id's not-started record is written again and every claim takes a fresh id. The pin is kept as
the regression configuration `regress/held-pin` (expects a `Progress` violation).

- **Buggy rule** (`Bug = "ReuseNoHold"`): a remembered id is taken back and used for the claim
  itself, and a win with it goes straight to `Call`.
- **The review's counterexample:** drive 1's marker insert commits and errors; its not-started
  insert commits and errors, so the id is remembered. Drive 2, in the same process, takes the id,
  "wins" the marker that the not-started record already voids, and fires; its result write is
  lost. Drive 3, in a new process, sees the marker voided, claims `attempt:retry:1:...`, and fires
  again. Faults: two ambiguous replies, one lost result write (a third ambiguous reply or a crash).
- **What the prototype found:** two shorter traces.
  - `NotStartedExclusive` is violated in an 11-state trace: the marker insert errors without
    committing, the not-started insert commits and errors, the id is remembered, and the next claim
    in the process reuses it, wins a fresh marker that is voided from the start, and calls the
    effect.
  - `AtMostOnce` is violated in a 21-state trace with no crash: driver 1's marker and not-started
    inserts both commit and error; driver 2 in the same process takes the remembered id and wins
    attempt 0 (the marker already carries that id) and fires; driver 1, re-driving, loses attempt
    0, finds it voided, claims attempt 1 and fires.
- **Regression configs:** `regress/reuse-nohold.cfg` (expects `NotStartedExclusive`) and
  `regress/reuse-nohold-fire.cfg` (invariants reduced to `AtMostOnce`, expects `AtMostOnce`).

### 5.2 Double fire 2: a Step's pause guard recorded as a tool failure

Found by #92's first review (`TestStepPauseGuard_InsideAToolIsNotRecordedAsAToolFailure`, in
`agent/step_pause_tool_test.go`). A retry-safe tool `book` runs a non-retry-safe Step
`charge-<tool-use id>` whose body fires and then pauses. The guard turns the pause into an error;
the tool call recorded that error as a failed result; the model, told the call failed, called
`book` again under a new tool-use id, and the new Step name fired the charge again. Fixed by
`stepPauseError`: the tool call records nothing for it, so the next attempt halts on the Step's
marker.

This bug is outside the claim rules proper: every claim behaved correctly, and the double fire
comes from the caller issuing a new logical call. Model 1 covers it with two additions, which it
needs anyway for resolution (section 4.4):

- **Nested calls:** a call may be a retry-safe tool whose body runs a Step (a child call with its
  own claim). The body can end with `ok`, with the effect's own failure (by the tool contract, the
  effect did not take place), or with an engine error raised after the child fired (the guard).
- **The caller:** a call belongs to an intent. When a call's recorded result is an error, the
  caller may issue a new call for the same intent. This is what the LLM does.
- **Invariant:** `AtMostOncePerIntent`: across all calls of one intent, at most one effect fired.
  A failure by the effect's own report does not count as fired.
- **Buggy rule** (`Bug = "PauseAsFailure"`): an engine error after the child fired is recorded as
  the parent's failed result.
- **Expected counterexample:** call `c1` claims and fires its Step, the guard error is recorded as
  `c1`'s failure, the caller issues `c2` for the same intent, `c2`'s Step has a new name, claims
  attempt 0 and fires. No fault is needed.
- **Regression config:** `regress/pause-as-failure.cfg` (expects `AtMostOncePerIntent`).

The same invariant covers the #90 finding F2 (next section), where the failure the caller reads
is a resolver's verdict.

### 5.3 Double fire 3: a v0.7.0 journal read as an empty run

Found by #92's first review (fix 2; the reviewer's writer is `TestReview92_WriteV070Journal`). A
v0.7.0 SQLite file keeps its journal in the table `steps`; P6a's store reads `bide_steps`, so a
finished v0.7.0 run looked empty, and a re-drive ran its side effect again. Fixed by
`sqlite.Open` refusing a file whose v0.7.0 `steps` table holds rows, and by the header rules.

- **Buggy rule** (`Bug = "LegacyEmpty"`): `Open` reads a `legacy` run as empty instead of refusing
  it.
- **Expected counterexample:** initial state with `legacy`, a recorded result and `fired = 1`; one
  driver opens it, sees no marker, claims attempt 0, fires. Two steps, no fault.
- **Regression config:** `regress/legacy-empty.cfg` (expects `AtMostOnce`). Not built yet: model 1
  as implemented has no journal-format guard (section 4.9), so this configuration comes with it.

This one is nearly trivial in the model, and that is the point of including it: it pins that the
model's `Open` refuses an unreadable run rather than treating it as new. Model 4 checks the header
rules themselves.

### 5.4 Further regression configurations

Two review findings that were not double fires are covered too, since the model states them as
properties:

- **The claim winner joined the loser's halt** (#92 first review, finding 3;
  `TestStep_ClaimWinnerRunsTheStepWhenALoserReadsFirst`). Buggy rule `Bug = "LoserLeads"`: the
  Step loser starts the shared flight instead of only joining one. Expected: `WinnerNeverHalts`
  violated (the winner takes the loser's halt as its own outcome and records not-started).
- **A live driver's call resolved as crashed** (#90 finding F2;
  `TestR90_LiveToolClaimIsClassifiedCrashed`). A second driver in another process opens the run,
  and the resume gate halts on the first driver's marker with `HaltCrashed`: the journal cannot tell
  a crashed claimant from a live one. Before the fix, only a `HaltContended` halt needed a minimum
  age, so `ResolveHaltRef(halt.Ref())` resolved the call while the first driver was inside the
  effect, and the first driver's real result then lost the insert. Buggy rule
  `LiveCheck = "cause"`: `Resolve` is enabled at once for a halt the gate labelled crashed.
  Expected: `NoLiveOverride` violated, and with the caller of 5.2, `AtMostOncePerIntent` (the
  operator's "not charged" is read as a failure and the caller asks again). As built, the model's
  resolver does not read the halt's cause, so the rule is `LiveCheck = "none"`
  (`regress/resolve-no-check.cfg` and `regress/resolve-no-check-intent.cfg`).

The prototype reproduces `NoLiveOverride` in a 7-state trace with `LiveCheck = "none"` (the
resolver writes between the winner's claim and its call). It also shows that the fixed lease check
is safe only when every driver that can run the call holds the lease: with one unleased driver,
`NoLiveOverride` fails at depth 12; with both leased, the configuration passes (0.4 M states). That
matches the limitation #90 documents ("a plain Run holds no lease"). It stays as a regression config
that is expected to fail, `limits/resolve-unleased-driver.cfg`, so the limitation cannot change
silently.

## 6. Keeping the model and the code in sync

A model that drifts from the code checks a protocol nobody runs. Three mechanisms, in decreasing
order of strength.

### 6.1 Trace validation

The Go test suites emit a log of protocol events; TLC checks that the log is a behavior of the
model. The technique is the one of Cirstea, Kuppe, Loillier, Merz and Ron, "Validating Traces of
Distributed Programs Against TLA+ Specifications" (2024).

**Hook points.** Two kinds of events, recorded by two mechanisms.

Store events are recorded by a test-only `Store` wrapper, `tracestore`, placed under the Journal,
where `chaos/bide.go` already places `crashingStore`. It needs no change to production code. It
logs every `Insert` with the key (parsed back into its kind, call and attempt), the claim id read
from the stored bytes, `inserted`, and the reply. Fault-injecting stores in the suites
(`faultStore` in `claim_reuse_test.go`, `commitThenFail`, `crashingStore`) add the ground truth of
an error reply (`committed` or `not_committed`), which the code itself never learns.

Journal and engine events are recorded by hook calls at these sites:

| Event | Function (P6a head) | Where |
|---|---|---|
| `claim_start` (id) | `Journal.claim` | after `newClaimID` |
| `pending_retry` (id, result) | `Journal.claim`, `Journal.retryNotStarted` | after `pendingClaims.take` and the not-started write |
| `claim_won`, `claim_lost` | `Journal.claim` | at each return |
| `pending_remember` | `claimMemo.remember` (called from `Journal.claim`) | entry |
| `voided_check` (result) | `Journal.claimNext`, `Journal.voided` | after the check |
| `gate_halt` | the resume gate in `loop.go` (`Agent.runLoop`'s `for id := range attempted`) | before the `ResumeHalt` return |
| `effect_skip` (cancelled before the call) | `journalStep` (`ctx.Err()` in the `doFresh` closure), `loop.go` (`sctx.Err()` in the `recordFresh` closure), `durableStep` | the early return |
| `effect_call` | the same three closures | at `started.Store(true)` / `called.Store(true)` |
| `not_started` | `Journal.notStarted` | entry |
| `flight_lead`, `flight_join` | `shareFlight`, `joinFlight` | entry |
| `step_loser` (joined, read, halted) | `journalStep` loser branch | each return |
| `resolve` (check outcome, write result) | `resolveHalt`, `checkNoLiveDriver` (P10 head) | after the check, after the write |
| `crash` | the harness (`crashingStore.dead`, `rmCrashStore` with `dead`) | when the process is taken to die |

The hooks compile only under a build tag, so the release build carries no cost and no symbol. A
pair of files in `agent` defines a constant `traceOn` (true under `bidetrace`, false otherwise)
and the emitting function; every hook site is written `if traceOn { trace(...) }`, so in the
default build the compiler drops the site, including the construction of its event. The benchmark
job runs without the tag, so the round-trip budget and the benchmarks are unaffected by
construction.

<!-- docsnip: skip sketch of the two build-tagged files, not an API -->
```go
//go:build bidetrace

package agent

import "github.com/bide-ai/bide/internal/prototrace"

const traceOn = true

func trace(ev prototrace.Event) { prototrace.Emit(ev) }

// At a hook site, in either build:
//	if traceOn { trace(prototrace.Event{Ev: "claim_start", Key: key, Claim: id}) }
```

**Event schema** (JSON lines, one file per test, one line per event, in emission order):

```json
{"i":7,"proc":"p1","drv":"d1","ev":"insert","key":{"kind":"marker","call":"c1","gen":0},"claim":2,"reply":"err","truth":"committed"}
{"i":8,"proc":"p1","drv":"d1","ev":"insert","key":{"kind":"not_started","call":"c1","gen":0,"claim":2},"rec":"not_started","reply":"err","truth":"committed"}
{"i":9,"proc":"p1","drv":"d1","ev":"pending_remember","call":"c1","gen":0,"claim":2}
{"i":10,"proc":"p1","drv":"d2","ev":"pending_retry","call":"c1","gen":0,"claim":2,"result":"ok"}
```

The emitter normalizes identifiers before writing: run IDs are dropped (one run per trace),
tool-use IDs and step names become `c1, c2, ...` in order of appearance, 32-hex claim ids become
small integers in order of appearance, and processes and drivers are the harness's names. Normalized
traces from different tests are often identical; CI checks each distinct trace once (by hash).

**Total order.** Within one process, events are totally ordered by a sequence number taken under
the emitter's mutex. The store is linearizable (A1), and `tracestore` logs an `Insert` while
holding a mutex around the inner call, so the logged order of store events is a valid
linearization. For the multi-process Postgres harness, each process writes its own file; the
multi-process paragraph below covers the merge.

**Producers.** Traces come from suites that already exist, run with `-tags bidetrace` and
`BIDE_TRACE_DIR` set:

- the DST crash sweep and randomized runs (`TestDST_NoDoubleFire_CrashSweep`,
  `TestDST_NoDoubleFire_Randomized`) and the Step crash tests (`agent/step_crash_test.go`);
- the chaos sweep through `crashingStore` (seven writes, one crash point each);
- the claim tests: `agent/claim_reuse_test.go`, `agent/not_started_rules_test.go`,
  `agent/step_flight_test.go`, the commit-then-fail sweep (seven writes, same and new process);
- storetest's ambiguous-claim and A1 at-most-once cases over MemStore and SQLite;
- the reference model's crash sweep (`TestRefModel_CrashSweep`) and a capped number of randomized
  scenarios (`BIDE_REFMODEL_N`);
- nightly, the multi-process HA harness (`TestHA_MultiProcessKillAndRestart`,
  `TestHA_MultiProcessStallPastTTL`).

One caveat on the producers. The DST and reference-model suites crash through a `Durable` wrapper
that intercepts `Do` (`crashStore` in `agent/dst_test.go`, `rmCrashStore` in
`agent/refmodel_test.go`). `journalOf` returns nil for such a wrapper, so those runs take the
transitional `Durable` path (`ClaimAttempt` without a Journal, `claimAttempt`, `probe`,
`durableStep`), which has no remembered claims. Their traces validate that
path, not the Journal's. See open question 10.

**How TLC checks a trace.** A trace spec, `ClaimsTrace.tla`, extends the model. It reads the
trace with `ndJsonDeserialize` (CommunityModules `Json`), taking the file path from the
environment (`IOEnv`), and adds a variable `l`, the index of the next event. Each event kind maps to
the model actions it can witness, constrained to agree with the event:

```tla
IsInsertMarker ==
  /\ Event.ev = "insert" /\ Event.key.kind = "marker"
  /\ \E d \in Drivers : d = DriverOf(Event.drv) /\ pc[d] = "ClaimInsert"
       /\ ClaimInsert(d)
       /\ (Event.reply = "ok") = (reply'[d] = "ok")
       \* Without "truth", both error replies are allowed and TLC explores both.
       /\ "truth" \in DOMAIN Event =>
            reply'[d] = IF Event.truth = "committed" THEN "err_c" ELSE "err_nc"
       /\ Event.reply = "ok" => marker'[Event.key.gen] = Event.claim
  /\ l' = l + 1

TraceNext == \/ IsInsertMarker \/ IsClaimStart \/ IsEffectCall \/ (* ... *) \/ SilentStep
TraceSpec == TraceInit /\ [][TraceNext]_<<vars, l>>
TraceAccepted == l <= Len(Trace)     \* TLC must report this violated: the whole trace was matched
```

Model steps with no event (a local decision such as `ClaimCheck`, or the `Lost` branch that
immediately precedes a logged `claim_start`) are `SilentStep`s: any model action whose label has no
hook, taken without advancing `l`. Unobserved nondeterminism, such as whether an error reply
committed when the harness did not log `truth`, is resolved by TLC's search: the trace is accepted
if some behavior of the model matches it. TLC runs breadth-first over this small product, and a
trace is accepted when `TraceAccepted` is violated with `l = Len(Trace) + 1`. If TLC finishes
without that violation, the trace is rejected, and the script reports the largest `l` reached
(kept with `TLCSet`): the first event the model cannot explain, which is where code and model
disagree.

**CI wiring and budget.** A `trace-validate` job, gated by the same `changes` filter as the Go
jobs:

1. `go test -tags bidetrace -run '<the producer list>' ./agent/... ./chaos/... ./agent/storetest/...`
   with `BIDE_TRACE_DIR` set, capped by `BIDE_REFMODEL_N` for the randomized parts.
2. Deduplicate normalized traces by hash.
3. Check each distinct trace with TLC (`scripts/tla.sh trace <file>`), several traces per JVM
   through a driver loop, since JVM start dominates a trace check.

Budget on a pull request: 8 minutes for the job, with the randomized producers sampled; nightly,
every producer at full size and the HA harness. The expected count after deduplication is in the
hundreds per run; the measured number sets the sample size in M3.

**Multi-process traces.** Each process of the HA harness writes its own trace. The merge takes
the store events in commit order (the `Seq` that `Insert` returns, A2) and every other event in its process's program order.
When that still leaves events unordered (two processes' local steps), the trace spec checks a set of
per-process sequences rather than one sequence: `l` becomes a function from process to index, and
`TraceNext` may advance any process whose next event matches. This is a known extension of the
technique and costs more search; it is scheduled last (M5).

### 6.2 Shared vocabulary

A Go test, `TestProtocolVocabulary` (in `agent`, internal), checks that the Go code and the spec
speak about the same records:

- The spec declares its record kinds in one delimited block of `Claims.tla`:
  `\* vocabulary: begin` ... `\* vocabulary: end`, for example
  `RecordKinds == {"marker", "retry_marker", "not_started", "result_tool", "result_step"}`.
- The test holds a table from each key constructor the claim code writes through
  (`toolAttemptStep`, `stepAttemptStep`, `retryAttemptStep`, `notStartedStep`, `ToolResultStep`,
  and the step name) and each record kind (`StepAttempt`, `StepNotStarted`, `StepToolResult`,
  `StepValue`) to a spec kind.
- It fails if the table and the spec block differ in either direction, if a constructor in the
  `keyConstructors` map of `agent/keys_test.go` with an `attempt:` prefix is missing from the
  table, and if the trace emitter's key parser does not round-trip each constructor's output.
- The existing `TestEngineKeys_WritesUseConstructors` already checks that every engine write uses
  a constructor, so the chain is: every write uses a constructor, every claim-code constructor is in
  the table, every table entry is in the spec, and back.
- As built (M4): the trace emitter does not exist before M3, so the key parser lives in the test
  (`parseClaimKey`, in `agent/protocol_vocabulary_test.go`) and round-trips each constructor's
  output to its kind, attempt number, base marker key and claim id; M3's emitter takes it over.
  The test runs in the Lint job on every change, since the Go tests skip a spec-only change.

### 6.3 Process

- **A CI path rule.** The claim code is marked with region comments
  (`// protocol:claims begin` / `// protocol:claims end`) in `agent/journal.go` (the claim section
  and the flight and memo types), `agent/attempt.go`, `agent/step.go` (`journalStep`,
  `durableStep`), `agent/loop.go` (the resume gate and the claim block of the tool call),
  `agent/halt.go` (`resolveHalt`, `checkNoLiveDriver`), `agent/keys.go` (the attempt constructors)
  and `agent/saga.go` (its `liveAttempts` use). A CI job fails a pull request whose diff touches a
  marked region unless it also changes `spec/tla/claims/` or its description holds a line
  `Protocol-Impact: none (<reason>)`. Region markers instead of file paths, because `loop.go` is
  large and mostly unrelated; the job also fails if a marker pair is broken.
  As built (M4): the markers also name the model actions a region implements
  (`// protocol:claims begin Claim ClaimInsert`), and cover models 1, 1b, 7, 8, 9 and 10, each by
  its directory name. `internal/tools/modelsync` runs in the Lint job, on pull requests and in the
  merge queue. Besides the path rule it checks that every marked action is defined in the spec and
  listed in the README's map, and that every mapped action is marked (or listed as having no Go
  code), so a rename on any side fails. The override is `Protocol-Impact: none (<reason>)` in the
  description or a commit message, or `Protocol-Impact: <model>[,<model>] none (<reason>)` for
  some models, and each one used is printed as a warning. See
  [spec/tla/README.md](../../spec/tla/README.md#keeping-the-code-and-the-models-in-step).
  The review checklist entry below is in the pull request template
  (`.github/pull_request_template.md`); its trace-validation item waits for M3.
- **The adversarial review gate.** Changes to claims already get an adversarial review (see the
  roadmap). The review checklist adds: the model-code map (section 4.4, kept in
  `spec/tla/README.md`) is still true for every function the diff touches; the reviewer's
  scenario, when it is a protocol bug, is first written as a model configuration and checked; the
  trace validation job passed on the branch.
- **Every counterexample becomes a deterministic Go test.** When TLC finds a violation in the real
  rules (not a regression config), the fix does not merge until a Go test reproduces the trace.
  The tools exist in the suites already: fault stores that commit and then error or fail without
  committing per key prefix (`faultStore`), insert and read hooks that block at a chosen call
  (`hookStore` in the #92 review tests), and separate store values for separate processes. M2
  adds a small helper package, `agent/internal/interleave`, that turns a TLC trace (read from
  TLC's `-tool` output, whose messages carry codes a program can parse; `-dumpTrace json` is
  not in v1.7.4) into a script for these tools: per-insert replies, crash points, and barriers
  at the hook points of section 6.1, so the two drivers of a counterexample step in the model's
  order. The helper writes a test skeleton; a person finishes it. The test is then mutation-checked
  like every bug test.

## 7. Tooling

- **TLC:** `tla2tools.jar` from the TLA+ release v1.7.4 (the latest non-prerelease; v1.8.0 is
  marked prerelease and its assets are republished), SHA-256
  `936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88`.
- **CommunityModules** (for `Json` and `IOUtils`, needed by trace validation):
  `CommunityModules-deps-202609120237.jar`, SHA-256
  `3d9a282c360e90d55e9bbe99caa2987d508fef1556d652760b4af4455e283733`.
- Both are recorded in `spec/tla/tools.lock` (URL, version, SHA-256); CommunityModules is added
  when trace validation needs it. `spec/tla/check.sh` downloads them into a cache directory and
  refuses a jar whose checksum differs. Upgrades are a pull request
  that changes the lock file and re-runs every configuration.
- **Java in CI:** `actions/setup-java` with Temurin 21, and `actions/cache` for the jars keyed by
  `tools.lock`'s hash. TLC runs with `-XX:+UseParallelGC` and `-workers auto`.
- **PlusCal translation check:** the translated TLA+ is committed between the
  `BEGIN TRANSLATION` and `END TRANSLATION` markers. CI copies each spec, runs
  `pcal.trans -nocfg` on the copy, and fails if the translation differs from the committed one
  (checksum comments included), so a PlusCal edit without re-translation cannot merge.
- **Expected-violation check:** for each regression config, CI runs TLC, requires exit status 12
  (safety violation) or 13 (liveness), and requires the reported invariant's name to match the
  config's `EXPECT` comment. A violation of any other invariant fails the check.
- **Layout:** as built, see [spec/tla/README.md](../../spec/tla/README.md#layout): `check.sh`,
  `tools.lock`, and `claims/` with `Claims.tla`, `ClaimsMC.tla`, the configurations, `regress/` and
  `findings/`. The trace spec `ClaimsTrace.tla` comes with M3.
- **Running it locally:** `spec/tla/check.sh` (what CI runs on a pull request),
  `spec/tla/check.sh nightly`, `spec/tla/check.sh run <file.cfg>`, `spec/tla/check.sh translate`
  (re-translate in place). The script needs Java 11 or later on `PATH` or in `JAVA_HOME`, `curl`
  and a SHA-256 tool; nothing else. The repository has no Makefile, and adding one only for this is
  not worth a second entry point.
- **Apalache:** v0.62.2, pinned in `tools.lock` like TLC and run by `spec/tla/check.sh apalache`
  in its own nightly job. TLC stays the checker of record on every pull request: it is exhaustive
  within its bounds, fast at the small bounds that find most bugs, and checks liveness. Apalache
  adds what TLC cannot do: bounded symbolic checks with the configuration's placements left to the
  solver, and inductive invariants, which prove a safety property at any depth, within the attempts and
  claim ids the check fixes. Status: model 1's inductive invariant proves `AtMostOnce`, `NotStartedExclusive`, `NoLiveOverride` and `AtMostOncePerIntent` for two drivers, with and without halt resolution; bounded symbolic checks proved too slow on these models to reach a fire (see [Apalache results](../../spec/tla/README.md#apalache-results)); model 9 has a typed wrapper and no check yet. Apalache remains the candidate for model 4, where `Seq`
  values and concurrent readers make explicit-state checking expensive.

## 8. Milestones

| # | Deliverable | Effort | Exit criteria |
|---|---|---|---|
| M0 | `spec/tla/` skeleton, `tools.lock`, `scripts/tla.sh`, CI job with the translation check on a trivial spec | 0.5 day | CI fails on a stale translation and on a jar with a wrong checksum. |
| M1 | Model 1: `Claims.tla` with both paths, the flight, the resolver, `Crash`; configs `ci-*`, `deep-*`, `live`, `late`; the README with the model-code map | 4 to 5 days | Every `ci-*` config passes within its budget on the CI runner; `deep-*` and `live` pass nightly; `EffectReachable` is reported violated and `BoundNotHit` holds in every config; the map covers every function in section 4.4; a second person has reviewed the map against the #92 head. |
| M2 | Regression configs of section 5, the expected-violation check, `agent/internal/interleave` and one Go test generated from a model trace | 2 to 3 days | Each regression config fails with its named invariant; the double fire of 5.1 found by the prototype (no crash, 21-state trace) is reproduced as a Go test that fails when a remembered id is used for the claim and passes with the fresh-id rule. |
| M3 | Trace hooks (build tag `bidetrace`), `tracestore`, the emitter and normalizer, `ClaimsTrace.tla`, the `trace-validate` job for the single-process producers | 5 to 7 days | Every trace from the producers is accepted; three Go mutants of the claim code (claim with a remembered id; remember nothing on a failed not-started write; let the Step loser lead a flight) each produce a trace that TLC rejects or that violates an invariant; the default build contains no hook code (no `trace` symbol in `go tool nm`) and the benchmarks are unchanged. |
| M4 | `TestProtocolVocabulary`, region markers, the path-rule job, the review checklist entry | 1 to 2 days | The path rule blocks a test PR that edits a marked region alone, and passes with a spec change or a `Protocol-Impact` line. |
| M5 | Multi-process trace merge and validation of the HA harness (nightly) | 3 to 4 days | The two HA tests' traces are accepted nightly for a week. |

Status: M0 and M1 done (#100); M2 done except `agent/internal/interleave` and its generated test;
M3 not started; M4 done (#125); M5 not started.

M0 to M4 is about three to four weeks of one engineer. Models 2 to 5 are each estimated at one
to two weeks when their trigger fires, including their trace hooks, on the infrastructure built
here.

## 9. Risks

- **State explosion.** Two drivers and two ambiguous replies already give about 17 million
  states for one call in the prototype. Every added dimension multiplies. Mitigations: canonical
  claim ids; symmetry for the safety configs; one call in the PR configs (the rules are per call;
  two calls only exercise independence, which `deep-calls` checks nightly); TLC's simulation mode
  (`-simulate`) with larger bounds nightly, which samples behaviors instead of enumerating them;
  and the resolver in its own config, as proposed.
- **False confidence from bounds.** A bug that needs three drivers, or four ambiguous replies,
  is outside `ci-*`. The bounds are written in the README beside each property, and `deep-faults`
  covers every fault placement on every write of one attempt. The small-scope argument (most
  protocol bugs have small counterexamples) is an observation, not a guarantee; the three
  historical double fires needed at most two drivers, three faults and three attempts. An
  inductive invariant checked by Apalache removes the depth and fault bounds for safety: model 1
  has one ([Apalache results](../../spec/tla/README.md#apalache-results)); the number of drivers,
  attempts and claim ids it covers stays bounded.
- **The model encodes the code's misunderstanding.** If the model is written from the code, it can
  share the code's error. The regression configs are the check that each invariant has teeth; the
  vacuity invariant checks the effect is reachable; TLC's coverage report (`-coverage`) must show
  every label taken in the `deep-*` configs; and the model-code map is reviewed by someone other
  than the model's author.
- **Trace hook overhead and leakage.** The hooks compile only under `bidetrace`, so production
  builds have no calls and no allocations. The risk that remains is a hook that changes behavior
  under the tag (taking a lock that serializes what would race). The emitter's mutex does
  serialize events; this can hide races in the traced run, but it cannot create behaviors the code
  does not have, and the untagged suites still run with `-race`.
- **Trace validation false rejections.** When the model abstracts two code steps into one, a trace
  can show an order the model does not allow. That is a model-code map error and is fixed in the
  map, not by loosening the trace spec. Expect a few in M3.
- **Maintenance cost.** Every protocol change now also changes a spec. That cost is the purpose;
  the path rule keeps it visible, and the `Protocol-Impact` escape keeps unrelated edits cheap.
- **Tool churn.** The pinned TLC is from 2024. If trace validation needs features only in the 1.8.0
  prerelease (open question 6), a nightly build must be pinned by checksum and mirrored, since the
  upstream prerelease asset is replaced.

## 10. Open questions

1. **Land model 1 inside #92, or as a PR stacked on it?** Recommendation: a separate PR stacked on
   #92, containing M0 to M2, merged before #92 is queued; #92's description links it, and #92
   does not merge until the model passes with P6a's final rules. #92 is already about 2,200 lines,
   and the model's review is a different review.
2. **Strict or weak A3?** Can an `Insert` that returned an error commit afterwards (a client
   timeout while Postgres is still committing)? Recommendation: check model 1 under both. If an
   invariant fails only under the weak reading, either the stores must enforce strict A3 (for
   Postgres, cancel and confirm the transaction's fate before returning) or the claim rules must
   change; decide before #92 merges. First reasoning suggests the claim-held pin is safe under the
   weak reading, since the pin and a late not-started record race on one key and the first
   writer wins either way, but the model should say so. Model 1's answer, for #92's final rules
   (no pin): every invariant holds under both readings at the checked bounds (`late-*` configs).
3. **How to treat the minimum halt age.** Recommendation: model 1 keeps it as the explicit
   assumption of section 4.4; model 3 adds a clock and checks it, together with lease expiry.
4. **Does pending-claim reuse help tool calls?** On the tool path, the resume gate halts on any
   live marker before a claim is attempted, so a remembered id for a marker that did commit is
   never reused through a resume; reuse is reachable there only between two drivers of one process
   that both passed the gate. Recommendation: add a reachability check to M1 ("a reused id wins a
   marker it committed earlier, on the tool path") and, if it is unreachable through a resume,
   document that B11 reuse benefits Steps only, rather than change code. Superseded: #92's final
   rules never reuse a remembered id, and the resume gate retries its not-started record instead
   (checked by `regress/gate-no-retry`).
5. **Where the hooks live.** Recommendation: store events through the `tracestore` wrapper (no
   production change), engine events through build-tagged hooks; no runtime hook variable, so a
   release binary cannot be made to emit traces.
6. **TLC version for trace validation.** Recommendation: start M3 on v1.7.4 with CommunityModules;
   the technique needs only `Json`, `IOUtils` and `TLCGet`. v1.7.4 has no `-dumpTrace json`, so
   the counterexample-to-test helper parses `-tool` output instead. Move to a pinned 1.8.0 build
   only if a concrete feature is missing, and record which.
7. **Liveness on every PR?** Recommendation: no. Liveness cannot use symmetry, and the prototype's
   liveness check at the `ci-same` bounds took 15 minutes against 1 minute 19 seconds for safety
   over the same 16.6 million states; run `live` nightly and on PRs that change `Claims.tla`
   itself. As built: liveness runs on every pull request at one error reply, where it takes
   seconds, and at two nightly.
8. **PlusCal or plain TLA+?** The roadmap says PlusCal. It fits the drivers, which are sequential
   programs; crash, delayed commit and the trace spec are written in TLA+ beside the translation.
   Recommendation: keep PlusCal for the driver and resolver processes and accept the split.
9. **Who reviews models?** Recommendation: the model-code map is reviewed by a second person on
   every change, as the adversarial review already requires for the code; a model change without
   that review does not merge.
10. **Which claim implementation do the DST and reference-model traces validate?** Their crash
    wrappers intercept `Do`, so they drive the transitional `Durable` path, not the Journal's
    (section 6.1). Recommendation: move both suites' crash injection to the storage port, as #92
    did for chaos (`crashingStore` under a Journal), so they exercise the rules the model states;
    keep one `Durable`-path suite until P15 removes that path, and validate its traces against a
    `DurablePath` variant of the model (no remembered claims) rather than leave it unchecked.
