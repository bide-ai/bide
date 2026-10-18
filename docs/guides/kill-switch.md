# Kill switch: stop every agent, or one tool, at once

A model misbehaves, or a provider has an incident, and you need automated action to stop now. The
kill switch is a durable emergency stop: a **stop** covers every run, one tool, or one agent, and
every process that shares the store holds the covered runs, as a pause, before any side effect that
has not begun. When the stop is lifted the runs carry on where they were. The design, with the
failure modes and the formal model, is in the [design note](../design/kill-switch.md); this page is
how to use what has shipped.

Shipped: the control run, the engine's holds, recovery, the signed audit trail with `bide audit
stops`, the model (`agent.StopControl`, `agent.StopSwitch`, `agent.WithStopSwitch`,
`audit.NewStopTrail`), and the operator command `bide stop` (and `bide-hsm stop` for a key in an HSM
or a KMS). `bide view` shows an active stop on every page and which runs it holds. A stop and its lift notify through the [notifier](#notifications). Not shipped yet: `bide audit notifications` coverage of them.

## What a stop does

- A stop is **a pause, not a failure**. A covered run returns `*agent.Stopped` (an `agent.Pause`):
  its journal is intact, and the run is driven again, with `Paused().RootRunID`, once the stop is
  lifted. `RecoverLoop` does that for runs nobody is waiting on.
- It is checked **before any side effect begins**: at a drive's start, at a turn's start (so the
  model proposes nothing new), before a call's claim, after the claim, and immediately before the
  call begins. A held call writes no attempt marker; if its claim had already been won, the attempt
  is recorded as not started, so a resume claims it afresh.
- A call that has **already begun finishes, and its result is journaled**: a stop never cancels a
  running call, because abandoning a call of unknown effect is what this runtime exists to avoid. To
  end a run itself, `agent.Cancel` has the same rule.
- A call whose attempt marker is written and which has no result is **never fired again by a stop,
  by its lift or by recovery**: it halts for its outcome as it always did.
- **Scopes**: global, a tool by exact name, a plan flow by its name, an agent by exact name (its
  `Identity.Actor`; **a run with no identity is never covered by an agent stop**, so give every agent
  an identity with `agent.WithIdentity` before relying on one, or use a tool or global stop). No
  patterns. A tool stop leaves the model's other turns running and holds only calls to that tool.
- **Compensation continues by default**: a saga's rollback is not held by a stop, since it undoes
  damage already done. A stop set with `Compensation: agent.CompensationBlock` holds the rollback
  too, which then resumes after the lift.
- **Approvals keep collecting** while a run is stopped, and nothing executes; approval deadlines
  keep running, so a long stop can turn a pending approval into an expiry.

## Setting it up

One `StopControl` and one `StopSwitch` per process, over the same journal store as the runs:

<!-- docsnip: skip a sketch: the store, model and tools are the deployment's own -->
```go
ctl, _ := agent.NewStopControl(journal, agent.WithStopMode(agent.StopBasic))
sw, _ := agent.NewStopSwitch(ctl) // polls once a second, holds side effects when it cannot read
a, _ := agent.New(model, journal, agent.WithTools(tools...), agent.WithStopSwitch(sw))

// A recovery pass lists nothing while a global stop is active.
go agent.RecoverLoop(ctx, journal, agent.ResumeAgent(a), agent.WithRecoverStop(sw))

