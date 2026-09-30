# What we test, and how

This project's central claims are strong (at-most-once side effects, order-independent
convergence, a cryptographic record of exactly what an agent did), so the test suite is built
to **try to break those claims, not to confirm them**. The methods are adversarial (crash
injection, randomized schedules), differential (re-check a verdict with an independent
implementation), and conformance-based (match published cryptographic reference vectors).
This document describes what each pillar actually does and, importantly, the bounds each one
implies.

## Scope and one correction (read this first)

Everything below was written against the test code as it exists in the repository, not against
an idealized plan. One item is stated differently here than you may expect:

- The **differential oracle** work (a table oracle over emitted step tables, a rules oracle
  recomputing convergence from the combinator declarations, both extracted from the axiom-free
  Coq/Rocq proof) lives in the sibling `gsm` and `normalization-confluence` repositories, where
  gsm's convergence verdict is re-certified. It is not a pair of `oracle_test.go` /
  `astoracle_test.go` files inside Bide, and there is no `GSM_CONVERGENCE_CHECKER`
  environment variable in this repo. What Bide ships in-repo is a single optional hook,
  `GSM_AST_CHECKER`, in `govern/attested_e2e_test.go`, which runs the external verified oracle
  on the exact policy bytes Bide anchors (see Pillar 2). The two-independent-implementations
  idea is real; the code that runs both checkers is upstream in gsm, and Bide leverages it
  rather than re-implementing it.

Two module boundaries matter for running tests. The competitor benchmark adapters live in a
**separate module** (`benchmarks/`, its own `go.mod`) so their large dependency trees never
touch the Bide core; `benchmarks` is not in the workspace, so run its tests with `GOWORK=off`. The
workspace (`go.work`) stitches in the adapter modules `codec/gcf`, `govern`, `govern/postgreslog`,
`govern/redislog`, `govern/sqlitelog`, `mcp`, `store/postgres`, `store/sqlite`, and `trace`, plus
the example modules `examples/approval`, `examples/govern`, `examples/mcp`, `examples/observability`,
and `examples/plan`, and the test-only `integration` module. `govern` is its own module so the core
does not depend on gsm; the core's tests that need govern or gsm (the convergence test below, the
run-certificate tests over real gsm policies, and the `bide-audit` CLI tests fed by quorum runs and
convergence certificates) live in `integration`. `go test ./...` from the root covers the core
module only; run each module's tests from its own directory.

## Pillar 1: fair crash-injection chaos benchmark

**A deterministic crash-injection harness drives a non-idempotent charge through every crash
point and counts how many times it actually fired.** The invariant is at-most-once: `maxFired`
of 1 means the guarantee held; anything higher is a double-charge.

- `chaos/` (`chaos.go`, `chaos_test.go`) is the exportable harness. `Verify(name, sys, seeds)`
  runs an **exhaustive** single-crash sweep at every durable write point (crash there, then
  resume to a terminal state), then `seeds` **randomized** multi-crash schedules (default 500
  in the tests), and records `MaxFired` and `Violations`. `TestVerify_BidePasses` asserts
  the Bide reference adapter holds `maxFired=1`. `TestVerify_NaiveReferenceFails` asserts
  the naive at-least-once baseline double-fires (`maxFired>=2`); this is deliberate, and it
  proves the harness is **non-vacuous** (a correct loop passes, an incorrect one fails).
- `benchmarks/` runs the same harness against other Go agent SDKs from that separate module
  (`cd benchmarks && GOWORK=off go test -run Comparison -v`). The published cross-SDK result
  in `benchmarks/README.md`: Bide `maxFired=1` (PASS); trpc-agent-go `maxFired=6`; adk-go
  `maxFired=4`; langchaingo `maxFired=64`; eino `maxFired=64`; naive-loop `maxFired=5`.

**Fairness is the discipline that makes this credible, not a strawman contest.** Every
competitor adapter ships a fairness test proving its resume genuinely works before the crash
schedules are applied: `fairness_test.go` (trpc), `adk_fairness_test.go` (adk-go),
`lcg_fairness_test.go` (langchaingo), `eino_fairness_test.go` (eino). Each proves that resuming
a *completed* run is a genuine no-op (the charge does not re-fire), so where a double-fire
appears it reflects that SDK's real behavior. The findings are documented per-SDK: trpc-agent-go
and adk-go have real persistence and a narrow re-fire window (a crash between the side effect
executing and its record persisting); langchaingo and eino have no automatic crash-resume, so a
crash loses the run and re-invoking re-runs everything. Bide closes that window with a
durable attempt marker written before a non-idempotent tool, which is why it holds `maxFired=1`.

