# Composing durable work: steps, parallel fan-in, and sagas

The agent loop journals its own model turns and tool calls. But the same durable substrate is
exposed directly, so you can wrap *your own* work in the same at-most-once, crash-safe,
independently-provable guarantee. This is the "Option B" authoring model: write plain Go control
flow (`if` / `for` / functions), and name the operations that must survive a crash. There is no
graph DSL; the graph is a derived output (see [DEBUGGING.md](DEBUGGING.md) `RenderMermaid`).

Three primitives cover the common shapes, all built on the `Durable` port
([EXTENSION-POINTS.md](EXTENSION-POINTS.md)):

- **`Step[T]`**: one named durable step.
- **`Parallel[T]` / `Task[T]`**: durable fan-out/fan-in.
- **`RunSaga` + `CompensatedFunc`**: transactional agents with reverse-order rollback.

## `Step[T]`: one named durable operation

```go
func Step[T any](ctx context.Context, d Durable, runID, name string,
    fn func(context.Context) (T, error)) (T, error)
```

`Step` runs `fn` as a named durable step keyed by `(runID, name)` and returns its typed result.
On resume, a completed step returns its **recorded** result without re-running `fn`; if `fn`
errors, nothing is recorded and the step re-runs on the next attempt. It is the building block the
agent loop itself is made of, exposed for your own orchestration:

```go
inv, err := agent.Step(ctx, store, runID, "fetch-invoice",
    func(ctx context.Context) (Invoice, error) { return billing.Lookup(ctx, id) })
```

`Step` is a package function, not a method, because Go methods cannot add type parameters. The
result is journaled as a `StepValue` record, so it shows up in `RenderMermaid` as `step: <name>`
and is independently provable via `audit.ProveStep` (see [AUDIT.md](AUDIT.md)). `name` must be
unique within the run: a second `Step` with the same `(runID, name)` returns the first one's
recorded result.

`Step` is the idempotency guard the [MESSAGING.md](MESSAGING.md) webhook pattern uses to make a
redelivered inbound event replay instead of re-fire.

## `Parallel[T]` / `Task[T]`: durable fan-in

```go
type Task[T any] struct {
    Name string
    Fn   func(context.Context) (T, error)
}

func Parallel[T any](ctx context.Context, d Durable, runID string,
    maxConcurrency int, tasks ...Task[T]) ([]T, error)
```

`Parallel` runs each `Task` concurrently, each as its own durable `Step`, and returns the results
**in task order** (not completion order). It is the durable, auditable fan-in that a compliance
pipeline wants: run several independent checks at once (sanctions, credit, fraud), each crash-safe
and at-most-once, each result committed to the journal and provable on its own, then aggregate.

- Each task's `Name` is its durable memoization key within the run, so it **must be unique** across
  the tasks in one call.
- **All tasks run even if some fail**, so a failed check never hides the others. The returned error
  joins every task's error (`errors.Join`) and is `nil` only if all succeeded. A failed task was not
  journaled, so a later resume re-runs it while succeeded tasks are memoized.
- `maxConcurrency` caps in-flight tasks; `<= 0` means one goroutine per task.

```go
checks := []agent.Task[CheckResult]{
    {Name: "sanctions_check",     Fn: runSanctions},
    {Name: "pep_check",           Fn: runPEP},
    {Name: "adverse_media_check", Fn: runAdverseMedia},
}
results, err := agent.Parallel(ctx, store, runID, 0, checks...) // 0 = unbounded concurrency
```

This is deliberately a thin primitive over the journal, not a graph engine. Dynamic, model-driven
routing stays in plain Go and sub-agents; `Parallel` covers the **static** fan-out/fan-in that a
governed workflow's "parallel checks, then decide" stage is made of. The full worked flow (parallel
durable checks, then a governed decision, then an offline proof) is
[`examples/compliance`](../examples/compliance/main.go).

> Related but distinct: the agent loop already runs a *single turn's* tool calls concurrently
> (bounded by `SetMaxConcurrency`). `Parallel` is for fan-out you author yourself outside a model
> turn. See the parallel-tool concurrency notes in [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md).

## Sagas: transactional agents with reverse-order compensation

A saga is the sequential/hierarchical transactional tier: "charged the card and booked the flight,
then failed on the hotel, so cleanly refund and cancel." A tool declares how to undo its write with
the `Compensator` port ([EXTENSION-POINTS.md](EXTENSION-POINTS.md)); `CompensatedFunc` builds a
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

`RunSagaResult` is the `*Result`-envelope counterpart (see [DEBUGGING.md](DEBUGGING.md)), and
`StreamSaga` is the streaming counterpart of `Stream`. `SagaAborted` unwraps to its cause, so
`errors.Is` against a sentinel still works.

### How it survives a crash

The abort is derived from the journal: a saga step failure writes a durable `StepSagaFail` record,
so a crash at any point resumes correctly. On re-entry a recorded failure sends the run straight to
rollback, and **each compensation is itself a durable memoized step**, so it runs at-most-once if it
completes. This split is the precise contract, proven adversarially in `saga_dst_test.go` (see
[TESTING.md](TESTING.md)):

- The **forward** non-idempotent effect is **at-most-once** (halt on unknown outcome).
- **Compensators** are **at-least-once**: memoized so they run once if they complete, but a crash
  mid-compensation re-runs them. This is why a `Compensate` must be idempotent.

### The boundaries (read these before relying on it)

These are stated in full in [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md); in brief:

- **A saga step must be atomic.** The *failing* step itself is not compensated (there is no recorded
  result to drive `Compensate`), so a forward step must not leave a partial external side effect
  before returning an error. Make forward steps all-or-nothing or idempotent.
- **Unknown-outcome resume halts, it does not auto-roll-back.** If a non-retriable step crashes
  after its attempt marker but before any result, `RunSaga` returns `*ResumeHalt`: a human decides,
  because you cannot safely roll back a step that may have committed.
- **Compensation is hierarchical, not concurrent.** Rollback recurses through a sub-agent *tree*
  (one causal order). Truly concurrent agents mutating shared state out of order need the provable
  convergence of the governance tier ([GOVERNANCE.md](GOVERNANCE.md)), not a saga.

`SubAgent(name, description, sub)` composes agents into that durable tree: give the sub-agent the
**same** `Durable` store as the parent for a unified journal, and a crash anywhere in the tree
resumes the whole tree precisely (completed sub-agents reused, the in-flight one resumed, and
`ResumeHalt` / `PendingApproval` / `SagaAborted` from deep in the tree propagating up).

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
[MESSAGING.md](MESSAGING.md): the SDK provides the durable, at-most-once timer and its resume safety,
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
