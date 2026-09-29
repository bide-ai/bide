# Composing durable work: steps, parallel fan-in, and sagas

The agent loop journals its own model turns and tool calls. But the same durable substrate is
exposed directly, so you can wrap *your own* work in the same at-most-once, crash-safe,
independently-provable guarantee. This is the "Option B" authoring model: write plain Go control
flow (`if` / `for` / functions), and name the operations that must survive a crash. There is no
graph DSL; the graph is a derived output (see [DEBUGGING.md](debugging.md) `RenderMermaid`).

Three primitives cover the common shapes, all built on the `Durable` port
([EXTENSION-POINTS.md](../reference/extension-points.md)):

- **`Step[T]`**: one named durable step.
- **`Parallel[T]` / `Task[T]`**: durable fan-out/fan-in.
- **`RunSaga` + `CompensatedFunc`**: transactional agents with reverse-order rollback.

## `Step[T]`: one named durable operation

```go
func Step[T any](ctx context.Context, d Durable, runID, name string,
    fn func(context.Context) (T, error), opts ...StepOption) (T, error)
```

`Step` runs `fn` as a named durable step keyed by `(runID, name)` and returns its typed result.
On resume, a completed step returns its **recorded** result without re-running `fn`. It is the
building block the agent loop itself is made of, exposed for your own orchestration.

A step is a side effect unless you say otherwise, and it gets the same guarantee as a tool call:
it runs **at most once**. An attempt marker is journaled before `fn` runs, so if the process dies
after `fn`'s effect and before its result is recorded, the resumed step returns `*ResumeHalt`
instead of running `fn` again; `ResolveHalt` (with the step name as the ID) records the confirmed
outcome. The same holds when `fn` returns an error, since a failed call may still have taken
effect. A step that is safe to re-run declares it with `StepSafety`, and then simply re-runs after
a crash or an error:

```go
inv, err := agent.Step(ctx, store, runID, "fetch-invoice",
    func(ctx context.Context) (Invoice, error) { return billing.Lookup(ctx, id) },
    agent.StepSafety(agent.Safety{ReadOnly: true}))

res, err := agent.Step(ctx, store, runID, "reserve", // at most once; halts on an unknown outcome
    func(ctx context.Context) (Reservation, error) { return inventory.Reserve(ctx, sku) })
```

`Step` is a package function, not a method, because Go methods cannot add type parameters. The
result is journaled as a `StepValue` record, so it shows up in `RenderMermaid` as `step: <name>`
and is independently provable via `audit.ProveStep` (see [AUDIT.md](audit.md)). `name` must be
unique within the run: a second `Step` with the same `(runID, name)` returns the first one's
recorded result.

`Step` is the idempotency guard the [MESSAGING.md](messaging.md) webhook pattern uses to make a
redelivered inbound event replay instead of re-fire.

## `Parallel[T]` / `Task[T]`: durable fan-in

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
[`examples/compliance`](../../examples/compliance/main.go).

> Related but distinct: the agent loop already runs a *single turn's* tool calls concurrently
> (bounded by `SetMaxConcurrency`). `Parallel` is for fan-out you author yourself outside a model
> turn. See the parallel-tool concurrency notes in [KNOWN-LIMITATIONS.md](../KNOWN-LIMITATIONS.md).

## Sagas: transactional agents with reverse-order compensation

A saga is the sequential/hierarchical transactional tier: "charged the card and booked the flight,
then failed on the hotel, so cleanly refund and cancel." A tool declares how to undo its write with
the `Compensator` port ([EXTENSION-POINTS.md](../reference/extension-points.md)); `CompensatedFunc` builds a
typed tool that carries both the forward action and its undo:

```go
book := agent.CompensatedFunc("book_flight", "book a flight", agent.Safety{},
    func(ctx context.Context, in BookArgs) (Booking, error) { return airline.Book(ctx, in) },
    func(ctx context.Context, in BookArgs, out Booking) error { return airline.Cancel(ctx, out.PNR) })
```

Run the agent with `RunSaga` instead of `Run`. If a step fails after earlier compensatable writes
succeeded, `RunSaga` rolls those writes back **in reverse order** (recursing through sub-agent
trees) and returns `*SagaAborted`:

