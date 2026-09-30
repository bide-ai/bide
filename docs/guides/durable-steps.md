# Composing durable work: steps, parallel fan-in, and sagas

The agent loop journals its own model turns and tool calls. But the same durable substrate is
exposed directly, so you can wrap *your own* work in the same at-most-once, crash-safe,
independently-provable guarantee. This is the "Option B" authoring model: write plain Go control
flow (`if` / `for` / functions), and name the operations that must survive a crash. There is no
graph DSL; the graph is a derived output (see [Debugging](debugging.md) `RenderMermaid`).

Three primitives cover the common shapes, all built on the `Durable` port
([extension points](../reference/extension-points.md)):

- **`Step[T]`**: one named durable step.
- **`Parallel[T]` / `Task[T]`**: durable fan-out/fan-in.
- **`RunSaga` + `CompensatedFunc`**: transactional agents with reverse-order rollback.

## `Step[T]`: one named durable operation

<!-- docsnip: api agent -->
```go
func Step[T any](ctx context.Context, d Durable, runID, name string,
    fn func(context.Context) (T, error), opts ...StepOption) (T, error)
```

`Step` runs `fn` as a named durable step keyed by `(runID, name)` and returns its typed result.
On resume, a completed step returns its **recorded** result without re-running `fn`. It is the
building block the agent loop itself is made of, exposed for your own orchestration.

A step is a side effect unless you say otherwise, and it gets the same guarantee as a tool call:
it runs **at most once**. An attempt marker is journaled before `fn` runs, so if the process dies
after `fn`'s effect and before its result is recorded, the resumed step returns `*OutcomeUnknown`
instead of running `fn` again; `ResolveHaltRef` (with the halt's `Ref()`) records the confirmed
outcome. The same holds when `fn` returns an error, since a failed call may still have taken
effect. A step cancelled (or whose store fails) after its marker is written and before `fn` is
called does not call `fn`, and records that the attempt did not start, so the next call runs `fn`
under a new marker instead of halting; only a process that dies in that gap leaves a halt. A step
that is safe to re-run declares it with `StepSafety`, and then simply re-runs after
a crash or an error:

<!-- docsnip: setup ctx context.Context; store agent.Durable; runID string; type Invoice struct{}; type Reservation struct{}; id, sku string; billing interface{ Lookup(context.Context, string) (Invoice, error) }; inventory interface{ Reserve(context.Context, string) (Reservation, error) } -->
```go
inv, err := agent.Step(ctx, store, runID, "fetch-invoice",
    func(ctx context.Context) (Invoice, error) { return billing.Lookup(ctx, id) },
    agent.StepSafety(agent.Safety{ReadOnly: true}))

res, err := agent.Step(ctx, store, runID, "reserve", // at most once; halts on an unknown outcome
    func(ctx context.Context) (Reservation, error) { return inventory.Reserve(ctx, sku) })
```

`Step` is a package function, not a method, because Go methods cannot add type parameters. The
result is journaled as a `StepValue` record, so it shows up in `RenderMermaid` as `step: <name>`
and is independently provable via `audit.ProveStep` (see [Audit](audit.md)). `name` must be
unique within the run: a second `Step` with the same `(runID, name)` returns the first one's
recorded result. It shares the run's journal with the engine's own records, so a name that starts
with a prefix the engine reserves for them (`@`, `run:`, `tool:`, `attempt:`, `approval:`,
`approval-tally:`, `signal:`, `await-timeout:`, `await-resolved:`, `timer:`, `interrupt:`, `chan:`,
`chanack:`, `turn/`, `start/`, `from/`, `audit:`; see `agent.IsReservedStepName`) is `ErrConfig`,
for `Step` and for a `Parallel` task alike.

The same memoized journal is the idempotency guard the [Messaging](messaging.md) webhook pattern
uses (through `Run` and `SendOnce`) to make a redelivered inbound event replay instead of re-fire.

## `Parallel[T]` / `Task[T]`: durable fan-in

<!-- docsnip: api agent -->
```go
type Task[T any] struct {
    Name   string
    Fn     func(context.Context) (T, error)
    Safety Safety // as StepSafety: the zero value is a side effect
}

func Parallel[T any](ctx context.Context, d Durable, runID string,
    maxConcurrency int, tasks ...Task[T]) ([]T, error)
```

`Parallel` runs each `Task` concurrently, each as its own durable `Step`, and returns the results
**in task order** (not completion order). It is the durable, auditable fan-in that a compliance
pipeline wants: run several independent checks at once (sanctions, credit, fraud), each crash-safe,
each result committed to the journal and provable on its own, then aggregate.

- Each task's `Name` is its durable memoization key within the run, so it **must be unique** across
  the tasks in one call; a repeated name returns `ErrConfig` before any task runs.
- Each task is a `Step` with the task's `Safety`: mark checks and lookups `ReadOnly`, and leave a
  side effect at the zero value so it runs at most once.
- **All tasks run even if some fail**, so a failed check never hides the others. The returned error
  joins every task's error (`errors.Join`) and is `nil` only if all succeeded. Succeeded tasks are
  memoized on resume; a failed `ReadOnly` task re-runs, and a failed side effect halts.
- `maxConcurrency` caps in-flight tasks; `<= 0` means one goroutine per task.

<!-- docsnip: setup ctx context.Context; store agent.Durable; runID string; type CheckResult struct{}; runSanctions, runPEP, runAdverseMedia func(context.Context) (CheckResult, error) -->
```go
checks := []agent.Task[CheckResult]{
    {Name: "sanctions_check",     Fn: runSanctions,    Safety: agent.Safety{ReadOnly: true}},
    {Name: "pep_check",           Fn: runPEP,          Safety: agent.Safety{ReadOnly: true}},
    {Name: "adverse_media_check", Fn: runAdverseMedia, Safety: agent.Safety{ReadOnly: true}},
}
results, err := agent.Parallel(ctx, store, runID, 0, checks...) // 0 = unbounded concurrency
```

This is deliberately a thin primitive over the journal, not a graph engine. Dynamic, model-driven
routing stays in plain Go and sub-agents; `Parallel` covers the **static** fan-out/fan-in that a
governed workflow's "parallel checks, then decide" stage is made of. The full worked flow (parallel
durable checks, then a governed decision, then an offline proof) is
[`examples/govern/compliance`](../../examples/govern/compliance/main.go).

