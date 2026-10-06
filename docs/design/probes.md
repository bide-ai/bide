# Probes: deciding unknown outcomes automatically (design note)

Status: design, not implemented. The maintainer's decisions on the first draft are recorded in
[Decisions](#decisions), and the design is checked by a TLA+ model (model 13,
`spec/tla/probes/`) before any Go code is written. The API names are a sketch. The note states
what a probe is, the exact conditions under which acting on a probe's verdict keeps the
at-most-once guarantee, the attempt-scoped results that keep a stale result from standing, how
verdicts are journaled and audited, how probing interacts with claims, leases and recovery, and
what the model checks.

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

## Decisions

The maintainer decided the first draft's open questions as follows; the rest of the note is written
to them.

1. **Probing and acting are separate.** Any driver may probe: a pure lookup, with no claim. A
   fencing write (an atomic check-and-void, a replay under the dedup key) and acting on any verdict
   need the claim on the next attempt, and the original driver's lease must have lapsed and the
   attempt must be older than `MinAge`.
2. **NotHappened counts only when the attempt marker records `Fenced`.** Otherwise it is Unknown.
3. **bide enforces that a stale result never stands.** Results are keyed per attempt, and the
   call's outcome is the result of its latest attempt, so a result from a voided or older attempt
   can never win. This is the larger piece of work: it changes the journal format, the store
   contract and the models.
4. **`AttemptKey(ctx)` is added.**
5. **v1 covers tool calls only.** `Journal.Step` and `plan` flow nodes come later.
6. to 8. **Later:** probing in the drive (on `ErrToolOutcomeUnknown`), retry-safe saga steps, and
   MCP tools.
9. **Unknown probes are capped.** After the cap the halt stays as today; a person can grant a
   fresh probe.
10. **TLA+ first.** Model 13 (`spec/tla/probes/`) lands before the code.

## 1. API sketch

### Declaring a probe

A probe is code, and `ToolSpec` is plain data read once at registration, so the probe itself is an
interface the tool implements, as `Compensator` is, and its contract is data on the spec:

<!-- docsnip: skip proposed API sketch, not implemented -->
```go
// Prober is implemented by a tool that is not retry-safe and can tell, after a crash, whether a
// call's effect took place. bide calls Probe on resume for a call whose latest attempt has a
// marker and no result, before it halts.
type Prober interface {
    // Probe is a pure lookup: it must not change anything at the provider.
    Probe(ctx context.Context, call ProbeCall) (ProbeVerdict, error)
}

// Fencer is implemented by a prober whose provider can fence an attempt (Fenced). bide calls Fence
// only while it holds the claim on the next attempt (see Concurrency). It is the atomic
// check-and-void: it returns Happened if any attempt up to call.Attempt landed, and otherwise
// makes sure none of them can land, then returns NotHappened.
type Fencer interface {
    Fence(ctx context.Context, call ProbeCall) (ProbeVerdict, error)
}

// ProbeSpec is a tool's probe contract (ToolSpec.Probe). It is data, journaled on every attempt
// marker the tool writes, so a resume probes an attempt only under the contract it fired under.
type ProbeSpec struct {
    // Contract names what the probe relies on: the keys the call sends and the lookups the probe
    // makes ("payments-charge/v2"). A marker written under another contract (or none) is not probed.
    Contract string
    // Fenced declares that the call's requests carry a fence the provider enforces (see
    // Soundness). Only then may the tool's Fencer return NotHappened.
    Fenced bool
    // MinAge: do not act on a verdict for an attempt younger than this.
    MinAge time.Duration
    // Timeout bounds one probe or fence. One that times out, errors or panics is Unknown.
    Timeout time.Duration
    // MaxUnknown is how many Unknown probes are recorded for an attempt before the gate stops
    // probing it and halts as today, until a person grants more (Reprobe).
    MaxUnknown int
}
```

For `agent.Func` tools, an option builds them:

