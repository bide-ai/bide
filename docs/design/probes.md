# Probes: deciding unknown outcomes automatically (design note)

Status: design, not implemented. Nothing in this note ships yet; the API names are a sketch. It
states what a probe is, the exact conditions under which acting on a probe's verdict keeps the
at-most-once guarantee, how verdicts are journaled and audited, how probing interacts with claims,
leases, recovery and sagas, and how the TLA+ claim model is extended to check it.

## The problem

A tool call that is not retry-safe writes an attempt marker before it fires. If the process dies
between the marker and the journaled result, a resume cannot tell whether the effect took place, so
it halts with `*OutcomeUnknown` ([the guarantee](../GUARANTEE.md#the-three-cases-when-a-crash-hits),
case 3). The halt stays until a person or a reconciler verifies the outcome out of band and calls
`ResolveHalt`. That keeps the effect at most once, and it costs liveness: every crash in that window
needs an outside actor.

Many providers can answer "did this call's effect happen?" themselves: a payment looked up by its
idempotency key, a message by its client-supplied id, a row by its call key. A **probe** lets a tool
declare that question. On resume, before halting, bide asks it:

- **Happened(outcome)**: the effect took place; bide records the outcome as the call's result and
  continues.
- **NotHappened**: the effect did not take place and cannot take place later; bide voids the attempt
  and runs the call again under the next attempt.
- **Unknown**: bide halts as today.

The hard part is NotHappened. "It has not happened yet" is not "it will not happen": the original
attempt's request may still be in flight. Most of this note is about when NotHappened is safe to act
on.

## 1. API sketch

### Declaring a probe

A probe is code, and `ToolSpec` is plain data read once at registration, so the probe itself is an
interface the tool implements, as `Compensator` is, and its contract is data on the spec:

<!-- docsnip: skip proposed API sketch, not implemented -->
```go
// Prober is implemented by a tool that is not retry-safe and can tell, after a crash, whether a
// call's effect took place. bide calls Probe on resume for a call whose attempt marker has no
// result, before it halts.
type Prober interface {
    Probe(ctx context.Context, call ProbeCall) (ProbeVerdict, error)
}

// ProbeSpec is a tool's probe contract (ToolSpec.Probe). It is data, journaled on every attempt
// marker the tool writes, so a resume probes an attempt only under the contract it fired under.
type ProbeSpec struct {
    // Contract names what the probe relies on: the keys the call sends and the lookups the probe
    // makes ("payments-charge/v2"). A marker written under another contract (or none) is not probed.
    Contract string
    // Fenced declares that the call's requests carry a fence (see Soundness), so the probe may
    // return NotHappened. Without it a NotHappened verdict is treated as Unknown.
    Fenced bool
    // MinAge: do not probe an attempt younger than this (the provider's read lag). Probing earlier
    // is safe but returns Unknown more often.
    MinAge time.Duration
    // Timeout bounds one probe. A probe that times out, errors or panics is Unknown.
    Timeout time.Duration
}
```

For `agent.Func` tools, an option builds both:

<!-- docsnip: skip proposed API sketch, not implemented -->
```go
charge, err := agent.Func("charge", "Charge a card", chargeFn,
    agent.WithProbe(agent.ProbeSpec{Contract: "payments-charge/v2", MinAge: 30 * time.Second},
        func(ctx context.Context, c agent.ProbeCall, in ChargeIn) (agent.ProbeVerdict, error) { ... }))
```

`WithProbe` is generic over the tool's input type and is refused with `ErrConfig` when it does not
match the tool's, or when the tool is `ReadOnly` or `Idempotent` (a retry-safe call writes no
marker, so it never halts and has nothing to probe).

### What the probe receives