e, _ := ctl.SetStop(ctx, agent.StopRequest{
	Scope: agent.StopScope{Kind: agent.StopTool, Name: "refund"},
	Reason: "refund provider incident", By: "dayna",
})
// ... later
ctl.LiftStop(ctx, e.ID, agent.LiftRequest{By: "dayna"})
```

A plan flow takes its switch with `Builder.WithStopSwitch` (or `plan.WithLoadedStopSwitch` for a
loaded one): a run is held at its start, and each node before its claim and, with the control run
read then, after the claim and before its body, where a won claim is recorded as not started.

An agent or flow without a switch **refuses to run** (`ErrConfig`) on a store whose control run holds a
record, so a process cannot silently disregard a stop. The check costs one read per store every few
seconds when no stop exists (the budget tests `TestBudget_*` in `agent/journal_x_test.go` and `TestLowering_NoHistoryReadsPerNode` in `plan` count it).

## Setting, listing and lifting a stop: `bide stop`

```
bide stop -db run.db -all -reason "model misbehaving"            # basic mode: no keys
bide stop -db run.db -tool refund -tool payout -reason "provider incident"
bide stop -db run.db -list                                       # the active stops; -json for scripts
bide stop -db run.db -show 3f9c2a10                              # one stop and its lift records
bide stop -db run.db -lift 3f9c2a10 -reason "provider recovered" -yes
```

- **There is no default scope.** Exactly one of `-all`, `-tool NAME`, `-flow NAME` and `-agent NAME` sets
  a stop (the last three repeat, one stop each). A bare `bide stop -db run.db` lists the active stops
  and exits 2: it never stops anything by accident. Setting a scope that is already stopped writes
  nothing and says so (the check reads the control run just before the write, but it cannot be atomic:
  two operators setting the same scope at the same instant can both write, and each of those stops needs
  its own lift).
- `-reason` is required, at most 1 KiB, and is shown in logs, notifications and the viewer: put no
  secret and no customer data in it. `-block-compensation` holds a saga's rollback too.
- **An `-agent` stop covers runs whose identity names that agent.** A run with no identity
  (`agent.WithIdentity`) is never covered by one, and the command says so every time. Use `-all` or `-tool`
  to cover those.
- **The prompt.** A stop is written without a prompt when `-yes` is given or standard input is not a
  terminal (an incident script must neither hang nor be refused), and asked about once on a terminal.
  A lift, `-apply-pending`, `-upgrade` and `-downgrade` ask on a terminal and are refused without one
  unless `-yes` is given. What will be written is always shown first. A cron job's `/dev/null` is not a
  terminal.
- **The journal** is `-db FILE` (it must already exist: a mistyped path never creates an empty
  journal) or `-postgres-env VAR`, the variable that holds the DSN. A DSN never goes on the command
  line, and a key never does either: `-key` names a key file, or for `bide-hsm stop` a key reference.
- **Exit statuses.** 0 success; 1 refused (the control did not accept the record, no such stop, the
  prompt was declined, the stops are unknowable); 2 usage; 3 the journal, the policy or the key could not
  be read or opened; 5 half done (below). The table is in `bide help stop` and the [CLI reference](../reference/cli.md#bide-stop).
- In basic mode the setter is `-as NAME` or your OS user, recorded with the host name, and shown as
  "asserted, not authenticated". `-list` always says which mode it read.

### Audited mode from the command line

Give the deployment's policy and sign with an enrolled key:

```
bide stop -db run.db -tool refund -reason "provider incident" \
    -stop-policy stop-policy.json -as ops -key ops.key
bide stop -db run.db -lift 3f9c2a10 -reason "provider recovered" \
    -stop-policy stop-policy.json -as risk -key risk.key        # one lifter's approval