> Related but distinct: the agent loop already runs a *single turn's* tool calls concurrently
> (bounded by `SetMaxConcurrency`). `Parallel` is for fan-out you author yourself outside a model
> turn. See the parallel-tool concurrency notes in [KNOWN-LIMITATIONS.md](../KNOWN-LIMITATIONS.md).

## Sagas: transactional agents with reverse-order compensation

A saga is the sequential/hierarchical transactional tier: "charged the card and booked the flight,
then failed on the hotel, so cleanly refund and cancel." A tool declares how to undo its write with
the `Compensator` port ([extension points](../reference/extension-points.md)); `CompensatedFunc` builds a
typed tool that carries both the forward action and its undo:

<!-- docsnip: setup type BookArgs struct{}; type Booking struct{ PNR string }; airline interface{ Book(context.Context, BookArgs) (Booking, error); Cancel(context.Context, string) error } -->
```go
book := agent.CompensatedFunc("book_flight", "book a flight", agent.Safety{},
    func(ctx context.Context, in BookArgs) (Booking, error) { return airline.Book(ctx, in) },
    func(ctx context.Context, in BookArgs, out Booking) error { return airline.Cancel(ctx, out.PNR) })
```

Run the agent with `RunSaga` instead of `Run`. If a step fails after earlier compensatable writes
succeeded, `RunSaga` rolls those writes back **in reverse order** (recursing through sub-agent
trees) and returns `*SagaAborted`:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string -->
```go
_, err := a.RunSaga(ctx, runID, input)
var aborted *agent.SagaAborted
if errors.As(err, &aborted) {
    // aborted.Cause is why it rolled back; the completed writes were compensated.
}
```

`RunSagaResult` is the `*Result`-envelope counterpart (see [Debugging](debugging.md)), and
`StreamSaga` is the streaming counterpart of `Stream`. `SagaAborted` unwraps to its cause, so
`errors.Is` against a sentinel still works.

### How it survives a crash

The abort is derived from the journal: a saga step failure writes a durable `StepSagaFail` record,
so a crash at any point resumes correctly. On re-entry a recorded failure sends the run straight to
rollback, and **each compensation is itself a durable memoized step**, so it runs at-most-once if it
completes. This split is the precise contract, proven adversarially in `saga_dst_test.go` (see
[Testing](../testing/testing.md)):

- The **forward** non-idempotent effect is **at-most-once** (halt on unknown outcome).
- **Compensators** are **at-least-once**: memoized so they run once if they complete, but a crash
  mid-compensation re-runs them. This is why a `Compensate` must be idempotent.