<!-- docsnip: skip proposed API sketch, not implemented -->
```go
type ProbeCall struct {
    RunID, ToolUseID, ToolName string
    Args        json.RawMessage // the arguments the call ran with (the accepted ones, when journaled)
    Attempt     int             // the live attempt being probed (0 is the first)
    AttemptedAt time.Time       // the live marker's AttemptedAt
    Contract    string          // the contract stamped on that marker
}

// OnceKey returns the i-th key NextOnceKey handed the call. It is the same for every attempt of
// the call, since NextOnceKey's numbering restarts each time the call runs.
func (c ProbeCall) OnceKey(i int) string

// AttemptKey returns the key AttemptKey(ctx) handed attempt c.Attempt: unique per attempt, for
// providers whose fence voids one attempt (see Soundness, condition F-void).
func (c ProbeCall) AttemptKey() string
```

The call's stable identity is `(RunID, ToolUseID)`; `NextOnceKey` already derives its keys from it
(`SubRunID(runID, toolUseID)` plus a counter), so a probe can recompute exactly the keys the call
sent. `AttemptKey(ctx)` is a new accessor for the one case that needs a per-attempt key.

A probe should key its lookup on these identities, not on `Args`: tool middleware may rewrite
arguments, and a lookup by business fields can match a different call.

### What it returns

<!-- docsnip: skip proposed API sketch, not implemented -->
```go
type ProbeVerdict struct{ /* unexported */ }

func Happened(out Outcome) ProbeVerdict  // out as for ResolveHalt: Result, IsError, Evidence
func NotHappened(evidence any) ProbeVerdict
func Unknown(reason string) ProbeVerdict
```

`Happened` reuses `Outcome` unchanged. `IsError` marks an effect that took place as a known failure
(a declined charge is recorded at the provider: it happened, and the model reads a failure; in a
saga it rolls back). `Evidence` is what the probe read (the provider's record), journaled beside the
result exactly as `ResolveHalt` journals it today. A probe that returns an `error` is Unknown.

### How it fits ResolveHalt

A Happened verdict is recorded through the same path as `ResolveHalt` with `Outcome.Evidence`: the
prober claims the attempt after the live one, records the result under the call's result key
(first write wins), and marks it `Reconciled`. The differences are who decides (the tool's probe,
not an operator) and that the result also names the verdict record (section 3).

`ResolveHalt` is unchanged and stays the escape for Unknown. Both take the claim on the attempt after
the live one before recording, so a probe and a resolution of the same call exclude each other: the
second finds the claim taken (`*HaltInFlight`) or the result recorded (`*HaltAlreadyResolved` if it
differs).

NotHappened has no `ResolveHalt` counterpart today. `Outcome{IsError: true}` records a failure and
never re-runs; NotHappened re-runs. Section 2 shows that the two share the same hazard, so the
fence conditions below also say when an operator's "it did not happen" resolution is safe.

## 2. Soundness

### Definitions

- The **effect** of attempt *n* is the change at the provider that the call's request causes (the
  charge, the accepted message, the committed row). It takes place at one instant, its
  **landing**, or never.
- A verdict is **sound** at the instant *t* it is journaled when:
  - **Happened(out)** is sound if attempt *n*, or an earlier attempt of the call, landed before *t*,
    and `out` is that landing's outcome.
  - **NotHappened** is sound if no attempt of the call up to *n* landed before *t*, and none of them
    can land after *t*.
  - **Unknown** is always sound.

The second clause of NotHappened is the whole difficulty. A probe observes the provider; it cannot
observe a request that has not reached the provider yet. Nothing on the bide side can stop the
original attempt: [leases are not fenced](../KNOWN-LIMITATIONS.md#durability-and-recovery), a holder
stalled past its lease wakes still driving, and a driver that won its claim calls the tool with no
further check. A process that died may still have a request in a socket buffer, a proxy, or the
provider's queue. Any fence must therefore be enforced **by the provider, at the instant the effect
would land**.

### The double-fire counterexample

With a probe that only looks the call up (no fence):

1. Driver A claims attempt 0, builds the charge request, and stalls (a GC pause, a suspended VM)
   before or while sending it. Its lease lapses.
2. Driver B takes the run over. The resume gate finds attempt 0's marker with no result and probes:
   the provider has no charge for the call. NotHappened.
3. B voids attempt 0, claims attempt 1, and charges. The provider charges (first effect).
4. A wakes and its request reaches the provider, which charges again (second effect).

The same trace arises without a stall when the probe's read is not linearizable with the effect:
A's charge lands at step 1, and B's lookup at step 2 reads a lagging replica or search index that
does not show it yet.

Waiting for A's lease to lapse does not help (A is past its lease in the trace above). Waiting for a
client-side timeout does not help either: a timeout stops the client waiting, not the request.

The hazard is not new. `ResolveHalt(..., Outcome{IsError: true})` after an out-of-band lookup that
reads "absent" has the same trace with step 3 replaced by "the model, reading a failure, calls the
tool again under a new call id". `WithMinHaltAge` is today's mitigation, and it is only as good as
the bound it assumes on how long a request can stay in flight.

### The soundness conditions

**Happened** is sound when:

- **H1 (positive observation).** The probe returns Happened only after reading a provider record
  that exists only if the effect landed, keyed by the call's identity (a `OnceKey` or `AttemptKey`
  the call sent), not by business fields another call could share.