```

`-stop-policy` is the policy document the deployment pins (`agent.StopPolicyDoc` as JSON: the approvers'
public keys, the setters and lifters, the lift quorum, the deployment ID); `-stop-root DIGEST` names the
root policy when the file is a later one. `-as` is your approver id and `-key` is your private key file
(`bide keygen` makes one; on Unix it must not be readable by group or others). With a key in an HSM or a
KMS use `bide-hsm stop`, which takes the same flags and a key reference after `-key` (a `pkcs11:` URI,
`awskms:`, `gcpkms:` or `azurekv:`); the PIN or credentials come from the paths `bide-hsm approve` uses.
Before it writes anything the command checks that your id and key are the ones the pinned policy enrols
for the role (once a policy transition has taken effect the control judges the record instead). A lift
prints how many lifts the stop holds and how many more it needs; the stop stays in force until the quorum
is met. Every audited write goes to the control run and the stop trail, in the order below.

The command refuses, with exit 1 and nothing written, a record the deployment's processes would ignore,
rather than report it as in force: a plain stop or lift on an audited deployment (give `-stop-policy`,
`-as` and `-key`), a policy file whose chain the store does not reach (the wrong root, or a fork: give
the deployment's root policy, or `-stop-root`), and the first audited write over stops that were set
plain (run `-upgrade` first, which adopts them). After a write it reads the control run back and fails if
the record was rejected or the stop is not in force in the policy's view. A deployment that moved back to
basic by a signed `-downgrade` is basic again.

- `-reaudit` appends to the trail what the control run has and the trail lacks.
- `-apply-pending` lists the signed records that sit on the trail and never took effect, each with its
  signer and age (and SUPERSEDED ones, which it leaves), asks, and makes them effective under your signed
  `apply` record. A signature cannot be revoked: applying makes an old pending lift effective.
- `-upgrade` moves a deployment to audited mode: it lists the active plain stops, sets each again
  signed, then records the change. Roll the policy file out to every process first; `-downgrade` records a
  move back to basic and is refused while any stop is active.

**Exit 5, half done.** An audited stop is written to the control run first and the trail second. If the
trail write fails the stop IS in force; run `bide stop -reaudit` (same `-db` and `-stop-policy`) to put it
on the trail. Do not repeat the stop: a repeat is a second stop with its own ID. A lift is written to the
trail first; if the control-run write then fails, the lift is on the trail and not in force, and
`bide stop -apply-pending` makes it effective. `bide audit stops` shows the same two states.

## What `bide view` shows

`bide view -db run.db` (or `-postgres-env`) reads the control run beside the runs and stays read-only.

- **A banner on every page** while a stop is in force: its scope, reason, who set it and how long ago,
  how many lifts it holds of how many it needs, whether compensation is held too, and the mode. It also
  appears, red, when the stops **cannot be judged** (a basic viewer on a signed deployment, a policy chain
  it cannot walk), when signed records **cannot be checked**, when records were **rejected** (a forged
  record, or one signed under a policy that does not enrol the signer), and when the control run **cannot
  be read**: the page never says "no stop" about a control run it could not read. Text from the store is
  cleaned of control and bidirectional characters, escaped and cut.
- **Plain stops under an audited viewer.** A viewer given the stop policy, on a deployment that has not
  moved to audited mode, shows "STOPPED FOR BASIC PROCESSES": the plain stops in force for the basic
  processes are ignored by audited ones, so part of the deployment is stopped and part is not. Move it with
  `bide stop -upgrade`, or run every process the same mode.
- **Freshness.** The control run is read at most once every couple of seconds for all pages, so a stop just
  set or lifted can take that long to show; the banner says when it was read (`read <time>`, and `read_at` in
  `/stops.json`).
- **`-stop-policy FILE`** (and `-stop-root`) read the control run as an audited deployment does and check
  every signature. Without it the viewer reads as a basic one and the banner says "signatures not
  verified".
- **Which runs a stop holds**, in the run list and on a run's page, as `held by stop <id>`. A global stop
  holds every run that is not over, not halted for an unknown outcome and not made of durable steps with
  no `run:start`; a flow stop the runs of that flow; a tool stop a run with a call to that tool that the
  journal shows no result for. A saga that is rolling back (a failed step) is only "may be held" under a
  stop that lets compensation continue, which is the default. **An agent stop can only say "may be held"**: an agent stop covers runs by
  the identity of the process that drives them (`Identity.Actor`), which the journal does not record. The
  state is derived from the journal: the viewer cannot tell a run a live process is holding from one no
  process is driving, and a held run is held at its next drive (a call that has begun finishes).
- `/stops.json` serves the banner as JSON. `bide stop -lift` shows how many runs the stop holds, by the
  same rule, before it asks (it lists at most 2000 unfinished runs and stops after 5 seconds, showing "at least N held
  (count incomplete)" and going on to the question, so a huge or slow store never holds up a lift).
- The control run (`@stop`) is not a run: it is not listed, counted or shown as needing attention.

## Basic and audited mode

The mode is each process's configuration (`WithStopMode`), never something the store says.

| | Basic | Audited |
|---|---|---|
| Who may stop or lift | anyone with store access | enrolled approvers; a lift needs a quorum |
| Records | plain, the operator as given | signed, bound to the deployment |
| Keys | none | the approvers' (files, HSM, KMS) |

**Basic mode trusts store access completely.** Anyone who can write to the store can set a stop,
lift one, or write records a basic process does not check. It is an emergency brake for one team and
gives no tamper evidence. Banks run audited mode (`WithStopPolicy`, `WithStopRoot`,
`WithStopVerifier`): every record is signed by an enrolled approver and names the deployment, a lift
needs `LiftQuorum` distinct lifters (one not the setter, if the policy says so), and the policy
changes only by a signed transition approved by the policy in force, so offboarding or rotating a
key never drops a stop. Pin the deployment's **root** policy (a process that pins a later one must
name the root, `WithStopRoot`); a pin the chain does not reach, or a basic process that meets
signed stops, makes the stops *unknowable*, and the engine then holds side-effect calls a stop
could cover until the process is configured. That is the safe direction. Forged records in an
audited deployment are only rejected (`StopState.Rejected`). See the [design note](../design/kill-switch.md#7a-modes-basic-and-audited).

## Notifications

A stop that is set, and a stop that is lifted, are events a person must hear of: `stop_set` (key
`stop:<id>`) and `stop_lifted` (key `stop:<id>:lifted`) go through the [notifier](notifications.md), so
delivery has the notification trail's claim, acknowledgement, retry, overdue and escalation, and survives
the death of the process that set the stop. They are observed from the control run, never raised by a drive,
by two paths that name an event by the same key:

<!-- docsnip: skip a sketch: the journal, webhook, trail and control are the deployment's own -->
```go
// The durable path: a periodic sweep reads the control run (stops set or lifted within the last
// WithNotifyStopsWithin, default 7 days).
agent.NotifyPending(ctx, journal, webhook, trail, agent.WithNotifyStops(ctl))

