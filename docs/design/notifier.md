# Design: tell a human when a run needs them

Status: shipped in v0.16.0 (`agent.WithNotifier`, `agent.NotifyPending`, `plan.Builder.WithNotifier`, the `notify` package, TLA+ Model 14); `audit.VerifyNotifications` with `bide audit notifications` (build plan item 7) follows in the next release. The viewer's "Needs attention" view (item 8) is not built. It adds a
small `Notifier` seam, a signed webhook implementation, and an audited delivery trail that gives
notify-once across crashes and makes the notification itself evidence. The recipe "Get notified when a run needs a human"
wires the existing hooks to a webhook with an in-process dedupe; this note is the crash-safe,
auditable version of that recipe.

## Why

bide halts instead of guessing when a side effect's outcome is unknown (`*OutcomeUnknown`, cleared
with `ResolveHalt`) and pauses at approval gates (`*ApprovalPending`, answered with
`SubmitDecision`). Both stop a run until a person acts. Nothing built in tells the person. The
signals exist but are scattered:

- `Run` (and the typed and plan-flow equivalents) returns the typed error to its caller.
- `RecoverLoop` re-drives a halted or waiting run on every pass. It does not pass the halt or the gate
  to `WithRecoverErrors`: both are pauses, which a pass counts as success, so that handler sees only
  genuine failures. The error is visible to the `Resumer` the loop calls, before the loop classifies it.
- `MemWaker.Start(ctx, every, onError)` reports failures of timer wakeups, and likewise not a pause;
  the error is visible to the resume callback given to `NewMemWaker`.

A caller can build an alert from these (the Resumer and the callback included), and the recipe shows
how. It cannot make the alert
crash-safe, because the facts "this halt was already reported" and "this report is in flight" live
in the caller's memory. It also cannot answer the question an auditor asks of an agent that moves
money: *when were the approvers told, and was it within the time the policy allows?* The failure a
halt exists to prevent is a silent stall, so a missed notification is the expensive case; a
duplicate is not.

Goals:

1. A human is told, with an explicit and bounded duplicate rate and no silent drops, when a run
   halts on an unknown outcome, waits for approval, is aborted, or cannot be recovered.
2. The notification is evidence: observed, delivered and acknowledged times are in a tamper-evident
   log that `bide audit` can verify.
3. A late or failing notification is itself an event, not a silence.

Not the same as [durable signals](design-durable-signals.md), whose dropped stage 4 was a push
`Notifier` for *waking runs*. This is for alerting people, and it wakes nothing.

## Terms

- **Event**: a state that needs a person (the table under "Events"). Each has a stable **event key**.
- **Notification**: one delivery of an event to a sink. An event may take several attempts.
- **Sink**: where a notification goes (a webhook URL in the shipped implementation).
- **Trail**: the audited, append-only record of an event's observation, claims and acknowledgement,
  one per run.

## What "once" can mean

For a side effect on the world, bide's rule is at most once: a crash between the attempt marker
and the result halts the run, because repeating the effect is the worse error. A notification
inverts the cost:

| Semantics | Crash or lost ack in the window | Cost |
|---|---|---|
| At most once (attempt marker, halt on unknown) | The notification is dropped, and the run needs a human for the notifier itself | A missed halt, the thing we are fixing |
| At least once (record success after the ack) | The next round delivers again | A duplicate message |

**Recommendation: at least once, with a stable delivery ID so a receiver can drop duplicates.**
The claim the docs may make is "no event is dropped while some process keeps recovering or
sweeping, and a duplicate happens only after a crash, a lapsed claim, or a lost acknowledgement".
The docs must not say "exactly once": no sender can promise that to a receiver that can lose its
reply. A receiver that stores the delivery ID gets exactly-once handling; Slack incoming webhooks
do not dedupe, so a Slack sink sees the duplicates.

A notification must never be run as a `Step` side effect of the run it reports: an at-most-once
Step that crashes mid-POST would halt that run on the notifier, a halt about a halt.

## Events

| Kind | Fires when | Event key |
|---|---|---|
| `outcome_unknown` | a drive ends with `*OutcomeUnknown` | `halted:<run>:<op kind>:<op id>:<attempt marker time, Unix ms>` |
| `approval_pending` | a drive ends with `*ApprovalPending` | `approval:<run>:<call id>:<r>`; a gate that asks again over a tally it does not act on is an event of its own, `approval:<run>:<call id>:reask-sod` or `:reask-v3` (`ApprovalReaskKey`) |
| `run_aborted` | the run reaches the `run:aborted` end marker | `aborted:<run>` |
| `recovery_failing` | a run has kept failing recovery for longer than `D` (default 15 minutes): a Resumer error, not resumable, or store errors | `recovery:<run>:<k>` |
| `notification_overdue` | an event is not acknowledged by its deadline (below) | `overdue:<event key>` |
| `resolved` | an `outcome_unknown`, `approval_pending` or `recovery_failing` event that a receiver was, or may have been, told of stopped being current: the all-clear (below) | `resolved:<event key>` |
| `digest` | a periodic summary of runs halted or waiting longer than `X` (below) | `digest:<bucket>` |

Ids are encoded as attempt keys encode them.

- The attempt marker's time (`OutcomeUnknown.AttemptedAt`) is the same on every drive that reports
  one halt, and differs for a later attempt of the same call after a resolution, so a re-halt is a
  new event. It is 0 when the marker carries no time.
- `r` in an approval key is the reminder index, always `0` in v1. One notification per gate, and
  an m-of-n gate gaining a decision is the same wait, not a new event. A later "still waiting after
  X" reminder is `r = 1, 2, ...` with the same record kinds and the same fold, so it needs no
  protocol change.
- `recovery_failing` is measured in time, in the trail, not in a count in memory. A crash loop that
  restarts the process every few minutes would reset an in-memory counter forever and never alert,
  and a crash loop is exactly when the alert is needed. The first failing pass appends
  `failing_since{run, at, cause class}`; later failing passes write nothing; the event is observed
  once `now - failing_since.at > D`, by whichever process notices, across restarts and processes; a
  pass that succeeds appends `recovered{at}`, which clears it. `k` in the key is the number of
  `recovered` records for the run, so a second episode is a new event.
- Plain errors returned from `Run` are not events: the caller has them. `run_aborted` is the
  terminal failure the engine itself journals.

## API

In package `agent`, stdlib only:

<!-- docsnip: skip proposed API, not implemented -->
```go
// Notifier delivers one notification. Notify must return nil only once the sink has accepted it.
// It may be called again for the same ID after an error, a crash or a lost reply, so a sink that
// can dedupe should use ID to do so.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

type Notification struct {
	ID       string   // hex SHA-256 of Key; stable across attempts, processes and restarts
	Key      string   // the event key
	Kind     NotificationKind
	RunID    string
	EventAt  time.Time // when the run entered the state, from the journal
	Observed time.Time // when a notifier process first saw it
	Attempt  int       // delivery attempt, from 1
	Err      error     // the typed error, for an in-process notifier; the webhook does not send its text
}

// NotifyTrail is the port for the audited delivery trail. audit.NotifyTrail implements it over an
// audit.EventStore; package agent cannot import audit.
type NotifyTrail interface { /* Append at the next position, Load; see "Notify once" */ }

func WithNotifier(n Notifier, trail NotifyTrail) Option // Agent (Build) and plan Flow
func WithNotifier(n Notifier, trail NotifyTrail) RecoverLoopOption

// NotifyPending runs one catch-up sweep: for every unfinished run it finds a gate waiting for
// approval and an effect with no result older than WithNotifyHaltAge, and delivers those events
// as a drive would, under the same keys. Call it at startup and on a timer.
func NotifyPending(ctx context.Context, j *Journal, n Notifier, trail NotifyTrail, opts ...NotifyOption) error
```

`WithNotifier` takes the trail: the crash-safe guarantee needs it, so a nil trail is `ErrConfig`.
Without a trail there is no notify-once; that is the recipe's in-process path, not a degraded mode
of this API.

