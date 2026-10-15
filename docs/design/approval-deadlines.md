# Design: approval deadlines, and clearing a stale decision

Status: shipped. Two roadmap items for the approval gate (see
[Human approval](../guides/hitl-approval.md) and [m-of-n approval](design-mofn-approval.md)):

- A deadline for a gate that never gets its approvals, so a run waiting on approvers who do not
  answer stops on a schedule, instead of staying unfinished until someone cancels it.
- A way past a stale decision: a decision written before a plan flow gate's request, which never
  counts but holds the name the approver's real decision needs.

## The deadline

**Declared on the policy.** `ApprovalPolicy.Timeout` (a `time.Duration`; zero means no deadline,
which is today's behavior). It applies to `SingleApproval` and m-of-n gates, on agent tool calls
and plan flow nodes alike, and is journaled with the policy wherever the policy is (the pause note,
the tally) as `timeout_ms`, omitted when zero, so a policy without a deadline encodes as it did
before.

**Measured from the recorded request, fixed once.** The first time a gate is evaluated with no
recorded outcome and a policy with a timeout, it records `approval-deadline:<call>` once
(insert-once, like every gate record): `requested_at`, the time the request was recorded, and
`expires_at`, `requested_at` plus the timeout. `requested_at` is the `recorded_at` of the flow
node's `approval-request` record, or of the agent call's `approval-pending` note when the run
journaled one, and the drive's clock reading otherwise (an agent call whose note a crash lost).
The deadline is read from that record from then on, never recomputed: a redeploy that changes the
timeout does not move a gate already waiting, and a later reading of the clock decides only whether
`expires_at` has passed. A gate that opened before its policy had a timeout gets its deadline the
first time it is evaluated under one, measured from its recorded request (so it may expire at that
evaluation). The clock is the Agent's (`WithClock`) or the drive's context clock, as the gate's
other times are.

**What "stops" means: a recorded expiry, which is final.** An evaluation that finds `expires_at`
passed and no recorded outcome records the expiry, and it records it under the same name the gate's
outcome takes, so an expiry and a passing tally cannot both be recorded:

- m-of-n: an `ApprovalTally` with `expired: true` (it does not pass) under the gate's tally step
  (the one it is counting under: its own, or a re-ask's step);
- `SingleApproval`: the gate's single decision record, a denial with `expired: true`.

Insert-once by name makes the first writer the outcome: whichever outcome is recorded first is what
the call does. A passing tally recorded first runs the call even if the deadline passes a moment
later, and an expiry recorded first means the call never runs. A decision whose write lands after
the expiry does not count, even if it was stamped on time: the gate reads the clock, then the
journal, then records its outcome, and a decision written between the read and the record is not
in the count. That fails closed (the call does not run), and every reader says why the decision
did not count (`agent.ReasonAfterExpiry`, "not counted: written after the gate expired":
`EvaluateGate`, `bide view`, `bide approve` and `audit.VerifyApprovals`), so the journal does
not look contradictory. The expiry is a denial in every other respect: an
agent's model reads the call's result as denied (`"tool call denied: the approval gate expired"`),
a plan flow returns `plan.ErrApprovalExpired` (which wraps `plan.ErrApprovalDenied`), a saga treats
it as a denial, and a late approval never reopens it.

**Late decisions do not count.** With a deadline, the gate counts a decision only if it was
recorded at or before `expires_at` (its `recorded_at`, stamped by `SubmitDecision` or `Approve`): a
decision recorded after the deadline, but before any drive recorded the expiry, does not count
(`ReasonLate`). `SubmitDecision` and `Approve` refuse a decision on a gate whose expiry is recorded
or whose `expires_at` has passed by their clock (`ErrApprovalExpired`, wrapping `ErrConfig`), and
record nothing. The two clocks are the deciding process's and the drive's; skew between them moves
the edge by the skew, which the deployment bounds (NTP), and an auditor sees both times.

**Re-asks.** A gate that asks again (a tally that passed on decisions an earlier format wrote, or
one recorded before separation of duties applied) keeps the deadline it opened with: the re-ask is
the same request. If the deadline passes while it re-asks, the expiry is recorded under the
re-ask's tally step, which is the step the re-ask's outcome would take. Separation of duties is
unchanged: a requester's decision fills no seat, and a deadline does not make it count.

**k-of-n partial tallies.** An expiry records the count at the time it is recorded (who approved,
who denied, who is pending, every decision record it read), like any terminal tally, so the
evidence shows how far the gate got. A count that reaches k when the gate reads the journal is
recorded as a pass, never an expiry; decisions written after that read are not in it (see above).

**Plan flows.** The flow gate records the deadline from its `approval-request`, and only
decisions recorded after the request and at or before `expires_at` count. A gate in a loop has a
deadline per iteration, measured from that iteration's request.

**The approvals intake and `EvaluateGate`.** `GateStanding` gains `Expired` (a recorded expiry:
`Status` is `GateDecided`, `Denied` is true) and `Deadline` (the recorded `expires_at`, zero for
none). A waiting gate past its deadline is reported as waiting with the deadline, since only a
drive records the expiry; the intake (`approvals.Handler`) and `bide approve` show it as expired
by their clock and refuse a decision on it, as `SubmitDecision` would.

**The notifier.** An expiry is an event, `approval_expired`, delivered like `approval_pending`, and
the pending approval's all-clear (`resolved`) gives the reason `expired`. An expiry needs a person
the same way a denial does not: the run is not waiting any more, but the action someone asked for
did not happen, which is why it is an event and not only an all-clear.

**Evidence.** The expiry tally is a terminal tally: `audit.ApprovalEvidence` emits it with the
deadline record, and `VerifyApprovals` reports an expired gate as not approved (it never ran), with
the decisions it read. A decision recorded after `expires_at` that the gate did not count is shown
as ignored, with the reason.

## Clearing a stale decision

A decision on a plan flow gate written into the journal before the gate's request (a record
written directly; `Approve` and `SubmitDecision` refuse one) never counts. For an m-of-n gate it
blocks only the identical decision: decisions are journaled under their identity (call, approver,
verdict, scheme, decision time), so the approver's new decision, signed with its own decision time,
is journaled under its own name and counts; resubmitting the stale one is refused with an error
that says to decide again. For a `SingleApproval` gate it holds the gate's one decision name, so the
approver's real `Approve` cannot be recorded, and the gate could never pass.

`Approve` on such a gate records the decision under a second, reserved name
(`approval:<call>:after-request`), insert-once like the first, and the gate honors that record when
the first one predates the request. No operator step is needed, and nothing is deleted: the stale
record stays in the journal, ignored by the gate and shown as such in the evidence and the viewer.
The flows model's early decision stops being final for its approver (they can decide again after
the request), `flows/limits/gate-early-stuck` is removed, and the limit it stated with it.

## Model

Model 1b (the approval gate) gains the deadline: a request time, an `Expire` step that records the
expiry under the outcome's name when the deadline has passed and nothing is recorded, decisions
stamped with a time, and the properties:

- `ExpiredNeverRuns`: an effect never fires on a call whose recorded outcome is an expiry;
- `OneOutcome`: the outcome is recorded once (an expiry and a passing tally cannot both be the
  outcome);
- `LateNeverCounts`: a passing outcome never rests on a decision recorded after the deadline.

A regression configuration restores a bug that lets a late decision count, and fails
`LateNeverCounts`.