- **A `CompensatedFunc` undo sees the arguments the forward call decoded.** It decodes the call's
  recorded arguments with the same strict decoder the forward call used. A call journaled before
  tool arguments decoded strictly may hold arguments only `encoding/json` accepts (a case variant,
  an unknown name); that call decoded them with `encoding/json`, so compensation does too.
- **Undo sees the arguments the tool accepted, after tool middleware.** When a middleware changes
  a compensable call's arguments in a saga, the arguments the tool receives are journaled before
  it runs, and compensation undoes those (a charge rewritten from 5 to 500 is refunded 500, even
  when its outcome was resolved with `ResolveHaltRef`). A rollback that re-runs a retry-safe call to
  learn its result runs it through the same middleware. Unchanged arguments journal nothing. A
  journal written before this has no such record, and compensation there uses the model's
  arguments. A middleware must rewrite a retry-safe compensable call's arguments the same way
  every time, since the first record is kept.

### The boundaries (read these before relying on it)

These are stated in full in [KNOWN-LIMITATIONS.md](../KNOWN-LIMITATIONS.md); in brief:

- **A saga step must be atomic.** The *failing* step itself is not compensated (there is no recorded
  result to drive `Compensate`), so a forward step must not leave a partial external side effect
  before returning an error. Make forward steps all-or-nothing or idempotent.
- **Unknown-outcome resume halts, it does not auto-roll-back.** If a non-retriable step crashes
  after its attempt marker but before any result, `RunSaga` returns `*OutcomeUnknown`: a human decides,
  because you cannot safely roll back a step that may have committed.
- **Rollback covers calls the abort cut off.** Rollback walks every tool call the model made, not
  only those that returned. A call of the failing turn that had not started when the step failed
  never starts. A side effect that started but has no recorded outcome (a sibling the failure
  cancelled mid-call) stops the rollback with a `*OutcomeUnknown` in `SagaAborted.CompensateErr`;
  resolve it with `ResolveHaltRef` and call `RunSaga` again to finish the rollback. A retry-safe call
  with a compensator is run again to learn its result, then undone. A sub-agent call is rolled back
  into whether or not it returned; a sub-agent whose own saga failed rolled itself back first, and
  if that rollback stopped (a crash, an unknown outcome, a failing compensator) the parent's stops
  there too and resumes it on the next `RunSaga`, so `SagaAborted` covers the whole tree. A
  completed write with no compensator, idempotent or not, is listed in `SagaAborted.Uncompensated`.
  So is any call whose tool is no longer registered when the rollback runs (its compensator and
  safety are unknown), and one of those with an attempt marker and no result stops the rollback
  with a `*OutcomeUnknown`.
- **A saga resumes as a saga.** A run's first drive records whether it runs as a saga, and resuming
  an unfinished saga through `Run` (or a run through `RunSaga`) is `ErrConfig`.
- **A reconciled failure aborts.** A crash between a step's failure and its record leaves the step
  with an unknown outcome. Resolving it with `ResolveHaltRef` and `Outcome{IsError: true}` records a failed
  step, and the next `RunSaga` rolls back as it would have had the failure been recorded. (A
  human's denial of a gated call is not a failure: the model reacts to it, as outside a saga.)
- **A step of unknown outcome holds its turn.** While a sub-agent step halts on an unknown
  outcome, no later call of the same turn starts: the step may prove to have failed, and the saga
  must not run a step a run that never crashed would not have reached.
- **Compensation is hierarchical, not concurrent.** Rollback recurses through a sub-agent *tree*
  (one causal order). Truly concurrent agents mutating shared state out of order need the provable
  convergence of the governance tier ([Governance](governance.md)), not a saga.

`SubAgent(name, description, sub)` composes agents into that durable tree: give the sub-agent the
**same** `Durable` store as the parent for a unified journal, and a crash anywhere in the tree
resumes the whole tree precisely (completed sub-agents reused, the in-flight one resumed, and
`OutcomeUnknown` / `ApprovalPending` / `SagaAborted` from deep in the tree propagating up). A
pause raised inside a sub-agent surfaces from the parent's own `Run` (match it with `AsPause` or
`errors.As`); resolve it against the **sub-run's** journal, `Paused().RunID` (with the halt's `Ref()`,
or the approval's `ToolUseID`), then re-run the run named by `Paused().RootRunID`, the top-level
run, with the root agent to resume down the path. An `OutcomeUnknown` or `ApprovalPending` from a
sub-agent propagates whatever the calling tool's safety; any other pause (`InterruptPending`,
`SignalPending`, `TimerPending`) needs a retry-safe calling tool, as it does anywhere. A `Sleep` in
a sub-agent schedules its wake (`Wake.RunID` the sub-run, `Wake.RootRunID` the root) for the root
run. When several calls of one turn pause, the run returns an `OutcomeUnknown` ahead of any other
pause (the run
cannot go on until it is resolved, whatever else is answered), and otherwise the first call's
pause. A call that loses its answer (`ErrToolOutcomeUnknown`) is held the same way as a halt: its
siblings in flight finish. A storage failure inside a sub-agent, or a lost answer in it, is not the
sub-agent's answer: the parent records nothing for the call and returns the error, and resuming
re-enters the sub-run.

