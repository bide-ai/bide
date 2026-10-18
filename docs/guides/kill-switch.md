# Kill switch: stop every agent, or one tool, at once

A model misbehaves, or a provider has an incident, and you need automated action to stop now. The
kill switch is a durable emergency stop: a **stop** covers every run, one tool, or one agent, and
every process that shares the store holds the covered runs, as a pause, before any side effect that
has not begun. When the stop is lifted the runs carry on where they were. The design, with the
failure modes and the formal model, is in the [design note](../design/kill-switch.md); this page is
how to use what has shipped.

Shipped: the control run, the engine's holds, recovery, the signed audit trail with `bide audit
stops`, and the model (`agent.StopControl`, `agent.StopSwitch`, `agent.WithStopSwitch`,
`audit.NewStopTrail`). Not shipped yet: `bide stop`, the viewer banner and the notifications; until
they are, a stop is set and lifted from Go (`StopControl.SetStop`, `LiftStop`).

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
