# Chaos benchmark

`chaos/` is a deterministic crash-injection benchmark for agent runtimes: it drives a
NON-IDEMPOTENT side effect (a "charge") through a fault schedule and checks it fires **at
most once**. It's the exportable form of Bide' internal DST, and it's meant to be
pointed at *any* Go agent SDK.

```
$ go run ./examples/chaosbench
  Bide    sweeps=7   schedules=1007  maxFired=1  PASS ✓ (at-most-once held)
  naive-loop   sweeps=5   schedules=1005  maxFired=5  FAIL ✗ (240 double-fires, worst=5)
```

## What it does

For a `System` under test, `Verify` runs an exhaustive crash-point sweep (crash at every
durable write, then resume to a terminal state) plus hundreds of randomized multi-crash
schedules, and counts how many times the real side effect executed. The invariant: never
more than once. A loop that relies on at-least-once + "make it idempotent yourself"
double-fires here, visibly (see `NaiveReference`, the baseline that fails, which also
proves the harness is non-vacuous: a correct loop passes it, that one doesn't).

## Pointing it at another SDK

Implement `chaos.System` for that SDK: wire it to perform exactly one non-idempotent side
effect (increment a counter) against a **fault-injectable, resumable** store, exposing:

```go
type System interface {
    NewRun() Run   // fresh store + zeroed counter
    Writes() int   // durable writes in a clean run (the sweep bound)
}
type Run interface {
    Step(crashAt int) (crashed bool) // run/resume one attempt, crashing at the crashAt-th write
    Fired() int                      // how many times the side effect actually ran
}
```

`Bide()` is the reference adapter; `NaiveReference()` is the at-least-once baseline.

**Fairness matters.** An adapter must represent that SDK's *best-effort* durability, not a
strawman; the benchmark's credibility is that it's fair. Two outcomes for an SDK
without side-effect-safe resume: it either **can't resume at all** (a crash loses the run,
model that as the finding, not a rigged double-fire), or it **resumes by re-running** the
tool (a genuine double-fire). Competitor adapters live in a **separate module** so their
dependency trees never touch the Bide core (see `benchmarks/`, when added).

## Scope

Same as the DST it derives from: this is randomized + exhaustive-over-write-points crash
injection modeling process death around durable writes: strong and non-vacuous, but not a
machine-checked formal proof over all interleavings.