### Where events are emitted, and the guarantee's two paths

Emission is at the end of a drive, where the engine has just decided a run stopped for a typed
reason. That single point is shared by a caller's `Run`, `Recover`, a `MemWaker` wakeup and
`RecoverLoop`, so no path needs its own wiring. The drive hands the event to a delivery goroutine
the notifier owns (`Agent.FlushNotifications` waits for it, bounded by a context), so a slow sink
never delays `Run` returning the typed error. A `Resumer` that drives no agent with the option is
covered by `WithNotifier` on `Recover` and `RecoverLoop`, which sees the pass's result.

**The at-least-once guarantee needs one of two catch-up paths.** A process can die between the
event and the first record in the trail, and then nothing remembers the event but the run's own
journal. Either:

- `RecoverLoop` runs (each pass re-drives a halted run, re-observes the event and finds its trail
  incomplete), or
- the application calls `NotifyPending` at startup and on a timer, which reads the journal's runs
  directly. An app that only ever calls `Run` is covered by the sweep.

With neither, delivery is best-effort: the event is sent from the drive that saw it, once, and a
crash before that is a miss. The guide states this plainly.

## Notify once, across crashes: the audited trail

Delivery state does **not** go in the run's journal. The run journal stays what resume reads and
what `Replay` projects; no `notify:` key is added, no exception to the ended-run rule is needed,
and the run lifecycle cannot be affected by a webhook. State goes in bide's existing audited event
machinery (`audit.EventStore`, `audit.EventLog`, `audit/eventstore.go`), where the records are
salted, Merkle-committed, signable and verifiable by `bide audit`.

### What EventStore gives, and what is missing

What exists:

- `EventStore.Append(ctx, runID, seq, leaf)` is append-only at a contiguous position, idempotent on
  an identical leaf and an **error on a different leaf at a stored `seq`**. That is insert-if-absent
  at the next position: two processes appending different records at the same `seq` cannot both
  win. It is the primitive the claim protocol already relies on, in a different table.
- Leaves are canonical, salted and committed (`EventLog`, `TreeHead`, `ProveConsistency`), and an
  event trail is a separate lifecycle from the journal ("A separate lifecycle: the BYO EventStore
  port", `docs/guides/audit.md`).

What is missing, and the smallest addition for each:

1. **A way to tell a lost race from a failure.** The interface says only that a different leaf at
   an existing `seq` "must error". A loser must reload and re-fold, not retry blindly. Add a
   sentinel `audit.ErrSeqTaken` that `Append` implementations wrap in that case, and a conformance
   test every store (the memory store and the documented Postgres `UNIQUE(run_id, seq)` shape) must
   pass. Backward compatible: a store that errors today and does not wrap it is treated as a
   failure, and the next round reloads anyway.
2. **Event types for the records.** `agent.RunEvent` is a closed set and the journal projection
   (`PersistJournal`) owns the positions of a run's trail, with a documented rule never to mix
   `PersistEvent` and `PersistJournal` on one run. So delivery records get their **own trail**, named
   `notify:<run>` (an `EventStore` run argument is an opaque string), and their own leaf kinds
   (below). That is a new set of kinds under the existing salted event-leaf format.
3. **A port in `agent`.** `agent` cannot import `audit`. `agent.NotifyTrail` is a minimal interface
   (append a typed record at a position, load the trail) and `audit.NewNotifyTrail(evStore)` adapts
   an `EventStore` to it, canonicalizing and salting the leaves.
4. **A durable store.** `MemEventStore` is single-process. The cross-process guarantee needs a
   durable `EventStore` with the `UNIQUE(run_id, seq)` shape the interface already documents.

Nothing else: no change to `Store`, to `Journal`, to the claim protocol or to `EventLog`.

### Records

All are leaves of the run's notify trail; state is a fold over the trail, so the trail is the only
source of truth.

| Record | Written | Fields |
|---|---|---|
| `observed` | once per event, first process to see it | `key`, `kind`, `event_at`, `observed_at`, `deadline_at`; for a halt or approval also `op_run`, `op_kind`, `op_id` |
| `claimed` | at the start of each delivery round | `key`, `holder`, `at`, `until` |
| `acked` | after the sink acknowledged | `key`, `first_sent_at`, `acked_at`, `attempts`, `status`, `auth` |
| `failing` | once, when 3 rounds (8 attempts) have claimed the event without acknowledging it | `key`, `failures` (rounds), `at` |
| `overdue` | once, when the deadline passes unacked | `key`, `at`, `latency` |
| `failing_since` | a recovery pass failed and no episode is open for the run | `run`, `at`, `cause` (a class, not text) |
| `recovered` | a recovery pass succeeded while an episode is open | `run`, `at` |
| `watch`, `unwatch` | in a segment of the open trail `notify:@open:<date>`, not a run's: an event whose all-clear may be owed is listed before it is observed, and unlisted when nothing more is owed | `key`, `run`, `at` |
| `checkpoint`, `seal`, `backfilled` | in an open-trail segment: the listed set carried into a new segment (first record), the pointer to the next segment (last record), the one-time listing of earlier events | `watching`, `backfilled`; `next` |
| `segment` | in the open trail's index `notify:@open:index:<year>`: a segment was created | `next` |
| `cleared` | the event stopped being current, while it was still open | `key`, `at`, `reason` (`resolved`, `decided`, `ended`, `recovered`) |

The fold of an event: **open** until `acked` or `cleared`; **claimed** if the last `claimed` has
`until` in the future. An acknowledged event is never `cleared` afterwards (the first terminal record
is final), so the end of a delivered event is recorded as the all-clear's own `observed` record,
below. That record has `reason` and no `deadline_at`.

### Protocol

For event `key`, a process with the notifier configured, usually the one holding the run's lease:

1. Load the trail and fold. `acked` or `cleared` for `key`: stop.
2. No `observed`: append it at the next `seq` (`event_at` from the journal, `deadline_at =
   observed_at + notify deadline`). On `ErrSeqTaken`, reload and continue from step 1.
3. Check the event is still current from the journal (halt still stands, no decision, run not
   ended). If not, append `cleared` and stop.
4. If another holder's `claimed.until` is in the future: stop (it will deliver). Otherwise append
   `claimed{holder, until = now + 2 min}`. `ErrSeqTaken` means another process won this round:
   reload, fold, and stop if its claim is live. The `holder` is unique to the deliverer: the
   configured name, the kind of deliverer (drive, recover, sweep, flow) and a random nonce per
   engine, so a drive's notifier and a sweep in one process, or a process that restarted under
   the same name, never read each other's live claim as their own; and one engine runs one round
   at a time per key.
5. Deliver (one round: up to 3 requests with backoff, below). On success append `acked`. On
   failure wait out the backoff and begin a new round at step 1. Before a round starts, and when
   one fails, count from the trail the rounds that claimed the event and never acknowledged it,
   crashed ones included; at 3 (ceil(8/3)) append `failing` once. The count is not process
   memory, so a deliverer that dies in every round still raises it.

**One claim record per round, not per attempt.** A round's budget (about 45 s) fits inside the
claim, so a live holder never renews mid-round; between rounds the claim lapses and the next round
re-claims. At the 15-minute backoff cap that is at most about four `claimed` records an hour for an
undelivered event, plus one per crash takeover, never one per request. A crashed holder delays
takeover by at most the 2-minute claim, which is inside the 5-minute default deadline. (Open
question 5 weighs a lease-backed claim that writes no record per round.)

**Ordering is the guarantee.** `acked` is appended only after the sink acknowledged, so a crash
anywhere before it leaves the event open and the next round sends it again: at least once. The
duplicate windows are exactly: a crash or error after the POST succeeded and before `acked` is
durable; a claim that lapses while a slow POST is still in flight; and a lost reply.

**Lost acknowledgement.** The sink accepted and the reply was lost, or the trail append failed
after the sink accepted. Both look like a failed attempt, so the next one resends with the same
`ID`. An append failure after a success is retried in place, under the round's claim, and if the
store stays down the process remembers that it delivered and records the acknowledgement on its
next round without sending again; only a process that dies first repeats the delivery.

**The stale-alert race.** Step 3 checks "still current" before step 5 sends, and the two are not
atomic. A halt resolved or a decision submitted in between can produce one alert for a state that
ended a moment ago. This is documented, not closed: closing it needs the send and the journal read
in one atomic step, which no webhook allows. The receiver can re-check through the view link.

### Never give up

An open event is retried for as long as the process runs. Backoff is capped exponential: 1 s base,
doubling, cap about 15 minutes, with jitter. After 8 failures the engine reports "notification
failing" (to `WithRecoverErrors`, as a `failing` record, and in the viewer) and keeps trying. Stop
conditions are `acked` and `cleared` only. Giving up silently on the event whose purpose is to end
silence is the wrong default.

### The all-clear

A receiver that was told of an event holds an incident open (a page, a ticket, a Slack thread). The
trail recorded when the event stopped being current (`cleared`), but nothing told the receiver, so
the incident stayed open until a person closed it. `resolved` is that notification. It is delivered
by the same engine as any event: its own key (`resolved:<event key>`), its own `observed`, `claimed`
and `acked` records, the same claim race, retry, backoff and `failing` record, and the stable ID
`NotifyID(resolved:<event key>)`.

**When it is owed.** For an `outcome_unknown`, `approval_pending` or `recovery_failing` event that is
no longer current, in either of two states:

- *acknowledged*: the receiver was told. The all-clear says the event is over, with the reason
  (`resolved`, `decided`, `ended`, `recovered`) the currency check gives;
- *cleared after a delivery was attempted* (the trail holds a `claimed` for it): the receiver may have
  been told, since a request can land although its reply was lost or its call abandoned. Clearing
  stops the event's own delivery, so the all-clear is the only message the receiver gets about it,
  and it names the event so the receiver can close an incident it may or may not have opened.

An event never claimed was never sent, so nothing is owed. `run_aborted` is a fact and never stops
being current, so it has no all-clear. An overdue notice and an all-clear are not incidents and have
none.

**Never overdue.** An all-clear is observed with no `deadline_at`; no `overdue` record is written for
it and no overdue notice is sent. The deadline exists because silence about an event that needs a
person is dangerous. A missing all-clear leaves an incident open, which fails safe: the next person
to look sees the run is fine. It is still retried for as long as the process runs, and raises the same
`failing` record after the same number of rounds, reported to `WithNotifyErrors`. The model checks
that it never goes overdue.

**Ordering.** Delivery order between an event and its all-clear is not guaranteed, and need not be.
Event keys are never reused (a halt's key holds its attempt marker's time, a gate's its reminder
number, a recovery failure's its episode) and an event that stopped being current does not become
current again, so the all-clear's `clears.id` is final: a receiver that gets an all-clear for an ID
treats every delivery of that ID, before or after, as superseded. The all-clear can arrive first only
when the event was cleared before it was acknowledged (a call in flight or a lost reply); the model
shows the case (`limits/clear-before-event`). A receiver without a way to remember an ID it has not
seen (a stateless relay) may ignore an all-clear for an ID it does not know; one that does remember
(PagerDuty's `dedup_key` does) gets the right incident state either way. No sequence number is
needed because the identity is the order.

**How an all-clear is found.** A *scan* of a run's trail reads it, folds it, and for each such
event checks currency as a round does, from the journal. The scan is queued on the notifier's worker
after every drive of a root run (`Run`, a plan flow), after every recovery pass over a run, and for
every unfinished run the sweep (`NotifyPending`) lists, so a decision, a resolution, the run's end and
a recovery are each noticed by the next process that touches the run. A round that clears an event
also queues its all-clear, and a scan queues an all-clear that was observed and not acknowledged, so a
process that dies after any record resumes from the trail. The scan only reads and appends through
the claim protocol, so any number of processes may run it.

**A run that completed while nobody scanned.** The sweep and a recovery pass list unfinished runs,
and a run that completes in a process that dies before its scan is not one. The *open trail*
(`notify:@open`, `agent.NotifyOpenTrail`) closes that gap without reading every finished run. Before
a round observes an `outcome_unknown`, `approval_pending` or `recovery_failing` event, it appends
`watch{key, run}` to the open trail (idempotent while the key is listed); if that append fails the
event is not observed or sent in that round, so no event that was delivered is missing from the
list. A sweep (after its run loop), a `Recover` pass and each pass of `RecoverLoop` queue a scan of
the open trail: it folds the list, loads the trail of each run that has a listed event, and scans it
as above, whether or not the run has finished. `unwatch{key}` is appended when nothing more is owed:
the all-clear was acknowledged, or the event was cleared before any delivery was attempted. A key
listed for over an hour whose run's trail has no record of it (the process died between the two
appends) is unlisted, and listed again if it is observed later.

*Clocks.* The time of an open-trail record is structural. It is the writer's clock, moved later when
that is needed for the record to fall in its segment's UTC day and after the record before it (a
process whose clock is behind a segment another process's clock ran ahead into follows the chain and
writes there); the writer's own reading is kept in `clock_at` (open-trail records only, always earlier
than `at`), so the move is visible, and it only ever makes a time later. No deadline or latency is
computed from an open-trail time.

