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
Bide             sweeps=8   schedules=208   maxFired=1  PASS ✓ (at-most-once held)
trpc-agent-go    sweeps=5   schedules=205   maxFired=6  FAIL ✗ (119 double-fires, worst=6)
langchaingo      sweeps=4   schedules=204   maxFired=64  FAIL ✗ (204 double-fires, worst=64)
eino             sweeps=4   schedules=204   maxFired=64  FAIL ✗ (204 double-fires, worst=64)
adk-go           sweeps=6   schedules=206   maxFired=4  FAIL ✗ (45 double-fires, worst=4)
naive-loop       sweeps=5   schedules=205   maxFired=5  FAIL ✗ (45 double-fires, worst=5)
```

`maxFired` is the most times a single non-idempotent side effect ("charge") actually
executed across a crash schedule. **1 is correct; anything higher is a double-charge.** The
failure shapes fall into two camps. **Real persistence, narrow re-fire window** (trpc-agent-go,
adk-go): resume genuinely works, but a crash in the window between a side effect *executing*
and its record *persisting* re-fires it (worst 4 to 6). **No crash durability at all**
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

## CANDIDATE, pending fairness review: LangGraph (Python)

Not part of the result table above, and not run in CI. These numbers wait for an
independent fairness review before they appear anywhere else.

`python/langgraph/` is a Python harness, isolated from the Go modules, pinned with `uv`
(`pyproject.toml` + `uv.lock`): langgraph 1.2.12, langgraph-checkpoint-sqlite 3.1.1,
langgraph-checkpoint 4.2.0, Python 3.13. `chaos.py` is a line-for-line port of
`chaos.Verify` and of Go's `math/rand/v2` PCG, so it runs the same crash schedules (checked
against Go's output in the tests) and prints the same rows.

Run it (needs `uv`):

```
cd benchmarks/python/langgraph
uv run pytest -q        # fairness checks
uv run python bench.py  # the benchmark, every configuration
```

Measured on 2026-10-03:

```
langgraph        sweeps=9   schedules=209   maxFired=3  FAIL ✗ (40 double-fires, worst=3)
```

That row is the `trpc.go` shape (`start → charge`, the side effect in the `charge` node) on a
`SqliteSaver` with `durability="sync"`, which LangGraph documents as its most durable mode.
`bench.py` also runs three other documented ways to write the side effect (inside a
`@task` called from the node; the Functional API's `@entrypoint` + `@task`, the pattern the
docs give for side effects; a node `CachePolicy` on a durable `SqliteCache`) under each
durability mode. Every configuration double-fires. Under `"sync"`, maxFired is 3 (node),
4 (`@task` in a node, and the cache variant) and 5 (Functional API); `"exit"` reaches 6.
maxFired differs between them mainly because each makes a different number of writes, so
the same seeded schedules land on different events; it is not a ranking of how protective
each one is.

**Crashes are real.** Each step runs in a forked child process. The checkpointer is a
`SqliteSaver` whose `put` and `put_writes` (and, for the cache variant, `SqliteCache.set`)
report to the parent and wait; at the crash point the parent sends `SIGKILL`, so the
process dies before that write reaches SQLite, with no exception handling or cleanup.
LangGraph issues writes from background threads, so the hook holds one lock across the
hand-off and the write: a crash at write K means writes 1..K-1 committed and nothing after
did, the same model as `chaos.Run`. The charge appends a line to a separate file and
fsyncs it before returning. Resume is what the docs describe: `invoke(None, config)` on the
same `thread_id` when the thread has a checkpoint, the original input when it has none.

**Fairness checks** (`test_fairness.py`, all pass, every variant and mode): a clean run
charges exactly once; resuming a completed thread is a no-op (no charge, no writes); a crash
at any write before the charge, then resume, charges exactly once; and in the `"sync"` graph
variants a crash after the start step persisted resumes without re-running it, so resume
continues from the checkpoint rather than restarting the thread.

**The window.** A single crash at the first durable write after the side effect ran (its
result never persisted) re-fires the charge on resume, in every variant and durability mode
(`test_crash_right_after_charge_refires`). Under `"async"` the charge can run before even
the start step's writes are persisted, so the window is wider. That is LangGraph's
documented model. The [Functional API docs](https://docs.langchain.com/oss/python/langgraph/functional-api#idempotency)
say: "A **task** that started but did not finish may run again on that resume, so design
side effects to be idempotent. Use idempotency keys or verify existing results to avoid
unintended duplication." The [durability modes](https://docs.langchain.com/oss/python/langgraph/checkpointers#durability-modes)
section describes `"sync"` as: "LangGraph persists changes synchronously before the next
step starts."

Known limits of this measurement: SQLite only (not Postgres or LangGraph Platform); the
order of LangGraph's background writes varies between runs, so the K-th write is not
always the same event and the double-fire count moves by a few between runs (maxFired was
stable over five runs); and a side effect written with an idempotency key, as the docs
advise, would not double-charge, which is true of every SDK in this table.

## Adding another SDK

Implement `chaos.System` for it (see `trpc.go` as a template), add it to `bench_test.go`,
and, importantly, add a fairness check like `fairness_test.go` so the adapter represents
that SDK's *best-effort* durability, not a rigged failure. Two outcomes: the SDK has
no resumable-run concept (a crash loses the run; re-invoke re-runs everything) or it resumes
by re-executing (a genuine double-fire, as trpc does here).
