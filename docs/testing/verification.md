# How bide is verified

bide makes a small number of strong promises: a side effect runs at most once, even across crashes
and competing drivers; a resumed run continues exactly where it stopped; and the journal is a
complete, tamper-evident record of what happened. This page describes the discipline that keeps
those promises true as the code changes. The individual test suites are described in
[Testing](testing.md); the promises themselves are stated precisely in the
[Guarantee](../GUARANTEE.md) and bounded in [Known limitations](../KNOWN-LIMITATIONS.md).

## The rule: no fix without a failing test

Every bug fix follows the same three steps, and the pull request shows all three.

1. **Reproduce it.** A suspicion from reading code is not a bug yet. It becomes one when a test
   fails because of it, and the pull request quotes that failure: for example, `charged 2 times,
   want 1`, or `the losing driver was told it won`.
2. **Fix it,** and show the same test passing.
3. **Mutation-check the fix.** Disable each part of the fix in turn and confirm a test fails. A
   part whose removal no test notices is either unnecessary, and removed, or untested, and gets a
   test. This is how a fix that passes by accident is caught.

A test that can fail only by chance is not accepted. When a bug depends on timing, the test forces
the interleaving instead of hoping for it (see below), so it fails every time on the unfixed code.

## Adversarial audits

Beyond fixing what turns up, each subsystem is audited on purpose: the SQL stores, the `plan`
runtime, halts and approvals and sagas, streaming, middleware, sessions, and concurrency throughout.
An audit looks for ways to break a guarantee, and every finding goes through the rule above before
anything changes. Findings that cannot be reproduced are recorded as suspicions, not fixed.

## Techniques