*Bounded.* The open trail is append-only and verifiable, and a read does not grow with history. It
is kept in one segment per UTC day, `notify:@open:<date>` (`agent.OpenSegment`), and an index,
`notify:@open:index:<year>` (`agent.OpenIndex`), holds one `segment` record per segment created. A
lookup reads the last record of the newest index (this year's, or the nearest earlier one the first
time in a year) and then the segment: a few loads however long the gap and however old the
deployment. A writer that finds the newest segment is from an earlier day appends `seal{next}` to it
(the append race makes the seal final: no record can follow it), then writes `checkpoint{watching,
backfilled}`, the set listed in the sealed segment, as the first record of today's, then indexes
today's. So a read is the listed set plus one day's records, and the index is a year of days at most.
A checkpoint must equal the fold of the segment it continues, which `bide audit notifications`
checks. A crash is repaired by the next writer at any step: after the seal the chain names the next
segment, which gets its checkpoint; a segment reached through the chain but missing from the index is
indexed; the index names an empty segment only for the very first one, which has nothing to carry. A
peer whose clock is behind follows the seals to the newest segment.

*Events from before the open trail.* The first sweep (`NotifyPending`), `Recover` pass or
`RecoverLoop` pass that finds no `backfilled` marker lists every event in every run's trail that can
still owe an all-clear (the run list includes finished runs; each trail is read once, ever), then
appends `backfilled`. Listing is idempotent, so two processes backfilling at once list each event
once and race only for the one marker, and an interrupted backfill starts over. The marker is
carried by every checkpoint.

*Rolling upgrades.* Processes of the previous version write no open-trail records, so one still
running after a new process has backfilled delivers events it never lists. Nothing relies on the
one-time backfill alone: (1) every scan of a run's trail (after each drive, recovery pass and sweep of
an unfinished run, and for each run the open trail lists) lists any event that belongs on the open
trail and is not listed, so the first new-version scan of an unfinished run lists what the old
process delivered, and its all-clear follows when it ends; (2) a second backfill pass runs
the upgrade window (`WithNotifyUpgradeWindow`, a day by default) after the first, in any `NotifyPending`, `Recover` or `RecoverLoop`,
and lists what a scan could not reach (a run the old process saw to completion, which no sweep or
recovery pass lists as unfinished). There are two passes, ever: the checkpoint carries their count
(`backfills`), the time of the last (`backfill_at`) and the window the first pass recorded
(`backfill_window`, also on the first marker); the second pass waits for the recorded window, not
its own process's setting, and the verifier requires the second marker no earlier than the first
plus that window. An old process that delivers an event after
the second pass, in a run that ends before any new-version scan reaches it, leaves it unlisted, and
the audit reports it ("observed after the open trail began and was never listed"); set the window
to the time the upgrade takes to reach every process (a week for a staged rollout across regions).