```go
_, err := a.RunSaga(ctx, runID, input)
var aborted *agent.SagaAborted
if errors.As(err, &aborted) {
    // aborted.Cause is why it rolled back; the completed writes were compensated.
}
```

`RunSagaResult` is the `*Result`-envelope counterpart (see [DEBUGGING.md](debugging.md)), and
`StreamSaga` is the streaming counterpart of `Stream`. `SagaAborted` unwraps to its cause, so
`errors.Is` against a sentinel still works.

### How it survives a crash

The abort is derived from the journal: a saga step failure writes a durable `StepSagaFail` record,
so a crash at any point resumes correctly. On re-entry a recorded failure sends the run straight to
rollback, and **each compensation is itself a durable memoized step**, so it runs at-most-once if it
completes. This split is the precise contract, proven adversarially in `saga_dst_test.go` (see
[TESTING.md](../testing/testing.md)):

- The **forward** non-idempotent effect is **at-most-once** (halt on unknown outcome).
- **Compensators** are **at-least-once**: memoized so they run once if they complete, but a crash
  mid-compensation re-runs them. This is why a `Compensate` must be idempotent.

### The boundaries (read these before relying on it)

These are stated in full in [KNOWN-LIMITATIONS.md](../KNOWN-LIMITATIONS.md); in brief:

- **A saga step must be atomic.** The *failing* step itself is not compensated (there is no recorded
  result to drive `Compensate`), so a forward step must not leave a partial external side effect
  before returning an error. Make forward steps all-or-nothing or idempotent.
- **Unknown-outcome resume halts, it does not auto-roll-back.** If a non-retriable step crashes
  after its attempt marker but before any result, `RunSaga` returns `*ResumeHalt`: a human decides,
  because you cannot safely roll back a step that may have committed.
- **Rollback covers calls the abort cut off.** Rollback walks every tool call the model made, not
  only those that returned. A side effect that started but has no recorded outcome (a sibling the
  failure cancelled mid-call) stops the rollback with a `*ResumeHalt` in
  `SagaAborted.CompensateErr`; resolve it with `ResolveHalt` and call `RunSaga` again to finish the
  rollback. A retry-safe call with a compensator is run again to learn its result, then undone. A
  sub-agent call is rolled back into whether or not it returned. A completed write with no
  compensator, idempotent or not, is listed in `SagaAborted.Uncompensated`.
- **Compensation is hierarchical, not concurrent.** Rollback recurses through a sub-agent *tree*
  (one causal order). Truly concurrent agents mutating shared state out of order need the provable
  convergence of the governance tier ([GOVERNANCE.md](governance.md)), not a saga.

`SubAgent(name, description, sub)` composes agents into that durable tree: give the sub-agent the
**same** `Durable` store as the parent for a unified journal, and a crash anywhere in the tree
resumes the whole tree precisely (completed sub-agents reused, the in-flight one resumed, and
`ResumeHalt` / `PendingApproval` / `SagaAborted` from deep in the tree propagating up). A
`ResumeHalt` or `PendingApproval` raised inside a sub-agent surfaces from the parent's own `Run`
(match it with `errors.As`); resolve it against the **sub-run's** ID and tool-use ID carried on the
signal (`RunID`, `ToolUseID`), then re-run the run named by `RootRunID`, the top-level run, with
the root agent to resume down the path. The same holds for `Interrupted`, `Awaiting`, and
`Sleeping` from a sub-agent; a `Sleep` in a sub-agent schedules its wake for the root run.

### Clearing a `ResumeHalt`: `ResolveHalt`

A `ResumeHalt` is deliberately terminal until a human confirms the real outcome: the runtime cannot
know whether the non-idempotent side effect (a charge, a send) actually committed. Once you have
verified it out of band, `ResolveHalt` is the sanctioned escape. It injects the missing tool result
under the halted tool-use ID (the same journal key the loop uses), so a re-run proceeds past the
halt instead of halting again:

```go
var halt *agent.ResumeHalt
if errors.As(err, &halt) {
    // operator confirmed the charge did go through
    _ = agent.ResolveHalt(ctx, store, halt.RunID, halt.ToolUseID, "charged (confirmed)", false)
    msg, err = a.Run(ctx, halt.RootRunID, input) // resumes past the halt
}
```