// The prompt path: a process with a switch learns of a change within its poll interval.
n, _ := agent.NewStopNotifier(journal, agent.WithNotifier(webhook, trail), ctl)
sw, _ := agent.NewStopSwitch(ctl, agent.WithStopObserver(n.Observe))
```

- The trail's claim on the event key is the one dedupe: any number of processes, and both paths, send one
  notification. A `stop_set` that is lifted before anyone was told of it owes nothing and is not sent; the
  lift is.
- An audited stop is lifted by its quorum, so `stop_lifted` is raised once, when the quorum lands, and a
  partial lift raises nothing (it is in `bide stop -list`). The all-clear of a `stop_set` has reason `lifted`.
- Only stops the control accepts are news: a forged or foreign record raises nothing.
- Routing (`WithNotifyRoute(agent.NotificationStopSet, ...)`) and escalation (`WithNotifyEscalation` for
  `stop_set`) apply: a stop nobody acknowledged reaches the next receiver. The payload (`stop`) carries the
  scope, the setter, the reason, the mode and the compensation flag, never a key or a secret.
- The observer never delays or fails a check point. The switch hands each change to it through a slot that
  keeps only the latest state, on a goroutine of its own: an observer that blocks costs its later calls and
  nothing else, and one that panics ends only its call. A notifier that blocks or panics costs the
  notification, which the sweep finds again.
- `bide stop` does not notify: it has no notifier. A deployment runs the sweep (or a `StopNotifier`) in the
  process that already holds its notifier, and checks the receiver with `bide notify test -kind stop_set`.
  `bide audit notifications` does not yet check that every stop was told (a follow-up).

## The audit trail

An audited control writes every record to a **stop trail** too (`agent.WithStopTrail`,
`audit.NewStopTrail` over an `audit.EventStore`): a salted, Merkle-committed event leaf per signed
record, whose heads an `audit.AuditedEventStore` signs and anchors like any other trail. The control
run enforces a stop; the trail is its evidence. The order keeps the evidence at least as strong as
the effect: a **stop** is written to the control run first, so it is in force at once, and to the
trail second; **every other record** (a lift, a policy transition, the root, a mode change) is
written to the trail first, so none is in force without its evidence. A failed trail write is a
`*agent.StopTrailError`: for a stop it says the stop *is* in force and carries the stop, and
`StopControl.Reaudit` appends what the trail lacks; for any other record nothing was written. Before
a record that rests on earlier ones (a lift names its stop, a transition its root) goes to the trail, the
control run's records the trail lacks are appended first, in store order, so the trail never holds a
lift ahead of its stop. Only records the control accepts are appended: a forged record written straight
into the control run is a finding of `bide audit stops`, never evidence.

**Keeping the audit passing.** `bide stop` appends signed leaves to the trail and nothing else: one key
touch in an incident. A signed head over the trail, which is what attests that nothing was deleted, is
made by a separate, schedulable job, and **`bide audit stops` fails by default on any record after the
newest signed head**. So the deployment must run, on a schedule shorter than the longest unattested tail
it accepts:

```
bide audit stops -db run.db -stop-policy stop-policy.json \
    -sign-head -log-key log.key -pubkey log.pub -head-out stop-heads.jsonl