*Isolation from recovery.* The scans and the backfill run on the notifier's worker, never on the
recovery pass or drive that queued them: a trail store that errors fails only the job (reported to
`WithNotifyErrors`, retried after the backoff), one that panics fails the round (a panic in a
`Notifier` fails the call), and a store that hangs does not hold the pass: `Recover` and a flush wait
for the scans for at most ten seconds, after which they leave them to run on, or to the next process,
while events a person is waiting for are still waited for as before. `Recover`'s count and error are
what they would be with a healthy store.

*Roots.* An event is filed under its root run's trail. A plan flow's run is its own root whatever its
ID holds (a flow named `tenant>flow1` is not a sub-agent of `tenant`): the hook path
(`RunNotifier.DriveEnd`, from the flow's `RunRef`) and the sweep (from the run's `run:start`, which
records `RunKindFlow`) agree on it. Any other run with a `>` in its ID is a sub-agent's, attributed to
the root its ID names.

**An ID too long to encode.** A key holds each ID in encoded form, and one longer than 96
characters is replaced by its digest, which cannot be turned back into the ID. The `observed` record
of a halt or an approval therefore also carries `op_run`, `op_kind` (`tool`, `step` or `gate`) and
`op_id`, which the currency check reads in preference to the key. A record from before them has none
and is read from its key, which works for every ID that fits.

**Cost.** One trail read per drive end, recovery pass and swept run, one more for the open trail per
sweep and recovery pass plus one per run listed, off the drive's path, and a journal read for each
acknowledged event not yet cleared.

### Routing and escalation

**Routing** sends each event to one receiver, chosen by its kind (`WithNotifyRoute`) and, for an
approval, by its approvers (`WithNotifyApproverRoute`). **Escalation** (`WithNotifyEscalation`) tells
the next receiver of a bounded chain when an event is not acknowledged by its deadline, or, for an
approval, is not decided by it. Both are decided in the engine, not in a `Notifier` that fans out, and
the reason is the audited trail. A `Notifier` that sent to several sinks would return one result, so the
trail would hold one `acked` for all of them, a failure of one would retry all of them, and an auditor
could not say through which receiver a person was told. In the engine, each delivery to each receiver is
a key of its own, so it has its own `observed`, `claimed` and `acked` records, its own retries, backoff,
`failing` and `overdue`, and the claim race and the duplicate bound hold for it as they hold for any key.
The unit the protocol, the fold and the model were built on does not change: a key.

**What a route is.** A route maps an event to a receiver name; `observed` records it (`receiver`, empty
for the default receiver, so a trail written without routing is unchanged). The delivery ID is
`NotifyID(key)` as before. The all-clear and the overdue notice of an event follow its receiver. An
approval whose approvers are routed to several receivers is several deliveries, each under the key
`to:<receiver>:<key>`; a gate with one receiver for all its routed approvers keeps its plain key. Sharing
one key between receivers is the mistake the key rule prevents, and the model shows it
(`regress/shared-ack`): the first receiver's `acked` would close the event for the others.

**What a step is.** Step `l` of the chain of event `k` is the key `escalated:<l>:<k>`, kind `escalated`,
observed with the receiver, the level and a deadline of its own, the step's. It is an ordinary event in
every other respect, with two differences: it has no overdue record (its deadline is the next step's), and
it stands as long as the event it escalates (`current` reads that event), so it is cleared with it and the
receiver it told gets the all-clear (`resolved:escalated:<l>:<k>`). A step is observed only if, on the
trail prefix the deliverer loaded, the event is not cleared, no receiver of the event or an earlier step
has acknowledged it (an approval is satisfied only by being decided), the event's deadline (step 1) or the
previous step's (later steps) has passed, and, for a later step, the previous step is observed. The append
that observes it is at the position after that prefix, so the check and the observation are one atomic step
of the trail: no step is observed after an acknowledgement or a clearing the trail shows. The chain is
bounded by its configured length (at most four) and ends there; the last step's deadline passing raises
nothing more.

**Who runs it.** A worker job, the *watch* of an event, wakes at the deadline of the current step, reads
the trail and submits the next step's delivery; it writes nothing until a step is due. It is queued when
an event is routed and, for an event already in a trail, by the scan of the run's trail (after every drive
of a root run, every recovery pass, every run `NotifyPending` lists), so a process that dies with a watch in
memory leaves it to the next one, which continues from the trail's last step. Two processes that both find
a step due submit it twice; the trail gives it one `observed` and one deliverer at a time.

**Limit.** An escalation made from the scan, after a restart, rebuilds the event from the observed
record's operation fields and reads the rest from the journal (a gate's `approval-pending` record, the
halted call's request), so it carries the gate's tool, policy and approvers and the call's tool name, but
not the cause of a halt, which only the drive that saw it knows. A deployment with no process alive at a
deadline escalates at the next pass.

### Behavior fixed by the adversarial review

An independent review of the first implementation found these, and each is now a test:

- **The overdue notice is derived from the trail.** It is delivered inside the round that holds the
  original event's claim, as part of it (no claim of its own, so an undelivered event writes one claim
  per round however many notices it owes). Every round of an open, overdue event sends the notice
  unless the trail holds an acknowledged one, so a death after the `overdue` record, a failed first
  delivery, a sweep and a restart all re-send it. A delivered but unrecorded notice is recorded
  without sending again.
- **Every `Notify` call is bounded** by an eighth of the claim (so `WithNotifyClaimTTL` must be at
  least 30 seconds, and at least eight times the longest a sink takes; a call that outlives its limit
  says so and names the option), and abandoned when the time is up
  even if the Notifier ignores its context (it counts as failed, and an abandoned call that later
  succeeds is a duplicate within the stated bound). A round makes at most four calls (the overdue
  notice is the fourth), so its calls take half the claim, its backoff and acknowledgement retries
  a little more, and it starts only when more than three quarters of its claim remain: it ends inside
  the claim it holds (the budget counts the backoff between attempts and the acknowledgement's
  retries, whose sleeps are capped at what remains of their limit). Rounds of different events run concurrently (up to 8), so a hung sink holds
  neither the others nor, for long, its own slot. An abandoned call that has not returned is not
  repeated: its event makes no new call until it does, and an engine has at most 256 calls running,
  so a Notifier that never returns cannot gather goroutines; the refusal is reported to
  `WithNotifyErrors`. `RecoverLoop` bounds its flush on the way out (500 ms)
  and cancels the calls still in flight.
- **A peer sends the overdue notice when the holder is gone.** The holder sends the notice inside its
  own rounds, so a holder that claimed before the event was overdue, and may be dead for the whole
  claim, would delay it by the claim (an hour at `WithNotifyClaimTTL(time.Hour)`). Such a peer sends
  it under a short claim of its own on the notice's key (long enough for the notice's call and its acknowledgement: a quarter of the event's claim and half a minute, at least two minutes, at most the event's claim), so peers do not send it
  together; a holder that is only slow skips the notice while a peer's claim on it is live, so the
  two do not both send it, and sends it in a later round if the peer's send never lands. A holder that claimed after the event was overdue is left to send it, so the common case
  writes no extra claims.
- **Claim liveness does not trust synchronized clocks.** `claimed.until` is written by one clock and
  read by another. Another holder's claim counts as live until `until` plus a skew margin (default
  2 minutes, `claimTTL`). A peer whose clock is ahead takes a live claim over only if it is ahead by
  more than the claim's remaining time plus the margin: at the defaults, about 2.5 minutes at the
  moment a round starts, less as the round runs. Run NTP-class synchronization on every deliverer.
  The cost is a takeover after a crash that is up to the margin later.
- **Pacing comes from the trail.** A new round starts no earlier than the last claim's time plus the
  backoff for the rounds the trail records (no jitter, so every deliverer agrees). However many
  sweeps or processes look at an undelivered event, it writes about four claims an hour at the cap.
