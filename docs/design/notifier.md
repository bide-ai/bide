# Design: tell a human when a run needs them

Status: proposal (draft, not implemented, not scheduled). It adds a small `Notifier` seam and a
signed webhook implementation, and reuses the journal's insert-if-absent records for notify-once.
Until it ships, the recipe "Get notified when a run needs a human" wires the existing hooks to a
webhook with an in-process dedupe; this note is the crash-safe version of that recipe.

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
in the caller's memory, not in the journal. The failure a halt exists to prevent is a silent
stall, so a missed notification is the expensive case; a duplicate is not.

Goal: a human is told, with an explicit and bounded duplicate rate and no silent drops, when a run
halts on an unknown outcome, waits for approval, or is aborted.

Not the same as [durable signals](design-durable-signals.md), whose dropped stage 4 was a push
`Notifier` for *waking runs*. This is for alerting people, and it wakes nothing.

## Terms

- **Event**: a state a run enters that needs a person: `outcome_unknown`, `approval_pending`,
  `run_aborted`. Each has a stable **event key** (below).
- **Notification**: one delivery of an event to a sink. An event may take several delivery
  attempts.
- **Sink**: where a notification goes (a webhook URL, in the shipped implementation).

## What "once" can mean

For a side effect on the world, bide's rule is at most once: a crash between the attempt marker
and the result halts the run, because repeating the effect is the worse error. A notification
inverts the cost:

| Semantics | Crash or lost ack in the window | Cost |
|---|---|---|
| At most once (attempt marker, halt on unknown) | Notification is dropped, and the run now needs a human for the notifier itself | A missed halt, the thing we are fixing |
| At least once (record success after the ack) | The next pass delivers again | A duplicate message |

**Recommendation: at least once, with a stable delivery ID so a receiver can drop duplicates.**
The claim the docs may make is "no event is dropped while some process keeps recovering, and a
duplicate happens only after a crash or a lost acknowledgement in a delivery window". The docs must
not say "exactly once": no sender can promise that to a receiver that can lose its reply. The
delivery ID gives a receiver that stores it exactly-once handling; Slack incoming webhooks do not
dedupe, so a Slack sink sees the duplicates.

A notification must also never be run as a `Step` side effect of the run it reports: an at-most-once
Step that crashes mid-POST would halt that run on the notifier, a halt about a halt.

## API

In package `agent`, stdlib only:

<!-- docsnip: skip proposed API, not implemented -->
```go
// Notifier delivers one notification. Notify must return nil only once the sink has accepted it.
// It may be called again for the same Key after an error, a crash or a lost reply, so a sink that
// can dedupe should use Key (or Notification.ID) to do so.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

type NotificationKind string

const (
	NotifyOutcomeUnknown NotificationKind = "outcome_unknown"
	NotifyApprovalPending NotificationKind = "approval_pending"
	NotifyRunAborted     NotificationKind = "run_aborted"
)

type Notification struct {
	ID       string           // hex SHA-256 of Key; stable across attempts, processes and restarts
	Key      string           // the event key
	Kind     NotificationKind
	RunID    string
	At       time.Time        // when the event was first observed by this process
	Attempt  int              // delivery attempt number, from 1
	Err      error            // the typed error: *OutcomeUnknown, *ApprovalPending, or the abort error
}

func WithNotifier(n Notifier) Option          // on Agent (Build) and on a plan Flow
func WithNotifier(n Notifier) RecoverLoopOption
```

`Err` carries the typed error so an in-process notifier can `errors.As` for detail (the halted
`OpRef`, the approval `ToolName`, the quorum tally) instead of a second parsing layer.

### Where events are emitted

One place: the end of a drive, where the engine has just decided a run stopped for a typed
reason. That is the single point every path passes through (a caller's `Run`, `Recover`, a
`MemWaker` wakeup, `RecoverLoop`), so emission needs no per-path wiring and cannot be forgotten by
one of them. The drive does not wait for delivery: it hands the event to a delivery goroutine the
agent owns (flushed on `Close`, with a bounded wait), so a slow sink never delays `Run` returning
the typed error that already tells the caller.

`RecoverLoop`'s `WithNotifier` covers a loop whose `Resumer` is a bare function with no agent behind
it. It is also the catch-up path: each pass re-drives a halted run, re-observes the event, and
finds the notify record missing or unfinished, which is what makes a crash between "event" and
"delivery" self-healing.

### Event keys

