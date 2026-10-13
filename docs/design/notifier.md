# Design: tell a human when a run needs them

Status: approved, in progress. Build plan item 3 (the trail port, records, fold and `EventStore`
adapter) has shipped, item 4 (the notifier core: events at the end of a drive, claim and delivery,
overdue, `recovery_failing`) is in review, and item 6 (`NotifyPending`) is built on it. It adds a
small `Notifier` seam, a signed webhook implementation, and an audited delivery trail that gives
notify-once across crashes and makes the notification itself evidence. Until it ships, the recipe "Get notified when a run needs a human"
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
| `approval_pending` | a drive ends with `*ApprovalPending` | `approval:<run>:<call id>:<r>` |
| `run_aborted` | the run reaches the `run:aborted` end marker | `aborted:<run>` |
| `recovery_failing` | a run has kept failing recovery for longer than `D` (default 15 minutes): a Resumer error, not resumable, or store errors | `recovery:<run>:<k>` |
| `notification_overdue` | an event is not acknowledged by its deadline (below) | `overdue:<event key>` |
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
| `observed` | once per event, first process to see it | `key`, `kind`, `event_at`, `observed_at`, `deadline_at` |
| `claimed` | at the start of each delivery round | `key`, `holder`, `at`, `until` |
| `acked` | after the sink acknowledged | `key`, `first_sent_at`, `acked_at`, `attempts`, `status`, `auth` |
| `failing` | once, when 3 rounds (8 attempts) have claimed the event without acknowledging it | `key`, `failures` (rounds), `at` |
| `overdue` | once, when the deadline passes unacked | `key`, `at`, `latency` |
| `failing_since` | a recovery pass failed and no episode is open for the run | `run`, `at`, `cause` (a class, not text) |
| `recovered` | a recovery pass succeeded while an episode is open | `run`, `at` |
| `cleared` | the event stopped being current | `key`, `at`, `reason` (`resolved`, `decided`, `ended`) |

The fold of an event: **open** until `acked` or `cleared`; **claimed** if the last `claimed` has
`until` in the future.

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
  it under a short claim (at most 2 minutes) of its own on the notice's key, so peers do not send it
  together; a holder that is only slow may send it too, one duplicate the notice's stable ID lets the
  sink drop. A holder that claimed after the event was overdue is left to send it, so the common case
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
  for a plan flow's gate, the node's own result (`node:<name>`). The sweep applies the same test.
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

**`bide audit`.** A verifier `audit.VerifyNotifications(trail, tree head, run evidence)` and a
`bide audit` verb that runs it (the CLI change belongs to the code PR). It checks:

- the trail's leaves are contiguous and committed under the supplied (optionally signed) tree head,
  and consistent with an earlier head (`ProveConsistency`), so nothing was dropped or rewritten;
- for every approval gate in the run's approval evidence, the trail holds `observed` for that gate;
- `acked_at - observed_at` is within the notify deadline, or an `overdue` record exists for it;
- no `acked` without an earlier `observed`; no `acked` for a key `cleared`.

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
- **Retries within a round.** Up to 3 requests with backoff (1 s, 2 s; jitter), `Retry-After`
  honored on 429. Network errors, timeouts, 429 and 5xx retry; any other 4xx is a permanent error for
  the round (the outer backoff still tries again later, since a receiver's configuration can change).

### Request

`POST <url>`, body `application/json`:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `Bide-Delivery` | `Notification.ID`, identical on every attempt of an event |
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
  "view": { "command": "bide view -run orders-1042", "url": null }
}
```

- `kind` is one of `outcome_unknown`, `approval_pending`, `run_aborted`, `recovery_failing`,
  `notification_overdue`, `digest`. Exactly one of `halt` (`cause` as an enum, `attempted_at`),
  `approval`, `abort` (a reason code from a closed set, not text) or `failing` (`passes`, a cause
  class) is non-null where the kind has one; `digest` has `rows`.
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
  Events API through a small relay, an email gateway); a community notifier can implement the
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
- **Reminder and escalation policy** beyond the key shape that admits it (`r` in the approval key)
  and the one-shot overdue event.
- **Routing rules** (which team gets which tool). One notifier per agent; fan out in the receiver,
  or compose `Notifier` values.
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
| 7 | `audit.VerifyNotifications` and the `bide audit` verb | M | 3 | verifier in `audit` can start early; it reads the trail strictly (refuses a record kind it cannot read, unlike the engine's `Load`); the verb is a `cmd/bide` change |
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