- **A decided gate is not reported.** "Current" reads what the gate itself reads: the single
  approver's decision (`approval:<call>`), an m-of-n gate's terminal tally, the tool's result, and,
  for a plan flow's gate, the node's own result (`node:<name>`). The sweep applies the same test. A gate
  that asks again (`reask-sod`, `reask-v3`) is decided only by its re-tally (`approval-tally:<call>:sod`
  or `:v3`) or the tool's result: the tally it asks over is what the gate does not act on. The key says
  which ask an event is, so the catch-up scan and an escalation rebuilt from the trail ask the re-ask's
  question, and take its policy and approvers from the re-ask note (`approval-pending:<call>:sod` or
  `:v3`) when the journal proves it (`agent.ValidReaskNote`). A re-ask is routed by the approvers of the
  policy it asks under, like the first ask.
- **A run that was aborted has a catch-up path.** `NotifyPending` also reads runs holding
  `run:aborted` (completed and cancelled runs are excluded from the listing) and raises
  `run_aborted` for any whose trail has no event, if the abort was within `WithNotifyAbortedWithin`
  (24 hours), so enabling the sweep does not page for every run that ever aborted. `run:aborted`
  records `aborted_at` (informational, like `started_at`: no drive, resume or recovery pass reads it,
  and a run:aborted without it loads and behaves the same; it is stamped by the drive's clock, so a
  run driven with `WithClock` carries that clock's time, and the sweep compares it with the
  notifier's own: a deployment that drives runs with a clock other than wall time gives the notifier
  the same one with `WithNotifyClock`). For a run an earlier version aborted the
  sweep uses the newest time the journal records for the run (an attempt marker's, else the start),
  which is no later than the abort; a run whose journal records no time is not raised. A known limit
  for journals written before `aborted_at`: model turns carry no time, so a run whose last activity
  was model turns only is dated by its last attempt marker or its start, and a long-running one may
  be dated too early to be caught up. It costs one
  point read for each aborted run, every sweep, and a History read for each legacy one.
- **Recovery episodes close on any pause**, a halt, a gate, a timer or a signal, which a pass counts
  as success. A run no Resumer can drive (`ErrNotResumable`) is a recovery failure and alerts after
  the duration. A pass ended by the shutdown's cancellation is not a failure.
- **`recovery_failing` carries a class, never the Resumer's text.** `Notification.Err` is the matching
  sentinel (`ErrStorage`, `ErrNotResumable`) or nil.
- **Halt keys for an untimed marker** use the live attempt's generation (the negative of one more than
  it), not 0, so a re-attempt after a not-started record is a new event. If the marker cannot be
  read, the worker tries again: the halt is queued at once under a provisional key and its real key
  is computed in the worker, so a drive (`Run`) never waits on the journal for it, and a failed read
  never produces a guessed key.
- **An idle notifier holds no goroutine.** The dispatcher exits when nothing is queued and starts on
  the next event; an event queued while it leaves starts another.
- **`FlushNotifications` returns what was not delivered**: the last error of each event still queued
  when the worker stops, and, for an event whose round deferred to another deliverer's live claim or
  to its pace, that deferral (checked against the trail first, under the flush's own deadline and
  at most two seconds, so a hung trail does not hold a shutdown; if the check fails the deferral stays, so an event the other deliverer has
  since acknowledged is not reported), so a caller that exits after it knows. Key locks are removed when no
  round holds or waits for them. A plan flow keeps one notifier per
  store, not per `Journal` value.
- **A trail with a record kind a newer version wrote loads**, in its position, and the fold skips it,
  so a rolling upgrade does not stop the older processes.
- **`RecoverLoop` reports its undelivered events** at exit to `WithRecoverErrors` (the flush error),
  which is not discarded.
- **A late success after an abandon is a duplicate within the bound.** A call abandoned at its timeout
  that later succeeds means the sink has the notification, and the next attempt repeats it with the
  same ID: a duplicate the receiver can drop, like any lost-reply duplicate.
- **`VerifyNotifications` must read the trail strictly.** The engine's `Load` keeps a record kind it does
  not know in its position so an older process keeps delivering; the verifier (build plan item 7) must
  refuse a kind it cannot read, or an older verifier would pass a trail it did not check.
- **The all-clear's scans cost trail reads** per drive end, recovery pass and swept run, plus the
  open trail's (see "The all-clear").
- **Known cost:** a recovery pass does a trail read for each run it drives that pauses or succeeds, to
  close an open episode. It is one point read of a short trail; a deployment with very many paused
  runs may want a longer recovery interval.

## Notification as evidence

Three times for one event, all in the audited trail:

| Time | Record | Meaning |
|---|---|---|
| T1 | `observed.observed_at` (and `event_at`, so detection latency is `T1 - event_at`) | a notifier process saw the event |
| T2 | `acked.first_sent_at` (and each `claimed.at`) | the first request was sent |
| T3 | `acked.acked_at` | the sink answered 2xx |

