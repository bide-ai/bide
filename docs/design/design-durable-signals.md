# Design: Durable Signals

Status: stages 1-3 shipped (Signal/Await, AwaitFor, ordered channels). Stage 4 dropped in
favor of the existing Waker (see "Waking the run"). The ordered-channel section below reflects
the shipped explicit-Ack design, which supersedes the earlier cursor sketch.

## Why

Ambient runs have to *receive* external events while unattended: a webhook fires, a
message lands on a queue, an upstream run finishes, a human replies out of band. Today the
runtime can wake a run on a timer (`Sleep`/`Waker`) and resume a human answer
(`Interrupt`/`Resume`), but there is no general, typed, externally-pushed event delivered
*into* a run. Durable signals close that gap and complete the ambient lifecycle (sleep,
wake on an event, act).

The headline property, and the reason it belongs in this runtime rather than a message bus:
**at-least-once transport in, exactly-once application to the run.** Transports (webhooks,
queues, upstream runs) redeliver; a signal is applied to its run at most once. This extends
the SDK's at-most-once guarantee from side-effect *execution* to event *ingestion*, the
missing half for event-driven agents.

## Placement in the existing model

A signal is the externally-initiated dual of `Interrupt`/`Resume`, and the payload-carrying
form of a `Waker` trigger:

- **Interrupt (pull):** the run asks a question; a human `Resume`s. One answer per key.
- **Signal (push):** an external source delivers; the run `Await`s. May arrive *before* the
  run waits (buffered).
- **Sleep/Waker (time):** wake at a deadline. A signal is a wake on an *event*, with data.

All three ride the same substrate: a named durable step (`Durable.Do`, at-most-once by
name) plus `History` replay, and a typed pause error that `Run` propagates. No new
persistence model.

## API (new file `signal.go`)

### Consume (run-side, inside a retry-safe tool)

```go
// Await blocks the run until a single-shot signal `name` is delivered, then returns its
// payload. Until then it returns *Awaiting and the run pauses durably. On resume it returns
// the journaled payload (deterministic replay). Retry-safe-tool only, like Interrupt/Sleep.
func Await[T any](ctx context.Context, name string) (T, error)

// AwaitFor is Await with a deadline: it returns (payload, true) if the signal arrives first,
// or (zero, false) if the timeout elapses first. It composes Await with the durable timer,
// so the deadline survives a crash. This is the ambient "wait for X, give up after D" case.
func AwaitFor[T any](ctx context.Context, name string, d time.Duration) (T, bool, error)

// Receive returns the oldest not-yet-acked message on an ordered channel, in delivery order.
// Pauses (*Awaiting) when the channel is drained. The run must Ack(channel, msg.Key) after it
// durably handles the message; until then Receive keeps returning the SAME message, which is
// what makes streaming consumption replay-safe and exactly-once (Receive writes nothing; only
// Ack writes). Call from a retry-safe tool, like Await.
type Received[T any] struct {
	Key     string
	Payload T
}
func Receive[T any](ctx context.Context, channel string) (Received[T], error)
func Ack(ctx context.Context, d Durable, runID, channel, key string) error
```

### Deliver (external, at-most-once intake)

```go
// Signal delivers a single-shot signal to a run, journaled at-most-once by name: a redelivery
// (a retried webhook) is a no-op and the first payload wins. Safe from any process; the
// store's PK / ON CONFLICT is the cross-process dedup. After delivering, wake the run.
func Signal[T any](ctx context.Context, d Durable, runID, name string, payload T) error

// Send appends a message to an ordered channel for a run, deduped by key: a redelivery with
// the same (runID, channel, key) is a no-op and the first payload wins. At-most-once per key.
func Send[T any](ctx context.Context, d Durable, runID, channel, key string, payload T) error
```

### Pause error (parallels `*Interrupted` / `*Sleeping`)

```go
type Awaiting struct {
	RunID  string
	Name   string
	Prompt any // optional caller payload: what the run is waiting for
}

func (e *Awaiting) Error() string // "run <id> awaiting signal <name>"
```

## Journal semantics