A sub-run's ID is `agent.SubRunID(parentRunID, toolUseID)`: the parent's run ID, `>`, and the
tool-use ID encoded so that no ID a model sends can name another call's sub-run. A session's
journal and turn runs are named the same way, `"<session id>>@session"`, `"<session id>>@turn/<n>"`
and `"<session id>>@event/<encoded key>"`. A top-level run ID therefore may not contain `>` (`Run`,
`RunSaga`, `Stream` and the rest return `ErrConfig`), so none can name a sub-run or a session's
run; `/` is fine (`tenant/123`). `agent.IsSubRun` and `agent.IsSessionRun` tell them apart.
`Recover` skips sub-runs, which their root's re-run resumes, and session runs, which the session
resumes when the turn's message is sent again.

### Clearing an `OutcomeUnknown`: `ResolveHaltRef`

An `OutcomeUnknown` (formerly `ResumeHalt`) is deliberately terminal until a human confirms the real
outcome: the runtime cannot know whether the non-idempotent side effect (a charge, a send) actually
committed. Once you have verified it out of band, `ResolveHaltRef` is the sanctioned escape. It
records the missing result under the halted operation's key (the same journal key the loop or
`Step` uses), so a re-run proceeds past the halt instead of halting again:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; store agent.Durable; input string; err error; msg agent.Message -->
```go
if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
    // operator confirmed the charge did go through
    _ = agent.ResolveHaltRef(ctx, store, halt.Ref(), agent.Outcome{Result: "charged (confirmed)"})
    msg, err = a.Run(ctx, halt.RootRunID, input) // resumes past the halt
}
```

`halt.Ref()` names the run, the operation (`Op`: a tool call, `OpTool`, with its tool-use ID, or a
`Step`, `OpStep`, with its name), and the halt's `Cause`. Pass the result value an actual call would
have returned, and `IsError: true` if the verified outcome was a failure the model should react to
(in a saga, a failed step, which rolls the saga back). It is idempotent (the first result for an
operation wins), so a retry or a racing driver records it at most once. A tool call's result and a
step's are separate journal records, so it refuses an operation that only the other kind attempted.

`ResolveHaltRef` will not resolve an effect a driver may still be running. `HaltContended` means
another driver of the same run won the claim while this one ran (a node that took over after a lease
lapsed); `HaltCrashed` means the halting driver knew of no live claimant, which is not proof that
none is running. So, whatever the cause:

- On a store that leases runs (`MemStore`, `store/postgres`), `ResolveHaltRef` takes the root run's
  lease while it resolves and returns `*HaltInFlight` while a driver holds it. Only drivers that
  lease the run (`Lease`, `Recover`, `RecoverLoop`) are seen; a plain `Run` holds no lease.
- On a store that cannot (`store/sqlite`, a custom `Durable`), it requires `WithMinHaltAge`, so the
  halt is resolved only once no driver can still be running it.
- A `HaltContended` halt always requires `WithMinHaltAge`.
- `WithoutLiveDriverCheck()` skips the first two, for an operator who knows no driver is running
  (every worker stopped).

An operation that already has a different outcome (an earlier resolution, or the result the live
driver recorded) is refused with `*HaltAlreadyResolved` (`ErrAlreadyResolved`, which wraps
`ErrConfig`), which reports the outcome that stands; the same outcome again returns nil. A
`HaltRef` built by hand must state its cause.

`ResolveHalt(ctx, store, runID, toolUseID, result, isError, opts...)` and `ResolveStepHalt` remain
as transitional wrappers that take the cause as `HaltCrashed`, with the same checks.

### Resolving without a human: reconcilers

The verifier does not have to be a person. Most systems that lack an idempotency key still leave a
queryable record (a sent-message id, a row, a log line), so a reconciler can query that record,
decide, and call `ResolveHaltRef` itself. Two options make that safe:

