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

## Measured

Two environments, each scenario run seven times, medians reported. Every run journals to the
in-memory `MemStore`, so the journal-records figure is an in-memory write rate, not a durable-store
one. Each run writes four journal records in these scenarios.

**A standard GitHub Actions runner** (`ubuntu-latest`, 4 vCPU, Go 1.27), from the
[Benchmark workflow](../../.github/workflows/bench.yml). Anyone can reproduce these: Actions,
Benchmark, Run workflow.

| Scenario | Wall-clock | Runs/s | Journal records/s | p50 | p99 | Peak goroutines | Heap delta |
|---|---|---|---|---|---|---|---|
| Overhead: `-runs 5000 -concurrency 256` | ~209 ms | ~23,900 | ~95,700 | 0.14 ms | 62 ms | 293 | ~10 MB |
| I/O fan-out: `-runs 20000 -concurrency 5000 -latency 50ms` | ~980 ms | ~20,400 | ~81,400 | 193 ms | 381 ms | 5,575 | ~44 MB |

**A 10-core Apple silicon Mac** (darwin/arm64, Go 1.27), measured with other work running (a
virtual machine using a full core, load average about 4), so treat it as a lower bound for that
machine.

| Scenario | Wall-clock | Runs/s | Journal records/s | p50 | p99 | Peak goroutines | Heap delta |
|---|---|---|---|---|---|---|---|
| Overhead: `-runs 5000 -concurrency 256` | ~95 ms | ~52,700 | ~210,700 | 2.4 ms | 25 ms | 319 | ~9 MB |
| I/O fan-out: `-runs 20000 -concurrency 5000 -latency 50ms` | ~469 ms | ~42,600 | ~170,500 | 101 ms | 139 ms | 5,563 | ~62 MB |

The fan-out rows are the point. Each run makes two model calls at 50ms each, so ~100ms of
unavoidable wait, and 20,000 of them, 5,000 in flight at a time, finish in about half a second on
the Mac, where the median run takes the model's own ~100ms because the runs fully overlap. On the
4-vCPU runner the same work takes about one second: with fewer cores the framework's CPU work, not
the model wait, sets the pace. Either way it is a few thousand goroutines and tens of MB in one
commodity process instead of a cluster.

## What this does NOT claim

- Not lower latency than the model. The provider dominates per-call wall-clock; the win is
  throughput, concurrency, and operational simplicity, not per-call speed.
- The stub model has no real inference; this measures framework overhead and concurrency, not a
  provider.
- `MemStore` is the in-memory floor. In production the durable store's write throughput
  (SQLite, Postgres, partitioned) is the real ceiling at high fan-out, not goroutines. Benchmark
  against your store to find that limit; do not quote these numbers as a production SLA.
