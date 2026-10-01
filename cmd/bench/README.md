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

Two environments, medians reported. Every run journals to the in-memory `MemStore`, so the
journal-records figure is an in-memory write rate, not a durable-store one. Each run writes six
journal records in these scenarios, the `@journal` header among them (five at v0.8.0, whose row
the runner table keeps as history).

Latency is reported as the mean run latency (concurrency / throughput: the harness keeps a fixed
number of runs in flight, so by Little's law this is the mean time a run takes), p90 and p99. The
p50 is printed in the raw output but not reported: in this closed-loop harness it is bimodal,
sitting on a scheduling cliff, and a single v0.8.0 binary's p50 has ranged from 0.19 to 2.77 ms
between runs.

**A standard GitHub Actions runner** (`ubuntu-latest`, 4 vCPU AMD EPYC 7763, Go 1.27), each
scenario run 21 times, from a single-ref run of the [Benchmark workflow](../../.github/workflows/bench.yml)
at v0.9.0 ([run 36802470752](https://github.com/bide-ai/bide/actions/runs/36802470752); a same-ref A/B run on the
same CPU model put the noise within 2.7% on every metric shown), with the v0.8.0 row kept for comparison. Anyone can reproduce these: Actions, Benchmark, Run workflow, with `base` and `head`
empty. Runner CPU models vary between jobs (AMD EPYC 7763 and 9V74 among them), and the job summary
states the model; published numbers always come from a single-ref run and name its CPU model, and
a reproduction is comparable only on the same model.

| Scenario | Wall-clock | Runs/s | Journal records/s | Mean latency | p90 | p99 | Peak goroutines | Heap delta |
|---|---|---|---|---|---|---|---|---|
| Overhead: `-runs 5000 -concurrency 256` | ~197 ms | ~25,400 | ~152,400 | 10.1 ms | 22 ms | 50 ms | 302 | ~17 MB |
| Overhead, v0.8.0 (history) | ~211 ms | ~23,700 | ~118,400 | 10.8 ms | 26 ms | 52 ms | 304 | ~14 MB |
| I/O fan-out: `-runs 20000 -concurrency 5000 -latency 50ms` | ~1.01 s | ~19,900 | ~119,300 | 251 ms | 321 ms | 447 ms | 5,669 | ~74 MB |
| I/O fan-out, v0.8.0 (history) | ~1.05 s | ~19,000 | ~95,000 | 263 ms | 322 ms | 419 ms | 5,805 | ~64 MB |

At v0.9.0 the fan-out p99 is higher than at v0.8.0 (447 vs 419 ms) while throughput and the mean
improved: the sixth journal record each run writes (the `@journal` header, #92) reshapes the
latency distribution. The `Record` shrink planned after P12 is expected to recover some of it.

**A 10-core Apple silicon Mac** (darwin/arm64, Go 1.27), measured at v0.7.0, each scenario run
seven times, when each run wrote four journal records. It was measured with other work running (a
virtual machine using a full core, load average about 4), so treat it as a lower bound for that
machine.

| Scenario | Wall-clock | Runs/s | Journal records/s | Mean latency | p90 | p99 | Peak goroutines | Heap delta |
|---|---|---|---|---|---|---|---|---|
| Overhead: `-runs 5000 -concurrency 256` | ~95 ms | ~52,700 | ~210,700 | 4.9 ms | 12 ms | 25 ms | 319 | ~9 MB |
| I/O fan-out: `-runs 20000 -concurrency 5000 -latency 50ms` | ~469 ms | ~42,600 | ~170,500 | 117 ms | 127 ms | 139 ms | 5,563 | ~62 MB |

The fan-out rows are the point. Each run makes two model calls at 50ms each, so ~100ms of
unavoidable wait, and 20,000 of them, 5,000 in flight at a time, finish in about half a second on
the Mac (measured at v0.7.0), where the mean run takes about 117 ms, close to the model's own
~100ms, because the runs fully overlap. On the 4-vCPU runner the same work takes about one second: with fewer cores the
framework's CPU work, not the model wait, sets the pace. Either way it is a few thousand goroutines
and tens of MB in one commodity process instead of a cluster.

## Comparing two refs

Two single-ref runs can land on different CPU models, which moves the numbers more than most
changes do, so do not compare them. The workflow's A/B mode compares two refs on one machine: it
builds `cmd/bench` at `base` and at `head` in one job and runs the two binaries interleaved (base,
head, base, head, ...) for the configured repeats, so drift in the machine's speed during the job
reaches both. The job summary states the CPU model and gives, for each scenario and metric (mean
latency, p90 and p99 among them), the median at base, the median at head and the percentage change from base to head.

```
gh workflow run bench.yml -R bide-ai/bide --ref main -f base=v0.8.0 -f head=main
```

`base` and `head` take a tag, a branch or a commit; `repeats` (default 21) counts runs per scenario
and per ref. To see the noise floor of a comparison, run it with the same ref as base and head. A/B
results are for judging a change, not for publishing: published numbers come from single-ref runs.
The scenarios, medians and percentage changes are in
[`.github/scripts/bench.sh`](../../.github/scripts/bench.sh), whose `--self-test` runs in CI.

## What this does NOT claim

- Not lower latency than the model. The provider dominates per-call wall-clock; the win is
  throughput, concurrency, and operational simplicity, not per-call speed.
- The stub model has no real inference; this measures framework overhead and concurrency, not a
  provider.
- `MemStore` is the in-memory floor. In production the durable store's write throughput
  (SQLite, Postgres, partitioned) is the real ceiling at high fan-out, not goroutines. Benchmark
  against your store to find that limit; do not quote these numbers as a production SLA.