```

It verifies the trail against the policy and the control run first, signs exactly the records it
verified (never a second read of the store), and appends the head as one JSON line to the head file,
which `-sth stop-heads.jsonl` reads back. The heads already in the file are verified with the trail,
so a trail that shrank or was rewritten since an earlier head is refused, not re-signed; with nothing
new it signs nothing. Exit statuses are `bide audit`'s (1: the trail does not verify, nothing is signed;
3: the key could not sign or the file could not be written). `-log-key` is a key file; a log key held in
an HSM or a KMS is signed by `bide-hsm audit stops`, below. The head file is not itself anchored: publish
it somewhere the deployment's store writers cannot edit.

With the log key in an HSM or a KMS, run the same job with `bide-hsm`:

```
bide-hsm audit stops -db run.db -stop-policy stop-policy.json \
    -sign-head -log-key 'awskms:arn:aws:kms:us-east-1:111122223333:key/1234abcd-...' \
    -pubkey log.pub -head-out stop-heads.jsonl
```

It is `bide audit stops` (the verification, the head file, the refusals and the exit statuses are the same
code), with `-log-key` naming the key by reference (a `pkcs11:` URI, `awskms:`, `gcpkms:` or `azurekv:`, as
for `bide-hsm stop`) and the key flags of `bide-hsm approve` (`-alg`, `-allow-extractable`,
`-allow-software-key`, `-verify-attestation`) added. The reference holds no secret: a PKCS#11 PIN comes
from `pin-source`, and a KMS key from the SDK's credential chain. The key is opened only when there is
something to sign, after the trail has verified and the key is checked against `-pubkey`: a trail that does
not verify signs nothing and opens no key, and a run with nothing new (the newest head already commits to
every record) opens none either. `bide-hsm audit` runs the `stops` verb only; the other `bide audit` verbs
sign nothing.

`bide audit stops` verifies the trail offline: each record must verify under the policy the
deployment pinned (`-stop-policy`, the policy document with the approvers' public keys) and the chain
of signed transitions from its root; with signed heads (`-sth`, `-pubkey`) a deleted or reordered
record is found; and against the store (`-db`, `-postgres-env`) every record of the control run must
be in the trail and the reverse (a stop written around the trail, or a row deleted from the store).
Basic mode keeps no trail. Records after the newest signed head are not attested: with no
`-max-unattested` any such tail fails (whoever can write the trail's store could have deleted the newest
records unseen), and `-max-unattested` accepts a tail younger than that.

A crash between "trail first" and the control-run write leaves a signed lift (or transition, root, mode
change) on the trail that never took effect. That is not a forgery: `bide audit stops` lists it as
**UNAPPLIED** and passes, and `StopControl.ApplyPending` writes it to the control run if the control would
accept it. That is an **operator action**: a signature cannot be revoked, so a signed lift that sat on
the trail can be made effective by whoever runs it, however old. `PendingRecords` lists each pending
record with its signer and age first. `ApplyPending` is signed: the applier (an approver the policy
enrols) writes an `apply` record, to the trail and the control run, naming the digests applied, before it
applies any, so who applied what, and when, is evidence; an applier the policy does not enrol is refused
before anything is written. It applies only the records at the end of the trail; one that a stored record follows (the operator tried
again, and the new record went through) is **SUPERSEDED**: listed, not a failure, not in the stops shown,
and not applied, since applying it would put it after records the trail has behind it. A record on the
trail that the fold rejects, or a stop on the trail that the control run lacks, is a problem.

## How fast, and when the control run cannot be read

- A call about to begin reads the control run itself, so a stop committed before that read is seen:
  the bound is the store's own visibility (on Postgres, read the control run from the primary).
- A drive's start, a turn's start and a call before its claim use the switch's last read, which is
  at most `WithStopPoll` old (default 1s).
- When the control run cannot be read, a side-effect call runs only on a read no older than
  `WithStopStaleness` (default 15s), else it is **held** (`StopCauseUnreadable`): failing closed
  costs almost nothing, because the control run is in the store a call needs anyway. A read-only
  call and a model turn are never held for it (with nothing read yet they proceed as if no stop
  existed). Reads are one at a time and honor each caller's context: a slow read delays the callers that
  need a read, never the cached checks. `WithStopErrors` receives every failed read.

The formal model ([model 16](../../spec/tla/README.md#model-16-the-kill-switch)) checks these
rules, with counterexample configurations for each one removed; it models a staleness of zero, and
the bounded cache is covered by the Go tests.