Bounds: this is randomized plus exhaustive-over-write-points crash injection modeling process
death around durable writes. It is strong and non-vacuous, but it is not a machine-checked
formal proof over all interleavings.

The internal form of the same discipline is the core DST: `dst_test.go`
(`TestDST_NoDoubleFire_CrashSweep`, `TestDST_NoDoubleFire_Randomized`) crashes at the Kth
durable write and asserts the charge fires at most once and the run ends either completed or in
`ResumeHalt`; the sweep test even fails itself if no crash point ever exercises the halt path,
so it cannot pass vacuously. `saga_dst_test.go` applies the same crash sweep and randomized
schedules to saga compensation.

`refmodel_test.go` generalizes it into model-based differential testing. A generator builds random
scenarios: turns of parallel tool calls (read-only, retry-safe, and non-retry-safe tools, calls
that fail, approval-gated calls with a scripted decision, compensable calls, sub-agents two levels
deep), run plain or as a saga, with a schedule of injected storage failures (a transient failed
write, or a process that dies at the write). A reference model in `refmodel_ref_test.go`, a plain
interpreter of the same scenario with no journal, no concurrency, and no crashes, written from the
documented semantics and calling nothing in `agent`, computes the outcome a correct runtime settles
at. The harness drives the real runtime to quiescence as an operator would (resume after a crash,
decide approvals, reconcile halts with the call's true outcome) and requires the settled outcome,
every side effect and compensation, the conversation each model call is shown, `IsComplete`, a
re-drive of the settled run, and a replay of its journal (`agent.Replay`) to agree with the
reference. `TestRefModel_CrashSweep` also crashes a set of scenarios at every write of every drive
attempt in turn. A failing scenario is shrunk to a minimal one. `BIDE_REFMODEL_N` and
`BIDE_REFMODEL_SEED` run more seeds; each minimal scenario it has found is pinned in
`refmodel_cases_test.go`.

## Pillar 2: differential testing against a verified oracle

**gsm proves at build time that every interleaving of agent events converges to the same valid
state; a second, independent implementation re-checks that verdict, so a bug in one verifier
cannot silently pass a non-convergent machine.** The two-independent-implementations principle:
if two programs written from the same axiom-free proof, by different routes, both accept a
machine, a single implementation bug is far less likely to have admitted a bad one.

- In Bide, `govern/attested_e2e_test.go` (`TestAttestedEventTool_RealPolicyDigest`) builds
  a real gsm policy, takes its `PolicyDigest`, governs a real transition through
  `AttestedEventTool`, and confirms the journaled leaf carries that exact digest and the
  resulting state digest. It independently recomputes the digest with the same domain-separated
  SHA-256 formula the `bide-audit verify-governance` CLI uses (without importing gsm), and
  asserts parity, so the SDK, the verifier CLI, and gsm agree on the policy's identity. When
  `GSM_AST_CHECKER` is set to an external checker binary, the test writes the policy bytes to a
  temp file and runs that checker on them, failing if the external verified oracle rejects a
  policy gsm built as convergent. Without the env var the oracle step is skipped.
- The two checkers themselves (the table oracle over emitted step tables and the rules oracle
  over combinator declarations, both extracted from the axiom-free Coq/Rocq proof) live in the
  `gsm` / `normalization-confluence` sibling repos, as noted in the correction above.

Bounds: Bide leverages gsm's proof; it does not claim to have proven the underlying theorem
or that its own Go code is axiom-free. The precise convergence claim is order-independent replay,
not "agents always agree" (see Pillar 3).

## Pillar 3: convergence and traceability at scale

**Many concurrent governed agents each apply the same event multiset in a different order; the
test asserts every one converges to the same machine-checked normal form AND every one's
governed action verifies offline against a signed tree head.** This is
`integration/convergence/e2e_convergence_test.go` (`TestE2E_ManyAgentsConvergeAndAreTraceable`), exercising scale,
order-independent convergence under real concurrency (including a compensating cap that fires at
different steps for different orders), and cryptographic traceability in one test.

The domain is two capped counters with distinct additive events (+1 and +2) plus a boolean flag,
applied as the multiset `inc_a, add2_a, add2_a, inc_a, inc_b, inc_b, raise_flag`; every ordering
reaches the normal form `a=5, b=2, flag=true`. Each agent runs its own `MemStore`, and
convergence is checked inline (not accumulated), so memory stays bounded by the in-flight set
rather than by total N. For each agent the test signs a tree head over its journal
(`audit.NewTreeHead` + `SignTreeHead`), proves one governed tool call (`audit.ProveToolCall`),
and verifies the bundle against the public key.

Scale tiers:

- Default: `5000`, `10000`, `20000` (`-short` collapses to a single `N=200`).
- `E2E_HUGE=1` adds `100000`.
- `E2E_HUGE=million` adds `100000` and `1000000`.
- `E2E_HUGE=tenmillion` adds `10000000`.

Measured on one dev machine (illustrative, not a production SLA). Each agent gets its own store
that is dropped after its proof verifies, so the LIVE heap stays bounded by the in-flight set, not
total N:

- Default scales (5,000 / 10,000 / 20,000): each converges and verifies in well under two seconds,
  live heap a few MB.
- 1,000,000 agents (`E2E_HUGE=million`): ~1m18s at ~12.8k agents/s, peak ~2,600 goroutines.
- 10,000,000 agents (`E2E_HUGE=tenmillion`): ~13m19s at ~12.5k agents/s, peak ~2,667 goroutines,
  live heap ~3 MB. The ~1,420 GB "total alloc" is cumulative allocation churned and freed across the
  run; the live heap does not grow, which is the point.

The in-memory store encodes each record to JSON on write and decodes it on read, the same work the
SQLite and Postgres stores do, so these figures include that cost.

The shape is the result: throughput roughly flat and live heap flat from thousands to ten million
agents. The ceiling is time (and, in production, the durable store's write throughput), not process
memory. Every agent in every tier violated the capped invariant at some step (the event multiset
sums past the cap) and was compensated before converging, so these are violation-inducing runs, not
happy-path ones.

The precise bounds:

- The model is a **stub** (`seqModel`) that drives a fixed event sequence. The test measures the
  framework and governance/audit machinery, not a real LLM; a live model's latency would dominate.
- `MemStore` is the **in-memory floor**. A durable store's throughput is the real production
  ceiling; the per-run store here isolates runtime scaling from store capacity, it does not model
  a durable backend's write cost.
- "Arbitrary-order-all-converge" structurally requires the **commuting regime**. `Build()`
  succeeding is the proof that these events commute after compensation (WFC + CC). A fully
  non-commuting case (for example ship-before-pay) deliberately would not converge under
  arbitrary order and would be rejected by `Build()`; that harder case needs causal ordering and
  is proven in the Coq/gsm layer instead, not asserted at scale here.

Related governance tests: `govern/federated_test.go` proves crash recovery by reconstructing
state from an event log, order-independence of federated events, and that a partial sync matches
the central view. `govern/attested_replay_test.go` replays the state digests bound into the
single governed-action leaf.

## Pillar 4: RFC 6962 conformance

**The Merkle commitment is checked against the published Certificate Transparency reference
vectors, so it is the real RFC 6962 construction and not a homegrown look-alike.**

- `audit/merkle_test.go` (`TestMerkle_MatchesRFC6962Vectors`) hardcodes the canonical 8-leaf CT
  test tree and the published Merkle Tree Hash at each size 0..8, and asserts our `merkleRoot`
  matches every one. `TestMerkle_InclusionRoundTrip` verifies inclusion proofs for trees of many
  sizes (1..33, exercising ragged-tree recursion), confirms a forged leaf is rejected, and
  confirms a proof does not verify against a different tree's root.
  `TestMerkle_JournalSelectiveDisclosure` proves one journal record with only that record plus
  its proof plus the root, and confirms the proof does not verify a different record.
- `audit/consistency_test.go` verifies append-only consistency proofs against RFC-correct roots
  for every `(m, n)` up to 24 (`TestConsistency_RoundTripAgainstRFCRoots`), includes a
  hand-derived 1-to-2 vector (`TestConsistency_HandDerived1to2`), proves a rewritten early record
  is detectable (`TestConsistency_DetectsRewrite`), and runs the append-only flow over a real
  journal (`TestConsistency_JournalAppendOnly`).
- `audit/sth_test.go` covers signed tree heads: `TestSTH_SignVerifyAndTamper` confirms the
  signature commits to size, root, and time (any field change or wrong key breaks verification),
  and `TestSTH_EndToEndComplianceFlow` walks the full flow (publish an STH, disclose one record
  against its signed root, then prove append-only growth between two signed STHs).
- `audit/verify/verify_test.go` checks that the standalone verifier agrees with the SDK on
  inclusion, consistency, and signed tree heads. `audit/absence_test.go` covers absence proofs
  (an absent key proves against the root; a present key cannot).

Bounds: conformance to the published vectors establishes the commitment is spec-correct. It does
not, by itself, provide tamper-evidence; that requires anchoring the commitment out-of-band, which
is a deployment requirement documented in the [audit guide](../guides/audit.md).

## Pillar 5: architecture enforcement

**A test enforces the stdlib-only / no-heavy-deps boundary so dependencies keep pointing inward.**
`architecture_test.go` (`TestCoreHasNoAdapterImports`) runs `go list -deps` on the core module and
fails if the core's runtime import graph contains any adapter package (`model/`, `store/`, `trace`,
`middleware`, `govern`) or any heavy infrastructure (`opentelemetry`, `modernc.org/sqlite`,
`jackc/pgx`, `temporal`, `weaviate`, `blackwell-systems/gsm`). It skips (does not fail) if
`go list` is unavailable. `TestCoreModuleHasNoGSM` checks the module graph as well: read with no
workspace, the core module's `go mod graph`, `go.mod` and `go.sum` never name gsm. In the govern
module, `TestGovernImportsNoCoreInternal` checks that no govern package imports a package under
the core's `internal/`, since govern is versioned apart from the core. This is the ports-and-adapters discipline verified mechanically: the
core depends only on its ports (interfaces), never on a concrete adapter.

## Pillar 6: deterministic replay for regression and debugging

**Because every run journals a complete, ordered history, a recorded run can be replayed exactly
as a test.** `agent.Replay` (`recovery.go`) returns a `Model` that re-emits the recorded model
outputs for a run in order, so an agent built on it re-executes the past run against a fresh store
with no live LLM. `agent.ReplayEvents` reconstructs the durable semantic events from the journal.

`replay_test.go` verifies both properties this rests on: `TestReplayEvents_MatchesLiveStream`
confirms the journal projection reproduces the same semantic events the live stream emitted (it is
not inventing a different history), and `TestReplayEvents_AppendOnlyAcrossCrash` confirms that a
crash mid-run leaves a prefix of the events, that resuming appends the rest without rewriting the
prefix, and that recomputing from the same journal is deterministic. See
[the debugging guide](../guides/debugging.md) for the replay, event-reconstruction, and Mermaid tools built on
the journal.

## Pillar 7: standard per-package unit tests

Beyond the pillars above, each package carries conventional unit tests:

- Core (`agent_test.go`, `stream_agent_test.go`, `concurrency_test.go`, `saga_test.go`,
  `session_test.go`, `result_test.go`, `errors_test.go`, `hitl_test.go`, `maxturns_test.go`,
  `system_prompt_test.go`, `dynamic_prompt_test.go`, `typed_test.go`, `typed_native_test.go`,
  `sampling_test.go`, `retrieval_test.go`, `tool_middleware_test.go`): the agent loop, streaming,
  concurrency, sagas, sessions, typed results, human-in-the-loop, turn limits, prompts, sampling,
  retrieval/RAG, and tool middleware.
- `audit/` also covers policy, signing (including signature-scheme agility), anchoring, event
  sink/store, proof bundles, and governance-absence proofs.
- `govern/` covers the in-memory and persistent governors, attestation, and the
  `redislog` / `sqlitelog` event-log backends.
- `middleware/` covers cost tracking, reliability, retry, and tool retry/wrapping.
- Model adapters: `model/anthropic/` and `model/openai/` cover request/response translation,
  prompt caching, rate limiting, sampling, and (OpenAI) response formats.
- Stores: `store/sqlite/` and `store/postgres/`. Also `schema/`, `trace/`, and `mcp/`.

## Evaluation: statistical, and distinct from the provable layer

The `eval` package answers a different question from the pillars above: "does the model decide
well?" That cannot be proven, because the model is stochastic, so it is measured. `eval.Run` executes
labeled cases multiple times, scores each run with rule-based or LLM-judge metrics, and reports a
pass-rate distribution (`eval.Report`), not a single verdict. Use `Runs > 1`, pin the model version,
and set temperature 0 for the most reproducible baseline (still not perfectly deterministic).

Misconfiguration is an error, never a panic: `eval.Run` returns an error wrapping `agent.ErrConfig`
before executing anything when the `RunFunc` is nil or a metric is unusable (a nil `*regexp.Regexp`
passed to `Matches`, a nil predicate or judge model, a nil `Fn`, an empty or repeated name).
`Matches` takes a compiled `*regexp.Regexp`, so a bad pattern is the caller's `regexp.Compile`
error, and `AgentRunner` returns `(RunFunc, error)`, refusing a nil agent or store.

For rigor it does more than a bare pass-count:

- **Confidence intervals.** Every rate carries a 95% Wilson score interval, so a lucky 4/5 reads as
  "80%, 95% CI [38%, 96%]" and does not masquerade as precise; the interval tightens as `Runs` grows.
- **Trajectory metrics.** It scores the agent's behavior from the durable journal, not only the final
  message: `CalledTool`, `ToolOrder`, `MaxSteps` evaluate which tools ran, in what order, and whether
  the agent looped. This is the agent-specific part, and it uses data (the journal) that only this
  SDK has. If `AgentRunner` cannot read a run's journal back, the output's `TraceErr` carries the
  failure and the trajectory metrics leave that run unscored rather than score an empty trajectory
  (zero steps would be within any `MaxSteps` limit).
- **Unscored is not failed.** A metric's `Fn` returns `(bool, error)`; an error means the metric could
  not score the run (an LLM judge whose provider is down, a trajectory that could not be read), so
  the run is unscored for that metric. Each `MetricStat` reports `Passes`, `Scored` and `Unscored`
  per case, per tag and overall, and the rate and its Wilson interval use scored runs only, so a
  judge outage does not read as the agent failing. `Judge` treats only a failed judge call as
  unscored: any reply other than `PASS` is a fail, so an output that steers the judge cannot drop
  itself out of the pass rate.
- **Latency percentiles** (p50/p95) per report. Runs go through `AgentRunner`, so each evaluation run
  is itself durable and auditable.
- **Significance-tested regression comparison.** `Compare(old, new, opts...)` pairs each metric across
  two reports and tests whether a rate moved by more than sampling noise: it picks Fisher's exact
  test or a two-proportion z-test by the minimum-expected-cell-count rule, applies Benjamini-Hochberg
  correction across the metric family, and labels each result with a typed `MetricDirection`
  (`DirectionRegression`, `DirectionImprovement`, `DirectionFlat`, `DirectionInconclusive`). A
  metric is inconclusive, with no test run and no place in the correction family, when it is
  missing from one report, has no scored runs in one, or has unscored runs above the tolerance in
  either (`WithUnscoredTolerance(frac)`; the default 0 makes any unscored run inconclusive). Every
  metric is inconclusive when the two reports ran different case sets (their
  `Provenance.CaseSetHash` differs or is missing), since a moved rate may only mean the cases
  changed; `WithCaseSetMismatchAllowed()` instead compares over the cases both ran, matched by each
  `CaseReport.Hash`, and notes that scope on every compared metric.
  `Comparison.Gate()` is the CI gate: it returns nil only when some metric was compared and every
  metric is flat or improved, and otherwise an error wrapping `ErrRegression` or `ErrInconclusive`,
  so an unmeasured metric never passes. Every `Report` carries
  `format: "bide.eval.report.v2"` (`eval.ReportFormat`), and `Compare` returns an error wrapping
  `eval.ErrFormat` (itself wrapping `agent.ErrProtocol`) for a report with any other format, so a
  report of another layout, or a zero `Report`, never compares as if it had no metrics and lets a
  gate pass.
- **Pre-registered sample sizing.** `RequiredRuns(baselineRate, minDetectableDrop, alpha, power)`
  returns the runs-per-arm needed to detect a given regression, using the pooled-variance normal
  approximation (probit via Beasley-Springer/Moro), or an error for arguments outside their domain
  (a baseline outside [0, 1], alpha or power outside (0, 1), NaN). Size the run before spending on
  it, rather than reading significance into an underpowered sample.
- **Reproducibility provenance.** Each `Report` carries a `Provenance{ModelID, Temperature, Seed,
  Timestamp, CaseSetHash}`; `Run` always fills `CaseSetHash` from `HashCases` (a length-prefixed
  sha256 over the case set), and each `CaseReport` carries its own case's hash; `Compare` checks
  both before trusting a delta.
- **Stratified breakdown.** `Case.Tags` plus `Report.ByTag` report pass rates per slice (region,
  difficulty, product line), so an aggregate that hides a failing subgroup is visible.
- **Governance-held metric.** `GovernanceHeld(name, compliant)` scores whether the governed
  invariant held on each run (`compliant` receives the evaluation's context and the run's output,
  and an error from it leaves the run unscored), which is what makes the two-number report legible: model-correct X%
  (statistical) alongside governance-held 100% (deterministic). It keeps the boundary below explicit
  inside the report itself.

Remaining bounds (stated so the harness is not oversold): there is no built-in persistent result
store yet (you keep the JSON `Report`s, though `Compare` now does the cross-version diff with a
significance test), and per-run token cost is not captured: `Agent.RunResult` returns the run's
`Usage`, but `eval.AgentRunner` drives `Run` and `eval.RunOutput` has no usage field, so a report
carries no token counts. Dataset discipline (labels, held-out splits, adversarial coverage) is the user's to bring:
the harness measures whatever cases it is given, so a weak case set yields a confident-looking but
uninformative report. Those are additive, not corrections.

The boundary is the point, and it is stated in the package doc and repeated here so it is never
blurred in a claim:

- **eval measures the model, statistically and best-effort.** A pass rate is a signal, not a
  guarantee. An LLM-as-judge metric is itself a model grading a model, so it too is a signal.
- **The provable layer is separate.** The governed policy's convergence and invariant enforcement
  (govern + the gsm proof) bound what the model can do for all inputs; the audit trail proves what it
  did. Those are machine-checked and cryptographic, not statistical.
- So the correct posture for a validating buyer (for example under model-risk rules like SR 11-7):
  the model's judgment is validated statistically here; the guardrails around it are validated
  deterministically by governance; the record is provable by audit. Never present an eval pass rate
  as one of the machine-checked guarantees.

`eval.AgentRunner` wraps an agent as an eval `RunFunc`, so evaluation runs are themselves durable
and can be replayed and audited like any other run. Each run ID carries a nonce drawn per runner,
so a second eval over the same store samples the model again rather than replaying the first
eval's recorded answers. If the context is cancelled before every run finishes, `eval.Run` returns
the error and no report, since a partial report would count cut-short runs as failures. A runnable, self-contained
walkthrough (labeled cases, rule-based and stub metrics, a deliberately flaky model so the report
shows a rate below 100%) is [`examples/eval`](../../examples/eval/main.go): `go run ./examples/eval`.

## Methodology

The principles evident across the suite:

- **Fair benchmarks.** Every competitor adapter is verified not to be a strawman: a fairness test
  proves its resume genuinely works before crash schedules are applied, so a reported double-fire
  reflects that SDK's real behavior.
- **Differential / oracle testing.** A convergence verdict from one implementation is re-checked
  by an independent implementation extracted from an axiom-free proof; digests are recomputed by a
  separate code path and asserted equal.
- **Determinism and replay.** The journal is a complete, ordered history, so runs replay exactly;
  regression and debugging are pure functions of recorded records.
- **Scale is measured, not claimed, with the bounds stated.** Throughput and memory numbers come
  from actual runs and are reported with their limits (stub model, in-memory floor, commuting
  regime).
- **Cryptographic conformance to published vectors.** The Merkle commitment matches the RFC 6962
  Certificate Transparency reference tree, so the construction is verifiably the standard one.

## Running the tests

```sh
# Full suite (core module).
go test ./...

# Fast pass: skips heavy scale sweeps (e2e convergence collapses to a single small scale).
go test -short ./...

# Chaos benchmark: Bide passes at-most-once, the naive baseline fails (non-vacuous).
go test ./chaos -run Verify -v

# Cross-SDK chaos comparison (separate module; keeps competitor deps off the core).
cd benchmarks && GOWORK=off go test -run Comparison -v

# E2E convergence + traceability at larger scale (gated by wall time), from integration/.
E2E_HUGE=1          go test ./convergence -run TestE2E_ManyAgentsConvergeAndAreTraceable -v   # adds 100k
E2E_HUGE=million    go test ./convergence -run TestE2E_ManyAgentsConvergeAndAreTraceable -v   # adds 100k + 1,000,000
E2E_HUGE=tenmillion go test ./convergence -run TestE2E_ManyAgentsConvergeAndAreTraceable -v -timeout 0   # adds 10,000,000 (~13 min)

# Second trust root: re-certify the anchored policy with the external verified oracle.
cd govern && GSM_AST_CHECKER=/path/to/checker go test . -run TestAttestedEventTool_RealPolicyDigest -v
```

There is no `Makefile` in this repository; the `go test` invocations above are the interface.