**`bide audit`.** `audit.VerifyNotifications` and the verb `bide audit notifications` (shipped;
see [Audit: notification evidence](../guides/audit.md#notification-evidence-were-the-people-told)).
It checks:

- every leaf is a notification record this version reads, strictly (an unknown kind or field is an
  error, not skipped), and the trail is a history a notifier writes: an event is observed once and
  first, nothing follows its acknowledgement or clearing, an acknowledgement follows a claim, an
  overdue notice follows its `overdue` record, an all-clear follows the acknowledgement or clearing
  it reports (and an `overdue` record is never written for a `resolved` key), records are dated in
  order within the clock skew, and recovery episodes open and close in turn;
- with signed heads over the trail (`audit.SignNotifyTrail` or the ones `audit.AuditedEventStore`
  anchors; kind `events`, run ID the trail's name), every head verifies and its first `size` leaves
  are exactly the leaves it commits to, so nothing was dropped, reordered or rewritten before the
  newest. Records after the newest head are read and reported as not attested, with their count and
  the age of the oldest (`-max-unattested` can fail it). Consistency between heads is not a separate
  step: each head is checked against the full trail;
- with routing and escalation, the receiver and level each event records are well formed; an
  escalation step is dated after the deadline it follows, in order, for an event that is not
  cleared (nor, unless an approval, acknowledged); an all-clear and an overdue notice go to their
  event's receiver; and a fan-out delivery (`to:<receiver>:<key>`) names its receiver;
- for every approval gate, unknown-outcome halt and aborted run in the supplied journals
  (`agent.JournalNotifyEvents`, keyed as the notifier keys them), the trail holds `observed` for
  it, or the event is younger than the grace period;
- `acked_at` is within the event's recorded deadline (and within an operator bound, `-max-ack`), or
  the event is reported `acked_late`; an open event past its deadline is `overdue`;
- an `overdue` record has an overdue notice, a recovery failure episode open longer than the
  threshold has a `recovery_failing` event, and an event cleared after a delivery was attempted has
  its all-clear.

**Signing and anchoring the trail.** The trail is an `EventStore` trail: Merkle-committable and
append-only by the store's contract (`UNIQUE(run_id, seq)`). `audit.AuditedEventStore` wraps that
store and signs and anchors the trail as it is written, mirroring `AuditedStore` for the journal:
the same `Signer` (so the HSM, KMS and ECDSA signers work unchanged), the same `Anchor`, and the head
`audit.SignNotifyTrail` signs (kind `events`, run ID the trail's name).

The design choices, and why:

- **A wrapper over the `EventStore`, found through `Unwrap`.** The trail adapter
  (`audit.NotifyTrail`) already sits on an `EventStore`, so signing there needs no change to
  `agent`, which cannot import `audit`. The wrapper also signs any other event trail written through
  it, and the signing key lives where the store is wired, not in the engine.
- **Off the delivery path.** A round holds a claim for a fixed budget (`roundBudget`) that counts
  the store's round trips as short. Publishing inside `Append` would add a signature and a network
  call to each of a round's appends, and a slow (not failed) anchor would break that arithmetic.
  `Append` therefore returns when the store has the leaf, and a background publisher publishes the
  head. A failure goes to `OnError`, never to the delivery, as for `AuditedStore`.
- **Bounded.** An attempt (read the trail, sign, publish) is abandoned after a timeout, even if the
  anchor or signer ignores its context, and a failed attempt pauses publishing for a cooldown
  before one probe, so an outage costs one timeout per cooldown and no goroutine loop.
- **Heads only grow.** Each trail has a slot held while its head is built and published, and a head
  is published only if the trail is longer than the last one published. Two processes can still
  publish out of order; both heads are prefixes of one trail and the verifier compares by size.
- **A bounded window.** The records between the last published head and the end of the trail are the
  window. `NotifyPending` (for each run it looks at), the recovery pass (for each run it recovers),
  and `FlushNotifications` ask the trail to sign again any trail that grew past its last head (a
  sweep and, once a minute, a recovery pass also name the global trails: the digest's, the open
  trail's index for this and last year, today's segment and the newest three), or
  that an earlier attempt failed to publish (`agent.NotifyTrailAnchorer`, optional; best effort,
  reported to `WithNotifyErrors`, never a delivery error). The verifier reports the window's size
  and age and can fail it (`-max-unattested`).
- **The model is unchanged.** Model 14 (`spec/tla/notify`) models the trail's protocol: appends at a
  position, the lost race, claims, acknowledgements. Anchoring reads the trail after an append has
  committed, writes only to the anchor, and never changes what `Append` returns or when, so it adds
  no protocol step and no state the delivery guarantees read; see the model's README.

It reports "approvers were notified at T" with the T1, T2, T3 above, and how the delivery was
authenticated: `auth` records `signature` (the signature scheme and which secret slot signed, current
or previous, never the secret), `mtls` (whether a client certificate was presented, its SPKI hash,
and whether the receiver was pinned or checked against a custom CA), and the negotiated TLS
version. An audit policy can then require that approval notifications were sent with mTLS.

What this does and does not prove: `acked` means the sink's endpoint accepted the request, not that
a person read it. Times are the notifying process's clock; anchoring the trail's tree head with a
timestamp (`audit/timestamp.go`, `audit.Anchor`) bounds them from above ("no later than").

**Viewer.** The approval gate shows `notified 09:14:07, acknowledged 09:14:08 (1 attempt)`, or
`not notified` / `overdue since 09:19:07` in the "Needs attention" list. The viewer reads the trail;
it sends nothing.

### Time limits

- **Notify deadline.** `WithNotifyDeadline(d)`, default 5 minutes from `observed_at`. Past it with
  no `acked`, the next round appends `overdue{latency}`, emits a `notification_overdue` event (to the
  same sink, or to a separate `WithOverdueNotifier` if the first is the thing that is down), flags
  it in the viewer's "Needs attention" list, and **keeps retrying the original**.
- **Digest.** Optional `WithDigest(every, olderThan)`: every `every`, one `digest` notification
  lists runs halted or waiting longer than `olderThan`, as structured rows (run, kind, since). A
  digest has its own trail (`notify:@digest`), keyed by time bucket, with the same records and fold.
- **Detection latency** is recorded on every event (`observed_at - event_at`), so an audit sees how
  long a stall was undetected, not only how long it was unannounced.

## The shipped webhook notifier

Package `notify` in the core module (`net/http`, `crypto/hmac`, `encoding/json`; no new dependency):

<!-- docsnip: skip proposed API, not implemented -->
```go
n, err := notify.NewWebhook(os.Getenv("BIDE_WEBHOOK_URL"),
	notify.WithSecrets(current, previous), // sign with current; previous is accepted by receivers during rotation
	notify.WithTimeout(10*time.Second),    // per request
	notify.WithClientCert(certFile, keyFile), // optional mutual TLS
	notify.WithServerCA(caFile),              // optional private CA for the receiver
	notify.WithServerPin(spkiSHA256),         // optional pin of the receiver's key
)
```

### Transport and secrets

- **HTTPS only.** `NewWebhook` refuses an `http://` URL (`ErrConfig`) except for `localhost`,
  `127.0.0.1` and `[::1]`, which exist for development and tests.
- **No redirects.** A signed body must not go somewhere the operator did not name.
- **Secrets come from configuration or the environment**, as arguments to `NewWebhook`. They are
  never written to the journal, the trail or a payload, and never appear in an error string.
- **Rotation.** With two secrets the request carries two signatures, so a receiver that has either
  secret verifies it: rotate by deploying `previous = old, current = new`, updating the receiver,
  then dropping `previous`.
- **TLS version.** Minimum TLS 1.2, 1.3 negotiated when the receiver offers it. Stdlib `crypto/tls`
  only; no cipher-suite list beyond the stdlib defaults.
- **Optional mutual TLS.** `WithClientCert` presents a client certificate and key, given as files or
  PEM from configuration or the environment, never written to the journal, the trail or a payload.
  The pair is re-read through `tls.Config.GetClientCertificate` when the files change (checked by
  modification time at most once a minute, and on a handshake failure), so a rotated or short-lived
  certificate (cert-manager, SPIFFE) is picked up without a restart; a read that fails keeps the
  last good pair and reports the error to `WithRecoverErrors`.
- **Receiver verification.** `WithServerCA` sets a CA bundle for a private receiver in place of the
  system roots (it does not add to them, so a private receiver cannot be impersonated by a public
  CA). `WithServerPin` pins the SHA-256 of the receiver certificate's SubjectPublicKeyInfo: the
  handshake fails unless a certificate in the verified chain matches, and a pin set may hold two
  hashes so the receiver can rotate. Pinning is in addition to normal verification, never instead of it.
- **mTLS is not a replacement for the HMAC signature**, which is always on:
  - Slack, PagerDuty and most webhook receivers do not accept client certificates; the signature
    works with every receiver that can read a header.
  - TLS ends at proxies, load balancers and gateways, so the receiving application often never sees
    the client identity; the signature travels with the payload to whatever finally reads it.
  - A signature can be verified later, over the stored body, as evidence; a handshake leaves
    nothing to check afterwards.

  mTLS answers "who may connect" and the signature answers "who sent this body". They compose.
- **Retries within a round.** The notifier core runs the round (up to 3 calls with its own backoff),
  so `Webhook.Notify` makes one request per call by default; `WithRequestsPerCall(n)` (up to 3) adds
  in-call retries with backoff (1 s, 2 s; jitter) for a caller without the core. `Retry-After` on a
  429 or 5xx is waited out before the call returns, capped at 60 s and never past the call's context
  deadline. Network errors, timeouts, 429 and 5xx are retried; any other 4xx is a permanent error for
  the round (`agent.ErrNotifyPermanent`; the outer backoff still tries again later, since a
  receiver's configuration can change).
- **Auth record.** `auth.secret_slot` is `current` (one secret), `current+previous` (signed with both during a rotation) or empty (unsigned, which the webhook never sends).
- **Proxy.** The environment's proxy applies unless `WithServerCA` or `WithServerPin` is set (then
  direct, unless `WithProxy` is given). A proxy only tunnels the TLS session.

### Request

`POST <url>`, body `application/json`:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `Bide-Delivery` | `Notification.ID`, identical on every attempt of an event and equal to the payload `id`; `notify.Verify` requires it to be present and equal to the signed `id`, since the signature covers the body, not headers |
| `Bide-Timestamp` | Unix seconds when this request was sent |
| `Bide-Signature` | `v1=<hex HMAC-SHA256(secret, Bide-Timestamp + "." + body)>`, one per secret, space separated |

The signature covers the timestamp, so a captured request cannot be replayed with a fresh one. A
receiver recomputes over the raw body, compares in constant time, and **rejects a timestamp older
than 5 minutes** (a receiver-side setting; the sender's docs recommend 5) to stop replay. The
delivery ID is the dedupe key.

### Payload, `bide.notification.v1`

Structured fields only. The payload never carries `Err.Error()` text, tool arguments, results, or
model output: error text can embed arguments, card or account numbers, or personal data, and a
webhook is a wider audience than the journal.

```json
{
  "schema": "bide.notification.v1",
  "id": "9f2c...e1",
  "key": "approval:orders-1042:call_01:0",
  "kind": "approval_pending",
  "run_id": "orders-1042",
  "event_at": "2026-10-13T09:14:05Z",
  "observed_at": "2026-10-13T09:14:07Z",
  "deadline_at": "2026-10-13T09:19:07Z",
  "attempt": 1,
  "summary": "Run orders-1042 is waiting for approval of tool refund (call call_01).",
  "step": { "op_kind": "tool", "op_id": "call_01", "tool": "refund" },
  "halt": null,
  "principal": { "on_behalf_of": "desk-7", "authority_ref": null },
  "approval": {
    "tool_use_id": "call_01",
    "tool": "refund",
    "policy": { "need": 2, "of": 3 },
    "approvers": ["alice", "bob", "carol"],
    "approved": 0
  },
  "abort": null,
  "view": { "command": "bide view -db /var/lib/orders/run.db -run orders-1042", "url": null }
}
```

- `kind` is one of `outcome_unknown`, `approval_pending`, `run_aborted`, `recovery_failing`,
  `notification_overdue`, `resolved`, `digest`. Exactly one of `halt` (`cause` as an enum,
  `attempted_at`), `approval`, `abort` (a reason code from a closed set, not text), `failing`
  (`passes`, a cause class) or `clears` (`id`, `key` and `kind` of the event a `resolved`
  notification ends; `reason` says why: `resolved`, `decided`, `ended` or `recovered`) is non-null
  where the kind has one; `digest` has `rows`. A `resolved` payload has no `deadline_at`, and its
  `event_at` is when a notifier found the event no longer current, an upper bound on when it ended.
- `summary` is generated from the structured fields, so a Slack workflow can post it without a
  template and without error text.
- **Minimal by default.** The fields above are everything: kind, run ID, step, tool name, approver
  list and the view pointer. Nothing that can hold cardholder or account data. Anything more is an
  explicit allowlist, `notify.WithFields(notify.ErrorText, notify.Args("refund"))`, named
  field by field, and a deployment under PCI-style rules should leave it empty and put detail behind
  the view link, where access control applies.
- New fields are additive within `v1`; receivers must ignore unknown fields. A breaking change is `v2`.

### No action links; who acts

- **The notification carries a view pointer only.** The `bide view` command, and a `url` only when
  the deployment sets a base URL for its viewer. Never an approve or resolve link. A link that
  decides on a click is a phishing target (an attacker who can forge or replay a message gets a
  one-click approval), and it would bypass the point of approvals here: a decision is signed with
  the approver's key and counted by the gate. Acting from the notification is a non-goal.
- **Separation of duties.** The payload carries what a router needs: the gate's approver IDs and
  its policy, `k` of `n`, and the run's principal (`principal.on_behalf_of`, `authority_ref`; null
  when the run has none). bide does know the principal: `run:start` journals it
  (`agent.Principal`, `agent/runstart.go`) and restores it on every drive. It is the party the agent
  acts for (a desk, an account, a user), which is not always the person who asked, so a receiver
  routes *away from* the principal as one input and keeps its own rule for who the requester is.
  The approvers are then asked by routing, which is advisory. What bide enforces today is that the
  gate counts a decision only from a listed approver with a verifying signature, one seat per key.
  It does **not** today refuse a decision because the approver is the run's principal: neither
  `SubmitDecision`, `TallyApprovals` nor `VerifyApprovals` reads the principal (checked in
  `agent/approval.go` and `audit/approval.go`), and `OnBehalfOf` is a free-form string with no
  defined mapping to an approver ID. Open question 12 covers closing that.

## Fit with the existing pieces

- **Typed errors.** Unchanged. `Run` still returns `*OutcomeUnknown` and `*ApprovalPending`; the
  notifier is an additional consumer.
- **`WithRecoverErrors`.** Still receives genuine failures only (halts and gates never went to it), and now also "notification failing" through `WithNotifyErrors`.
- **`MemWaker`.** A timer wakeup that resumes into a halt or a gate ends in the same drive, so it
  notifies without a separate `onError` wiring.
- **Leases and multiple processes.** The process holding the run's lease when it observes the event
  usually delivers, which keeps two recovery processes from both trying. Correctness does not depend
  on it: the claim in the trail does, via the append race at `seq`. Without a `Leaser`, or when a
  lease lapses mid-POST, two processes can deliver; that is one duplicate per lapse. Configure the
  notifier, and a durable shared `EventStore`, on every process that drives runs.
- **Run viewer.** "Needs attention" is derived from the journal (halted, awaiting approval), not
  from the trail, so it is correct with no notifier. With a trail it adds the notified, acknowledged
  and overdue columns above.

## Protocol impact

1. **The run journal protocol is unchanged.** No new reserved prefix, no new journal record, no
   change to the claim protocol, the tool-call state machine, the attempt rules, the ended-run rule
   or `Replay`.
2. **A new audited trail.** New leaf kinds (`observed`, `claimed`, `acked`, `failing`, `overdue`,
   `cleared`) in the salted event-leaf format, a `notify:<run>` trail name, `audit.ErrSeqTaken`, the
   `agent.NotifyTrail` port and `audit.NewNotifyTrail`. `bide version` lists the artifact formats
   `bide audit` reads; the new kinds are added there, and a verifier from before them must reject a
   trail it cannot read rather than skip leaves.
3. **Nothing in the run reads the trail.** The lifecycle never consults it, which is why the
   lifecycle model does not change.
4. **Redaction.** A trail holds run IDs, tool names and approver IDs, not arguments, so redacting a
   journal record does not reach it and needs no trail redaction by default; with an allowlist field
   enabled, the deployment treats the trail like any event trail (the `EventStore` redaction note
   in `docs/guides/audit.md`).

### Does the TLA+ lifecycle model need an update?

No. Model 10 (`spec/tla/lifecycle`) is unchanged, because the notify trail is a separate stream
that no lifecycle action reads or writes. A new small model, "14: notifications" (`spec/tla/notify`; 13 is the probes model), composes the
lifecycle's halt, pause and end states as an input with the trail protocol. It is TLC-only (a
claim-and-append protocol over one trail; no need for Apalache) and checks, within stated bounds
(processes, rounds, crash and ambiguous-append placements, claim lapse, a sink that may fail):

1. **No silent drop.** Under fairness for a recovering or sweeping process and a sink that
   eventually accepts, every event that stays current reaches `acked`; with a sink that never
   accepts, `failing` is raised and the event stays open.
2. **Duplicates bounded.** Deliveries of one key never exceed 1 + the crashes, claim lapses and lost
   replies in the history, and equal 1 in a history with none. The model states at-least-once; it
   must not be read as exactly once.
3. **No concurrent double while a claim is live.** No two processes are in flight for one key while
   one `claimed.until` is in the future; and of two appends at one `seq`, exactly one wins.
4. **Overdue is raised by the deadline.** An event with no `acked` past `deadline_at` gets
   `overdue` in a bounded number of rounds, whatever the sink does.
5. **A crash loop still alerts.** A run that fails every recovery pass for longer than `D`, under
   any number of process crashes and restarts (each restart losing all memory), gets a
   `recovery_failing` event observed within a bounded number of rounds, and a successful pass
   clears the episode. This is a case of no silent drop, listed apart because a counter held in
   memory would miss it.
6. **No interference with the run lifecycle.** Every Model 10 invariant holds with the notify actions
   interleaved, and the lifecycle's state space (projected on its own variables) is unchanged.
7. **Final records.** Once a key is `acked` or `cleared` nothing more is appended for it, and
   `observed`, `overdue`, `failing`, `acked` and `cleared` are each appended at most once per key.
8. **The all-clear** (`Clears`, key `r_<event>`). None is observed or delivered for an event that
   still stands; none for an event never claimed; none before the event is acknowledged or cleared; it
   is never overdue; and under fairness, with a sink that accepts in the end, every event that stopped
   standing after a delivery was attempted gets its all-clear acknowledged. It is not delivered
   after the event: an event cleared with a request in flight is never acknowledged, and its
   all-clear is (`limits/clear-before-event`).

Stale suppression is stated as what holds (no round starts for an event already `cleared`) and what
does not (the race between the currency check and the send).

## Alternatives considered

- **Document the hooks only (the recipe).** Shipped as the stopgap. It cannot survive a restart or
  a crash between the event and the POST, and it leaves no evidence.
- **At-most-once via the attempt marker.** Rejected above.
- **Delivery records in the run journal under a `notify:` prefix.** Considered in the first draft.
  Rejected: it touches the journal protocol, needs an exception to the ended-run rule for
  `run_aborted`, adds records Replay and the lifecycle model must learn to ignore, and keeps the
  evidence in a store whose records a journal GC may remove. The audited trail has its own lifecycle
  and is already meant to outlive the journal.
- **A side table or state at the sink.** A second atomicity story with no audit; the `EventStore`
  already gives append-if-absent with a verifiable log.
- **Built-in Slack, PagerDuty, email.** Rejected for the core: each is a dependency, a credential
  model and a changing API. A user points the webhook at them (a Slack incoming webhook, PagerDuty's
  Events API through a small relay, an email gateway); the shipped relay (`bide notify relay`, package
  `notify/relay`) is such a relay, outside the core; a community notifier can implement the
  one-method interface.
- **Notify from `RecoverLoop` only.** A run driven by a plain `Run` call, a timer or `Recover` would
  not notify, and an app without a loop would have no path; hence the drive-end emission and
  `NotifyPending`.
- **Exactly once to the sink.** Not achievable without a sink-side contract; offered as the
  delivery ID a sink may honor.

## Non-goals

- **Acting from the notification** (approve or resolve from Slack). It needs authenticated
  callbacks, the approver key story and the m-of-n signature path; a later step. That inbound
  direction is where mutual TLS matters most, since it would authenticate the system calling bide
  rather than the system bide calls; this note's mTLS support is the outbound half only.
- **Reminder policy** beyond the key shape that admits it (`r` in the approval key) and the one-shot
  overdue event. Escalation is a bounded chain of receivers (see "Routing and escalation"), not a
  schedule of repeats.
- **Routing rules beyond kind and approver** (which team gets which tool, by amount, by principal). A
  route is a kind or an approver ID; anything finer is done in the receiver.
- **Proving a person read it.** `acked` is the endpoint's acceptance.
- **Notifying about ordinary tool errors or a plain error returned from `Run`.** The caller has
  those.

## Open questions

1. **Duration for `recovery_failing`.** Recommend `D = 15 minutes` of continuous failure, configurable,
   measured from the durable `failing_since` record. It costs two records per failure episode
   (`failing_since`, `recovered`), nothing per failed pass.
2. **Notify deadline default.** Recommend 5 minutes, configurable per agent and per event kind
   (approvals usually tighter than halts that wait on a provider).
3. **Where `notification_overdue` goes.** Recommend the same sink by default, and an optional second
   sink, because the usual cause of an overdue event is the first sink being down.
4. **The ErrSeqTaken conformance test.** Recommend adding it to the `EventStore` contract and its
   documented Postgres shape before the notifier, since the claim protocol depends on it.
5. **Claim per round or lease-backed.** The design writes one `claimed` record per round (about four
   an hour at the backoff cap). A claim held through the `Leaser` writes none, but then needs a lease
   key that `ReapLeases` does not reap (it deletes lapsed leases for runs the store has no entry for,
   and for run IDs containing `>`), so a pseudo run ID needs checking. Recommend the record per
   round for v1: it needs no change to `Leaser`.
6. **Package placement.** Recommend `notify` in the core module (stdlib only) so the recipe's
   `agent.WithNotifier(notify.NewWebhook(...), trail)` is one import, and the adapter in `audit`.
7. **Delivery inline or in a goroutine.** Recommend the agent-owned goroutine, flushed on `Close`:
   `Run` must not wait on a slow sink. State in the guide that the guarantee then needs `RecoverLoop`
   or `NotifyPending`.
8. **`run_aborted` and plain failures.** Recommend v1 covers only the journaled terminal failure
   (`run:aborted`) and `recovery_failing`, and revisit if a general terminal-failure record appears.
9. **Digest scope.** Recommend v1 ships the digest as optional and off by default, over runs the
   sweep sees, and leaves per-tenant digests to the receiver.
10. **mTLS scope in v1.** Recommend shipping the client certificate with reload, the custom CA and
    the SPKI pin together, since each is small over `crypto/tls`, and leaving certificate revocation
    checking to the receiver's side. Inbound mTLS waits for the act-from-Slack step.
11. **When to build it.** Recommend building after the user approves this note, in the order of the
    build plan below.
12. **Refuse an approval whose approver is the run's principal.** bide can: the principal is in
    `run:start`, so `SubmitDecision` can read it, `TallyApprovals` (a pure function) can take it as
    an input beside the policy and never count a matching approver, and `VerifyApprovals` recounts
    with the same function from the run's evidence. The obstacles are that `OnBehalfOf` is free-form
    and not an approver ID, and that the principal may be the customer rather than the requester.
    Recommend an opt-in policy field (`ExcludePrincipal`, comparing `OnBehalfOf` to an approver ID
    exactly), enforced at count time and at verify time, with a decision refused early at
    `SubmitDecision` as feedback only (as `WithDecisionCheck` already is). It changes what the
    approval tally and evidence record and needs an update to the approval-gate model (Model 1b), so
    recommend it as its own change, not part of the notifier.

## Build plan

No durable `EventStore` exists today. `audit` defines the interface and ships only `MemEventStore`;
`store/sqlite` and `store/postgres` implement `agent.Store`, not `EventStore`. Sizes are rough
(S under a day, M one to three days, L about a week) for one engineer, excluding review.

| # | Item | Size | Depends on | Notes |
|---|---|---|---|---|
| 1 | `audit.ErrSeqTaken` contract and a store conformance suite | S | none | the claim protocol rests on it; ships first |
| 2a | SQLite `EventStore` (`store/sqlite`) | M | 1 | `UNIQUE(run_id, seq)`; passes the suite |
| 2b | Postgres `EventStore` (`store/postgres`) | M | 1 | same schema shape; multi-process test as in `ha_multiproc_test.go` |
| 3 | `agent.NotifyTrail` port, `audit.NewNotifyTrail` adapter, leaf kinds, fold | M | 1 (interface) | pure code; the fold is unit and fuzz tested |
| 4 | Notifier core: events at drive end, observe, claim, backoff, `failing`, `overdue`, `recovery_failing`, delivery goroutine | L | 3 | the crash sweep and DST cases from the existing harnesses |
| 5 | Webhook with HMAC, rotation, HTTPS-only, mTLS and reload | M | the `Notifier` interface only | can start at once |
| 6 | `NotifyPending` sweep | S | 4 | |
| 7 | `audit.VerifyNotifications` and the `bide audit notifications` verb (shipped) | M | 3 | verifier in `audit` can start early; it reads the trail strictly (refuses a record kind it cannot read, unlike the engine's `Load`); the verb is a `cmd/bide` change |
| 8 | Viewer "Needs attention": notified, acknowledged, overdue | M | 3 | reads the trail; `internal/runview` change |
| 9 | Model 14 (TLC) and its CI shard | M | design approved | starts at once; must land before 4 merges, like the other models |
| 10 | Guide, recipe update, CHANGELOG, i18n, snippets | S | 4, 5 | docs current with the code |
| 11 | Principal exclusion (open question 12) | M | none | independent of the notifier; includes Model 1b |

Order and parallelism:

- **First:** 1, then 2a and 2b in parallel with 3. Items 5 and 9 can start the day the note is
  approved.
- **Critical path:** 1, 3, 4, 6, 10: about two weeks. Items 2a, 2b, 5, 7, 8, 9 and 11 run beside it.
- **Gates:** 2a or 2b (one durable store, the one the deployment uses) before 4 can be called
  crash-safe in production, and 9 before 4 merges. The adversarial review that touches journals,
  leases and proofs applies to 3, 4 and 9.
