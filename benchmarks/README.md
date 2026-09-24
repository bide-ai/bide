# Chaos benchmark — cross-SDK results

This is a **separate module** (its own `go.mod`) so competitor SDKs' large dependency trees
never touch the go-agents core. It runs the [chaos benchmark](../docs/CHAOS-BENCHMARK.md)
against other Go agent SDKs.

Run it:

```
cd benchmarks && GOWORK=off go test -run Comparison -v
```

## Result

```
go-agents        maxFired=1   PASS ✓ (at-most-once held)
trpc-agent-go    maxFired=5   FAIL ✗ (70 double-fires, worst=5)
langchaingo      maxFired=64  FAIL ✗ (204 double-fires, worst=64)
naive-loop       maxFired=5   FAIL ✗ (45 double-fires, worst=5)
```

`maxFired` is the most times a single non-idempotent side effect ("charge") actually
executed across a crash schedule. **1 is correct; anything higher is a double-charge.** The
three failure shapes are distinct: **trpc-agent-go** has working resume but a narrow re-fire
window; **langchaingo** has *no durability at all*, so retries re-run everything unboundedly
(it charges up to 64 times); the naive baseline sits between.

## The trpc-agent-go finding (and why it's fair)

trpc-agent-go has a real, LangGraph-style checkpoint/resume in its graph executor. The
adapter (`trpc.go`) builds a two-node graph (`start → charge`), runs it under a checkpoint
saver, and models a crash by **losing** every checkpoint written from crash-point K onward
(trpc treats a save error as non-fatal and keeps running in-memory, so a real crash = "what
was written after K never persisted"). It then resumes from the last surviving checkpoint.

**This is not a strawman — the adapter is verified fair** (`fairness_test.go`): resuming a
*completed* run is a genuine no-op (the charge does **not** re-fire). So trpc's resume really
works; the double-fires happen specifically when a crash lands in the window between the
side-effect node running and its checkpoint persisting — after which resume correctly
re-executes that node. That is the documented LangGraph model: **nodes must be idempotent**;
a non-idempotent side effect double-fires across a crash. trpc's own docs acknowledge it.

go-agents closes exactly that window: it writes a durable *attempt marker* before a
non-idempotent tool, so resume can tell "never ran" (safe to run) from "ran, outcome unknown"
(halt) — and never re-fires. That's why it holds `maxFired=1`.

## The langchaingo finding

langchaingo (`langchaingo.go`) has **no durable resume or checkpoint of any kind** — so a
crash mid-tool loses the run, and the only recovery is re-invoking the agent, which re-runs
everything. The adapter models the crash by cancelling the run's context right after the
charge fires; "resume" is a fresh invocation. It is fair (`lcg_fairness_test.go`: a clean
single invocation fires exactly once), and the double-fire is inherent, not injected — this
isn't a bug in langchaingo, durable side-effect safety is just an absent feature. Under
repeated crashes it charges up to 64 times.

## Adding another SDK

Implement `chaos.System` for it (see `trpc.go` as a template), add it to `bench_test.go`,
and — importantly — add a fairness check like `fairness_test.go` so the adapter represents
that SDK's *best-effort* durability, not a rigged failure. Two honest outcomes: the SDK has
no resumable-run concept (a crash loses the run; re-invoke re-runs everything) or it resumes
by re-executing (a genuine double-fire, as trpc does here).