- **H2 (faithful outcome).** `out` is the outcome of that landing, or one the model and any
  `Compensator` can use in its place (a compensator needs the charge id, so the probe must return
  it).

H1 does not need a linearizable read: a lagging read can only miss a landing, which yields Unknown
or (for an unfenced probe) is downgraded to Unknown. A Happened verdict can never cause a second
effect, because it never re-runs anything. Probes that only ever return Happened or Unknown keep
at-most-once under H1 alone.

**NotHappened** is sound when H1's keying holds, the attempt marker records `Fenced` (the request
was sent with the fence below), and one of these holds:

- **F-dedup (provider deduplicates by the call's key).** Every request of the call carries the same
  key derived from the call identity (`OnceKey`), and the provider applies at most one request per
  key for as long as any request of the call can arrive. Then no NotHappened is needed at all: the
  probe re-sends the request with the same key (replay) and returns Happened with the provider's
  answer, which is either the original landing's outcome or a landing that happens now, once.
  This is sound only inside the provider's deduplication window: an attempt older than the window
  (less a margin for clock skew) must return Unknown.
- **F-void (linearizable observe-and-void).** Each attempt's request carries a per-attempt key
  (`AttemptKey`), and the provider offers one atomic operation that, for that key, either reports
  the landing or records the key as void so that a later request with it is rejected. The probe
  returns NotHappened only after that operation recorded the void (or found it already recorded). A
  read followed by a separate void is not enough: the request can land between them.
- **F-expiry (provider-enforced deadline).** Each request carries a deadline *D* that the provider
  enforces when it would apply the effect (not merely when it receives the request: a request
  received before *D* and applied after it must be rejected), and the probe returns NotHappened only
  after reading, linearizably, that the effect is absent at a time later than *D* plus the bound on
  the clock skew between the prober and the provider.

And, for F-void and F-expiry:

- **R (a rejected attempt records no result).** A call whose request the fence rejected returns an
  error wrapping `ErrToolNotCalled` (proposed: `ErrAttemptVoided`, which wraps it), never a result
  and never a plain error. The journal has one result key per call, first write wins; a stalled
  driver whose attempt was voided must not record "failed" for a call whose next attempt succeeds.
  bide can check the verdict record before writing a result, but that check and the write are not
  atomic, so R is a contract on the tool, not something bide can enforce (see open question 3).
- **O (order).** The fence holds before the verdict is journaled: the probe returns NotHappened only
  after the void is recorded or the deadline has passed. A verdict write that errors and commits
  later is then still true when it lands.

Two properties follow from these conditions and are what the concurrency design relies on:

- **Stability.** A sound Happened stays true (an effect cannot un-land), and a sound NotHappened
  stays true (the fence never lifts). So two probes of one attempt never return Happened and
  NotHappened both. A recorded verdict that disagrees with a later probe is evidence that the probe
  is unsound, and is reported as such (section 3).
- **Idempotence.** Probing again is harmless: a void already recorded reads as NotHappened, and a
  replay under F-dedup returns the same answer.

**A probe contract applies only to attempts that fired under it.** A probe that looks up a key the
call sent is wrong for an attempt made before the tool sent that key: it finds nothing and would
answer NotHappened for an attempt that may have landed. So the attempt marker records the tool's
`ProbeSpec.Contract` and `Fenced` at the time it fired, as it already records "not retry-safe" by
existing, and a resume probes a marker only when the tool's current contract equals the marker's
(and treats NotHappened as Unknown unless the marker says `Fenced`). Every marker written before
probes existed carries no contract, so it halts as today.

### Which guarantee holds when

| The call's tool | Verdict acted on | Guarantee for the call |
| --- | --- | --- |
| No probe, or marker without a contract | none (halt) | at most once; halts until `ResolveHalt` |
| Probe satisfying H1 and H2, not fenced | Happened, or halt | at most once; some halts clear automatically |
| Probe satisfying H1, H2 and one of F-dedup, F-void, F-expiry, with R and O | any | exactly once per call, as defined below |
| Probe declared fenced that violates a condition | any | **none**: double effect possible, as for a mislabelled `Idempotent` tool |

**Exactly once, precisely.** For a call whose probe satisfies the conditions above: the effect lands
at most once (safety, which holds whatever happens), and if the run is driven again until a probe
returns a decisive verdict and the re-run attempts eventually stop crashing, the call ends with its
effect landed exactly once and that landing's outcome recorded, or with a recorded known failure
from a landing that did not change state (`Happened` with `IsError`). It does not cover:

- the model calling the tool again under a new tool-use id, which is a new call (the existing
  `NextOnceKey` caveat; a business key the provider dedupes on covers it);
- a probe that keeps returning Unknown, which leaves the run halted (a Happened-only probe whose
  effect never landed is in this case: its attempt halts until resolved);
- the provider's own durability, and keys or voids that the provider forgets (F-dedup's window).