- New kind: `StepSignal StepKind = "signal"` (in `store.go`).
- **Single-shot delivery:** `Do(runID, "signal:"+name, ...)` records `Record{Kind:
  StepSignal, Result: payload}`. Idempotent by name. `Await` scans `History` for
  `"signal:"+name`: present decodes and returns; absent returns `*Awaiting`.
- **AwaitFor:** journals a companion deadline (`await-timeout:<name>`, the same pattern as
  `Sleep`'s wake time), then resolves the race through one more named step,
  `await-resolved:<name>`: if the signal is present it records `(v, true)`; else if the
  deadline has passed it records `(zero, false)`; else it records nothing, schedules the
  waker for the top-level run, and returns `*Awaiting`. Once the outcome is recorded every
  later entry returns it, so a signal delivered after the timeout won cannot flip a re-run
  tool to the signal branch.
- **Ordered channel (shipped, explicit-Ack):** `Send` records `"chan:"+channel+":"+key`
  (`StepSignal`), deduped by that name so a redelivery is a no-op. `Ack` records
  `"chanack:"+channel+":"+key` (`StepValue`). `Receive` scans `History`, collects the acked
  keys, and returns the first message under `"chan:"+channel+":"` (in delivery order) whose
  ack is absent. `Receive` writes nothing; only `Ack` writes, so on a tool re-run `Receive`
  returns the same oldest-unacked message deterministically, and the run consumes exactly once
  by looping Receive, durably handle, Ack. This is replay-safe without any per-execution cursor
  state, so it needs no change to the `Durable` interface.
- Delivery and consumption both go through `Durable.Do`, so both are at-most-once and
  replay-deterministic by construction.

## Waking the run (composition with Waker)

An `Awaiting` run is paused exactly like a `Sleeping` one, so delivery must trigger a
resume. Reuse the `Waker` seam: after journaling, the deliverer calls
`w.Schedule(runID, "signal:"+name, now())` and the existing `MemWaker.Fire` resumes the run
on its next tick; or the deployment resumes `Run(runID)` directly (webhook handler ->
`Signal` -> `Run`). This deliver-then-wake idiom is why stage 4 (a separate push-`Notifier`
type) was dropped: immediate resume is already achievable with the existing `Waker`, so a
parallel notifier would be speculative surface for no gain. No new durable machinery is
required: the signal record already lives in the journal, so a restarted deployment rebuilds
pending awaits by scanning runs, exactly as it does for timers.

## Ordering and concurrency

- Single-shot and keyed delivery need only `Durable.Do` (the store's PK / ON CONFLICT gives
  cross-process dedup). Ship this first.
- Ordered channels need monotonic positions under concurrent delivery. Two options:
  (a) derive order from `History` insertion order and dedup by idempotency key, adding no
  store API; (b) add an atomic next-seq to the store. Prefer (a) first, to avoid widening
  the `Durable` interface.

## Retry-safety

`Await`/`Receive`, like `Interrupt`/`Sleep`, must be called from a retry-safe tool
(`Safety.ReadOnly` or `Idempotent`): on resume the tool re-runs from the top until the
await resolves, so everything before the `Await` call must be safe to repeat.

## Rollout

1. **Single-shot `Signal`/`Await`** (+ `*Awaiting`, `StepSignal`): shipped. Mirrors
   `Interrupt`/`Resume`, reuses `Do`.
2. **`AwaitFor`** (Await plus durable timeout): shipped. The ambient "wait or give up" case.
3. **Ordered channels `Send`/`Receive`/`Ack`**: shipped, explicit-Ack design (above).
4. **Push-`Notifier`**: dropped. Deliver-then-wake via the existing `Waker` covers it.

## Tests

- **DST:** deliver a signal across every crash point; assert it is applied at-most-once and
  the run resumes deterministically (mirror `dst_test.go`).
- **Duplicate delivery** (same name or key) applies once.
- **AwaitFor** races: signal-first and timeout-first, each across a crash.
- **Ordered channel:** interleaved `Send`/`Receive`, consume-once, order preserved, resume
  mid-stream.

## Positioning

This completes the ambient pillar: an agent that sleeps, wakes on an event, and acts, all
durable and at-most-once. The one-line claim to add to the README alongside the ambient
section: *at-least-once transport in, exactly-once application*, the at-most-once guarantee
extended to the ingestion boundary.