Pass the result value an actual call would have returned, and `isError=true` if the verified outcome
was a failure the model should react to. It is idempotent (first result for a `(runID, toolUseID)`
wins), so a retry or a racing driver injects it at most once.

### Resolving without a human: reconcilers

The verifier does not have to be a person. Most systems that lack an idempotency key still leave a
queryable record (a sent-message id, a row, a log line), so a reconciler can query that record,
decide, and call `ResolveHalt` itself. Two options make that safe:

- **`WithMinHaltAge(d)`** refuses to resolve a halt younger than `d`, measured from the attempt
  marker (`ResumeHalt.AttemptedAt`) to now. A provider's record can lag the send by seconds, so a
  reconciler that queries too early reads "absent" and re-fires the exact effect the halt prevents.
  A minimum age keeps "unknown" unknown until the record has had time to appear; too soon returns
  `*HaltTooYoung`, so the reconciler waits and retries.
- **`WithEvidence(v)`** records the resolution as reconciled and stores what was read to decide,
  signed alongside the outcome (`Record.Reconciled` / `Record.Evidence`). The journal is the audit
  record, so a reconciled outcome that did not say it was reconciled, and on what basis, would be a
  hole in the very thing the trail exists to protect. With it, a later reader tells a reconciled
  step from a clean one and re-checks the evidence.

```go
var halt *agent.ResumeHalt
if errors.As(err, &halt) {
    sent, record := providerSays(halt) // query the system of record
    if sent {
        _ = agent.ResolveHalt(ctx, store, halt.RunID, halt.ToolUseID, "sent (reconciled)", false,
            agent.WithMinHaltAge(30*time.Second), // do not decide before the record can settle
            agent.WithEvidence(record))           // journal the basis, signed with the outcome
    }
}
```

The human stays the fallback for the genuinely unknowable case, not the default.

## Durable timers: `Sleep` / `WaitUntil` and the `Waker`

An agent often has to wait: for a deadline, a cool-off, a scheduled follow-up. `Sleep(ctx, name, d)`
and `WaitUntil(ctx, name, until)` make that wait durable. Called from inside a retry-safe tool, they
journal the wake time once (at-most-once by name) and pause the run with `*Sleeping`, the same durable
pause as `Interrupt`. Because the wake time is journaled on the first call and memoized, a resumed or
crash-recovered run waits to the same absolute instant rather than restarting the clock; no goroutine
is held blocked across the wait.

```go
wait := agent.Func("cooldown", "wait before retrying", agent.Safety{ReadOnly: true},
    func(ctx context.Context, _ struct{}) (string, error) {
        if err := agent.Sleep(ctx, "cooldown", time.Hour); err != nil {
            return "", err // *Sleeping propagates out of Run; the run is paused durably
        }
        return "resumed", nil
    })
```

Re-invoking `Run` with the same runID at or after the wake time resumes past the `Sleep`. What
re-invokes it is a **`Waker`**, the time-driven sibling of the inbound event trigger in
[MESSAGING.md](messaging.md): the SDK provides the durable, at-most-once timer and its resume safety,
and the trigger is pluggable. Bind one with `agent.WithWaker(ctx, w)` and `Sleep` registers its wake
automatically. `MemWaker` is the reference in-process implementation:

```go
w := agent.NewMemWaker(func(ctx context.Context, runID string) error {
    _, err := a.Run(agent.WithWaker(ctx, w), runID, savedInput) // resume; may sleep again
    return err
})
w.Start(ctx, time.Second, nil) // tick: resume every run whose timer is due
```

Boundaries: `MemWaker` is a local-dev default, not a durable scheduler. Its in-memory timer set is
lost on process exit, so the wake times must also live in the journal (they do), and a restarted
deployment rebuilds pending wakes by scanning runs or hands the trigger to an external scheduler
(cron, a queue). Tests inject a clock with `agent.WithClock` to advance time deterministically. This
is the piece that makes an always-on ambient agent turnkey: a durable wait plus a trigger, with
at-most-once and crash-resume intact across the wait.