Like `Safety`, a probe is a declaration bide trusts. bide cannot check that a probe is sound; a
probe that answers NotHappened without a fence opts the tool out of at-most-once, exactly as
labelling a card charge `Idempotent` does today.

## 3. Journaling and audit

### Records

- **Attempt marker** (existing, `StepAttempt`): gains the probe contract it fired under
  (`Contract`, `Fenced`), empty for a tool with no probe. These fields sit behind a pointer, as
  `ModelTurn` does, to keep `Record` small (`TestRecord_Size`).
- **Verdict record** (new, kind `StepProbe`), key `attempt:probe:<marker key>`, one per attempt,
  first write wins. It holds the verdict (`happened` or `not_happened`), the probe's evidence, the
  contract, the probe's time, and who decided: the tool name, the contract, and the driver's lease
  holder id when it holds one. Unknown verdicts are not journaled (see below).
- **Result record** (existing, `StepToolResult`) for a Happened verdict: `Reconciled` and `Evidence`
  as `ResolveHalt` writes them, plus the name of the verdict record, so a reader can tell a
  probe's outcome from an operator's resolution and from a clean result.

A NotHappened verdict record voids the attempt it names: `liveAttempts`, `liveAttempt` and
`claimNextAttempt` treat it as they treat a not-started record, so the next drive claims the next
attempt through the ordinary claim path. This changes a rule of the claim protocol ("only a
not-started record voids an attempt", and only the claim's own driver writes it); the change is
sound exactly when the verdict is, which is what the formal plan checks.

### Errors and timeouts

A probe that returns an error, exceeds `ProbeSpec.Timeout`, panics, is cancelled (the drive's
lease is lost, the run is cancelled), or answers NotHappened for a marker not stamped `Fenced` is
Unknown, and the run halts as today. The halt reports the attempt to probe: `OutcomeUnknown` gains a
`Probe` field with the verdict's reason and time, so an operator sees "probed at 12:04, unknown:
timeout" rather than a bare halt. Unknown is reported through the drive's events, not journaled:
journaling it would add records on every recovery pass of a halted run, and it decides nothing.

A probe whose verdict disagrees with the recorded verdict for the same attempt (one driver recorded
Happened and another's probe answers NotHappened) halts the run with an error naming both: a sound
probe cannot produce that pair.

### Audit

`audit.Evidence` includes verdict records as material actions, with inclusion proofs, so an evidence
package shows, for a call decided by a probe, the attempt marker, the verdict and its evidence, and
the result. `bide-audit verify-evidence` reports such a call in plain words, for example:

```
tool call "toolu_01": outcome from probe (payments-charge/v2, happened, attempt 0)
tool call "toolu_02": attempt 0 voided by probe (payments-charge/v2, not happened); ran as attempt 1
```

bide-audit does not show `Reconciled` results today; this would add a line for them too, so an
operator's resolution and a probe's verdict are both visible.

## 4. Concurrency

### Who probes, and who acts

Acting on a verdict takes a claim:

- **Happened:** the prober claims the attempt after the live one (as `ResolveHalt` does) and records
  the verdict and the result. A driver that loses the claim reads the result.
- **NotHappened:** the prober records the verdict (which voids attempt *n*), then claims attempt
  *n+1* through `claimNextAttempt`, and only the winner of that claim runs the call. A loser halts
  `HaltContended`, as any claim loser does today.

So only a claim winner records a result or fires the effect, and the claim protocol's exclusivity is
unchanged.

The probe itself is not claimed: by stability and idempotence, two drivers probing one attempt get
the same verdict, and both verdict records name one key, so the first stands and the second is
identical. Claiming before probing, as `ResolveHalt` claims before recording, would also work, but
it costs an attempt number and two records for each Unknown probe (the prober must void its own
claim), and the claim protocol's rule that a re-attempt follows only a voided attempt would need an
exception for those claims. Open question 1 asks which the maintainer prefers.

Only a `HaltCrashed` resume probes. A `HaltContended` halt means another driver won the claim while
this one ran and may be running the effect now: probing it races a live call for no benefit, so it
halts as today, and the next drive, after that driver has finished or died, finds a crashed marker
and probes it.

### Leases and recovery

A lease does not make a probe sound (the counterexample has the old driver past its lease); the
fence does. Leases still matter for cost and order:

- Under `Lease`, `Recover` and `RecoverLoop`, the resume gate runs in the lease holder, so normally
  one driver probes. The probe runs under the drive's context: a probe that outlives the lease is
  cancelled with the drive (`ErrLeaseLost`) and is Unknown. `ProbeSpec.Timeout` should be well under
  three quarters of the lease TTL.
- A halted run stays unfinished, so each `RecoverLoop` full pass drives it again, and with a probe
  each such drive probes again. A halt whose probe keeps answering Unknown costs one probe per pass.
  `MinAge` and a backoff measured from the last probe (reported in the halt, not journaled) bound
  that; open question 9 asks whether to cap it.
- `ResolveHalt` holds the run's lease while it resolves, so a leased prober and a resolution do not
  overlap; on a store without leases, the claim on the next attempt still excludes them.

### Sagas and compensation

A saga's rollback halts today on a call that was attempted with no result (`saga.go`, the rollback
walk). With a probe:

- **Happened(out):** record the result, then compensate it with `out` (if not `IsError`). H2 matters
  here: the compensator receives the probe's outcome in place of the call's own.
- **NotHappened:** record the verdict; the call changed nothing and is not re-run during a rollback,
  so the rollback continues without compensating it. The fence is what makes this safe: without it,
  the original request could land after the rollback finished, leaving an effect with no
  compensation.
- **Unknown:** halt as today.

In the forward direction, a NotHappened call in a saga runs again under the next attempt unless a
rollback or cancellation has been requested, in which case the claim's cancellation check stops it
(`agent.Cancel`'s rule that no effect fires under a claim won after it is unchanged).

An approval gate is per call, so a recorded approval covers the re-attempt, as it already covers a
re-attempt after a not-started record.

## 5. Formal plan

### Which model

Extend model 1 (`spec/tla/claims/Claims.tla`), not model 9: the probe changes which records void an
attempt and who may write them, which is model 1's subject, and `TestProtocolVocabulary` already
ties model 1's record kinds to the Go key constructors, so the new `attempt:probe:` key must appear
in its `RecordKinds`. The saga rollback case goes into model 9's rollback walk in a second step.

### Changes to model 1

- **Constants.** `Probe \in {"none", "observe", "dedup", "void", "expiry", "unfenced", "stale"}`.
  `"none"` leaves every existing behaviour and state space unchanged; CI compares the state counts of
  the existing configurations before and after.
- **Effect split.** With `Probe # "none"`, the driver's `Call` step becomes `Send`, which puts
  `<<c, x, id>>` into a new variable `wire`, and a provider action `Land` that removes it and applies
  it: under `"void"` it is rejected if `<<c, x>> \in pvoid`; under `"dedup"` it applies only if
  `applied[c]` is false; under `"expiry"` it is rejected if `expired[c][x]`. An applied landing
  increments the existing ghost `fired[c]` and sets `firedAt[c][x]`. The driver's reply is "ok",
  "rejected" (which, by R, goes to its not-started path), or none (a crash or timeout). A `Tick`
  action sets `expired[c][x]` for an attempt.
- **Prober.** At the resume gate (label `Open`), a driver that finds a live marker with no result
  probes, choosing its verdict from the provider's state:
  - `"observe"`: Happened if `fired[c] > 0`, else Unknown.
  - `"dedup"`: replay: land a request under the call's key (applies only if none has), then
    Happened.
  - `"void"`: atomically, Happened if attempt `x` or an earlier one landed, else add `<<c, x>>` to
    `pvoid` and NotHappened.
  - `"expiry"`: NotHappened only if `expired[c][x]` and nothing landed, else Happened or Unknown.
  - `"unfenced"`: NotHappened whenever `fired[c] = 0` at the read.
  - `"stale"`: `"void"` with a read that may return a state from before the last landing (the
    lagging replica).
  It then writes the verdict record (through the existing `Reply`, so the write may error, and under
  `LateCommit` commit later), and continues to the resolver's claim-and-record steps (Happened) or to
  the ordinary `Claim` (NotHappened).
- **Voiding.** The definition `Voided(c, x)` gains a disjunct for a NotHappened verdict record of
  `<<c, x>>`.

### Properties

- `AtMostOnce` (existing, unchanged): the headline. Expected to hold for `observe`, `dedup`,
  `void` and `expiry`, and to fail for `unfenced` and `stale`.
- `VerdictTrue` (new): a recorded Happened implies `fired[c] > 0`; a recorded NotHappened for
  `<<c, x>>` implies `firedAt[c][x] = 0`, now and in every later state.
- `ResultTrue` (new): a result recorded from a Happened verdict implies `fired[c] > 0`, and a result
  recorded by a driver implies its own attempt landed (fails when R is broken).
- `NotStartedExclusive` and `WonKeyEmpty` (existing): `WonKeyEmpty` holds only for not-started keys
  under the new voiding rule, and a won claim's attempt may now be voided by a verdict while its
  driver is in `Send`; the property is restated as "a won claim's attempt is voided only by a fenced
  verdict", and `NotStartedExclusive` gains its verdict counterpart (`VerdictTrue`).
- `NoLiveOverride` (existing): restated for probes as "a recorded probe result never contradicts the
  landing": a stalled driver's own result may lose to a Happened verdict about the same landing,
  which the existing property forbids for operator resolutions.
- `ExactlyOnce` (new, liveness): for `dedup`, `void` and `expiry`, with fairness on `Land`, `Tick`
  and the driver's re-drive and finite fault budgets, every issued call is eventually recorded with
  `fired[c] = 1`, or with a known failure. This is `Progress` without the `Excused` disjunct for a
  live marker whose knowledge a crash erased: the probe replaces that knowledge.

### Configurations

| Configuration | Probe | Expect |
| --- | --- | --- |
| `probe-observe-same.cfg` | `observe`, 2 drivers, 1 process | pass |
| `probe-dedup-cross.cfg` | `dedup`, 2 drivers, 2 processes | pass |
| `probe-void-same.cfg`, `probe-void-cross.cfg` | `void` | pass |
| `probe-expiry-cross.cfg` | `expiry` | pass |
| `probe-void-resolve.cfg` | `void`, with the resolver (`HasResolver`, lease check) | pass |
| `limits/probe-unfenced.cfg` | `unfenced` | fail `AtMostOnce` |
| `limits/probe-stale.cfg` | `stale` | fail `AtMostOnce` |
| `regress/probe-fenced-records.cfg` | `void`, `Bug = "FencedRecords"` (R broken) | fail `ResultTrue` |
| `regress/probe-unstamped.cfg` | `void`, `Bug = "NoContractStamp"` (an unstamped marker probed) | fail `AtMostOnce` |

Each passing configuration is also run for vacuity (`EffectNotReachable` must fail). The bounds
follow the existing ones: one call, two drivers, attempts 0..3, one crash, one or two error replies,
`LateCommit` on and off. `limits/probe-unfenced.cfg`'s shortest counterexample should be the trace
in section 2: claim, stall in `Send`, probe, void, claim attempt 1, land, land.

### Apalache

Extend `ClaimsInductive.tla`'s `IndInv` with the provider state (`wire`, `pvoid`, `applied`) and the
verdict records, with the conjuncts that make the new invariants inductive: a voided `<<c, x>>` has
no landing and every in-flight request for it is rejected; under `dedup`, `applied[c]` is true
exactly when `fired[c] = 1`. Prove `AtMostOnce` and `VerdictTrue` for `Probe \in {"dedup", "void"}`
at the existing scope (two drivers, two processes, one call, attempts 0..3, 6 to 8 claim ids), and
add a bounded symbolic regression (`check --length=N`) that must find the `unfenced`
counterexample, as the existing one must find F2. Shard the inductive checks by probe mode and run
the shards in parallel; run TLC on the inductive invariant first as a cheap check that it is a true
invariant.

### Tests

Before the code: a test of the counterexample, with a fake provider whose requests can be held in
flight, that double-fires with an unfenced probe and does not with a void or dedup fence; a test
that an unstamped marker is not probed; a test that a fenced attempt's rejection records no result
(R); and the crash tests run with a probe-able tool, failing the store at every write point
including the verdict record's.

## 6. Examples

### Payments API with idempotency keys (Stripe-style): F-dedup, replay

The tool sends `Idempotency-Key: NextOnceKey(ctx)`. The probe replays the same request with the same
key and returns `Happened` with the response (the provider returns the original response for a key
it has seen, errors included, or performs the charge now). That is exactly once at the provider,
inside the provider's key retention window; Stripe, for one, documents that keys may be pruned once
they are at least 24 hours old. So the probe returns `Unknown` when `AttemptedAt` is older than the
window less a margin, and a run halted that long goes to an operator.

Why not just mark the tool `Idempotent`? Inside the window the effect is the same, and that is a
reasonable choice. The probe adds two things: a call older than the window halts instead of being
charged again under a pruned key, and the verdict is journaled with its evidence, where a retry-safe
re-run leaves no trace that it was a recovery.

### Email API with client-supplied message ids: Happened only

The tool sends a client-supplied message id derived from `NextOnceKey`, and the provider lets you
look sent messages up by it, but does not reject a second send with the same id. The probe returns
`Happened` (with the provider's message record as evidence) when the lookup finds the message, and
`Unknown` otherwise; the spec is not `Fenced`. An absent result proves nothing: delivery-event and
search APIs commonly lag, and the original request may still be in flight. This probe never re-runs
a send, so at-most-once rests on H1 alone, and it clears the common case (the send went through and
only the journal write was lost) without an operator.

### Database insert keyed by the call: F-dedup or F-void

The tool inserts into a table with a unique key on `call_key = NextOnceKey(ctx)`.

- **As F-dedup:** the probe performs the insert itself with `ON CONFLICT (call_key) DO NOTHING`,
  then reads the row and returns `Happened` with it. A late insert from the stalled driver conflicts
  and changes nothing (and its tool must treat the conflict as R requires, recording no result of
  its own). This is sound under the database's own isolation, since the unique index is the single
  point where the two inserts are ordered.
- **As F-void, when the insert must not be performed by the probe** (it fires triggers, or a second
  system reads the table): the table holds `(call_key, attempt, state)` with a unique key on
  `(call_key, attempt)`; the probe runs `INSERT ... (AttemptKey, 'void') ON CONFLICT DO NOTHING` and
  then reads the row for that attempt. If its own void row is there, the original insert for that
  attempt can no longer commit, and the probe returns `NotHappened`; if the original row is there, it
  returns `Happened`. A plain `SELECT` followed by a re-run is the counterexample: the original
  transaction can commit between them.

If the table is in the same database as the journal, an outbox (the effect and its result in one
transaction) avoids the unknown window altogether; that is outside this note.

### Fire-and-forget webhook: no sound probe

A POST to a receiver that keeps no record bide can read, accepts no idempotency key, and has no
deadline it enforces. No observation can establish H1 and no fence exists, so the only sound probe
returns `Unknown`, which is the same as declaring none. The tool keeps today's behaviour: halt, and
an operator decides. If the receiver can be changed, giving it a dedup key turns it into the
first example.

## 7. Docs to update when built, and open questions

### Docs

- [GUARANTEE.md](../GUARANTEE.md): case 3 gains the probe path; the "at-most-once, not exactly-once"
  paragraph gains the precise exactly-once statement of section 2 and its conditions; a new boundary
  condition, "a probe must be sound", beside "the tool must declare its safety accurately"; "What a
  resume reads from the journal" gains the marker's probe contract and the verdict records.
- [KNOWN-LIMITATIONS.md](../KNOWN-LIMITATIONS.md): probes cover tool calls, not `Step`s or flow nodes
  (if that is the decision); an unsound probe opts out of at-most-once; dedup windows; Unknown probes
  cost one probe per recovery pass; R is a contract bide cannot enforce.
- [CONCEPTS.md](../CONCEPTS.md): Probe, the three verdicts, fence; the Reconciler entry points to
  probes as the in-process form.
- Guides: [durable-steps.md](../guides/durable-steps.md) (the `ResolveHalt` section, and sagas),
  [reliability.md](../guides/reliability.md), [debugging.md](../guides/debugging.md) (halt
  triage), [audit.md](../guides/audit.md) (verdict records in evidence), and
  [extension-points](../reference/extension-points.md) (`Prober`). The i18n copies of each, the
  README's guarantee 1, and the CHANGELOG.
- Formal: [spec/tla/README.md](../../spec/tla/README.md) (model 1's rules, properties,
  configurations, Apalache results), [formal-verification.md](../formal-verification.md) (counts and
  the Apalache summary), and [formal-models.md](formal-models.md).

### Open questions for the maintainer

1. **Probe before the claim, or after?** This note recommends probing without a claim and claiming
   to act (sound verdicts are stable and idempotent, and an Unknown probe then writes nothing). The
   alternative, claim then probe, makes the prober unique but costs an attempt number and two
   records per Unknown probe and needs an exception in the claim rules.
2. **Gate NotHappened on a declared, stamped `Fenced`** (recommended), or trust any probe's
   NotHappened? The gate means a Happened-only probe cannot opt out of at-most-once by mistake.
3. **Rule R.** A voided attempt's late result is kept out only by the tool's contract (return
   `ErrAttemptVoided`). Closing it in bide needs results keyed per attempt, a journal change. Accept
   the contract for the first version?
4. **`AttemptKey(ctx)`**: add a per-attempt key accessor for F-void fences, or leave per-attempt keys
   to the tool (it can read the attempt number from `ProbeCall` but not, today, at call time)?
5. **Scope.** Tool calls only first, with `Journal.Step` and `plan` flow nodes (which halt the same
   way) later?
6. **In-drive probing.** Probe immediately when a call fails with `ErrToolOutcomeUnknown` (a side
   effect's error after its deadline), rather than only on resume?
7. **Retry-safe saga steps** reported in `SagaAborted.UnknownOutcome`: probe them too?
8. **MCP tools** cannot declare a probe through MCP annotations. Offer a host-side wrapper that adds
   one, or leave them unprobed?
9. **Unknown backoff.** Is `MinAge` plus a backoff enough, or should a contract carry a cap after
   which the halt waits for `ResolveHalt` without probing?
10. **Model work first.** Should the model 1 extension and its counterexample configurations land
    (as the TLA+ model did for #92) before any Go code?