Keys are built from the facts that make the event new, encoded the way attempt keys encode ids:

| Event | Key | New when |
|---|---|---|
| Unknown outcome | `halted:<run>:<kind>:<op id>:<attempt n>` | a re-halt after `ResolveHalt` runs the call again and reaches a new attempt number |
| Approval pending | `approval:<run>:<call id>` | one per gate; for a plan flow node, the node key stands in for the call id |
| Run aborted | `aborted:<run>` | once per run |

An m-of-n gate that gains a decision but still waits does **not** produce a new event: it is the
same wait. (Open question 2.)

## Notify once, across crashes

The records live in the run's own journal under a new reserved prefix `notify:` and use only the
store's existing insert-if-absent, so cross-process dedup is the same PK/ON CONFLICT mechanism the
claim protocol uses. No new `Store` method.

```
notify:claim:<key>:<n>   n-th delivery claim; body { holder, at, deadline }
notify:done:<key>        written after the sink acknowledged; body { attempt, at }
```

Delivery of event `key`:

1. If `notify:done:<key>` exists: stop.
2. If the event is no longer current (the halt was resolved, the decision was submitted, the run
   ended otherwise), stop and record nothing. A stale alert is worse than none.
3. Insert `notify:claim:<key>:<n>` for the next `n`. If it already exists, another process or an
   earlier life of this one holds it: stop if its `deadline` has not passed; otherwise take
   `n+1` (a crashed deliverer's claim lapses).
4. Call `Notify` with the per-attempt timeout.
5. On a nil return, insert `notify:done:<key>`. On an error, leave the claim; the next pass tries
   `n+1` after backoff.

The ordering is the whole guarantee. `done` is written only after the ack, so a crash anywhere
before step 5 leaves the event undelivered-as-far-as-the-journal-knows and the next pass sends it
again: at least once. A crash after the POST succeeded and before `done` is the duplicate window.

**Lost acknowledgement.** The sink accepted and the reply was lost, or the store failed the `done`
insert after the sink accepted. Both look like a failed attempt, so the next attempt resends with
the same `ID`. A store error on the `done` insert is retried in place before giving up the attempt,
because that window is cheap to narrow and the duplicate it avoids is real.

**Attempt cap.** After `N` claims (default 8, spread over passes with capped exponential backoff)
the engine stops retrying, writes no `done`, and reports the failure to `WithRecoverErrors`. The
event remains visible as "not notified" (see the viewer below) so the failure is not silent.

**Who notifies.** The process that holds the run's lease when it observes the event delivers;
that already keeps two recovery processes from driving the same run. Leases can lapse mid-POST,
and deployments can run without a `Leaser`, so the claim record is the correctness mechanism and
the lease is the usual-case optimization: with a correct lease and no crash there is one attempt.
When a lease lapses during a POST and a second process claims after the first claim's `deadline`,
both may deliver; that is a duplicate, bounded by one per lapse, and the model (below) checks that
bound.

## The shipped webhook notifier

Package `notify` in the core module (`net/http`, `crypto/hmac`, `encoding/json`; no new
dependency):

<!-- docsnip: skip proposed API, not implemented -->
```go
n := notify.NewWebhook(url,
	notify.WithSecret(secret),            // enables the signature header
	notify.WithTimeout(10*time.Second),   // per attempt
	notify.WithHTTPClient(client),
)
agent.WithNotifier(n)
```

One `Notify` call makes up to 3 requests with backoff (1 s, 2 s, then fail; jitter) and honors
`Retry-After` on 429. It retries a network error, a timeout, 429 and 5xx; any other 4xx is a
permanent error returned at once, since resending a rejected body cannot succeed. Redirects are not
followed (a signed body must not go somewhere the operator did not name). The outer attempt cap
above is what bounds total effort.

### Request

`POST <url>`, body `application/json`, with:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `Bide-Delivery` | `Notification.ID`, identical on every attempt of an event |
| `Bide-Timestamp` | Unix seconds when this attempt was sent |
| `Bide-Signature` | `v1=` + hex of HMAC-SHA256(secret, `Bide-Timestamp` + `.` + body), present only with a secret |

A receiver recomputes the signature over the raw body, compares in constant time, and rejects a
timestamp more than five minutes old. The delivery ID is the dedupe key.

### Payload, `bide.notification.v1`

```json
{
  "schema": "bide.notification.v1",
  "id": "9f2c...e1",
  "key": "halted:orders-1042:tool:call_01:1",
  "kind": "outcome_unknown",
  "run_id": "orders-1042",
  "observed_at": "2026-10-13T09:14:07Z",
  "attempt": 1,
  "summary": "Run orders-1042 halted: the outcome of tool refund (call call_01) is unknown.",
  "halt": { "op_kind": "tool", "op_id": "call_01", "tool": "refund",
            "cause": "crashed", "attempted_at": "2026-10-13T09:13:58Z" },
  "approval": null,
  "abort": null,
  "hint": { "view": "bide view -run orders-1042",
            "action": "Check the provider, then resolve with agent.ResolveHalt." }
}
```

- `kind` is one of `outcome_unknown`, `approval_pending`, `run_aborted`. Exactly one of the
  `halt`, `approval` and `abort` objects is non-null, matching `kind`.
- `approval`: `{ "tool_use_id", "tool", "need", "approved", "approvers" }`; the quorum fields are
  omitted for a 1-of-1 gate. `abort`: `{ "error": "<string>" }`.
- `summary` is a ready-to-post line, so a Slack workflow can use it without a template.
- **Arguments are not included.** Tool arguments can hold secrets and personal data, and a webhook
  is a wider audience than the journal. `WithArgs(true)` adds `halt.args` / `approval.args`
  for a deployment that decides otherwise.
- New fields are additive within `v1`; a receiver must ignore unknown fields. A breaking change is
  `v2`.

Run IDs are not secret, but they carry a tenant prefix in some deployments; the redaction option
is a `WithRunIDFunc` hook, deferred (non-goal below).

## Fit with the existing pieces

- **Typed errors.** Unchanged. `Run` still returns `*OutcomeUnknown` / `*ApprovalPending`; the
  notifier is an additional consumer, not a replacement for handling them.
- **`WithRecoverErrors`.** Still receives genuine failures. It also receives "notification gave
  up after N attempts". It no longer needs to dedupe halts for alerting, though a halted run
  re-driven each pass still reports there as today.
- **`MemWaker`.** A timer wakeup that resumes into a halt or a gate goes through the same drive
  epilogue, so it notifies without a separate `onError` wiring.
- **Leases and multiple processes.** As above: lease for who, claim records for correctness.
  Any process with the notifier configured can deliver; a fleet where only some processes have the
  sink configured notifies only when one of those holds the run. Recommended: configure it on
  whatever runs `RecoverLoop`.
- **Run viewer, "Needs attention".** The list is derived from the journal (halted, awaiting
  approval), not from notification state, so it stays correct with no notifier. It can add one
  column read from `notify:done` / `notify:claim`: "notified at ..." or "not notified" (a failed
  or unconfigured sink). The viewer does not send anything.

## Protocol impact

1. **New reserved prefix `notify:`.** Add it to `reservedPrefixes` in `agent/keys.go`,
   `IsReservedStepName`, the key table in `docs/design/protocol.md`, and the test that asserts
   every engine key starts with a listed prefix.
2. **No change to the claim protocol, the tool-call state machine or the attempt rules.** `notify:`
   records are not attempt markers, are never read by `ResolveHalt`, and play no part in deciding
   whether a run is started, halted, paused or ended. Lifecycle derivation must ignore them; a
   test reads a run's status with and without `notify:` records and asserts it is equal.
3. **Terminal runs.** `run_aborted` is delivered after the run has ended, and a late `done` record
   must still be writable. If the journal refuses appends to an ended run, `notify:` needs an
   exemption (as `run:cancel-requested` has its own rules). To be checked against the ended-run
   rule before implementation. (Open question 3.)
4. **Audit.** `notify:` records are not leaves of the audit chain: they are delivery bookkeeping,
   not a run effect, and including them would make evidence depend on whether a webhook was up.
5. **Older readers.** A journal with `notify:` records read by an older version must list and
   replay unchanged. The reservation applies on write, so this should hold, and the compatibility
   test (an older reader on a newer journal) must cover it.

### Does the TLA+ lifecycle model need an update?

Model 10 (`spec/tla/lifecycle`) does not change: notification does not alter the lifecycle, and
that non-interference is the property to preserve, not to re-prove inside Model 10. A new small
model, "13: notifications", would compose the lifecycle's halt, pause and end states with a
notify protocol. A check would need to show, within stated bounds (processes, claims, crash and
ambiguous-write placements, claim `deadline` lapse):

1. **No silent drop (bounded liveness).** Under fairness for the recovering process and a sink that
   eventually accepts, every event that stays current has a `done` record. Without the sink
   assumption, an event is either delivered or the attempt cap is reached and reported.
2. **Bounded duplicates.** Deliveries of one key never exceed 1 + the number of crashes and
   ambiguous replies in the history, and equal 1 in a history with none. This is the exact
   statement of at-least-once; the model must not claim exactly once.
3. **No concurrent double with a live claim.** Two processes are never both in flight for one key
   while a claim's `deadline` has not passed.
4. **Stale suppression.** No attempt starts for an event whose halt or gate was already resolved
   before the claim.
5. **Non-interference.** Adding the notify actions to the lifecycle model leaves every Model 10
   invariant (and the existing configurations' state counts for the lifecycle variables) intact.
6. **Idempotence of `done`.** A duplicate `done` insert, or `done` after a later claim, leaves one
   record and no regression to "undelivered".

TLC is enough; Apalache is not needed for the small claim-and-done protocol. The lead asks the user
before the model is built or a stronger checker is chosen.

## Alternatives considered

- **Document the hooks only (the recipe).** Shipped as the stopgap. It cannot survive a restart
  or a crash between the event and the POST, which is the property asked for.
- **At-most-once via the attempt marker.** Rejected above: it turns a missed notification into a
  halt that itself needs a human.
- **Built-in Slack, PagerDuty, email.** Rejected for the core: each is a dependency, a credential
  model and an API that changes. A user points the webhook at them (Slack incoming webhook,
  PagerDuty Events API via a small relay, an email gateway). A community `notifier` can implement
  the one-method interface.
- **Notify from `RecoverLoop` only.** Simpler, but a run driven by a plain `Run` call, a timer or
  `Recover` would not notify, and a pause that no loop sees would be silent.
- **Record state outside the journal (a side table or the sink).** A second store means a second
  atomicity story; the journal already gives insert-if-absent next to the event it describes.
- **Exactly-once to the sink.** Not achievable without a sink-side contract; offered as the
  delivery ID a sink may honor.

## Non-goals

- **Acting from the notification** (approve or resolve from Slack). It needs authenticated
  callbacks, the approver key story, and the m-of-n signature path; a later step, and the payload's
  `hint` is a pointer only.
- **Reminders and escalation** (renotify after an hour, page a second person). A later layer on
  `notify:done` and a clock; v1 notifies when the event begins.
- **Routing rules** (which team gets which tool). One notifier per agent; fan out in the receiver
  or compose `Notifier` values.
- **Redacting run IDs or per-tenant secrets.** A deployment wraps `Notifier`.
- **Notifying about ordinary tool errors or a plain error returned from `Run`.** The caller has
  those; only states the runtime itself parks a run in.

## Open questions

1. **What counts as "failed"?** An error returned from `Run` is not journaled as terminal in
   general; `run:aborted` (a saga whose rollback ended the run) is. Recommend v1 covers only
   `run_aborted` and leaves plain errors to the caller, and revisit once a terminal-failure
   record exists.
2. **Quorum progress.** Notify on each decision of an m-of-n gate? Recommend no: one event per gate,
   and the payload's `approved` count is the state at first observation. A "needs your signature"
   nudge belongs with reminders.
3. **Append to a terminal run.** Does the ended-run rule accept a late `notify:done`? Recommend a
   narrow `notify:` exemption if not, since the alternative (a side journal) adds a second store
   story. Verify before implementation.
4. **Delivery inline or in a goroutine.** Recommend the agent-owned goroutine with a bounded flush
   on `Close`: `Run` must not wait on a slow sink. Cost: a process killed before the goroutine runs
   delivers only through a recovery pass, so a deployment without `RecoverLoop` gets best-effort.
   State that in the guide.
5. **Arguments in the payload.** Recommend off by default with an explicit opt-in.
6. **Attempt cap and backoff.** Recommend 8 claims, capped exponential backoff (1 s base, 5 min
   cap), and a `WithNotifyRetry` option. The numbers are not load-bearing; the cap is.
7. **Package placement.** Recommend `notify` in the core module (stdlib only) rather than a nested
   module, so the recipe's `agent.WithNotifier(notify.NewWebhook(...))` is a single import.
8. **When to build it.** The project is stabilizing. Recommend no implementation until the freeze
   lifts; the recipe covers the interim, and its page should link to this note as the planned
   crash-safe version.
