# Chaos benchmark: cross-SDK results

This is a **separate module** (its own `go.mod`) so competitor SDKs' large dependency trees
never touch the Bide core. It runs the [chaos benchmark](../docs/design/chaos-benchmark.md)
against other Go agent SDKs.

Run it:

```
cd benchmarks && GOWORK=off go test -run Comparison -v
```

## Result

```
Bide        maxFired=1   PASS ✓ (at-most-once held)
trpc-agent-go    maxFired=5   FAIL ✗ (70 double-fires, worst=5)
adk-go           maxFired=4   FAIL ✗ (45 double-fires, worst=4)
langchaingo      maxFired=64  FAIL ✗ (204 double-fires, worst=64)
eino             maxFired=64  FAIL ✗ (204 double-fires, worst=64)
naive-loop       maxFired=5   FAIL ✗ (45 double-fires, worst=5)
```

`maxFired` is the most times a single non-idempotent side effect ("charge") actually
executed across a crash schedule. **1 is correct; anything higher is a double-charge.** The
failure shapes fall into two camps. **Real persistence, narrow re-fire window** (trpc-agent-go,
adk-go): resume genuinely works, but a crash in the window between a side effect *executing*
and its record *persisting* re-fires it (worst 4–5). **No crash durability at all**
(langchaingo, eino): a crash loses the run and re-invoking re-runs everything unboundedly (up
to 64 charges). The naive baseline sits with the first camp's shape but for a different
reason (at-least-once loop, no attempt marker).

## The trpc-agent-go finding (and why it's fair)

trpc-agent-go has a real, LangGraph-style checkpoint/resume in its graph executor. The
adapter (`trpc.go`) builds a two-node graph (`start → charge`), runs it under a checkpoint
saver, and models a crash by **losing** every checkpoint written from crash-point K onward
(trpc treats a save error as non-fatal and keeps running in-memory, so a real crash = "what
was written after K never persisted"). It then resumes from the last surviving checkpoint.

**This is not a strawman; the adapter is verified fair** (`fairness_test.go`): resuming a
*completed* run is a genuine no-op (the charge does **not** re-fire). So trpc's resume really
works; the double-fires happen specifically when a crash lands in the window between the
side-effect node running and its checkpoint persisting, after which resume correctly
re-executes that node. That is the documented LangGraph model: **nodes must be idempotent**;
a non-idempotent side effect double-fires across a crash. trpc's own docs acknowledge it.

Bide closes exactly that window: it writes a durable *attempt marker* before a
non-idempotent tool, so resume can tell "never ran" (safe to run) from "ran, outcome unknown"
(halt), and never re-fires. That's why it holds `maxFired=1`.

## The langchaingo finding

langchaingo (`langchaingo.go`) has **no durable resume or checkpoint of any kind**, so a
crash mid-tool loses the run, and the only recovery is re-invoking the agent, which re-runs
everything. The adapter models the crash by cancelling the run's context right after the
charge fires; "resume" is a fresh invocation. It is fair (`lcg_fairness_test.go`: a clean
single invocation fires exactly once), and the double-fire is inherent, not injected: this
isn't a bug in langchaingo, durable side-effect safety is just an absent feature. Under
repeated crashes it charges up to 64 times.

## The eino finding

eino (ByteDance) HAS checkpoint/interrupt/resume, but it is **HITL-interrupt-driven, not
automatic crash-resume**: a checkpoint is written only when a node interrupts (and you resume
with `ResumeWithData`). There is no per-step checkpoint, so an unplanned process crash has
nothing to resume from: the run is lost and re-invoking re-runs everything. So for crash-safety
eino sits with langchaingo (`eino.go`; fair per `eino_fairness_test.go`): maxFired=64.

## The adk-go finding

ADK-Go (Google's Agent Development Kit) has **real event persistence**: every event is
appended to a `session.Service` as the run proceeds, and re-invoking the runner with the same
session replays that history to the model. So a history-aware model, which is what a real LLM
is, since it sees the conversation, de-dupes work that was durably recorded. The adapter
(`adk.go`) wires a mock model + one `charge` function-tool + a runner over a crash-injecting
`session.Service` that fails the Nth `AppendEvent` (a real persist failure), and lets the
session survive across steps; "resume" is a fresh `runner.Run` on that same session.

**It is fair** (`adk_fairness_test.go`): resuming a *completed* run is a genuine no-op; once
the charge's function-response event is durably recorded, the replayed history tells the model
"already charged" and it does not re-fire. So ADK's persistence really works. The double-fires
(worst=4) happen specifically when the crash lands in the window between the `charge` tool
*executing* and the `AppendEvent` that *records* its result: the record is lost, the replay
sees no charge in history, and it re-fires. ADK has no framework-level attempt-marker /
halt-on-unknown-outcome to close that window: the same gap trpc has, and the same one
Bide closes to hold `maxFired=1`.

## Adding another SDK

Implement `chaos.System` for it (see `trpc.go` as a template), add it to `bench_test.go`,
and, importantly, add a fairness check like `fairness_test.go` so the adapter represents
that SDK's *best-effort* durability, not a rigged failure. Two outcomes: the SDK has
no resumable-run concept (a crash loses the run; re-invoke re-runs everything) or it resumes
by re-executing (a genuine double-fire, as trpc does here).