- **`WithMinHaltAge(d)`** refuses to resolve a halt younger than `d`, measured from the live
  attempt marker (`OutcomeUnknown.AttemptedAt`; an attempt recorded as never started does not
  count) to now. A provider's record can lag the send by seconds, so a
  reconciler that queries too early reads "absent" and re-fires the exact effect the halt prevents.
  A minimum age keeps "unknown" unknown until the record has had time to appear; too soon returns
  `*HaltTooYoung`, so the reconciler waits and retries. A marker with no usable timestamp (none, or
  a zero or negative one, which the engine never writes) is `ErrConfig`: the age cannot be known,
  so the halt is not resolved blind. A marker stamped in the future counts as too young.
- **`Outcome.Evidence`** (or the `WithEvidence(v)` option) records the resolution as reconciled and stores what was read to decide,
  signed alongside the outcome (`Record.Reconciled` / `Record.Evidence`). The journal is the audit
  record, so a reconciled outcome that did not say it was reconciled, and on what basis, would be a
  hole in the very thing the trail exists to protect. With it, a later reader tells a reconciled
  step from a clean one and re-checks the evidence.

<!-- docsnip: setup ctx context.Context; store agent.Durable; err error; func providerSays(*agent.OutcomeUnknown) (bool, json.RawMessage) -->
```go
if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
    sent, record := providerSays(halt) // query the system of record
    if sent {
        _ = agent.ResolveHaltRef(ctx, store, halt.Ref(),
            agent.Outcome{Result: "sent (reconciled)", Evidence: record}, // the basis, signed with the outcome
            agent.WithMinHaltAge(30*time.Second))                         // do not decide before the record can settle
    }
}
```

The human stays the fallback for the genuinely unknowable case, not the default.

## Durable timers: `Sleep` / `WaitUntil` and the `Waker`

An agent often has to wait: for a deadline, a cool-off, a scheduled follow-up. `Sleep(ctx, name, d)`
and `WaitUntil(ctx, name, until)` make that wait durable. Called from inside a retry-safe tool, they
journal the wake time once (at-most-once by name) and pause the run with `*TimerPending`, the same durable
pause as `Interrupt`. Because the wake time is journaled on the first call and memoized, a resumed or
crash-recovered run waits to the same absolute instant rather than restarting the clock; no goroutine
is held blocked across the wait.

```go
wait := agent.Func("cooldown", "wait before retrying", agent.Safety{ReadOnly: true},
    func(ctx context.Context, _ struct{}) (string, error) {
        if err := agent.Sleep(ctx, "cooldown", time.Hour); err != nil {
            return "", err // *TimerPending propagates out of Run; the run is paused durably
        }
        return "resumed", nil
    })
```

Re-invoking `Run` with the same runID at or after the wake time resumes past the `Sleep`. What
re-invokes it is a **`Waker`**, the time-driven sibling of the inbound event trigger in
[Messaging](messaging.md): the SDK provides the durable, at-most-once timer and its resume safety,
and the trigger is pluggable. Bind one with `agent.WithWaker(ctx, w)` and `Sleep` registers its wake
automatically, through `Schedule(ctx, agent.Wake{RunID, RootRunID, Name, FireAt})`. A `Schedule`
that fails fails the run with an error wrapping `ErrStorage` instead of pausing it (a paused run
with no wake registered might never wake), and the tool call records nothing, so re-driving the run
(`RecoverLoop` does, on its next pass) reaches the `Sleep` again and schedules again. `MemWaker` is
the reference in-process implementation:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; store *sqlite.Store; savedInput string -->
```go
ctx, cancel := context.WithCancel(ctx)
var w *agent.MemWaker
w = agent.NewMemWaker(func(ctx context.Context, runID string) error {
    _, err := a.Run(agent.WithWaker(ctx, w), runID, savedInput) // resume; may sleep again
    return err
})
done := w.Start(ctx, time.Second, nil) // tick: resume every run whose timer is due

// On shutdown: stop the loop and wait for it (including any Fire in flight) before closing the
// store the resumed runs write to.
cancel()
<-done
store.Close() // e.g. a store/sqlite or store/postgres Store
```

`Start` returns a channel that closes once the loop has stopped, after any in-flight `Fire` has
returned. Waiting on it before closing the store keeps a resume from writing to a closed store.

Boundaries: `MemWaker` is a local-dev default, not a durable scheduler. Its in-memory timer set is
lost on process exit, so the wake times must also live in the journal (they do), and a restarted
deployment rebuilds pending wakes by scanning runs or hands the trigger to an external scheduler
(cron, a queue). Tests inject a clock with `agent.WithClock` to advance time deterministically. This
is the piece that makes an always-on ambient agent turnkey: a durable wait plus a trigger, with
at-most-once and crash-resume intact across the wait.
