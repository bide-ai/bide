# Scale and concurrency benchmark

`cmd/bench` is a load harness that drives many durable agent runs concurrently in one process
against a stub model, so it measures what the framework adds (orchestration, journaling, the
agent loop, concurrency) rather than LLM latency. Optional `-latency` simulates per model-call
I/O wait to show how Go absorbs large concurrent I/O-bound fan-out.

```
go run ./cmd/bench -runs 20000 -concurrency 5000 -latency 50ms
```

## What it shows

Agent workloads are overwhelmingly I/O-bound: a run spends its wall-clock waiting on model and
tool calls. Go's scheduler and cheap goroutines mean one process can keep a very large number of
those durable, journaled runs in flight at once. That is the "library, not cluster" pillar with
the concurrency turned up.

## Measured (illustrative, one dev machine, darwin/arm64, Go 1.27)

Framework overhead only (no simulated latency), 5,000 runs at concurrency 256:

```
throughput ~128,000 runs/s (~383,000 durable steps/s)
run latency p50 1.1ms, p99 8.9ms
peak goroutines 340, heap delta ~8.5 MB
```

The step figure above was recorded by an earlier version of the harness, which multiplied runs by
three. The harness now counts the journal records the runs actually wrote (four per run in this
scenario) and reports them as journal records/s. Every run here journals to the in-memory
`MemStore`, so that figure is an in-memory write rate, not a durable-store one.

I/O-bound fan-out (each run makes two model calls at 50ms each, so ~100ms of unavoidable wait),
20,000 runs at concurrency 5,000:

```
20,000 runs complete in ~448ms (throughput ~44,600 runs/s)
run latency p50 ~100ms (the model wait itself; runs fully overlap)
peak goroutines ~5,470, heap delta ~35 MB
```

The second run is the point: 20,000 runs, 5,000 in flight at a time, each blocking ~100ms on the
model, finish in about half a second because they overlap, on a few thousand goroutines and tens of MB. That is the cost and
simplicity story, one commodity process instead of a cluster.

## What this does NOT claim

- Not lower latency than the model. The provider dominates per-call wall-clock; the win is
  throughput, concurrency, and operational simplicity, not per-call speed.
- The stub model has no real inference; this measures framework overhead and concurrency, not a
  provider.
- `MemStore` is the in-memory floor. In production the durable store's write throughput
  (SQLite, Postgres, partitioned) is the real ceiling at high fan-out, not goroutines. Benchmark
  against your store to find that limit; do not quote these numbers as a production SLA.
