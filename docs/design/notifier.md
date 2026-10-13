# Design: tell a human when a run needs them

Status: proposal (draft, not implemented). It adds a small `Notifier` seam, a signed webhook
implementation, and an audited delivery trail that gives notify-once across crashes and makes the
notification itself evidence. Until it ships, the recipe "Get notified when a run needs a human"
wires the existing hooks to a webhook with an in-process dedupe; this note is the crash-safe,
auditable version of that recipe.

## Why

bide halts instead of guessing when a side effect's outcome is unknown (`*OutcomeUnknown`, cleared
with `ResolveHalt`) and pauses at approval gates (`*ApprovalPending`, answered with
`SubmitDecision`). Both stop a run until a person acts. Nothing built in tells the person. The
signals exist but are scattered:

- `Run` (and the typed and plan-flow equivalents) returns the typed error to its caller.
- `RecoverLoop` hands errors to `WithRecoverErrors`, and a halted run re-driven on every pass
  reports again each pass.
- `MemWaker.Start(ctx, every, onError)` reports resume errors from timer wakeups.

A caller can build an alert from these, and the recipe shows how. It cannot make the alert
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
| `outcome_unknown` | a drive ends with `*OutcomeUnknown` | `halted:<run>:<op kind>:<op id>:<attempt n>` |
| `approval_pending` | a drive ends with `*ApprovalPending` | `approval:<run>:<call id>:<r>` |
| `run_aborted` | the run reaches the `run:aborted` end marker | `aborted:<run>` |
| `recovery_failing` | a run fails `N` consecutive recovery passes (default 5): a Resumer error, not resumable, or store errors | `recovery:<run>:<k>` |
| `notification_overdue` | an event is not acknowledged by its deadline (below) | `overdue:<event key>` |
| `digest` | a periodic summary of runs halted or waiting longer than `X` (below) | `digest:<bucket>` |

Ids are encoded as attempt keys encode them.

- A re-halt after `ResolveHalt` runs the call again and reaches a new attempt number `n`, so it is
  a new event.
- `r` in an approval key is the reminder index, always `0` in v1. One notification per gate, and
  an m-of-n gate gaining a decision is the same wait, not a new event. A later "still waiting after
  X" reminder is `r = 1, 2, ...` with the same record kinds and the same fold, so it needs no
  protocol change.
- `k` in a recovery key counts earlier `recovery_failing` events for the run that were cleared by a
  successful pass. The consecutive-failure counter is in memory, so a restart restarts the count
  toward `N`; an event, once observed, is durable.
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

// NotifyPending runs one catch-up sweep: for every run the store lists as halted or paused (and
// aborted or failing, where known) it observes the event, claims it, delivers it, and raises
// overdue. Call it at startup and on a timer.
func NotifyPending(ctx context.Context, j *Journal, n Notifier, trail NotifyTrail) error
```

`WithNotifier` takes the trail: the crash-safe guarantee needs it, so a nil trail is `ErrConfig`.
Without a trail there is no notify-once; that is the recipe's in-process path, not a degraded mode
of this API.

### Where events are emitted, and the guarantee's two paths

Emission is at the end of a drive, where the engine has just decided a run stopped for a typed
reason. That single point is shared by a caller's `Run`, `Recover`, a `MemWaker` wakeup and
`RecoverLoop`, so no path needs its own wiring. The drive hands the event to a delivery goroutine
the agent owns (flushed on `Close`, bounded wait), so a slow sink never delays `Run` returning the
typed error.

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
| `acked` | after the sink acknowledged | `key`, `first_sent_at`, `acked_at`, `attempts`, `status` |
| `failing` | once, after 8 failed attempts | `key`, `failures`, `at` |
| `overdue` | once, when the deadline passes unacked | `key`, `at`, `latency` |
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
   reload, fold, and stop if its claim is live.
5. Deliver (one round: up to 3 requests with backoff, below). On success append `acked`. On
   failure, count it; after the 8th total failure append `failing` once; wait out the backoff and
   begin a new round at step 1.

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
`ID`. An append failure after a success is retried in place before the attempt is abandoned,
because that window is cheap to narrow and the duplicate it avoids is real.

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

It reports "approvers were notified at T" with the T1, T2, T3 above.

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
  its policy, `k` of `n`. bide does not know who requested a run unless the application says, so
  *keeping a requester from being asked to approve their own request is the receiver's job*, using
  these fields. The enforcement that matters is not in the message: the gate counts a decision only
  from a listed approver with a verifying signature, one seat per key.

## Fit with the existing pieces

- **Typed errors.** Unchanged. `Run` still returns `*OutcomeUnknown` and `*ApprovalPending`; the
  notifier is an additional consumer.
- **`WithRecoverErrors`.** Still receives genuine failures, and now also "notification failing".
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
that no lifecycle action reads or writes. A new small model, "13: notifications", composes the
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
5. **No interference with the run lifecycle.** Every Model 10 invariant holds with the notify actions
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
  callbacks, the approver key story and the m-of-n signature path; a later step.
- **Reminder and escalation policy** beyond the key shape that admits it (`r` in the approval key)
  and the one-shot overdue event.
- **Routing rules** (which team gets which tool). One notifier per agent; fan out in the receiver,
  or compose `Notifier` values.
- **Proving a person read it.** `acked` is the endpoint's acceptance.
- **Notifying about ordinary tool errors or a plain error returned from `Run`.** The caller has
  those.

## Open questions

1. **Failure threshold for `recovery_failing`.** Recommend `N = 5` consecutive failed passes,
   configurable, counted in memory (a restart restarts the count). A durable count would put a
   record per failed pass in the trail, which is the growth this design avoids.
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
10. **When to build it.** Recommend building after the user approves this note, in this order: the
    `ErrSeqTaken` contract, the trail port and adapter, the model, then the webhook and the sweep.