<!-- docsnip: skip proposed API sketch, not implemented -->
```go
charge, err := agent.Func("charge", "Charge a card", chargeFn,
    agent.WithProbe(agent.ProbeSpec{Contract: "payments-charge/v2", MinAge: 30 * time.Second, MaxUnknown: 3},
        func(ctx context.Context, c agent.ProbeCall, in ChargeIn) (agent.ProbeVerdict, error) { ... }))
```

`WithProbe` is generic over the tool's input type and is refused with `ErrConfig` when it does not
match the tool's, or when the tool is `ReadOnly` or `Idempotent` (a retry-safe call writes no
marker, so it never halts and has nothing to probe). `WithFence` adds a `Fencer` and requires
`Fenced`.

### What the probe receives

<!-- docsnip: skip proposed API sketch, not implemented -->
```go
type ProbeCall struct {
    RunID, ToolUseID, ToolName string
    Args        json.RawMessage // the arguments the call ran with (the accepted ones, when journaled)
    Attempt     int             // the latest attempt (0 is the first)
    AttemptedAt time.Time       // its marker's AttemptedAt
    Contract    string          // the contract stamped on its marker
}

// OnceKey returns the i-th key NextOnceKey handed the call. It is the same for every attempt of
// the call, since NextOnceKey's numbering restarts each time the call runs.
func (c ProbeCall) OnceKey(i int) string

// AttemptKey returns the key AttemptKey(ctx) handed attempt n of the call.
func (c ProbeCall) AttemptKey(n int) string

// AttemptKey returns a key for the attempt of the tool call running in ctx: unique per attempt,
// and the same if that attempt's call is retried by middleware. "" outside a tool call.
func AttemptKey(ctx context.Context) string
```

The call's stable identity is `(RunID, ToolUseID)`; `NextOnceKey` already derives its keys from it
(`SubRunID(runID, toolUseID)` plus a counter), so a probe can recompute exactly the keys the call
sent. `AttemptKey` adds the attempt number, for fences that act per attempt.

A probe should key its lookup on these identities, not on `Args`: tool middleware may rewrite
arguments, and a lookup by business fields can match a different call.

### What it returns

<!-- docsnip: skip proposed API sketch, not implemented -->
```go
type ProbeVerdict struct{ /* unexported */ }

func Happened(out Outcome) ProbeVerdict  // out as for ResolveHalt: Result, IsError, Evidence
func NotHappened(evidence any) ProbeVerdict // only from a Fencer
func Unknown(reason string) ProbeVerdict

// Reprobe grants a halted call MaxUnknown fresh probes (decision 9); the next drive probes again.
func Reprobe(ctx context.Context, store *Journal, ref HaltRef) error
```

