# Durable signals

Ambient runs have to receive external events while unattended: a webhook fires, a
message lands on a queue, an upstream run finishes, a human replies out of band. Durable
signals deliver a typed, externally-pushed event *into* a paused run, durably. They are the
externally-initiated dual of `Interrupt`/`Resume` (which asks a human and resumes with the
answer): an outside system pushes an event, and the run resumes with its payload.

The headline property, and the reason this lives in the runtime rather than a message bus:
**at-least-once transport in, exactly-once application to the run.** Transports redeliver;
a signal is applied to its run at most once. Everything below is built on one substrate: a
named durable step (`Durable.Do`, at-most-once by name) plus `History` replay, and a typed
pause error that `Run` propagates. There is no new persistence model.

## Retry-safe tools (read this first)

Every consume-side primitive here (`Await`, `AwaitFor`, `Receive`) must be called from a
retry-safe tool, meaning `Safety.ReadOnly` or `Safety.Idempotent`. On resume the tool
re-runs from the top until the pause resolves, so everything before the call must be safe to
repeat. This mirrors `Interrupt` and `Sleep`. If a tool does a non-idempotent side effect and
then awaits, that side effect runs again on every resume attempt.

## Single-shot: Signal / Await

`Await[T]` blocks the run until a single-shot signal named `name` is delivered, then returns
its payload. `Signal[T]` delivers that payload from any process.

```go
// Run-side, inside a retry-safe tool: wait for the "payment-confirmed" event.
confirmed, err := agent.Await[PaymentConfirmed](ctx, "payment-confirmed")
if err != nil {
    return nil, err // an *Awaiting error here pauses the run durably
}
// ... continue with confirmed ...
```

```go
// Deliver-side, from a webhook handler in any process:
err := agent.Signal(ctx, store, runID, "payment-confirmed", PaymentConfirmed{...})
```

On first encounter with no signal recorded, `Await` returns the zero `T` and an `*Awaiting`
error that propagates out of `Run`, pausing the run durably. `Signal` journals the payload
at-most-once by name: a redelivery (a retried webhook, an at-least-once queue) is a no-op and
the first payload wins. The store's primary-key / `ON CONFLICT` is the cross-process dedup, so
`Signal` is safe to call concurrently from any process. A signal may arrive *before* the run
awaits; it is buffered in the journal and consumed when the run reaches `Await`.

Use distinct names for distinct awaits; each pauses and resolves independently.

## Signal with a deadline: AwaitFor

`AwaitFor[T]` is `Await` composed with a durable timer: it races the signal against a deadline
and returns whichever wins.

```go
v, ok, err := agent.AwaitFor[Approval](ctx, "approval", 24*time.Hour)
if err != nil {
    return nil, err
}
if ok {
    // signal won: v is the delivered payload
} else {
    // timeout won: v is the zero value; take the give-up path
}
```

The bool return is only meaningful when `err` is nil: `(payload, true, nil)` when the signal
wins, `(zero, false, nil)` when the timeout wins. The deadline is journaled once, on the first
encounter, as `now()+d`, so a resumed or crash-recovered run races against the same absolute
instant rather than restarting the clock. While neither side has resolved, `AwaitFor` returns
`*Awaiting` and pauses the run, exactly like `Await`. This is the ambient "wait for X, but give
up after D" case, and it survives a crash.

## Ordered channels: Send / Receive / Ack

A channel is the multi-message form of a signal: an ordered, per-run stream you consume
exactly once. `Send[T]` appends a message deduped by key; `Receive[T]` returns the oldest
not-yet-acked message in delivery order; `Ack` marks a message consumed so `Receive` advances.

```go
// Deliver-side: append a message, deduped by key.
err := agent.Send(ctx, store, runID, "events", eventID, MyEvent{...})
```

```go
// Run-side, inside a retry-safe tool: consume the stream exactly once.
for {
    msg, err := agent.Receive[MyEvent](ctx, "events")
    if err != nil {
        return nil, err // *Awaiting when the channel is drained: the run pauses here
    }
    // ... durably handle msg.Payload ...
    if err := agent.Ack(ctx, store, runID, "events", msg.Key); err != nil {
        return nil, err
    }
}
```

The exactly-once guarantee comes from the explicit ack, not from any per-execution cursor.
`Receive` writes nothing; only `Ack` writes. So on a tool re-run (a resume) `Receive`
deterministically returns the SAME oldest-unacked message, and the journaled ack is what makes
it advance past a handled message. The correct pattern is always the loop above: `Receive` ->
durably handle -> `Ack`. If the run crashes between handling and acking, the message is
re-returned on resume, so the handler must itself be idempotent (or be a durable step).

`Send` dedups by `(runID, channel, key)`: a redelivery with the same key is a no-op and the
first payload wins, the same at-most-once intake as `Signal`. Matching is on the exact channel
boundary, so channel `"a"` never picks up channel `"ab"`'s messages.

## Waking the run: the deliver-then-wake pattern

An `*Awaiting` run is paused exactly like a `Sleeping` one, so delivering a signal only records
the payload; something still has to re-invoke `Run` to resume the paused run. There are two
ways to do the wake:

1. **Direct re-invoke.** The deliverer calls `Signal` (or `Send`), then re-invokes
   `Run(ctx, runID, savedInput)` with the same runID. A webhook handler that does
   `Signal -> Run` is the simplest form.
2. **Via the Waker.** The deliverer calls `Signal`, then schedules a wake at the current time
   with the bound `Waker`: `w.Schedule(runID, "signal:"+name, now())`. The `MemWaker.Fire`
   loop resumes the run on its next tick. `AwaitFor` already schedules its own wake for the
   deadline side, so the timeout fires without an external nudge.

No new durable machinery is required for either path: the signal record already lives in the
journal, so a restarted deployment rebuilds pending awaits by scanning runs, exactly as it does
for timers. What re-invokes the run is deployment policy (an in-process loop, a cron, a queue),
just like the inbound trigger for any event-driven run.

## See also

- `pause.go`: `Signal`, `Await`, and the shared `Waker` / `Sleep` machinery.
- `awaitfor.go`: `AwaitFor`.
- `channel.go`: `Send`, `Receive`, `Ack`.
- `docs/DESIGN-durable-signals.md`: the design rationale and journal semantics.
