# Design: the kill switch (a signed, audited emergency stop)

Status: accepted. The control run, the fold and Model 16 (#393) and the engine's holds, recovery, `StopSwitch` and the audit trail with `bide audit stops` have shipped (unreleased); `bide stop` and the signed-head job `bide audit stops -sign-head` have shipped (unreleased); the viewer banner and the notifications follow. Decisions are in section 14.

## 1. Problem

`agent.Cancel` ends one run. An institution that runs agents which move money also needs to stop
automated action everywhere at once, for two reasons: a model misbehaves (a prompt injection, a
runaway loop, a bad deployment), or a provider has an incident (a payment API double-posts, a
model vendor returns garbage). The two requests are "stop every agent" and "disable the refund
tool across all runs, effective now". Both must be:

- effective before any side effect that has not started, in every process that shares the store;
- a pause, not a failure: affected runs stop durably and carry on when the stop is lifted, with
  the same at-most-once guarantee as any other pause;
- accountable: who stopped, who resumed, when, why, signed, in a trail that the audit machinery
  signs and anchors.

Not goals: stopping a call that is already running (it finishes and is journaled; section 6),
auto-expiry of a stop (a stop never lapses by itself), and stopping processes that do not run the
bide engine.

## 2. Summary of the design

- A **stop** is a signed record, written by an authorised person, with a **scope** (global, a
  tool name, a flow name, an agent name) and a reason. A **lift** is signed approvals by
  authorised lifters, a quorum of them, referencing the stop.
- The records live in a reserved **control run** (`@stop`) in the same journal store the runs use,
  written with insert-once names, so SQLite, Postgres and MemStore need no new schema and no new
  store method. A second copy goes to an **audit trail** (`audit.StopTrail` over an
  `AuditedEventStore`), which the existing machinery signs and anchors.
- Every process that runs agents holds a **StopSwitch**: a cache of the effective stops that it
  refreshes by polling the control run. Four **check points** consult it: drive start, turn
  boundary, immediately before a tool call is claimed and run, and a flow node's call. A covering
  stop makes the drive return a new pause, `*Stopped`.
- A call that is about to run reads the control run **synchronously**; the poll serves the other
  points. When the control run cannot be read, side-effect calls **fail closed** after a stated
  staleness bound.
- `bide stop` sets, lists and lifts. `bide view` shows an active stop on every page.

**Two modes.** The design has two modes that differ only in authorisation and evidence. **Basic**
(the default) needs no keys: anyone who can reach the store or run the CLI sets and lifts a stop,
and the records are plain. **Audited** is everything in sections 5 to 8: signed records, separation
of duties on lift, the anchored trail. Pause semantics, check placement, the in-flight rule, the
at-most-once guarantees, Recover behaviour, notifications and the fail-closed staleness rule are
the same code in both (section 7A).

## 3. What exists today that this builds on

- `agent/loop.go` claims the attempt marker (`claimNextAttempt`, the `claims` region) before the
  tool chain runs for a call that is not retry-safe, then `postClaim` (`agent/endmarker.go`)
  re-reads cancellation and, if the run was cancelled, records the attempt as not started
  (`recordNotStarted`, `agent/attempt.go`), so a resume re-attempts under a new numbered marker.
  Cancellation is the precedent for a store-polled signal that acts before an effect.
- `toolHandler` (`agent/toolexec.go`) runs `preExecute` (`agent/exec_gate.go`) before it begins the
  call, for every path that can run a call (first execution, recovery, RecoverLoop, a middleware
  retry). A held call returns a pause before `beginCall`, so it is not marked as run.
- The pause contract (`agent/pause.go`) is sealed to five types and a pause is not journaled; the
  run is re-driven with `RunRef.RootRunID`. `Recover` and `RecoverLoop` (`agent/recovery.go`)
  re-drive every unfinished run.
- Approvals sign with `agent.Alg` keys (Ed25519, ECDSA P-256, ML-DSA-65, hybrid) through
  `audit.Signer`; key sources are key files and `bide-hsm`/KMS through `approvecmd.KeySource`.
  Verification is `agent.ApproverVerifier` / `ApproverVerifierFor`. Separation of duties and
  m-of-n are in `docs/design/design-mofn-approval.md`.
- The notification trail (`agent.NotifyTrail`, `audit.NotifyTrail` over `AuditedEventStore`) is the
  precedent for an append-only trail that is Merkle-committed, signed and anchored.

## 4. Scopes and matching

| Scope | Matches | Check points it acts at |
|---|---|---|
| global | every run | drive start, turn boundary, every call |
| flow NAME | runs and nodes of the plan flow with that name | drive start, turn boundary, every call of the flow |
| agent NAME | runs of the agent with that name (its identity name) | drive start, turn boundary, every call |
| tool NAME | calls of the tool with exactly that name (and its compensation, section 9) | the call only; model turns continue |

Names match exactly. There are no globs: a mistyped glob that matches too much is an outage, and
one that matches too little is a stop that does not stop. Several stops may be active at once; a
run is covered if any active stop covers it. A scope is checked against the run's **tree**: a
sub-agent or session turn is covered when its own agent name, flow, or tool matches, or when the
tree root is covered by a global, flow or agent stop, so that a stopped root does not keep
running children.

A tool-scope stop does not stop the model turn: the model may still answer with other tools. The
call to the stopped tool pauses the run, with siblings handled as for a pending approval
(section 6.3).

## 5. The records

### 5.1 Storage

The control run `@stop` is in the journal store that runs use (default), or in a store given to the
switch (`WithStopStore`), for a deployment that wants a separate database. Entries are written
with `Store.Insert` and a name derived from the record's whole content, never updated or deleted
(store property A6): `@stop/set/<stop id>`, `@stop/lift/<stop id>/<lifter>/<digest>` (one lifter's
approval), `@stop/mode/<n>/<signer>/<digest>` (one signer's approval of a change of mode).

Names are content-derived, so there is no position to contend for: two operators setting at once
both succeed, a retry of the very same record is a no-op, and nothing needs to be re-signed after
a lost race (re-signing would mean a second HSM touch during an incident). A name ends in a digest
of the record, so a writer cannot squat a name with a forged record to keep the real one out. The
engine rejects `@stop` as a user run ID and `Recover` and `RecoverLoop` do not drive it
(`bide view` and other `Runs` consumers skip it, in their own PRs). The control run holds no end
marker, so redaction (which applies only to runs that are over) never reaches it.

### 5.2 Format

Canonical JSON (`agent/canonicaljson.go`), format `bide.stop/1`:

```
set:   {format, kind:"set", mode, scope:{kind, name}, compensation:"allow"|"block", reason,
        stop_id, at, by, host, policy_digest, alg, signature}
lift:  {format, kind:"lift", mode, stop_id, set_digest, reason, at, by, host, policy_digest,
        alg, signature}
mode:  {format, kind:"mode", mode, to:"basic"|"audited", seq, reason, at, by, host,
        policy_digest, alg, signature}
```

- `stop_id` is the hex SHA-256 of the canonical bytes of the set record without `stop_id` and
  `signature`. It is not chosen by anyone, so two different stops cannot share one.
- The signature covers a domain-separation label (`bide-stop/1`), the kind, and the canonical
  body. A lift signs `set_digest` (the digest of the whole signed set record), so a lift cannot be
  replayed against another stop, and cannot be signed before the stop it lifts exists.
- `mode` is the mode the record was written under. `by` is the enrolled approver id; `policy_digest`
  is the digest of the stop policy the writer signed under (section 7), as approvals do, and is
  evidence only: a record is accepted by its signature under the policy in force, so replacing a
  policy never lifts a stop. Rotating an enrolled key while a stop is active needs the old key in
  the verifier (the rotation window `ApproverVerifier.KeyIDs` describes), or the old stop's
  signature no longer verifies.
- `reason` is operator text, at most 1 KiB, valid UTF-8, no control or bidirectional characters.
  Operators are told in the help and docs not to put secrets or customer data in it; it is shown in
  logs, notifications and the viewer.
- `at` is the writer's clock and advisory: the record's order is the store's
  and the audit trail's, and a record's effect never depends on its timestamps.

### 5.3 Effective state

A stop is **active** from the commit of a valid `set` record until the store holds a valid lift
quorum for it, judged by the store's order of records, under the process's mode and the policy in force at each
record (`StopControl.State` is the one fold; Model 16 is its model). A record is valid when its signature verifies under the key the policy enrols for
`by` with the role the record needs (setter, lifter) and its `format`, `policy_digest` and
`set_digest` agree. An invalid record is ignored, reported to the switch's error handler, and shown
by `bide stop -list` as rejected. This direction is deliberate: someone who can write the database
but holds no enrolled key can neither stop (denial of service) nor lift (the dangerous direction).
Someone who can delete rows can hide a stop; that is detected through the audit trail (section 8),
and is the reason the store is not the only copy.

## 6. Check placement and semantics

### 6.1 The four check points

1. **Drive start** (`Agent.run`, `agent/loop.go`, after the run is opened, protocol region
   `lifecycle` `DOpen`): a covering global, flow or agent stop returns `*Stopped` before any model
   call, so a stopped agent spends no model budget. No record is written to the run.
2. **Turn boundary** (`agent/endmarker.go`, next to the cancel read in the `lifecycle` `DTurn`
   region): the same check before each model call, so a long run notices a stop within one turn.
   It stops a model turn from producing new tool calls.
3. **Before the claim** (`agent/loop.go`, at the top of each call's `g.Go` closure, before
   `claimNextAttempt`): a covering stop returns `*Stopped` for the call with no marker written.
   This is the preferred point: nothing durable exists to reconcile.
4. **After the claim and before the call**, twice, so that no path to the tool skips it:
   `postClaim` (`agent/endmarker.go`) reads the switch next to the cancellation read and, on a
   covering stop, records the attempt as not started (`recordNotStarted`) and pauses; and
   `toolHandler` (`agent/toolexec.go`) checks immediately after `preExecute` and before
   `countIn`/`beginCall`. The second covers recovery, a RecoverLoop resume, a middleware retry and
   the saga's re-run of a retry-safe step (`agent/saga.go`), which all pass through `toolHandler`.
   A refusal there is a `refuse(...)`-style not-called return wrapping `ErrToolNotCalled`, so the
   existing `called`/`notCalled` logic records the claim as not started.

The flow node path (`agent/flowgate.go` and the plan executor) gets the same check at the point
where a node's call is made, with the flow's name in scope.

Where the claim exists and the not-started record cannot be written (a store fault), the marker
stands and a resume halts on it, exactly as for a cancelled run today. That is safe and is the
existing rule, not a new one.

### 6.2 What a stop does not do

- A stop never writes a marker, a result, or an end record into a run. It does not abort a saga
  and does not trigger rollback. A run's journal after a stop and a lift is the journal of a run
  that was paused and resumed.
- A stop never clears or creates an attempt marker. A call whose marker is written and which has
  no result is **not** re-fired by the stop, by the lift, or by a Recover pass: the existing
  resume gate halts it (`*OutcomeUnknown`) before and after a stop alike. A stop can only add a
  `not-started` record to an attempt that it provably prevented from starting, in the same drive,
  before the tool chain was entered.

### 6.3 A call in flight

A call that has passed the last check (`beginCall` succeeded) **finishes, and its result is
journaled** under the existing non-cancellable context. The stop does not cancel contexts, shorten
timeouts or abandon it, because abandoning a call of unknown effect is the one thing this runtime
exists to avoid. An operator who wants the run itself ended uses `Cancel` as today, which has the
same in-flight rule. The window is the time between a call's check and its start (microseconds
in process) plus the call's own duration.

Parallel calls in a turn behave as for a pending approval: siblings that already started finish
and are journaled; calls that have not started are held; the run returns `*Stopped` after the
siblings have recorded their outcomes. In a saga, a held call is not a failure, so nothing aborts.

### 6.4 The pause

`*Stopped` is a sixth Pause type (`agent/pause.go`; the set is sealed, so every exhaustive switch
over pauses and the notification and viewer code are updated):

```
type Stopped struct { RunRef; StopID string; Scope StopScope; Since time.Time; Unreadable bool }
```

`Unreadable` is the fail-closed pause of section 10: no stop is known, the control run could not be
read. For a session turn, `Send` returns `*Stopped` before the turn's run is started, so nothing is
consumed; the caller retries after the lift. A sub-run's `*Stopped` surfaces at the root with
`RootRunID` as any sub-run pause does.

One non-authoritative note per (run, stop id) is written to the run's journal when a drive pauses
for a stop (as `writePauseNote` does for exec holds), so the run's own journal and its evidence
show why and when it stopped. It is never read for a decision.

### 6.5 Recover and RecoverLoop

- A drive that meets a covering stop returns `*Stopped`, which `Recover` and `RecoverLoop` count as
  **held by a stop**: not an error, not `NotifyRecoveryFailing`, not progress, reported in a new
  counter of the pass result and in the optional pass hook.
- Before listing runs, a pass reads the switch: under an active **global** stop it lists nothing
  and returns at once (a pass over a million stopped runs should cost one read, not a million
  opens). For scoped stops, whether a run is covered depends on the run (its flow, its agent, the
  tool it would call), so the drive's own checks decide.
- After a lift, runs continue at the next RecoverLoop pass (its interval, plus at most one poll
  interval for the switch to see the lift). An in-process caller that got `*Stopped` re-drives
  with `RootRunID` as for any pause. The engine does not auto-resume stopped runs in the caller's
  goroutine.
- Recover never re-drives past a stop: the check is in the drive, which is the only path to a
  tool, so there is no recovery path that skips it.

## 7. Who may stop and who may lift (separation of duties)

Policy (`agent.StopPolicy`, file form beside the approval policy file, digest recorded in every
record): the enrolled setters, the enrolled lifters, and the **lift rule**.

- **Setting needs one authorised person.** A stop is the safe direction and an incident is not the
  time for a second signature. Any enrolled setter's single signature is enough.
- **Lifting needs a quorum.** Recommendation, and what a bank's change-control normally expects:
  lift requires `k` distinct enrolled lifters (default `k = 2`), and **at least one of them is not
  the setter**, so the person who stopped cannot alone resume. The setter may be one of the
  lifters, since the person who understands the incident should be able to take part in ending it.
  `k` and "one must differ from the setter" are policy fields; a one-person institution sets
  `k = 1` explicitly, and the policy file says so in a field a reviewer can read.
- A policy that cannot be satisfied (fewer enrolled lifters than `k`, or `k` lifters that can
  never include a non-setter) is refused when it is loaded, as the SoD infeasibility check does for
  approvals. An emergency is the wrong time to find it.
- Identity: v1 enrols bare keys by approver id, the same trust level as a bare-key approval gate.
  Certificate-bound stoppers (the `approver-identity` machinery: chains, revocation status,
  execution-time re-check) are a follow-up and are not designed here; the record format has room
  (`by`, `key_id`, an optional chain field reserved in `bide.stop/1`).
- Keys: any `audit.Signer`. `bide stop` uses `approvecmd.KeySource`, so a key file or `bide-hsm`
  (PKCS#11, AWS KMS, GCP KMS, Azure Key Vault) works unchanged. Nothing secret is in argv: `-key`
  names a file or an HSM/KMS key reference, as `bide approve` does.

## 7A. Modes: basic and audited

**The mode is each process's configuration, never something the store says.** Every process
declares it (`WithStopMode`): audited needs a stop policy, basic takes none, and a control that
reads a control run holding records without a declared mode fails with `ErrConfig` at open (a
configuration error, never one the store can cause). Nothing in the control run can
switch a process's mode, make it fail, or downgrade it. (An earlier draft took the mode from the
control run's records; the review of #393 showed that one forged record then bricked every basic
deployment, so the records became history and evidence, and a mode change is a configuration
rollout.)

| | Basic (default) | Audited |
|---|---|---|
| Who may set or lift | anyone with store or CLI access | enrolled setters; lift by a quorum (section 7) |
| Records | plain `bide.stop/1` JSON, `mode:"basic"`, no signature | signed, as section 5.2 |
| `by` | as given (`-as`, else the OS user name), plus the host name; shown as "asserted, not authenticated" | enrolled approver id, authenticated by the signature |
| Lift | one lift record from anyone; the setter may lift | `k` lifters, one not the setter |
| Audit trail, anchoring | none; the control run is the only record | `StopTrail`, signed heads, anchors, `bide audit stops` |
| Keys, policy file | none | required |
| Notification, `bide view`, `bide stop -list` | yes, labelled with the mode | yes |
| Staleness | fail closed past `MaxStaleness`, the same default and the same reason (section 10) | same |

A basic record holds: scope, reason (same limits), compensation flag, `by`, `host`, `at` (the
writer's clock, advisory), `stop_id` (the same content hash). No key, token or argument is
recorded.

**Basic mode trusts store access completely.** Anyone who can write to the store can set a stop,
lift one, or write mode and policy records that a basic process does not check (it counts them as
`Unverified` and the engine says so loudly). Basic mode gives no protection against a store-level
adversary and no tamper evidence; it is an emergency brake for one team. That is what audited mode
with a pinned policy is for.

**Identical semantics.** Both modes produce the same effective stop set for the same records, so
the check points, the `*Stopped` pause, the in-flight rule, the marker rules (sections 6.1 to
6.5), compensation, approvals, sagas, sessions and the notifier are one implementation. Only the
fold's admission of a record (`StopControl.State`) and the CLI's signing step and trail write are
mode-specific. Model 16 has the mode as a parameter of that one rule and checks the safety
invariants under both.

**The trust root and policy rotation.** The anchor is the deployment's **root policy**, as in TUF. It
carries a **deployment ID**, a random value chosen at genesis (one per deployment, never shared
between staging and production), which is part of the policy's digest, repeated by every later
policy, and written into every signed record: a record signed for another deployment is rejected, so
staging's records copied into production do nothing. The first audited action (`bide stop
-upgrade`) writes a signed **root record** carrying that policy. An audited process pins one policy
from its own configuration (a file or an option): the root policy, which is the recommended and
simplest form, or a later policy that the chain reaches. It walks the chain forward from the first
root record that verifies under its own policy, and the control run's **policy transitions** carry
the later policies: a transition record names the next number, the
digest of the policy it replaces and the new policy in full (approvers with public keys, roles,
quorum, the setter rule), and counts when `LiftQuorum` distinct lifters of the policy in force have
signed the same document. A process folds the log forward from its pinned policy, judging each stop
and lift under the policy in force at its position, so:

- a stop set under policy 1 stays in force after a transition that offboards its setter or rotates
  its key; the policy in force governs what follows (an offboarded lifter's earlier lift no longer
  counts, a new stop needs a setter of the policy in force);
- a store writer with a key of their own, or any number of forged records, cannot extend the chain:
  a transition needs the signatures of the policy in force. A process whose pinned policy is not
  on the store's chain sees the records it cannot verify in `Rejected`; it is never made to fail by
  them;
- stopping stays possible while a transition is pending (a set is judged under the policy in force
  and a signed setter of it succeeds); a transition's new policy is validated when adopted (an
  invalid one is rejected, the old policy stays) and every policy is validated again whenever it is
  used, since a verifier can change after it was given;
- a **stop set during a rotation** is judged under the policy in force when its record lands: a
  setter the new policy still enrols succeeds, one it no longer enrols is rejected and the writer
  is told (`ErrStopRejected`) and sets it again with a current setter; nothing is accepted on the
  strength of a policy that has been replaced;
- a key that was **offboarded or rotated under the same id has no authority after the transition**:
  its later sets, lifts and approvals are rejected, and a lift it gave before the transition no
  longer counts toward the new quorum;
- the chain is checked for **forks** (a quorum of the replaced policy also approving a different
  successor) and **rollbacks** (a quorum approving a policy that was already in force): both are
  recorded in `StopState.ChainErr`, which makes the stops unknowable (below), not an error: the
  first lineage keeps being enforced. Two different roots that each verify under themselves are the
  same condition. Records that do not verify never cause an error. A pinned policy that the walk from
  the root never reaches is `ChainErr` too ("the pinned policy is not on the store's chain"): it
  cannot be told from a forged chain, so it is an alert (the engine's `OnError` and the `bide view`
  banner, PR 2) and fail-closed for covered side-effect calls, never a silent drop and never an
  `ErrConfig` that a store writer could trigger. A process pinned to a later policy that the chain
  reaches enforces the stops set under the earlier ones, because it judges each record under the
  policy in force at its position, from the root;
- a **basic process on an audited deployment** (a missing policy) sees signed records it cannot
  check: it counts them in `Unverified`, a plain lift does not lift a stop that was set under a signed
  record (a signed lift does, in its view, which is how a deployment that moved back to basic stays
  usable), and an active stop set under a signed record makes its stops unknowable, so it fails
  safe. This is no worse than basic's stated exposure: store access can already stop a basic
  deployment;
- **stops unknowable** (`StopState.UnknowableErr()`, `ErrStopsUnknowable`) is only for a process that
  genuinely cannot judge: a basic process holding an active stop set under a signed record (above),
  or an audited process whose pin no stored root's chain reaches (or whose chain forks, or whose root
  it cannot build). The engine (PR 2) fails closed on it for side-effect calls a stop could cover, and
  raises an alert (`OnError`, the `bide view` banner). A forged record is **never** a cause: an audited
  process whose pin the chain reaches knows the policy in force at every position, so a record that does
  not verify is a forgery, `Rejected` and nothing more. Otherwise a store-write attacker would hold a
  lever (pausing covered calls) against exactly the mode meant to resist them. The chain starts at the one root the process
  **trusts**: its configuration names the root policy (`WithStopRoot`, which defaults to the pin when the pin is the
  root), and a root record counts only if its policy is that one. A root a store writer appends with
  keys of their own, however it is chained to the pin (a self-signed root plus a transition into the
  public pinned policy), never governs a record. A process that pins a later policy must name the root
  (and have a verifier constructor to build it); a later pin without the root, or a trusted root that is
  not stored, is unknowable, never a silent drop. Records before any trusted root is stored are judged
  under the pin. A later-pin process configured without the root therefore sees the stops as
  unknowable, which (PR 2) pauses the covered side-effect calls until it is configured: the safe
  direction. Only verified records count toward
  anything: a fake root, transition or mode record is rejected and never sets `ChainErr`. The first
  audited write of any kind (a stop, a lift, a transition, a mode change) writes the root if none whose
  chain reaches the pin exists, so every audited deployment is walkable;
- a control needs a verifier constructor (`WithStopVerifier`, `audit.NewVerifier`) to adopt a
  transition; without it the transition is reported as rejected and the policy in force is the
  pinned one.

A process that is **restarted with a newer policy** pinned still works if its policy equals one on
the chain at or after the point where older records matter; the operator keeps pinning the first
policy to be safe. The rule of thumb: pin the policy the deployment started audited mode with.

**Moving to audited mode mid-stop.** `bide stop -upgrade` (with a policy and a key) first
**adopts** every stop that is active in the basic view: it sets it again, signed (reason prefixed
"adopted:", and the record names the digest of the plain record it re-states, so the history stays
traceable), because an audited process ignores unsigned stops; then it writes a mode record as
evidence. The deployment stays stopped, the plain originals stay in the log (basic processes that
have not been rolled over still see them), and the stops need an audited lift from then on. The
policy file reaches every process before the upgrade is signed (rollout order: policy, then the
upgrade); a process left basic is visible in `bide view` as unverified records.

**Moving back to basic** is a configuration rollout too. `bide stop -downgrade` records the change
as signed evidence (and in the audit trail) and refuses while any stop is active, so an operator
cannot lift a stop by leaving audited mode; no store-side rule can stop another process's
configuration from changing, and the trail and the viewer show the period as unaudited.

## 8. Audit (audited mode only)

`audit.StopTrail` is an append-only trail named `stop` over an `audit.EventStore`, passed through
`AuditedEventStore`, so each record is salted, Merkle-committed, covered by a signed tree head, and
anchored (including the S3 anchor) like the notify trail. A leaf is the canonical signed record, so
a verifier checks each record's own signature as well as the head's.

- **Ordering of the two writes.** Setting writes the control run first (so the stop is effective),
  then the trail. Lifting writes the trail first, then the control run (so a lift is never
  effective without its evidence). A failure of the second write is an exit status of its own,
  says which half landed, and repeating the same command completes it (both writes are
  idempotent).
- **Verification.** A new `bide audit` verb (`stops`) verifies the trail's heads, anchors and
  signatures, and, given the journal, cross-checks the control run: a record in the store that the
  trail lacks, or a trail record the store lacks, is a finding (a hidden stop, or a lift that was
  recorded but not effective). The trail is also the evidence of the **order** of stops and lifts.
- **What the trail proves and does not.** It proves who signed what and that the signed head was
  anchored by a time. It does not prove that every process saw the stop, and it records nothing
  about calls that started before the stop; the run journals hold those.

## 9. Sagas, compensation, approvals, sub-agents, sessions

**Compensation (recommendation: a stop does not block compensation by default).** A compensation
is a side effect, but it reverses damage already done, and during an incident an operator more
often wants the rollback of a saga to continue than to freeze half-undone. A stop therefore pauses
forward calls and leaves `Compensate` and the saga's retry-safe re-run for compensation running
(the rollback context, `rollbackCtx`, is recognised by the check). The record carries
`compensation: "allow" | "block"`; `bide stop -block-compensation` blocks the compensation of
calls the scope covers, for the case where the compensating action is itself the incident (a
refund tool that is misbehaving, whose `Compensate` is a refund). A blocked rollback pauses with
`*Stopped` and resumes after the lift; it does not abort or skip steps. The default and the flag
are shown by `bide stop -list` and in the notification.

**Approvals pending at a gate (recommendation: they keep collecting, and nothing executes).** An
approval decision is a record, not a side effect: approvers must be able to answer, the evidence
should not wait on the incident, and refusing decisions during a stop would only make approvers
retry later. So `Approve`, `SubmitDecision` and the approval page keep working, tallies are
counted, and the gate's outcome is journaled. Execution is what a stop prevents: `preExecute`
passes (the approval holds), the stop check follows it, and the call waits. Approval **deadlines
keep running** during a stop (the approval is a time-bound authorisation, and silently stretching
it would be a security change); an approval that expires while stopped is recorded as expired as
today, and the call, on resume, follows the existing expiry rule. This can turn a long stop into
denied calls, which is the conservative direction, and is stated in `KNOWN-LIMITATIONS`.
Alternative in section 14.

**Sagas.** A stop is not a failure, so a saga does not roll back because of it (6.2, 6.3). A run
cancelled while stopped is cancelled as usual (`Cancel` is not blocked by a stop: ending a run
never starts a forward effect).

**Sub-agents and delegation.** A sub-run is covered by its own scope or by its tree root's
(section 4). Its `*Stopped` surfaces at the root. Delegated authority (`audit` grants, the
`DGuard` region) is orthogonal: a stop is checked in addition to a grant's expiry, never instead.

**Sessions.** Each turn is a run; the turn start is a drive start. A stopped session is a session
whose next `Send` returns `*Stopped`; in-flight calls of a turn finish.

**Notifier.** Setting and lifting notify through the existing notifier. Two new notification
kinds, `stop_set` and `stop_lifted`, are observed by the notifier's sweep from the control run
(event keys `stop:<id>` and `stop:<id>:lifted`), so delivery has the notify trail's claim, ack,
retry and overdue behaviour and is durable if the process that set the stop dies. Routing and
escalation (`notify_routing`) apply: a stop that nobody acknowledged escalates. The payload carries
the scope, the setter's id, the reason and the compensation flag, never a key or a secret.

## 10. Distribution, latency and failure modes

**The StopSwitch.** One per store handle, shared by every Agent built with `WithStopSwitch(sw)`.
It polls `Load(after = cursor)` on the control run every `PollInterval` (default 1 s,
configurable down to 100 ms) and keeps the verified, effective stop set in memory. Records are
verified once. A poll that finds nothing new costs one indexed read.

**Latency bound, stated precisely.**

- A call about to run does a **synchronous** read of the control run after its claim, so for
  these calls the bound is the store's own visibility: a stop committed before the read is seen
  (store property A4, read-your-writes and monotone visibility). On Postgres the control read must
  go to the primary; reading a lagging replica would add the lag to the bound, and the docs say so.
- Drive start, turn boundary and a read-only call use the cache: the bound is `PollInterval` plus
  the poll's own duration, and a turn already past its check finishes, so a model turn can still
  complete after a stop and its tool calls are then held at the call.
- Lift latency is `PollInterval` plus the recovery pass interval (section 6.5).
- Multi-process: no coordination other than the store. Clock skew does not matter (nothing
  compares timestamps).

**Fail-safe when the control run cannot be read.** Staleness is measured by the reader's
monotonic clock since its last successful read, never by record times.

- A side-effect call (a tool that is not `ReadOnly`) whose synchronous read fails is allowed to
  use the cache only while the cache is younger than `MaxStaleness` (default 15 s,
  `WithStopStaleness`). Past it, the call is held with `*Stopped{Unreadable: true}`: **fail
  closed**. `MaxStaleness` of 0 means any read error holds the call.
- Read-only calls and model turns use the cache whatever its age and carry on, since they cause no
  side effect; the first side-effect call after that is held.
- A process that has never read the control run successfully holds side-effect calls at once.

Why fail closed. The control run is in the journal store by default, and a side-effect call needs
that store to write its attempt marker before it runs, so an unreadable control run is nearly
always an unusable journal as well: failing closed costs almost nothing the run did not already
lose. With a separate control store (`WithStopStore`) it costs a call held during the outage, which
is the intended price: an operator who has stopped something must be able to rely on the stop even
when a network partition cuts a process off from it. Failing open would let the same incident that
motivates a stop (a provider or network outage) also defeat it.

**A process without a switch.** A process that runs agents on a store without `WithStopSwitch`
would ignore every stop. To make that misconfiguration visible and not silent, a drive of an agent
without a switch reads the control run's first entry once per process; if the control run exists,
the drive fails with `ErrConfig` (named in the error). A deployment that has ever set a stop
cannot silently have a process that disregards it. (Decision in section 14.)

**Failure modes, listed.**

| Situation | Behaviour |
|---|---|
| Control read fails, cache fresh | cached stop set is used |
| Control read fails, cache older than `MaxStaleness` | side-effect calls held (`Unreadable`); reads and turns continue |
| A record fails verification | ignored, reported, shown as rejected; never makes or ends a stop |
| Lift quorum short | stop stays active |
| Two stops, one lifted | the other still covers |
| Audit trail unreachable at set | stop is effective; command exits non-zero naming the failed half; repeat completes |
| Audit trail unreachable at lift | no lift is effective; the command tells the operator |
| Store fault while recording a not-started | marker stands; resume halts on it (existing rule); safe |
| Process crashes between check and call | restart re-checks at the call |

## 11. CLI and view

Existing layout: single flag-driven commands (`bide approve -list`, `bide notify`, `bide audit
<verb>`), and no top-level `bide resume`, but "resume" already means re-driving a run in the API
and the docs. A command called `resume` would be read as that, so lifting is a flag of `stop`:

```
bide stop  -all|-tool NAME|-flow NAME|-agent NAME  -reason TEXT  -key KEY  -stop-policy FILE  -db/-postgres ...
           [-block-compensation] [-yes]
bide stop  -list [-json]                 # active stops, who set them, how many lifts they hold; rejected records
bide stop  -show ID                      # one stop and its lifts
bide stop  -lift ID  -reason TEXT -key KEY ...   # adds this lifter's signature; says how many more are needed
```

- No default scope: exactly one of `-all`, `-tool`, `-flow`, `-agent` is required, so a bare
  `bide stop` is a usage error that prints the active stops, never an accidental global stop.
  `-tool`, `-flow` and `-agent` repeat for several names (one stop each).
- Setting is fast: it prints exactly what it will sign (scope, reason, setter, policy digest) and
  signs without a prompt when `-yes` is given or stdin is not a terminal; on a terminal it asks
  once. Lifting always shows the stop, its setter, its age, and how many runs it holds, and asks.
- `bide-hsm stop` takes the same flags with `-key` naming the HSM/KMS key, as `bide-hsm approve`.
- `bide view` shows an active stop as a banner on every page (scope, setter, reason, age, lifts
  held and needed), a "held by stop" state on each affected run, and the rejected records. With
  `-stop-policy` it verifies signatures; without it the banner says "signatures not verified".
  The viewer stays read-only.
- Exit statuses follow the common table; the half-landed audit case has its own.
- The CLI reference is regenerated, `bide help stop` has Recipes, and the approval guides gain a
  "stopping everything" recipe.

**As built (PR 4).** `bide stop` is `cmd/bide/internal/stopcmd`, registered in the command table, and
`bide-hsm stop` is the same code with the HSM key source. Differences from the sketch above: the journal
is `-db` or `-postgres-env` only (no literal DSN on the command line); a stop is written without a
prompt when stdin is not a terminal, and a lift, `-apply-pending`, `-upgrade` and `-downgrade` are
refused without a terminal unless `-yes` (the lift is the dangerous direction); a repeated stop of the
same scope writes nothing; `-reaudit` and `-apply-pending` complete the half-landed writes (a repeated
stop would be a second stop, because the record's time is part of its identity, so "repeat the command"
is not the repair); exit statuses are 0, 1, 2, 3 and 5 (half done). `bide stop` appends leaves to the
trail only: signed heads come from `bide audit stops -sign-head -log-key -head-out` (`bide-hsm audit stops` for a log key
in an HSM or a KMS), a separate job that verifies the trail against the control run, signs exactly the
verified records and appends the head to a file `-sth` reads. The viewer banner, the per-run "held by stop" state and the run count on a lift follow.

## 12. Formal model

A new **Model 16: the stop**, small and separate, in `spec/tla/stop`, rather than a change to
Models 9 and 10. It abstracts a call as `idle -> claimed -> started -> done` with `claimed ->
voided` (not started) and crashes and recovery, plus a stop control with records that commit at
some point and a reader that reads the committed set at a point it chooses (a stale cache is a read
of an earlier point, bounded by a staleness rule), a fail-closed branch, drives that may overlap,
and a lift quorum of `k` lifters with a setter.

Invariants:

- **NoStartUnderStop**: a call starts only if the read made after its claim (the synchronous
  read, or the cached read inside the staleness bound) showed no covering active stop. This is the
  property the user asked for ("no side effect starts while a covering stop is visible"),
  stated against the read, not against wall-clock time, since a model cannot say how soon a
  committed write is visible beyond the store's A4.
- **AtMostOnceAcrossStop**: the at-most-once invariant of Model 9 holds with `Stop`, `Lift`,
  crash and recovery in the action set: a call whose marker is written and which has no result is
  never started by a stop, a lift or a recovery; a `voided` attempt starts at most once afterwards.
- **StopNeverEnds**: a stop never writes an end marker, never makes a run terminal, and never
  creates or clears a marker other than adding `voided` to a claim that had not started.
- **LiftNeedsQuorum** (audited): a stop leaves the active set only with `k` distinct valid
  lifters, one of them not the setter; an invalid record never changes the set. Under basic, any
  well-formed lift record lifts, and every other invariant here is checked unchanged.
- **ModeMonotone**: the mode goes from audited to basic only through a record carrying a lift
  quorum, and only when no stop is active; no drive acts under a mode other than the control
  run's last valid one.
- **RecoverRespectsStop**: no drive (including a recovery drive) starts a call past a visible
  covering stop; reachability configurations show a call does start after the lift.

Configurations follow the existing convention: a few small ones, `deep-*` ones for the nightly
shard, `regress` configurations that remove the check and are expected to violate
`NoStartUnderStop` (the check is not vacuous), and reachability configurations (a call can start
after the lift; a quorum can form; a stop can be reached while a call is in flight and the call
still finishes).

`modelsync` (`internal/tools/modelsync`, markers `// protocol:<model> begin <labels>`) is touched at:

- `toolcall` (Model 9) regions in `agent/toolexec.go` (`IEnter IBegin ICall`): the new check
  between `preExecute` and `beginCall`; and `LStart LPre` in `agent/loop.go`: the check before the
  claim. Model 9 gets a one-line abstract guard (`stopVisible`) and the regress/limits config
  files are updated, so these regions stay in sync; the stop semantics are in Model 16.
- `claims` (Model 1) `Claim Lost NotStarted` and `lifecycle` `DClaim DPost DCall`
  (`agent/loop.go`) and `DTurn DPost` (`agent/endmarker.go`): `postClaim`'s stop read adds a
  second cause for the existing "claim won, call not started" path. The model of that path
  (`NotStarted`) already allows any cause, so the labels gain a line, not a rule.
- `lifecycle` (Model 10) `DOpen` (`agent/loop.go`): drive start; and recovery's pickup.

New labels for Model 16 are placed on the new check functions. The exact set of marker names is
fixed when the code is written; `modelsync` fails the build on a mismatch either way.

Claim scope, stated narrowly: the model checks the protocol rules within its bounds; trace
validation covers the paths the test suites exercise; the latency figures of section 10 are
engineering bounds on the store, not model results. The `Protocol-Impact` line of each PR names
the models it changes.

## 13. Build plan (after approval)

Small PRs, one at a time, each with tests first, mutation checks, docs, a changelog fragment, and
a break-it review before merge (this touches claims, journal, leases, sagas and the loop):

1. `agent`: record format, canonicalisation, signing and verification, `StopPolicy`, the control
   run on `Journal` (reserved ID, insert-once names, exclusion from `Recover`/`Runs` consumers),
   effective-state fold, with MemStore and SQLite tests and a Postgres test in the existing
   harness. Model 16 with its configurations (the model PR can land first, as the existing models
   did).
2. `agent`: `StopSwitch` (poll, cache, staleness), `*Stopped`, the check points, Recover and
   RecoverLoop behaviour, the notes, compensation flag, fail-closed; crash-sweep, DST and
   multi-process tests; mutation checks of each check point (removing any one must fail a test).
3. `audit`: `StopTrail`, `bide audit stops`, cross-check, anchor tests.
4. `cmd/bide` and `bide-hsm`: `bide stop`, the `bide view` banner, CLI reference, `-list -json`.
5. Notifier: `stop_set` and `stop_lifted` kinds, routing, the notify model (Model 14) labels.
6. Docs sweep: README and the four i18n READMEs, `docs/GUARANTEE.md` (the guarantee, stated as in
   section 12), `KNOWN-LIMITATIONS` (in-flight calls, approval expiry, replica lag, bare keys),
   `CONCEPTS`, `getting-started`, a recipe and an example, godoc, docs-site sidebar.

## 14. Decisions

(17 to 19, added at the user's request, are at the end.)

1. **Scopes.** Global, tool, flow and agent, exact names only, no globs. Recommend yes.
2. **Where the records live.** A reserved control run (`@stop`) in the journal store, insert-once
   content-derived names, plus a signed and anchored audit trail; not a new store method or table.
   Recommend yes: no schema change on Postgres and SQLite, MemStore works, no new port for every
   store to implement.
3. **Pause, not fail**, with a sixth sealed Pause type `*Stopped`. Recommend yes.
4. **Check placement.** Drive start, turn boundary, before the claim, after the claim
   (`postClaim` and `toolHandler` after `preExecute`), and the flow node. Recommend all five;
   removing any leaves a path to a tool.
5. **Synchronous control read before every side-effect call**, poll for everything else.
   Recommend yes (one indexed read per side-effect call buys a bound equal to store visibility).
6. **Fail closed** for side-effect calls past `MaxStaleness` (default 15 s), reads and turns
   continue. Recommend yes, default 15 s.
7. **Separation of duties.** Set: one authorised person. Lift: `k = 2` distinct lifters, at least
   one not the setter, configurable, infeasible policies refused. Recommend this default; a
   one-person institution sets `k = 1` in writing.
8. **Identity.** v1 bare enrolled keys (files, `bide-hsm`, KMS); certificate-bound stoppers as a
   follow-up. Recommend deferring certificates; say it in KNOWN-LIMITATIONS.
9. **Compensation.** Not blocked by default; `-block-compensation` per stop. Recommend this.
10. **Pending approvals.** Keep collecting; nothing executes; deadlines keep running. The
    alternative is freezing deadlines for the duration of a stop (kinder to a long stop, but it
    changes the approvals' timing claims and their model). Recommend running the deadlines.
11. **Process without a switch.** Fail the drive with `ErrConfig` once a control run exists.
    Recommend yes; the alternative (ignore stops silently) defeats the feature. (Refined in #393's
    review: no store record may make a process fail, since anyone with store access can write one.
    A process's mode is its configuration; a basic process counts the audited-mode records it cannot
    check as `Unverified` and the engine reports them loudly.)
12. **CLI names.** `bide stop` with `-list`, `-show`, `-lift`; no `bide resume`; no default scope;
    set prompts only on a terminal without `-yes`. Recommend.
13. **Formal model.** A new Model 16 plus guard lines in the regions of Models 1, 9 and 10, rather
    than extending Model 9 and 10 themselves. Recommend (a small model checks deeply and fast).
14. **Audit write order.** Set: store then trail. Lift: trail then store. Recommend.
15. **A stop never auto-expires** and never cancels an in-flight call. Recommend.
16. **Notifications** observed by the notifier sweep from the control run, so they are durable and
    routable. Recommend.
17. **Two modes, basic the default.** Basic: no keys, anyone with store or CLI access sets and
    lifts; audited: sections 5 to 8. Recommend basic as the default for a plain `bide stop`, since
    a stop that needs key enrolment is not available in the first minute of an incident; and
    recommend that the docs, `GUARANTEE` and the bank guide say a bank must run audited. Recommend
    that the mode lives in the control run, so it cannot be set quietly by process flags.
18. **What basic records.** Scope, reason, compensation flag, `by` as given (else OS user), host,
    advisory time, content-hash id, in the control run only; no signature, no trail, no
    anchoring, labelled "asserted, not authenticated" everywhere it is shown. Recommend this, and
    the plain statement that basic offers no tamper evidence. Fail-closed staleness stays the same
    in both modes (same default, same configurable bound); recommend that, since pause semantics
    must be identical and its cost is the same.
19. **Downgrade and upgrade.** Downgrade needs a signed quorum record, is written to the trail
    first, and is refused while any stop is active; a basic-configured process meeting an audited
    control run refuses to drive. Upgrade is `bide stop -upgrade` with a policy file; stops active
    at the upgrade stay active and need an audited lift. Recommend exactly this. Open for you:
    whether an upgrade should also be refused while a stop is active (I recommend allowing it, as
    the direction is stricter, and refusing would block an operator who wants audit mid-incident).