**Crash injection.** Test stores fail at a chosen write, after the step's side effect has run and
before its record commits, which is the window at-most-once exists for. The deterministic
simulation tests (`agent/dst_test.go`, `agent/saga_dst_test.go`, `agent/mofn_dst_test.go`) sweep
the crash point across every write of a run and check that each resume either matches the clean
run or halts; a side effect never runs twice. The reference-model tests (`agent/refmodel_test.go`)
go further: random scenarios of parallel calls, sub-agents, approvals, failures and sagas, under
random crash schedules, must settle at exactly the outcome, side effects, compensations and
model-visible conversation that an independent crash-free interpreter of the same scenario computes. The [chaos benchmark](testing.md#pillar-1-fair-crash-injection-chaos-benchmark)
applies the same injection to bide and to other SDKs.

**Cancellation sweeps.** A context that reports itself cancelled after a chosen number of checks
lands a cancellation at every point of an operation. This is how a store that told a losing
driver it had won was found and pinned.

**Forced interleavings.** Races are made deterministic rather than left to the scheduler: a
rendezvous that holds two writers until both arrive, a model that blocks until a sibling is
mid-call, a server that keeps streaming until the client leaves. Each fails on the unfixed code on
every run.

**Overlapping drivers.** Two drivers of one run, in one process or two, race for the same step.
The exclusive attempt claim must let exactly one of them run a side effect, whatever their leases
say. A multi-process harness on Postgres (`store/postgres/ha_multiproc_test.go`) runs worker
processes through `agent.RecoverLoop` against one database while the test kills, stalls and
restarts them, and the database counts how often each side effect fired.

**Cross-process end to end.** `examples/approval` and `examples/plan` build real binaries, kill
and resume them across processes, and verify the resulting evidence with the `bide-audit` CLI,
including tampered and incomplete evidence that must be rejected.

**Conformance suites.** A port is held to its contract by a reusable suite that any
implementation, bide's or yours, can run:

- `agent/durabletest` checks a durable store (`agent.Durable`): the record `Do` returns on the live
  path is exactly the record a replay reads back, memoized or from `History`, in the journal's
  canonical form, for content whose encoding is easy to get wrong (HTML-significant characters,
  U+2028, NUL, invalid UTF-8, unusual number forms, key order); a caller that modifies a returned
  record cannot change the journal; every record carries a fresh salt; and a record is journaled
  even when the caller's context was cancelled while the step ran. `MemStore`, `store/sqlite`, and
  `store/postgres` run it; see [extension points](../reference/extension-points.md#implement-your-own-store).
- `govern/eventlogtest` checks a governed event log: dense, unique positions under concurrent
  appends from separate handles, and appends idempotent by id, so a repeated append (a retry, even
  concurrent with the original) is recorded once.
- `model/modeltest` checks a model adapter: an abandoned stream releases its response, and a
  response that ends before the turn finishes is an error, not an answer. Its `ReadSSE` and
  `CheckSSEPrefix` hold an adapter's SSE reader to one `Finish`, sent last, and to never turning a
  response cut short into a different complete answer. `CheckFinish` checks the `Finish` carries a
  neutral finish reason, and `ToolNames` checks the adapter's tool-name rule.
- RFC 6962 reference vectors check the Merkle tree and proofs
  ([Pillar 4](testing.md#pillar-4-rfc-6962-conformance)).

**Fuzzing.** The parsers and verifiers that read untrusted bytes have Go fuzz targets, each checking
a property, not only the absence of panics:

- `agent`: `FuzzRecordRoundTrip`, `FuzzDecodeRecord` and `FuzzEncodeRecord_FixedPoint` (a journal
  record decodes back to what was encoded, and re-encoding is a fixed point).
- `model/provider`: `FuzzSSEScanner` (the shared SSE framing).
- `model/anthropic`, `model/openai`, `model/gemini`: `FuzzStreamSSE` (one `Finish`, last; every
  failure an `ErrModel`; a response cut at any byte never succeeds with a different message).
- `audit`: `FuzzUnmarshalStrict` (an accepted proof file reads exactly as it decodes, and agrees
  with `encoding/json`), `FuzzArtifactVerify` (only a genuine artifact verifies),
  `FuzzInclusionProof`, `FuzzConsistencyProof`, `FuzzInclusionRaw`, `FuzzConsistencyRaw` (a
  mutated proof is rejected, and `audit` agrees with the standalone `audit/verify`).
- `plan`: `FuzzLoadConfig` (a config that loads validates, has a stable digest, and runs).
- `codec/gcf` (its own module): `FuzzEncodeToolResult` (GCF output reads back as exactly the tool's
  JSON value).

Each target's seed corpus holds the inputs of the bugs fuzzing has found, so `go test ./...`, which
runs the seeds only, keeps them fixed in CI. To fuzz one target:

```bash
go test -run '^$' -fuzz=FuzzStreamSSE -fuzztime=5m ./model/openai
```

**Race detection and stress.** The CI test run on Linux uses `-race`; macOS and Windows run the
plain suite, which also runs the scale tests at full size (they run smaller under `-race`). The
concurrency-heavy packages are also run repeatedly under `-race -count=N -cpu=1,2,8`, so
scheduling differences get many chances to surface.

## Continuous integration

Every pull request runs these checks, and all but Integration are required before it can merge:

- **Lint:** `gofmt`, `go vet` and `govulncheck` across every module, including the example
  modules; `doccheck` (`internal/tools/doccheck`), which requires a doc comment on every exported
  identifier and a package comment on every package; and checks that every module is built, tested and in `go.work`, and is classified
  as published or repo-only for releases.
- **Tests on Linux, macOS, and Windows,** with `-race` on Linux. On Linux the core, `govern` and
  `integration` modules are also tested with `GOEXPERIMENT=nojsonv2`, so the journal encoding does
  not depend on `encoding/json/v2`.
- **Integration** against real Postgres 16 and Redis 7. Each suite runs twice against the same
  services, so a test that passes only on a fresh database fails, and a skipped test fails the job,
  since a skip would mean nothing was tested. It is not a required check, so a failure here does
  not by itself stop a merge.
- **DCO** sign-off on every commit.

Pull requests merge through a merge queue, which runs the required checks again on the change
combined with `main` and any changes queued ahead of it, so every merge is tested against the code
it lands on. A pull request that changes only documentation skips the Go lint, build and tests;
the required checks still report, so it can merge.

## What this does not prove

- **Mutation checks are per fix, not exhaustive.** Each fix is checked by hand against its own
  mutants; the codebase as a whole is not put through automated mutation testing.
- **CI runs fuzz seeds, not fuzzing.** New inputs are searched for when someone runs the fuzzer;
  CI replays the seed corpus.
- **Tests cover the scenarios they model.** The deterministic sweeps and forced interleavings cover
  the windows each guarantee depends on, but a scenario nobody has modelled is not covered until
  someone does.
- **Model behaviour is measured, not proven.** Whether a model decides well is evaluated
  statistically with `eval`, and an eval pass rate is not one of the guarantees above (see
  [Evaluation](testing.md#evaluation-statistical-and-distinct-from-the-provable-layer)).

## Reproduce it yourself

```bash
export GOWORK=off
go test -race ./...                                   # core module
(cd store/sqlite && go test -race ./...)              # any other module the same way
(cd store/postgres && PG_DSN='postgres://user:pass@localhost:5432/db?sslmode=disable' \
  go test -race -count=2 ./...)                       # integration, against your Postgres
go test -race -count=20 -cpu=1,2,8 ./agent ./plan     # stress
go test -run '^$' -fuzz=FuzzUnmarshalStrict -fuzztime=5m ./audit   # fuzz one target
```

A report that comes with a failing test is the fastest kind to fix; see
[CONTRIBUTING](../../CONTRIBUTING.md).
