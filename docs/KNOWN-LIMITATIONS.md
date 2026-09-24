# Known limitations

Honest list of what doesn't work yet or has bounds. Correctness invariants hold within
these bounds; these are the edges.

## Deep agent-tree recursion is superlinear (time AND memory)

Execution and rollback use native synchronous recursion, so a deep sub-agent tree keeps
every level live and the goroutine stack balloons.

| Depth | Result |
|---|---|
| 50 | <10ms ✓ |
| 100 | fast ✓ (committed test) |
| 500 | ~4s ✓ (completed, invariant held) |
| 2500 | ~7.4 GB RAM, >8 min, killed (did not finish) |

- **Not a correctness bug** — the "no dangling side effects / at-most-once compensation"
  invariant held at every depth that completed.
- **Irrelevant in practice** — real agent trees are single-digit deep; 100 is already far
  beyond realistic.
- **Root cause is heap live-set, not stack depth (measured).** A synchronous agent tree
  keeps the entire ancestor chain alive — every parent is blocked waiting on its child,
  retaining its conversation + maps — so live heap is O(depth) with a large per-level
  constant (~MBs/level). We tried running each sub-agent on its own goroutine
  (subagent.go); it did NOT reduce memory (still ~7.4GB at 2500), confirming the cost is
  heap-resident live state, not the call stack. (The goroutine change was kept anyway: it
  makes sub-agents ctx-cancellable and avoids one monster stack.)
- **A real fix** would require heap-profiling to shrink the per-level footprint (what each
  blocked level retains), and/or a fundamentally different execution model that doesn't
  hold the whole chain live. Neither is a quick win, and it's not worth it until a workload
  genuinely needs >100-deep trees.

## Parallel tool calls — SHIPPED, concurrency status

Parallel tool execution is built: a turn's tool calls run concurrently via `errgroup`
(bounded by `SetMaxConcurrency`, unbounded by default), proven concurrent + `-race`-clean
(`concurrency_test.go`).

- ✅ **Concurrent `Do` on the same `(runID,name)` never double-runs `fn`.** All stores
  single-flight per key (`golang.org/x/sync/singleflight`); a side effect fires at most once.
- ✅ **Forward conversation order is deterministic.** The loop assembles tool-result
  messages in `uses` order (indexed slice), not completion order — so the transcript is
  stable regardless of which tool finishes first.
- ⚠️ **Journal `seq` under parallel tools is completion-order, not dispatch-order.** Benign
  in practice: tool results are matched by `tool_use` ID (order among siblings doesn't affect
  the model), and saga rollback of *parallel* siblings assumes they're independent/atomic
  (already a documented saga requirement). If a future need requires strict dispatch-order in
  the journal itself, add a dispatch-time execution index to `Record` + sort `History` by it.
- ✅ **`@llm/N` naming** is unaffected — parallelism is only *within* a turn's tools; model
  turns remain sequential (one per loop iteration).

## Saga semantics

- **A saga step must be atomic.** The failing step itself is not compensated (no recorded
  result to drive `Compensate`), so a step must not leave a partial external side effect
  before returning an error. Make forward steps all-or-nothing or idempotent.
- **Unknown-outcome resume halts, doesn't auto-rollback.** If a non-retriable step crashes
  after its attempt marker but before any result, `RunSaga` returns `*ResumeHalt` (a human
  decides) — you can't safely roll back a step that may have committed.
- **Distributed compensation is hierarchical, not concurrent.** Rollback recurses through
  a sub-agent *tree* (one causal order). Truly concurrent agents mutating shared state
  out-of-order need provable convergence (gsm territory) — not built.

## Replay fidelity

- `emitsFor` re-emits an assistant message as `reasoning → text → tool calls`, losing
  original interleaving. Multiple `Reasoning` blocks collapse to one (keeping the last
  signature) — a fidelity loss for multi-block extended-thinking replay. Fix: represent
  reasoning as an ordered list of parts, each with its own signature, end-to-end.

## Provider / schema

- `schema/` emits OpenAI-strict and a neutral dialect; a dedicated **Gemini** dialect
  (strip `additionalProperties`/`$ref` per its subset) is not yet done.
- Runtime (MCP) tools use the untyped path; there's no Go-struct typing for them (Go can't
  synthesize a struct type from a runtime schema).