`Happened` reuses `Outcome` unchanged. `IsError` marks an effect that took place as a known failure
(a declined charge is recorded at the provider: it happened, and the model reads a failure; in a
saga it rolls back). `Evidence` is what the probe read (the provider's record), journaled beside the
result exactly as `ResolveHalt` journals it today. A probe that returns an `error` is Unknown, and a
`Prober` that returns NotHappened is treated as Unknown: only a `Fencer` can establish it.

### How it fits ResolveHalt

A Happened verdict is recorded through the same path as `ResolveHalt` with `Outcome.Evidence`: the
actor claims the attempt after the latest one, then records the result under that attempt
(see [Attempt-scoped results](#attempt-scoped-results)), marked `Reconciled`. The differences are
who decides (the tool's probe, not an operator) and that the result also names the verdict record
(section 3).

`ResolveHalt` stays the escape for Unknown and after the cap. It takes the same claim, so a probe's
act and a resolution of the same call exclude each other: the second finds the claim taken
(`*HaltInFlight`) or the outcome recorded (`*HaltAlreadyResolved` if it differs).

NotHappened has no `ResolveHalt` counterpart today. `Outcome{IsError: true}` records a failure and
never re-runs; NotHappened re-runs. Section 2 shows that the two share the same hazard, so the
fence conditions below also say when an operator's "it did not happen" resolution is safe.

## 2. Soundness

### Definitions

- The **effect** of attempt *n* is the change at the provider that the call's request causes (the
  charge, the accepted message, the committed row). It takes place at one instant, its
  **landing**, or never.
- A verdict about attempt *n* is **sound** at the instant *t* it is journaled when:
  - **Happened(out)** is sound if some attempt of the call up to *n* landed before *t*, and `out`
    is that landing's outcome.
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

With NotHappened from a lookup alone (no fence), and decision 1's preconditions met:

1. Driver A claims attempt 0, builds the charge request, and stalls (a GC pause, a suspended VM)
   before or while sending it. Its lease lapses, and the attempt passes its `MinAge`.
2. Driver B takes the run over. The resume gate finds attempt 0's marker with no result and probes:
   the provider has no charge for the call.
3. B claims attempt 1, records NotHappened, and charges. The provider charges (first effect).
4. A's request reaches the provider, which charges again (second effect).

The same trace arises without a stall when the lookup is not linearizable with the effect: A's
charge lands at step 1, and B's lookup reads a lagging replica or search index that does not show
it yet.

The lapsed lease and `MinAge` are preconditions for acting (decision 1), not a fence: in the trace
both hold and the effect lands twice. Model 13's `regress/lease-as-fence` configuration is this
trace. A client-side timeout does not help either: it stops the client waiting, not the request.

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

H1 does not need a linearizable read: a lagging read can only miss a landing, which yields Unknown.
A Happened verdict can never cause a second effect, because it never re-runs anything. Probes that
only ever return Happened or Unknown keep at-most-once under H1 alone.

**NotHappened** is sound when H1's keying holds, every attempt's marker up to the probed one records
`Fenced` (each of their requests carried the fence), and one of these holds:

- **F-dedup (provider deduplicates by the call's key).** Every request of the call carries the same
  key derived from the call identity (`OnceKey`), and the provider applies at most one request per
  key for as long as any request of the call can arrive. Then no NotHappened is needed: the actor
  re-sends the request with the same key under the next attempt (a replay) and records the
  provider's answer, which is either the original landing's outcome or a landing that happens now,
  once. This is sound only inside the provider's deduplication window: an attempt older than the
  window (less a margin for clock skew) must be Unknown.
- **F-void (atomic, monotonic check-and-void).** Each attempt's request carries its `AttemptKey`,
  and the provider offers one atomic operation that, for every attempt up to *n*, either reports a
  landing or makes sure none of them can land: a watermark below which requests are rejected, or a
  tombstone per attempt key. The fence must be **monotonic**: once an attempt is voided it stays
  voided, whatever later fence operations arrive. A watermark that a later operation can set lower
  is not a fence; model 13 found the trace (`regress/void-not-monotone`): a prober stalled before its
  check-and-void applies it after a newer prober voided a later attempt, lowers the watermark, and
  re-opens that attempt, whose request then lands. A read followed by a separate void is not enough
  either: the request can land between them.
- **F-expiry (provider-enforced deadline).** Each request carries a deadline *D* that the provider
  enforces when it would apply the effect (not merely when it receives the request: a request
  received before *D* and applied after it must be rejected), and the fence answers NotHappened only
  after reading, linearizably, that the effect is absent at a time later than *D* plus the bound on
  the clock skew between the actor and the provider.

And, for every fence:

- **O (order).** The fence holds before the verdict is journaled: the `Fencer` returns NotHappened
  only after the void is recorded or the deadline has passed. A verdict write that errors and
  commits later is then still true when it lands.

The first draft's rule R ("a rejected attempt records no result", a contract on the tool) is no
longer needed: attempt-scoped results (below) make bide enforce it. A driver whose request the fence
rejected may record whatever its tool returns; it is the result of an older attempt, which no reader
takes for the call's outcome.

Two properties follow and are what the concurrency design relies on:

- **Stability.** A sound Happened stays true (an effect cannot un-land), and a sound NotHappened
  stays true (a monotonic fence never lifts). So two probes of one attempt never return Happened and
  NotHappened both. A recorded verdict that disagrees with a later one is evidence that the probe or
  fence is unsound, and is reported as such (section 3).
- **Idempotence.** Probing again is harmless, and fencing again is harmless: a void already in place
  reads as NotHappened, and a replay under F-dedup returns the same answer.

**A probe contract applies only to attempts that fired under it.** A probe that looks up a key the
call sent is wrong for an attempt made before the tool sent that key: it finds nothing, and a fence
keyed on it cannot reject that attempt's request. So the attempt marker records the tool's
`ProbeSpec.Contract` and `Fenced` at the time it fired, as it already records "not retry-safe" by
existing, and a resume probes a marker only when the tool's current contract equals the marker's,
and acts on NotHappened only if every attempt's marker records `Fenced` (decision 2). Every marker
written before probes existed carries no contract, so it halts as today. Model 13's
`regress/not-happened-unstamped` configuration shows the double effect when this check is skipped.

### Attempt-scoped results

Today a call has one result key (`tool:<id>`), first write wins. With probes, more than one driver
can hold a claim on the same call over time (the original attempt, the actor's next attempt), and a
driver whose attempt was voided can still finish its call late. With one key, its result can win:
model 13's `regress/stale-result-one-key` configuration is the trace. Driver A's request is rejected
by the void, A records "failed", and then the actor's re-run lands and its "ok" loses to A's: the
call reads "failed" although the effect landed once, and the model may call the tool again under a
new call id.

Decision 3 keys results per attempt:

- Each attempt *n* of a call has its own result key, `tool:<id>` for attempt 0 (the key today) and
  `tool:<id>:attempt:<n>` for *n* > 0, first write wins as today.
- **The call's outcome is the result of its latest attempt**: the highest-numbered attempt whose
  marker is recorded. A result recorded under an older attempt is kept in the journal (and in the
  audit trail) but never read as the call's outcome. A call whose latest attempt has a marker and
  no result is the unknown outcome the resume gate probes.
- The actor's claim on the next attempt is what makes its outcome the call's: a Happened verdict
  records the result under the actor's attempt; a NotHappened act runs the call under it.
- A `ResolveHalt` resolution, which already claims the attempt after the live one, records under
  that attempt too, so resolutions follow the same rule.

What it changes:

- **Journal format.** A new key shape and a new reading rule. Before 1.0 the dev tag is not bumped
  (see [known limitations](../KNOWN-LIMITATIONS.md#stores)); 1.0's `bide.journal.v1` includes it.
- **Store contract.** No new primitive: each key is still recorded at most once. A store that
  indexes tool results by tool-use id (for listing or a point read) must index by key instead, and
  the conformance suite (`agent/storetest`) gains cases for a call with results under several
  attempts.
- **Readers.** Replay, `Status`, the saga rollback walk, `audit.Evidence` and bide-audit read the
  latest attempt's result. Each gets a test of a stale older result.
- **Models.** Model 13 checks the rule; models 1 and 9, which state one result key per call today,
  are updated with the code.

## Which guarantee holds when

| The call's tool | Verdict acted on | Guarantee for the call |
| --- | --- | --- |
| No probe, or marker without a contract | none (halt) | at most once; halts until `ResolveHalt` |
| Probe satisfying H1 and H2, not fenced | Happened, or halt | at most once; some halts clear automatically |
| Probe satisfying H1 and H2, with a fence meeting F-dedup, F-void or F-expiry and O, every marker stamped `Fenced` | any | exactly once per call, as defined below |
| Fence declared that violates a condition | any | **none**: double effect possible, as for a mislabelled `Idempotent` tool |

**Exactly once, precisely.** For a call whose probe and fence satisfy the conditions above: the
effect lands at most once (safety, which holds whatever happens), and if the run is driven again
until a verdict is acted on and the re-run attempts eventually stop crashing, the call ends with its
effect landed exactly once and that landing's outcome recorded, or with a recorded known failure
from a landing that did not change state (`Happened` with `IsError`). It does not cover:

- the model calling the tool again under a new tool-use id, which is a new call (the existing
  `NextOnceKey` caveat; a business key the provider dedupes on covers it);
- a probe that keeps returning Unknown, which, after `MaxUnknown`, leaves the run halted as today (a
  Happened-only probe whose effect never landed is in this case);
- the provider's own durability, and keys or voids that the provider forgets (F-dedup's window).

Like `Safety`, a probe and a fence are declarations bide trusts. bide cannot check that a fence is
sound; a fence that answers NotHappened while a request can still land opts the tool out of
at-most-once, exactly as labelling a card charge `Idempotent` does today.

## 3. Journaling and audit

### Records

- **Attempt marker** (existing, `StepAttempt`): gains the probe contract it fired under
  (`Contract`, `Fenced`), empty for a tool with no probe. These fields sit behind a pointer, as
  `ModelTurn` does, to keep `Record` small (`TestRecord_Size`).
- **Verdict record** (new, kind `StepProbe`), key `attempt:probe:<marker key>`, one per probed
  attempt, first write wins, written only by the actor holding the next attempt's claim. It holds
  the verdict (`happened` or `not_happened`), the evidence, the contract, the time, and who
  decided: the tool name, the contract, whether a `Prober` or a `Fencer` answered, and the actor's
  lease holder id.
- **Unknown records** (new), key `attempt:probe-unknown:<marker key>:<k>`, one per Unknown probe up
  to the cap, each with its reason and time; and **grant records** (`attempt:probe-grant:<marker
  key>:<g>`) that `Reprobe` writes, each raising the cap by `MaxUnknown`. A driver writes the first
  free `k`; two drivers racing write two records, which the cap counts, so the cap holds within one
  record. They make the cap survive restarts and show every probe in the audit trail.
- **Result record** (existing, `StepToolResult`), now under its attempt's key: for a verdict,
  `Reconciled` and `Evidence` as `ResolveHalt` writes them, plus the name of the verdict record, so
  a reader can tell a probe's outcome from an operator's resolution and from a clean result.

### Errors, timeouts and the cap

A probe or fence that returns an error, exceeds `ProbeSpec.Timeout`, panics, is cancelled (the
drive's lease is lost, the run is cancelled), or answers NotHappened from a `Prober` or for a marker
not stamped `Fenced`, is Unknown: an Unknown record is written and the run halts as today. Once an
attempt has `MaxUnknown` Unknown records per grant, the gate does not probe it again and halts as
today; `Reprobe` grants a fresh round. The halt reports the last probe: `OutcomeUnknown` gains a
`Probe` field with its reason, time and the count, so an operator sees "probed 3 times, last at
12:04, unknown: timeout" rather than a bare halt.

A verdict that disagrees with the recorded one for the same attempt halts the run with an error
naming both: a sound probe and fence cannot produce that pair.

### Audit

`audit.Evidence` includes verdict, Unknown and grant records as material actions, with inclusion
proofs, so an evidence package shows, for a call decided by a probe, every attempt marker, the
verdict and its evidence, and the result of the latest attempt. `bide-audit verify-evidence`
reports such a call in plain words, for example:

```text
tool call "toolu_01": outcome from probe (payments-charge/v2, happened, attempt 1)
tool call "toolu_02": attempt 0 voided by fence (payments-charge/v2, not happened); ran as attempt 1
tool call "toolu_03": attempt 0 probed 3 times, unknown (timeout); halted
```

bide-audit does not show `Reconciled` results today; this would add a line for them too, so an
operator's resolution and a probe's verdict are both visible.

## 4. Concurrency

### Who probes, and who acts (decision 1)

- **Probing** is a pure lookup (`Prober.Probe`) and needs no claim. Any driver at the resume gate
  may probe; by stability and idempotence, two drivers probing one attempt see the same thing, and a
  lookup changes nothing at the provider.
- **Fencing and acting** need, together: the claim on the next attempt (the same claim
  `ResolveHalt` takes), the run's lease held by this drive (so the original driver's lease has
  lapsed, or it is this process's own from an earlier drive), and the probed attempt older than
  `MinAge`. Only then does the actor call `Fencer.Fence` or replay under the dedup key, write the
  verdict, and then:
  - **Happened:** record the result under its own attempt. A driver that loses the claim reads the
    outcome.
  - **NotHappened:** call the tool under its own attempt, which the claim already gives it.

So only a claim winner fences, records a verdict or a result, or fires the effect, and the claim
protocol's exclusivity is unchanged. The lease and `MinAge` preconditions reduce contention and give
a provider's records time to settle; they are not what makes NotHappened sound (the fence is).

An actor that crashes after its claim and before its verdict leaves the next attempt claimed with no
result. That attempt is now the latest, so the next drive probes it, and its fence covers every
attempt up to it, the original one included. A crash after a fence and before the verdict is
harmless by idempotence: the next fence reads the void already in place.

Only a `HaltCrashed` gate probes. A `HaltContended` halt means another driver won the claim while
this one ran and may be running the effect now: probing it races a live call for no benefit, so it
halts as today, and the next drive, after that driver has finished or died, probes.

### Leases and recovery

- Under `Lease`, `Recover` and `RecoverLoop`, the gate runs in the lease holder. A probe or fence
  that outlives the lease is cancelled with the drive (`ErrLeaseLost`) and is Unknown.
  `ProbeSpec.Timeout` should be well under three quarters of the lease TTL.
- A plain `Run` holds no lease, so it can probe but cannot act (decision 1): it halts after the
  probe, and a leased drive acts.
- A halted run stays unfinished, so each `RecoverLoop` full pass drives it again, and each such
  drive probes again until the cap; after it, the drive halts at the gate without probing. The cost
  of a halted, probe-able call is at most `MaxUnknown` probes per grant.
- `ResolveHalt` holds the run's lease while it resolves, so a leased actor and a resolution do not
  overlap; on a store without leases, the claim on the next attempt still excludes them.

### Sagas and compensation

Retry-safe saga steps are later (decision 7). For a saga step that is not retry-safe, the rollback
walk halts today on a call attempted with no result. With a probe:

- **Happened(out):** record the result, then compensate it with `out` (if not `IsError`). H2 matters
  here: the compensator receives the probe's outcome in place of the call's own.
- **NotHappened:** record the verdict; the call changed nothing and is not re-run during a rollback,
  so the rollback continues without compensating it. The fence is what makes this safe: without it,
  the original request could land after the rollback finished, leaving an effect with no
  compensation.
- **Unknown:** halt as today.

In the forward direction, a NotHappened call in a saga runs again under the actor's attempt unless a
rollback or cancellation has been requested, in which case the claim's cancellation check stops it
(`agent.Cancel`'s rule that no effect fires under a claim won after it is unchanged).

An approval gate is per call, so a recorded approval covers the re-attempt, as it already covers a
re-attempt after a not-started record.

## 5. Formal model

Decision 10: the model comes first. Model 13 (`spec/tla/probes/Probes.tla`, its own pull request)
is a design model with no Go code yet, in plain TLA+, so model 1's cost and Apalache proof are
unchanged. It restates the claim rules it needs and adds a provider whose requests stay in flight
until it applies them, leases that lapse while their holder keeps running, the pure lookup any
driver may make, acting under the next attempt's claim with the lease and `MinAge`, the `Fenced`
stamp, attempt-scoped results, and the Unknown cap with a person's grant. `spec/tla/README.md`
("Model 13") has the details.

Its invariants: `AtMostOnce` (the effect lands at most once), `OutcomeTrue` (the call's outcome
reads "ok" only if the effect landed and "failed" only if it never did) and `VerdictTrue` (a
recorded verdict is true, in every later state).

| Configuration | What | Result |
| --- | --- | --- |
| `probe-off`, `probe-observe`, `probe-void`, `probe-dedup`, `probe-void-unstamped` (ci); `deep-probe-void`, `deep-probe-dedup` (nightly) | no probe; Happened only; F-void; F-dedup; an unstamped attempt (halts) | all invariants hold; the effect is reachable in each |
| `probe-void-reach`, `probe-observe-reach` (ci) | reachability | a void and re-run, and a recorded Happened, are reached |
| `regress/lease-as-fence` | NotHappened from a lookup, with the lapsed lease and `MinAge` as the only fence | `AtMostOnce` fails (section 2's trace) |
| `regress/stale-result-one-key` | F-void with one result key per call | `OutcomeTrue` fails |
| `regress/not-happened-unstamped` | NotHappened acted on for an attempt not stamped `Fenced` | `AtMostOnce` fails |
| `regress/void-not-monotone` | a void that sets the watermark instead of raising it | `VerdictTrue` fails (found by the model) |

Bounds: one call, two drivers, attempts 0..2 (0..3 nightly), one crash (two nightly). Not modelled
yet: F-expiry, the saga rollback walk with a probe, and liveness (`ExactlyOnce`: every call is
eventually recorded with one landing, under fair landings and re-drives). When the code lands,
models 1 and 9 take the attempt-scoped result rule and the verdict record as a voiding record,
`TestProtocolVocabulary` gains the new keys, the code is marked against model 13, and model 1's
Apalache inductive invariant is extended for `void` and `dedup`.

### Tests

Before the code: a test of the counterexample, with a fake provider whose requests can be held in
flight, that double-fires with NotHappened from a lookup and does not with a monotonic void or a
dedup key; a test that a non-monotonic void re-opens an attempt; a test that an unstamped marker is
not acted on; a test that a stale older result never becomes the outcome; and the crash tests run
with a probe-able tool, failing the store at every write point including the verdict and Unknown
records.

## 6. Examples

### Payments API with idempotency keys (Stripe-style): F-dedup, replay

The tool sends `Idempotency-Key: NextOnceKey(ctx)`. The actor replays the same request with the
same key under its attempt and records the response (the provider returns the original response for
a key it has seen, errors included, or performs the charge now). That is exactly once at the
provider, inside the provider's key retention window; Stripe, for one, documents that keys may be
pruned once they are at least 24 hours old. So the probe answers Unknown when `AttemptedAt` is older
than the window less a margin, and a run halted that long goes to an operator.

Why not just mark the tool `Idempotent`? Inside the window the effect is the same, and that is a
reasonable choice. The probe adds two things: a call older than the window halts instead of being
charged again under a pruned key, and the verdict is journaled with its evidence, where a retry-safe
re-run leaves no trace that it was a recovery.

### Email API with client-supplied message ids: Happened only

The tool sends a client-supplied message id derived from `NextOnceKey`, and the provider lets you
look sent messages up by it, but does not reject a second send with the same id. The probe returns
`Happened` (with the provider's message record as evidence) when the lookup finds the message, and
`Unknown` otherwise; the tool has no `Fencer` and is not `Fenced`. An absent result proves nothing:
delivery-event and search APIs commonly lag, and the original request may still be in flight. This
probe never re-runs a send, so at-most-once rests on H1 alone, and it clears the common case (the
send went through and only the journal write was lost) without an operator.

### Database insert keyed by the call: F-dedup or F-void

The tool inserts into a table with a unique key on `call_key = NextOnceKey(ctx)`.

- **As F-dedup:** the actor performs the insert itself with `ON CONFLICT (call_key) DO NOTHING`,
  then reads the row and records it. A late insert from the stalled driver conflicts and changes
  nothing; whatever its tool returns is an older attempt's result, which attempt-scoped results
  keep from standing. This is sound under the database's own isolation, since the unique index is
  the single point where the two inserts are ordered.
- **As F-void, when the insert must not be performed by the actor** (it fires triggers, or a second
  system reads the table): a fence table holds one row per call, `(call_key, floor)`, and the
  insert runs in a transaction that reads the row with `FOR SHARE` and proceeds only if its
  `AttemptKey`'s attempt is at least `floor`. The `Fencer`, in one transaction, locks the row,
  returns Happened if the call's data row exists, and otherwise sets
  `floor = GREATEST(floor, n + 1)` (monotonic) and returns NotHappened. A plain `SELECT` followed by
  a re-run is the counterexample: the original transaction can commit between them; and
  `floor = n + 1` without `GREATEST` is `void-not-monotone`.

If the table is in the same database as the journal, an outbox (the effect and its result in one
transaction) avoids the unknown window altogether; that is outside this note.

### Fire-and-forget webhook: no sound probe

A POST to a receiver that keeps no record bide can read, accepts no idempotency key, and has no
deadline it enforces. No observation can establish H1 and no fence exists, so the only sound probe
returns `Unknown`, which is the same as declaring none. The tool keeps today's behaviour: halt, and
an operator decides. If the receiver can be changed, giving it a dedup key turns it into the first
example.

## 7. Docs to update when built, and open questions

### Docs

- [GUARANTEE.md](../GUARANTEE.md): case 3 gains the probe path; the "at-most-once, not exactly-once"
  paragraph gains the precise exactly-once statement of section 2 and its conditions; a new boundary
  condition, "a probe's fence must be sound", beside "the tool must declare its safety accurately";
  "What a resume reads from the journal" gains the marker's probe contract, the verdict, Unknown and
  grant records, and the rule that a call's outcome is its latest attempt's result.
- [KNOWN-LIMITATIONS.md](../KNOWN-LIMITATIONS.md): probes cover tool calls, not `Step`s, flow nodes,
  retry-safe saga steps or MCP tools; an unsound fence opts out of at-most-once; dedup windows; the
  Unknown cap and `Reprobe`; a plain `Run` probes but does not act.
- [CONCEPTS.md](../CONCEPTS.md): Probe, Fence, the three verdicts, attempt-scoped results; the
  Reconciler entry points to probes as the in-process form.
- Guides: [durable-steps.md](../guides/durable-steps.md) (the `ResolveHalt` section, and sagas),
  [reliability.md](../guides/reliability.md), [debugging.md](../guides/debugging.md) (halt triage,
  `Reprobe`), [audit.md](../guides/audit.md) (verdict records in evidence), and
  [extension-points](../reference/extension-points.md) (`Prober`, `Fencer`, the store contract's
  per-attempt result keys). The i18n copies of each, the README's guarantee 1, and the CHANGELOG.
- Formal: [spec/tla/README.md](../../spec/tla/README.md) (models 1, 9 and 13),
  [formal-verification.md](../formal-verification.md), and [formal-models.md](formal-models.md).

### Open questions for the maintainer

1. **Result key shape.** Keep `tool:<id>` for attempt 0 (no change for calls that never re-attempt)
   and add `tool:<id>:attempt:<n>`, as proposed, or key every attempt uniformly?
2. **Does the latest-attempt rule apply to every call, or only to probe-able tools?** Applying it to
   every call also closes the window where a `ResolveHalt` resolution and a stalled driver's late
   result race for one key (today `NoLiveOverride` covers it under the lease check). Recommended:
   every call.
3. **`Prober` and `Fencer` as two interfaces** (a pure lookup, and a write under the claim), as
   sketched, or one interface with a flag on the call?
4. **The F-expiry fence.** Model it now, or leave it until a tool needs it?
