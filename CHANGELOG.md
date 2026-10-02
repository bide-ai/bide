# Changelog

All notable changes to bide are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While bide is pre-1.0, a
minor version (0.x.0) may include breaking API or journal-format changes; each one is marked
**Breaking:** below. Curated highlights for each release are in [docs/releases](docs/releases).

## [Unreleased]

### Added

- The Run API, under transitional names (P14): `Agent.RunMessage(ctx, runID, input Message, opts...)`, `Agent.ResumeRun(ctx, runID, opts...)`, `Agent.StreamMessage` with `AgentStream.Result()`, `Agent.RunTypedMessage[T]` (a Go 1.27 generic method), `Session.SendMessage` and `Session.SendMessageOnce`, which the 1.0 rewrite renames `Run`, `Resume`, `Stream`, `RunTyped`, `Send` and `SendOnce`. Each returns a `Result` whenever the run ID is valid (`agent.ValidateRunID`), whatever the error; `Result.Output` holds a typed run's answer as journaled. The input is a `Message`, so a run can start from text and images.
- Run options (P14): the agent-and-run options (`WithMaxTurns`, `WithTokenBudget`, `WithSystemPrompt`, `WithSampling`, `WithToolChoice`, `WithWaker`, `WithIdentity`, `WithMaxConcurrency`, `WithClock`) now apply per run, and `WithSaga()`, `WithToolFilter(names...)` and `WithOutputMode(OutputTool | OutputNative)` are new. The journaling rule: a run's first drive journals its input and options in `run:start` (`RunStart` gains `Session`, `Typed`, `Settings`, `Principal`, `Tools` and `Ext`, and the kind `session_turn`), and every later drive, a recovery drive included, runs under them. A later drive's different turn limit or token budget is journaled as an amendment `run:limits:<n>`; any other different setting (input, saga, tool filter, system prompt, sampling, tool choice, output mode, typed schema, principal) is `ErrConfig`, before any model call. The tool filter is enforced at dispatch: a call outside it records an error result and never runs (model 10, finding L5). The principal (`OnBehalfOf`, `AuthorityRef`) is journaled and restored; the `Actor` stays live.
- `agent.Cancel(ctx, j, runID, reason)` and `agent.Status(ctx, j, runID)` (`RunStatus`, `RunState`) (P14, D1 and D8). `Cancel` writes `run:cancelled`, or on a saga the rollback request `run:cancel-requested`, which the saga's drive answers by rolling the run back and writing `run:cancelled`. A drive checks for the cancellation when it starts (and again once it has written `run:start` or a limit amendment, or lost the `run:start` insert to another drive: model 10's `DStart` and `DAmend` return to `DOpen`), at every turn boundary after its first, and after each won side-effect claim, before the call (model 10, L2); calls in flight finish, and a retry-safe call already dispatched in the current turn may still run (no `Get` per retry-safe call). A sub-run (a sub-agent's, a `SubRunFor` run, a delegation) reads its tree root's cancellation too, at each of those checks and from the root's own store (a sub-agent outside a saga may journal to another), so a `Cancel` of the root stops the whole tree (model 11, `NoFireAfterRootCancel`). A `plan` flow honours `Cancel`: its open reads its end markers, a node reads `run:cancelled` once it will run (after a side-effect node's claim is won), and its `run:complete` is read back. The first end marker in journal order is a run's end for every reader, and each writer of one reads the markers back (L3). `Cancel` of a run already cancelled returns nil, of a run otherwise over `ErrRunEnded`. `Status` reads one `Load` (L6). New errors: `ErrRunCancelled`, `ErrRunEnded`, `ErrNotStarted` and `ErrNotResumable`, none in a category (`ErrNotStarted` is often a race with a run's first drive).
- Recovery dispatch (P14): `agent.Resumer` (`func(ctx, runID, start RunStart) error`), `agent.ResumeAgent(a, opts...)`, `agent.ResumeTyped[T](a, opts...)` and `agent.ResumeAny(rs...)`. A recovery pass reads each run's `run:start` under its lease; a run with none is skipped and reported once per process (`ErrNotStarted`), and read again on every pass (L7); a run no `Resumer` drives is reported once (`ErrNotResumable`); the process remembers its 65,536 most recent reports. A run a pass drove to its cancelled end (a saga's cancellation rollback included) is recovered, not a failure. `ResumeAgent` takes only the deployment's options (a Waker, a clock, a concurrency cap, an identity's `Actor`).
- Sessions (P14, model 12's S3): a Send turn whose run was cancelled is recorded closed, with no answer and outside the transcript, by the next message's `Send`; `Send` of the cancelled message (the same text) returns `ErrRunCancelled`. A saga turn's `Cancel` writes only its rollback request: the next message's `Send` drives the turn's rollback itself, under the turn lease and without holding the session handle's mutex (other callers on the handle are not blocked behind its compensators: while it is in progress, another worker or a second caller on the same handle gets `ErrTurnContended`), and then reads the journal again and records the turn closed. Over a store with no `Leaser`, the handle's mutex stays held across the rollback, so two callers on one handle never drive it at once.

- `audit.WithRollbackGrants(ctx, signer, grants...)` binds further grants a saga rollback verifies its delegations against, beside the acting grant (`WithGrant`): a saga whose delegations were minted under several grants (the root grant expired or was rotated between drives) rolls back with all of them bound. Each journaled child grant is verified against its own parent among the bound grants, under that grant's signer; a grant bound this way is never minted from (model 11, finding D1) ([#133]).
- `agent.ToolSpec` (`Name`, `Title`, `Description`, `Input`, `Output`, `Safety`, `Approval`, `Timeout`), everything the agent knows about a tool, and `agent.SpecOf(t)`, which reads a tool's `Spec() ToolSpec` method or, for a tool without one, its `Name`, `Description`, `ArgsSchema` and `Safety` (deprecated from the start: see Deprecated). The agent reads each tool's spec once, when it is registered, and decides every call from that copy ([#117]).
- Tool options: `agent.ToolOption` with `WithSafety`, `WithApproval`, `WithTimeout`, `WithTitle` and `WithOutputSchema`, and `agent.SingleApproval()`, the one-decision gate. `Func` and `CompensatedFunc` take trailing options; `SubAgent(name, description, sub, opts...)` takes `WithApproval`, so a parent can require approval before it delegates, and refuses `WithSafety` and `WithTimeout`. An invalid option panics with an error wrapping `ErrConfig`, as `Func` does for an argument type it cannot describe, until the 1.0 rewrite returns errors ([#117]).
- Tool timeouts (`WithTimeout`, `ToolSpec.Timeout`): the call, tool middleware included, runs under the deadline. A result returned after the deadline is recorded; an error returned after it has an unknown outcome: a side effect records nothing, fails the drive with `ErrToolOutcomeUnknown` and halts on resume, and a retry-safe tool records the error. It bounds only tools that honor their context ([#117]).
- `agent.ToolCall` (`Use`, `Spec`, `RunID`) and `ToolCall.ErrorText(err)`, what tool middleware receives ([#117]).
- Tool-result, denial and saga-failure records journal the `Safety` and approval gate the call ran under (`Record.Safety`, `Record.Approval`); a saga rollback reads the recorded safety ([#117]).
- A tool that wraps another says so with an `Unwrap() Tool` method; the agent follows it to find a wrapped `SubAgent`, so a saga rollback and the tree's token budget recurse into its sub-run ([#117]).
- `mcp.WithApproval(name, policy)`: `Tools` fails with `ErrConfig` when the policy is nil or invalid, or when the server lists no tool of that name, so a misspelt gate never leaves the real tool ungated. Each MCP tool's spec: `Title` (its title, else its annotations' title), `Output` (its `outputSchema`) and `Timeout` (`WithCallTimeout`) ([#117]).
- `govern.EventToolConfig` and `govern.FederatedEventToolConfig`, whose `Options` pass `agent.ToolOption`s to the tool; `audit.AttenuationConfig`, and trailing `agent.ToolOption`s on `audit.AttenuatingSubAgent`, which go to its `SubAgent` ([#117]).
- `agent.RunFilter.LeaseLapsed` admits only the runs whose lease has lapsed, by the comparison `AcquireLease` makes; `MemStore`, SQLite and Postgres evaluate it over their leases table, and `storetest` checks it (`Lister_LeaseLapsed`) ([#126]).
- `agent.WithRecoverLapsedConcurrency` caps how many lapsed runs `RecoverLoop`'s lapsed loop drives at once (16 by default), apart from `WithRecoverConcurrency` ([#126]).
- `Leaser.ReapLeases(ctx, ended)` deletes the lapsed leases no recovery pass takes over (a finished run's, or one on a run the store does not hold), checking the expiry in the same statement; `RecoverLoop`'s lapsed loop calls it on each pass, so a holder that died between its run's last write and its release no longer leaves a lease every later pass reads. `MemStore`, SQLite and Postgres implement it, and `storetest` checks it (`Leaser_ReapLeases`) ([#126]).
- Postgres: `Open` creates two indexes on the leases table when they are missing (`<prefix>leases_expiry` and `<prefix>leases_run_c`, on `run_id` under the "C" collation), so the lapsed listing reads only lapsed leases, or pages through many in order without sorting; on a store whose role does not own the tables, the owner opens it once to create them ([#126]).
- `agent.Build(model, journal, opts...) (*Agent, error)`, construction from options, and `Agent.With(opts...)`, which returns a configured copy and leaves the agent unchanged. `Build` and `With` return every configuration problem as `ErrConfig` when the agent is built: a nil model, journal, option, tool, middleware or function; two tools with one name or one named `final_answer`; a tool name the agent's model refuses, when the model declares its rule (`ToolRules`); a non-object input schema; an invalid approval policy, an m-of-n policy with no `WithApproverVerifiers` or with two approvers on one signing key; a negative limit; a `WithRetrieval` k below 1; and a tool choice with an unknown mode or a tool the agent lacks. `Build` is the transitional name of the 1.0 `New`. `Agent.Journal()` returns the agent's journal ([#127]).
- Agent options: `WithTools`, `WithMiddleware`, `WithToolMiddleware`, `WithApproverVerifiers`, `WithToolErrorRedactor`, `WithSystemPromptFunc` (its function gets the run's `RunInfo` and may fail), `WithRetrieval`, `WithOptions`, and `WithMaxTurns`, `WithTokenBudget`, `WithSystemPrompt`, `WithSampling`, `WithToolChoice`, `WithWaker`, `WithIdentity`, `WithMaxConcurrency` and `WithClock`. The last value given for a setting wins; `WithSystemPrompt` and `WithSystemPromptFunc` share one slot. The agent's `WithIdentity`, `WithWaker` and `WithClock` apply to a run whose context carries none ([#127]).
- Option scopes are interfaces with unexported methods (`Option`, `RunOption`, `ParallelOption`, `StepOption`, `ResolveOption`, `LeaseOption`, `RecoverOption`, `RecoverLoopOption`, `RetrievalOption`, and `ToolOption`), and a setting for several scopes returns one of the combination types `AgentRunOption`, `ConcurrencyOption`, `ClockOption`, `SafetyOption` or `LeaseControl`, so an option passed where it does not apply does not compile. `RunOption` values are type-checked now and taken by the run API in the next redesign step ([#127]).
- `agent.RunInfo` (`RunID`, `RootRunID`, `ToolUseID`, `Saga`) and `RunInfoFrom(ctx)`, which describe the tool call a context belongs to, and `RunInfo.SubRunFor(name)`, the run ID of a programmatic sub-run (`<scope>>step:<name>`), which `Run` accepts from that call's context ([#127]).
- `agent.WithRetrievalRetry(n, base, max)`, a `RetrievalOption` for `WithRetrieval(r, k, opts...)`: a failed `Retrieve` is retried up to n more times within the retrieval step, with exponential backoff and full jitter, and only the attempt that succeeded is recorded. `agent.RetrieverFunc` adapts a function to a `Retriever`, which is how a policy wraps one ([#127]).
- `agent.WithSubRuns(agentFor)`, a tool option that declares the agent each of the tool's programmatic sub-runs (`RunInfo.SubRunFor(name)`) runs with. A saga's rollback walks every programmatic sub-run a call started, latest first and after the call's own compensation, as it walks a sub-agent's: a run in a saga's tree (a saga, or a plain run started from one's call) links each one in its journal (`@subrun/<call>/<name>`) before the sub-run records anything, and the rollback compensates the sub-run's writes with the declared agent's compensators, or, with none declared, reports them in `SagaAborted.Uncompensated`. A declared agent it cannot use (on another store, or a `WithSubRuns` function that panics) is listed there with the reason. A sub-run in a saga's tree must journal to the saga's store: `Run` refuses another with `ErrConfig` ([#127]).
- `agent.ToolRules`, an optional interface a `Model` implements (itself or through `Unwrap() Model`) to declare the tool setups its provider refuses: `ToolNameRule() *regexp.Regexp` and `RequiresToolsForRequired() bool`. `Build` and `With` refuse a tool name the declared rule does not match, and check nothing for a model that declares no rule. `model/openai` and `model/anthropic` declare `^[a-zA-Z0-9_-]{1,64}$`, and `model/gemini` its own `^[a-zA-Z_][a-zA-Z0-9_.:-]{0,63}$`, so a dotted MCP tool name still builds for Gemini. Tool choice `required` on an agent with no tools of its own is left to the run, since `RunTyped` supplies an answer tool: a run with nothing to call, under a model that declares it needs one, fails with `ErrConfig` before it opens the journal or calls the model ([#127]).
- `agent.ErrTurnContended`: a `Session.Send` or `SendOnce` whose turn run another holder leases returns at once with an error wrapping it, having driven nothing (no category, like `ErrLeaseLost`); send the message again later. `Agent.Session(ctx, id, opts...)` takes `LeaseOption` values (`WithLeaseHolder`, `WithLeaseTTL`) for that lease ([#137]).

#### Formal models

- P14 implements model 10's adopted rules L2 to L7 and model 12's S3 (rule 16 of the P14 contract): `findings/cancel-saga-marker`, `filter-request-only`, `status-gets`, `not-started-remembered` (model 10) and `s3-cancel-wedge` (model 12) are regression configurations now, each still failing under its old rule; the drive, `Cancel`, `Status` and session code carries the `protocol:lifecycle` and `protocol:sessions` markers of the actions that were on the no-code lists.
- A TLA+ model of the tool-call state machine of P12 (model 9, `spec/tla/toolcall`): the call state and began word, the base handler entered by several invocations (retries, a `next` left running), the tool middleware, the loop's record decision, the errgroup, retry-safe steps that change state, and the saga rollback's re-run and compensation, under store faults, crashes, cancellations and deadlines. It found six bugs in #117 before it merged (T1 to T6: a known failure or a result recorded for a call whose tool began, did not itself fail, or was still running; "already ran" for a tool that never began; an earlier attempt of a retry-safe saga step listed nowhere; a retry-safe tool begun after its chain returned), and a rollback re-run with no end; each is a regression configuration.
- A TLA+ model of the run lifecycle and recovery (model 10: `run:complete`, `run:aborted` and the reserved `run:cancelled`; leased and plain runs, `Recover` and `RecoverLoop` passes with the end-marker re-check under the lease, halts, pauses and `ResolveHalt`, under lease expiry, stalled holders, ambiguous writes and crashes), checked in CI. It shows the pickup latency the v0.9.0 docs describe (a dead holder's run waits behind the halted runs listed before it), checks a proposed fix, and states the property P14's `Cancel` must satisfy.
- Code and models are kept in step (milestone M4 of the formal-models plan): the Go code models 1, 1b, 7, 8 and 10 describe is wrapped in `// protocol:<model> begin <Action> ...` / `// protocol:<model> end` region markers, and the Lint job runs `internal/tools/modelsync`, which fails a pull request that touches a marked region without changing `spec/tla/<model>/` (unless the description or a commit message holds `Protocol-Impact: none (<reason>)`, printed as a warning), and any disagreement between the markers, the model-to-code maps in `spec/tla/README.md` and the specs' action names. `TestProtocolVocabulary` checks that the claim code's key constructors and record kinds match the record kinds `Claims.tla` declares, and runs in the same job.

#### Documentation

- [Formal verification](docs/formal-verification.md), an overview of bide's TLA+ models: why bide model-checks, what each model guarantees and which code it covers, the bugs the models caught before release (F1 to F5, P1, P2, T1 to T6, L1 to L3) and where each was fixed, what runs on a pull request and nightly, how the models and the code stay in step, the models planned next, and what the models do not cover. The formal-models plan's status markers and the roadmap are brought up to date (models 9 and 10, M4 done) ([#128]); a section explains TLA+, PlusCal, TLC and model checking for readers new to them ([#131]).
- Apalache (v0.62.2, pinned with its SHA-256 in `spec/tla/tools.lock`) checks the claim model nightly, beside TLC: `spec/tla/check.sh apalache [model|file.cfg]` downloads and verifies it, unpacks a fresh copy, and removes its work directories on exit, failure and interrupt. A typed wrapper (`ClaimsApalache.tla`) instantiates the model unchanged, and an inductive invariant (`ClaimsInductive.tla`) proves {{CHANGELOG_IND}} in every reachable state, at any depth and for any number of faults, with every placement of the drivers, kind of call and late-commit setting left to the solver; the drivers, processes, calls, attempts and claim ids stay bounded ([Apalache results](spec/tla/README.md#apalache-results)). A regression check must find #90's F2 (`NoLiveOverride`) symbolically. The new **Apalache (nightly)** job runs them; the required **Models** job is unchanged. Model 9 has a typed wrapper (`ToolCallApalache.tla`) and no Apalache check yet. To type model 1, `Claims.tla` changed three expressions without changing their meaning; TLC passes every configuration as before.

### Changed

- Three governance examples declare the gsm rules of six registries with combinators instead of closures (`examples/govern/compose`'s inventory, `coordination`'s two registries, `mesh`'s three lines), so both of gsm's extracted checkers, not only the table checker, certify them in the gsm machine gate. Their behaviour and output are unchanged ([#146]).
- **Breaking:** `agent.Record` shrinks from 368 to 256 bytes, from the 384-byte Go size class to the 256-byte one, so each heap copy of a record (a decode, an encode, a store's put) is smaller. Two groups of rarely set fields move behind embedded pointers: `*agent.ModelTurn` holds `Finish`, `RawFinish`, `Model`, `PromptDigest` and `ToolsDigest` (set on `StepModel` records only), and `*agent.ApproverSignature` holds `Approver`, `ApproverAlg` and `Signature` (set on decisions `SubmitDecision` records only). Direct access to those fields no longer compiles. Migration: read them with the nil-safe methods of the same names (`rec.Finish()`, `rec.Approver()`, ...), which return the zero value for a record that carries none, and build a record with `ModelTurn: &agent.ModelTurn{...}` or `ApproverSignature: &agent.ApproverSignature{...}`. The journal encoding is byte for byte the same: the members keep their names, order and `omitempty` behaviour, and existing journals decode as before. Measured A/B against main on two 4-vCPU runners (bench.yml, 21 interleaved runs per binary, AMD EPYC 7763 and 9V74): the overhead scenario ran +0.7% and +1.6% runs/s, mean latency -0.4% and -1.6%, p90 -1.0% and -9.1%, p99 -4.4% and -7.8%; the fanout scenario ran -1.4% and +1.2% runs/s, within the runners' noise, with mean, p90 and p99 within 2% either way. In the Go benchmarks (benchstat, 10 runs per ref), `RunTurns` allocates 4.4% fewer bytes (73.2 to 70.0 KiB/op) and `ToolCallSideEffect` 6.0% fewer (26.1 to 24.5 KiB/op), with 1.3% more allocations (a model record's `ModelTurn` is its own allocation); `RecoverPass10k` is unchanged, and no time per op changed significantly ([#140]).
- **Breaking:** `agent.Recover` and `agent.RecoverLoop` take an `agent.Resumer` (`func(ctx, runID, start RunStart) error`) instead of `func(ctx, runID) error`. Migration: pass `agent.ResumeAgent(a)`, or add the `start agent.RunStart` parameter to your own callback; a run an earlier version started is not driven by `ResumeAgent` (see the kind entry below) (P14).
- **Breaking:** `RunStart.Input` is a `Message` (a user message of one text part is still journaled as a JSON string, so existing records read back unchanged); read its text with `start.Input.Text()` (P14).
- **Breaking:** `agent.Sampling` and `agent.ToolChoice` marshal with snake_case JSON names (`temperature`, `top_p`, `max_tokens`, `stop`, `seed`; `mode`, `name`), as run:start journals them (P14).
- **Breaking:** `RunTyped` and `RunTypedNative` journal the typed start (output mode and the answer type's schema), so resuming a typed run started under this version through `Run`, or with another type, is `ErrConfig` (P14).
- **Breaking:** a run's first end marker in journal order is its end for every reader (P14, L3): a run whose `run:cancelled` precedes its `run:complete` returns `ErrRunCancelled`, not its answer, and `IsComplete` reports it not complete.
- **Breaking:** `ErrNotStarted` wraps no category (it wrapped `ErrConfig` earlier in this release cycle).
- P14 writes the kind of every run it starts in `run:start` (`RecordedStart(...).Kind` is `agent` for an agent run). A `run:start` an earlier version wrote, with no kind and no typed start, is driven by any agent entry point, as before: a session turn, a `SendOnce` turn or a typed run in flight across the upgrade still resumes through its session or `RunTyped` (its input is still held). **Upgrading:** recovery does not drive such a run through `ResumeAgent` or `ResumeTyped`: the record does not say whether a plain run or a typed one started it, so both return `ErrNotResumable` for it (`Recover` reports it once per process). Finish the runs in flight before upgrading, or recover them with a `Resumer` of your own, after `ResumeAgent` in `ResumeAny`, that knows which entry point started each (`RunTypedMessage` for a typed run, `ResumeRun` for a plain one).
- `WithToolChoice(ToolChoice{Mode: "none"})` is enforced at dispatch: a tool call the model makes anyway is refused with an error result, as a call outside the tool filter is (a typed run's answer tool excepted).
- A saga's rollback request is read before a recorded failure, and a failure's rollback of a saga whose rollback request exists ends `run:cancelled` (one more `Get`, on that rollback only), and so does a saga sub-run's failure rollback once its tree root was cancelled (up to two `Get`s more, from the root's store). A drive without `WithSaga` of an aborted saga (`RunMessage`, `ResumeRun`) reports `*SagaAborted`, not `ErrConfig`; a cancellation's rollback that finds `run:aborted` first reports the abort.
- The counting-store budget (docs/design/api-v1.md, 10.3) is raised by P14's reads, a maintainer decision: every model turn after a drive's first costs one more `Get` (`run:cancelled`), a side-effect call one more (`run:cancelled` once its claim is won), a completion one more (two on a saga: the end markers read back), and a recovery pass four `Get`s per driven run (its `run:start`). The #138 review added one `Load` after a first drive writes `run:start` (and after a limit amendment), reading the entries' names only; a sub-run's checks read its tree root's cancellation (up to two `Get`s each); a flow node that runs reads `run:cancelled` (one `Get`), and a flow's completion and its resumed open read the end markers. Over a transitional `Durable` with no `Journal`, each of these reads is a `History` (P14).
- A recovery pass costs less than it did before P14 although it reads each driven run's `run:start` (rule 14): the commonest start (a plain text agent start, with or without its kind) is read without the JSON decoder, exactly, its salt checked as the full decoding reads it (anything else takes the JSON decoding, into a pooled record whose name, kind and salt are matched without decoding strings; a record with any member but those and its result, or one of them twice, takes the full decoding, so a start either path reads is the start the full decoding reads, which a fuzz target checks), and a leased drive starts its renewal goroutine at the first renewal (half the TTL) rather than with the drive, so a short visit (a halted run, a run over since the listing) starts none. `BenchmarkRecoverPass10k` against `main` in the bench workflow's A/B on the final code: -33.6% and -30.8% time, -6.6% B/op, -5.9% allocs (runs 36957790848 and 36957793253; +95% with the first P14 draft). Every leased drive (`Lease`, a session turn) gains the renewer change.
- P14's per-run reads (the `Load` after `run:start`, the turn boundary's `run:cancelled`, the end marker's read-back) cost the closed-loop fanout scenario its tail on `MemStore`, whose one mutex every read and write takes: a drive's own cancellation checks and a writer's read-back no longer look the run up in the journal's known-runs set on a miss (the drive opened, or the writer just wrote to, the run), `MemStore` copies entry data outside its mutex (entries are never modified once appended), `run:start` is encoded in one pass, the input in place (the bytes unchanged), and a drive grows its goroutine's stack once, before it starts (`probeDriveStack`, sized against the drive's stack need, which `TestDriveStackHighWater` pins), rather than twice deep inside it. The bench workflow's A/B against `main` on the final code (runs 36957790848, AMD EPYC 9V45, and 36957793253, AMD EPYC 7763; 21 interleaved runs per binary, medians): `cmd/bench` overhead (mean, p90, p99) -2.5%, -30.4%, -37.5% and -1.9%, -16.7%, -14.3%; fanout -1.1%, -9.5%, -5.2% and -5.6%, -9.4%, -13.7%; benchstat (10 runs per ref) `BenchmarkRunTurns` +4.0% and +2.1% time, neither significant (p=0.165, p=0.280), +0.3% B/op, +0.9% allocs (8 per run); `BenchmarkToolCallSideEffect` -1.7% (not significant, p=0.796) and +3.3% (p=0.035) time, +0.5% B/op, +2.6% allocs (8 per call). Before these changes fanout was +3.4% to +10.6% at p99.
- **Breaking:** a finished agent run returns its recorded answer only to a drive with the input its `run:start` recorded; `Run`, `RunSaga`, `Stream` and the rest with another input are `ErrConfig`, as for an unfinished run (#70) and a finished flow's run. Before, a finished run answered any input with its own answer, so a `SendOnce` key reused for another message, whose run had finished and was not yet recorded, was answered with the first message's reply and recorded as that message's turn ([#137], review R137-2).

- A session's turn run is driven under its lease when the store implements `Leaser` (`MemStore`, SQLite, Postgres), as `agent.Lease` drives a run: the run loads its journal only once it holds the lease. A worker that finds another holder leasing the run returns its recorded answer if the run has finished, and `ErrTurnContended` otherwise (model 12, finding S4, `TurnLease`). **Cost:** a first `Send` turn over `MemStore` takes about 14 µs, 25 allocations and 1.7 KiB more (`BenchmarkSession_Send`, the lease's renewer goroutine and timer), and over SQLite or Postgres two more single-row statements (the lease's acquisition and release) ([#137]).
- `Leaser.ReapLeases` also deletes the lapsed leases of runs whose ID contains `>` (a session's or a sub-agent's run), which no recovery pass takes over, so a worker that died mid-turn does not leave a lease every later lapsed pass reads. `MemStore`, SQLite and Postgres implement it, and `storetest`'s `Leaser_ReapLeases` checks it. A custom `Leaser` must do the same ([#137]).

- The required Models job checks four configurations at a time (one TLC worker each) and, on a pull request, only the models whose `spec/tla/<model>/` directory the pull request changes; the merge queue and main still check every model. The largest passing pull-request configurations of models 1, 2, 7 and 8 (1.4 to 2.9 million states) and two of model 1's liveness checks run nightly, each safety one with a smaller pull-request counterpart of the same invariants, so every path keeps a passing configuration and its vacuity run on pull requests. The job takes about 6 minutes, down from 9 to 15.5 ([#132]).
- The gsm machine gate checks with gsm at 219dcaa, whose pinned rules checker carries normalization-confluence#11's fixes ([#147]).
- CI runs its two slowest jobs in parallel shards, with no check dropped: the Models configurations in six shard jobs by configuration directory (`check.sh`'s new `TLC_DIRS`), and the Linux Go tests in four jobs (the core's `agent` package, the rest of the core, the other modules, and the `nojsonv2` run), each with the same flags as before. The required check names are unchanged: a final **Models** and **Test (ubuntu-latest)** job needs every shard and fails unless each succeeded (or nothing needed checking). `.github/scripts/shards.sh` fails CI unless every module, every package of the split core and every pull-request configuration is in exactly one shard, naming the line to change, and its self-test proves it catches a missing, doubled or unknown entry. With every model and every module checked, the Models workflow took 3.5 to 4.1 minutes on this pull request's runs, against 7.6 to 7.8 on pushes to main and 9.8 to 10.7 in the merge queue before, and the CI workflow 5.2 to 6.0 minutes, against 7.5 to 8.4 on main (each from the run's creation to its last update) ([#143]).

- **Breaking:** `agent.Safety` is plain data, `{ReadOnly, Idempotent}`: comparable, and journaled. The approval gate is `ToolSpec.Approval`, set with `agent.WithApproval(agent.SingleApproval())` (for `Safety{RequiresApproval: true}`) or `agent.WithApproval(&agent.ApprovalPolicy{...})` (for `Safety{Approval: ...}`). `ApprovalPolicy` encodes as `need` and `approvers` ([#117]).
- **Breaking:** `agent.ToolHandler` is `func(ctx, ToolCall) (json.RawMessage, error)`: tool middleware reads `call.Use` and `call.Spec`, and passes `next` a copy with other `Use.Args` to rewrite arguments ([#117]).
- **Breaking:** `agent.Request.Tools` is `[]agent.ToolSpec`, sorted by name, and `ToolsDigest` takes `[]ToolSpec` (the digest is unchanged) ([#117]).
- **Breaking:** a tool result's `read_only` field is replaced by `safety` and `approval` (`Record.ReadOnly` is replaced by `Record.Safety`, and `Record.Approval` is new); a result journaled by an earlier pre-release carries no `safety`, so a rollback over it treats every completed call as a write (the safe reading); resuming across pre-releases is not guaranteed (the journal format stays `bide.journal.v1-dev`) ([#117]).
- **Breaking:** `govern.EventTool(gov, EventToolConfig{...})` and `govern.FederatedEventTool(gov, FederatedEventToolConfig{...})` replace their positional forms; `audit.AttenuatingSubAgent(name, description, sub, AttenuationConfig{Store, Narrow, Rules}, opts...)` replaces its positional form ([#117]).
- **Breaking:** `mcp`'s `Tools` fails with `ErrProtocol` when a server lists a tool whose `outputSchema` is not an object schema (a null one is no output schema) ([#117]).
- **Breaking:** new construction panics, each with an error wrapping `ErrConfig`: `audit.AttenuatingSubAgent` on a nil `AttenuationConfig.Store` or `Narrow`, or an option `agent.SubAgent` refuses; `govern.EventTool` and `govern.FederatedEventTool` on an invalid `agent.ToolOption` in their `Options` (and `EventTool` on `Attested` with an empty `PolicyDigest`) ([#117]).
- **Breaking:** `mcp.WithSafety` sets only `ReadOnly` and `Idempotent`; gate an MCP tool with `mcp.WithApproval`. `WithCallTimeout` also sets the tool's `ToolSpec.Timeout`, which the agent applies ([#117]).
- **Breaking:** only `agent.SingleApproval()` asks for the one-decision gate; it is a distinct value, never inferred from a policy's shape, so an `ApprovalPolicy` literal with no approvers (`{Need: 1}` included) is `ErrConfig`, in `agent.WithApproval` and `mcp.WithApproval`. `ApprovalPolicy.Clone` copies a policy, a `SingleApproval` included. `ApprovalPolicy` has an unexported field, so an unkeyed literal (`ApprovalPolicy{2, approvers}`) no longer compiles: name the fields ([#117]).
- **Breaking:** tool middleware passes `next` the `ToolCall` it was given (or a copy with other `Use.Args`); a call whose `Use.Name` or `Use.ID` it changed, or a `ToolCall` it built itself, fails with `ErrConfig` and the tool is not called ([#117]).
- **Breaking:** `SagaAborted.UnknownOutcome` lists saga steps that failed with an unknown outcome (a retry-safe step that returned `ErrToolOutcomeUnknown`, or an error after its deadline), which may have committed; their failure records carry `outcome_unknown`. The rollback reports them rather than take them for steps that changed nothing, and does not run their compensators on a result they never returned ([#117]).
- **Breaking:** `New` refuses (as `ErrConfig`, before any model call) a tool that unwraps (`Unwrap() Tool`) and is a `Compensator`, and one that wraps a sub-agent and has a `Timeout` ([#117]).
- `New` validates every tool's `ToolSpec.Approval`: a policy a custom `Spec()` returns that no option would build (a `SingleApproval` whose fields were changed, an m-of-n policy with no approvers) fails every run with `ErrConfig` before any model call, where it used to be approved, fire, and then fail to encode its result ([#117]).
- **Breaking:** a decorator that embeds a tool (`struct{ agent.Tool }`, overriding `Call`) has no `Spec` method, so its spec, read from the old method set, would have no approval gate and no timeout, and the gated tool inside would run ungated. `New` (and plan's tool check) now fails closed: a tool that embeds, at any depth, a tool with an `Approval` or a `Timeout` its own spec lacks is `ErrConfig` ("decorator hides the approval gate; implement Spec or Unwrap"). A decorator keeps them by implementing `Spec`, or `Unwrap() Tool`: `SpecOf` of a tool with no `Spec` method takes `Title`, `Output`, `Approval` and `Timeout` from the first tool on its `Unwrap` chain that has one ([#117]).
- Behaviour change for `mcp.WithCallTimeout` users: an error the server reports after the call's deadline (a late JSON-RPC error, a known failure) now has an unknown outcome under the agent's timeout rule, so a side effect records nothing and halts on resume instead of recording the failure. The agent cannot tell a late failure from a late answer to a call that took effect, and takes the safe side ([#117]).
- `agent.ErrToolNotCalled`, a condition (no category) for a tool call known never to have reached its tool. A tool middleware that ends a call without calling `next` returns an error wrapping it, and only then; `middleware.ToolRateLimit` and `middleware.ToolRetry` do. The agent tracks each call's state by compare-and-swap (reached, refused by the base handler, or closed when the chain returned without entering it): a call counts as not called only when refused, or closed with `ErrToolNotCalled`; any other closed call leaves a side effect's outcome unknown and the run halts. The base handler refuses an invocation that comes after the chain returned. The `ToolMiddleware` contract: reach the tool only through `next` ([#117]).
- The journal encodes `SingleApproval` as `{"single":true}`, the bide protocol's form (`ApprovalPolicy` has `MarshalJSON` and `UnmarshalJSON`); m-of-n policies encode as before ([#117]).
- `New`'s wrapper check walks the whole `Unwrap` chain, and plan's `Builder.Tool` (at `Build`) and `RegisterTool` run the same check ([#117]).
- `audit.AttenuatingSubAgent` reuses the grant a resumed delegation journaled instead of minting another, journals a delegation that ran without a grant, and refuses a delegation resumed under other authority, from an expired bound grant, or onto a sub-run that has records but no journaled authority, with an `ErrConfig` that records nothing (the run stops once siblings in flight finish; a sibling's pause is joined to it), so a re-drive under the right grant continues it. A delegation cannot run past its grant's `NotAfterUnix`: every tool call in its sub-run is refused, recorded, once the child grant has expired, and a delegation resumed after its journaled grant expired fails, recorded (a saga rolls back). A child grant's subject must be the sub-agent's name. Its rollback binding verifies the journaled grant (signature under the bound signer's key, attenuation of the bound parent, its subject) and stops with `ErrConfig` when no grant and signer are bound. A failure to read or write the delegation's authority in the store records nothing, so a resume retries the delegation, as for a plain `SubAgent`; only value records count as journaled authority ([#117]).
- **Breaking (journals):** a saga journaled by an earlier pre-release that holds an ungranted `audit.AttenuatingSubAgent` delegation cannot be rolled back after the upgrade: its sub-run records no authority, and the rollback stops (`ErrProtocol`) rather than guess. Finish or roll back such sagas before upgrading; journals are not promised across pre-releases ([#117]).
- The approval policy decodes strictly: `{"single":true}` alone is `SingleApproval`; `single` with any other value or beside `need` or `approvers`, a policy with no `need`, and duplicate, unknown or mistyped members are `ErrProtocol`. That is `ApprovalPolicy.UnmarshalJSON`, for a policy being configured or received; the approval inside a stored `Record` (`DecodeStoredRecord`, `DecodeRecord`, and so a proof bundle's `Record()`) is read leniently, ignoring members this version does not know, so a journal a newer version wrote stays readable ([#117]).
- A call that reached the base handler but returned before its tool's `Call` began (a saga-arguments write cut off by the saga's cancellation) counts as not called: a per-call word, set by compare-and-swap immediately before the tool and sealed by the loop when the chain returns, proves no call began and none can ([#117]).
- `New` refuses a wrapper that gives a wrapped sub-agent another `Safety`, as it refuses a `Timeout` ([#117]).
- A tool call's state is terminal once the middleware chain returns (a refused call too), so a `next` left running cannot reach the tool after the loop recorded the call; its saga arguments are journaled only once it reaches the tool, and "already ran" (`ErrToolReinvoked`) is decided by the compare-and-swap that begins the tool, so it is said only of a tool that began ([#117]).
- **Breaking (behaviour):** a side effect whose tool began and did not itself fail is never recorded as a known failure. When the chain returns an error for it (a middleware turned its success into an error, or returned a retry's refusal, or left the call running) and the context is live, the error wraps `ErrToolOutcomeUnknown`, nothing is recorded, and the run halts. The `ToolMiddleware` contract says so. Found by the TLA+ tool-call model. In a saga the same holds for a retry-safe step that changes state (`Idempotent`, not `ReadOnly`): its failure is recorded with an unknown outcome and listed in `SagaAborted.UnknownOutcome`, never skipped by the rollback as a step that made no change; outside a saga a retry-safe tool's error stays an ordinary failure ([#117]).
- In a saga, a compensable retry-safe step that changes state (`Idempotent`, not `ReadOnly`, a `Compensator`) journals its accepted arguments before each call whether or not a middleware changed them: the record is the step's "may have begun" marker, since such a step writes no attempt marker. When an earlier drive's attempt journaled it and left no outcome, a later known failure of the step (the tool's own error, a middleware's refusal, `ErrToolNotCalled`) is recorded with an unknown outcome and listed in `SagaAborted.UnknownOutcome`, and a rollback reports such a step that was later denied; within one drive the same holds for any retry-safe write a middleware ran again after an invocation that did not itself fail. A store fault writing that record (or a rewritten call's arguments) fails the run and records nothing, so a re-drive calls the tool; it used to be recorded as a known failure. Found by the TLA+ tool-call model (T3) ([#117]).
- **Breaking (behaviour):** a result needs positive proof: a tool middleware chain that returns a result while any invocation of the call's tool is still running in the process (a middleware that left `next` running and answered itself, from a cache say; a sibling invocation; one a cancelled drive left behind, counted per run and call) has an unknown outcome (`ErrToolOutcomeUnknown`), so a side effect halts and a retry-safe saga step is listed in `SagaAborted.UnknownOutcome` and never compensated, where it used to be recorded as succeeded and could be compensated before its effect landed. A saga rollback that re-runs a retry-safe step to learn the result to compensate, and gets an unknown outcome (a result check that rejects every success, say), reports the step in `SagaAborted.UnknownOutcome` and finishes, instead of stopping on every drive. A rollback re-run that a middleware answered without reaching the tool is not taken for the step's result either. The count is read whatever state the chain ends in, so a re-drive's cache answer while the earlier drive's invocation still runs is unknown too (T6). Found by the TLA+ tool-call model (T4) ([#117]).
- Once a tool call's middleware chain has returned, no invocation of `next` begins the tool, even a retry-safe one an earlier invocation reached: it used to begin again after the call's result was recorded, and after a saga's compensation. Found by the TLA+ tool-call model (T5) ([#117]).
- Tool timeouts are judged by the deadline itself (a context's error lags its timer), for the tool's timeout and the run's own deadline; only a call whose tool was actually called can be late or unknown: a call a tool middleware ended first is a known failure, and a tool whose deadline passed in the middleware is not started. Plan flows' Tool nodes apply the wrapped tool's `ToolSpec.Timeout` with the same rule ([#117]).
- `NextOnceKey` and `Safety.Idempotent` document that once keys are scoped to one tool call: a retry the model makes is a new call with new keys, so dedup across the model's retries needs a business key from the arguments ([#117]).
- **Breaking:** `agent.RunFilter.Admits` takes a third argument, `lapsed func() bool`, which reports whether the run's lease has lapsed; it is called only when the filter sets `LeaseLapsed` ([#126]).
- **Breaking:** `agent.Leaser` has a fourth method, `ReapLeases` ([#126]).
- **Breaking:** `agent.WithRetrieval(r, k)` is an agent `Option`, not a model middleware: the agent retrieves for the run's user message as a journaled engine step, at a drive's first model call, and every model middleware sees the request with the documents in it. Migration: `a.Use(agent.WithRetrieval(r, k))` becomes `agent.Build(model, j, agent.WithRetrieval(r, k))` or `a.With(agent.WithRetrieval(r, k))`. A retrieval outside an agent run no longer exists. The retrieval runs before the model middleware chain, so model middleware neither retries it nor prevents it: `middleware.Retry` no longer retries a failed retrieval (use `agent.WithRetrievalRetry`), and a model middleware that refuses the call (a policy gate, a spend cap) runs after the query has reached the `Retriever` and the documents are journaled. A policy that must keep a query from the store wraps the `Retriever` (see `agent.RetrieverFunc` and the RAG guide) ([#127]).
- **Breaking:** `agent.RetrievalTool(name, description, r, k, opts...)` takes the tool's name and description and the tool options, as `Func` does; `RetrievalOption`, `RetrievalName` and `RetrievalDescription` are removed ([#127]).
- **Breaking:** `trace.Instrument(tracer, opts...)` returns an `agent.Option`. Migration: `trace.Instrument(a, tracer)` becomes `agent.Build(model, j, trace.Instrument(tracer))` or `a.With(trace.Instrument(tracer))` ([#127]).
- **Breaking:** the context decorators `agent.WithIdentity(ctx, id)`, `agent.WithWaker(ctx, w)` and `agent.WithClock(ctx, now)` are renamed `ContextWithIdentity`, `ContextWithWaker` and `ContextWithClock` (transitional), and the `With` names are the options. A value bound to the run's context takes precedence over the agent's option ([#127]).
- **Breaking:** `agent.WithNow` (a `ResolveOption`) is replaced by `agent.WithClock`, and `agent.StepSafety` by `agent.WithSafety`, which a tool and a `Step` both take. `WithClock(nil)` is `ErrConfig` (`WithNow(nil)` was ignored) ([#127]).
- **Breaking:** `agent.Parallel(ctx, d, runID, tasks, opts...)` takes the tasks as a slice and the concurrency cap as `WithMaxConcurrency(n)` (a negative n is `ErrConfig`); the positional `maxConcurrency` is removed ([#127]).
- **Breaking:** `Lease` takes `LeaseOption`s, `Recover` `RecoverOption`s and `RecoverLoop` `RecoverLoopOption`s. `WithLeaseHolder` and `WithLeaseTTL` fit all three; `WithRecoverInterval`, `WithRecoverConcurrency` and `WithRecoverErrors` fit only `RecoverLoop`, so passing one to `Recover` or `Lease`, which ignored it, no longer compiles ([#127]).
- **Breaking:** `agent.RunScope` and `agent.InSaga` are replaced by `RunInfoFrom`: `SubRunID(info.RunID, info.ToolUseID)` is a call's sub-agent run ID, and `info.Saga` whether its run is a saga. A saga rollback's re-run of a retry-safe call now carries that call's `RunInfo` (it carried whatever scope the rollback's context held) ([#127]).
- **Breaking (behaviour):** whether a tool call is in a saga is its own run's flag (`RunInfo.Saga`). A plain run started from a saga's tool call (`child.Run` with a `SubRunFor` ID) is not a saga, and neither are its calls: they no longer inherit the saga mode of the call that started the run, so such a run's compensable calls do not journal their accepted arguments, and its sub-agents run as plain runs. Start the sub-run with `RunSaga` to keep it a saga. A sub-agent (`SubAgent`) called from a saga runs as one, as before ([#127]).
- A programmatic sub-run is refused (`ErrConfig`) once the tool call whose context names it (`SubRunFor`) has returned, so a goroutine that outlives its call cannot start a sub-run no call owns, which no rollback and no `Recover` would reach. In a saga, a programmatic sub-run's name must be at most 96 bytes once escaped, so its link can name it ([#127]).
- `Agent.WithSystemPrompt` and `Agent.WithSystemPromptFunc` share one slot: the later call wins (the function used to win whatever the order) ([#127]).
- The system prompt function (`WithSystemPromptFunc`) is called once per drive just before the drive's first model request, and not at all by a drive that sends none: reading back a finished run, or a resume whose pending tool calls pause or halt, no longer fails when the function does. A resumed turn's pending tool calls now run before the prompt function is called (it was called first, before them) ([#127]).

### Deprecated

- The string entry points are transitional (P14): `Agent.Run(ctx, runID, input string)`, `RunSaga`, `RunResult`, `RunSagaResult`, `Stream`, `StreamSaga`, `AgentStream.Final`, `Session.Send` and `Session.SendOnce` (strings), and the functions `RunTyped` and `RunTypedNative`. The 1.0 rewrite removes them for `Run`, `Stream`, `RunTyped` and `Session.Send`/`SendOnce` with a `Message` and run options.

- `agent.SpecOf` is transitional: the 1.0 rewrite gives `Tool` a `Spec` method, and `t.Spec()` replaces `SpecOf(t)` ([#117]).
- `agent.New` and the builder methods (`Use`, `UseTool`, `WithSampling`, `WithToolChoice`, `WithSystemPrompt`, `WithSystemPromptFunc`, `WithMaxTurns`, `WithTokenBudget`, `SetMaxConcurrency`, `WithApproverVerifiers`, `WithToolErrorRedactor`) are transitional: the 1.0 rewrite removes them and renames `Build` to `New`. They still change the agent they are called on. `New` keeps its lenient checks; only `Build` and `With` refuse a reserved tool name and a non-object input schema ([#127]).
- `agent.ContextWithIdentity`, `ContextWithWaker` and `ContextWithClock` are transitional: the run API takes the identity, Waker and clock as run options ([#127]).

### Removed

- **Breaking:** `Safety.IdempotencyKey` (the SDK never called it; `Idempotent` says the same, and the tool derives its own downstream key or uses `NextOnceKey`), `Safety.RequiresApproval` and `Safety.Approval` (see `WithApproval`) ([#117]).
- **Breaking:** `agent.ToolSafety`, `agent.WithToolSafety` and `agent.ToolErrorText`: tool middleware reads `ToolCall.Spec` and `ToolCall.ErrorText`, and no tool data travels in the context ([#117]).
- **Breaking:** `govern.AttestedEventTool`; an `EventToolConfig` with a `PolicyDigest` is the attested form. Migration: `AttestedEventTool(gov, name, desc, event, digest, safety)` becomes `EventTool(gov, EventToolConfig{Name: name, Description: desc, Event: event, PolicyDigest: digest, Attested: true, Safety: safety})`. The old form accepted an empty digest and still recorded the state digest and acting identity; `EventToolConfig{PolicyDigest: ""}` is the plain tool, which records neither, so a config that sets `Attested` with an empty `PolicyDigest` is refused (`EventTool` panics with `ErrConfig`) rather than silently dropping the attestation ([#117]).

### Fixed

- `examples/govern/mesh` was not convergent (each signal event set the level, so order mattered); gsm v0.11.0 wrongly certified it; signals now only raise the level (max), which commutes. `TestSignalsCommute` checks every pair of signals in both orders, and the example no longer shows a line standing down, which no order-independent event can do ([#145]).
- `plan`: a Tool node called a tool whose context's deadline had already passed (its timer not yet run), which the agent's base handler refuses; it now fails with `ErrToolNotCalled` without calling the tool (P14).
- `ResolveHaltRef`'s live-driver check leased everything before the first `>` of the halted run's ID. It did not see a live session turn's driver, which leases the turn's run (`<id>>@turn/<n>`), so it could resolve a call the turn was running, and it took a root run named like the session for a live driver (`*HaltInFlight`). It now leases the halted run's tree root: the turn's run for a session turn and every sub-run inside it, the root run otherwise, the run `RunInfo.RootRunID` names ([#137], review R137-1).

- A stale `Session` handle refused every new message for a turn another handle had finished: a handle whose `Send` of that turn's message failed or paused kept the turn open in its own view, and `Send` of another message returned `ErrConfig` without reading the journal. It now reads the journal once before it refuses (model 12, finding S1) ([#137]).
- Two callers on one `Session` handle sending one message (a redelivery, or a retry while the first still ran) recorded its turn twice, for `Send` and for one `SendOnce` key: the second caller's append started past the slot the first had recorded and reloaded. A handle now skips the append of any run among the turns it has loaded (model 12, finding S2) ([#137]).
- Two workers given one session message both drove its turn's run, and each counted only the spend it had seen, so a turn under `WithTokenBudget` spent up to its budget once per worker (twice its budget in the test). Over a store with leases one worker drives a turn at a time (model 12, finding S4) ([#137]).

- `agent.New` read a tool's spec twice, once for its name and once for the rest, so a tool whose answers differed had its calls decided by the second ([#117]).
- A saga rollback did not recurse through a tool that wraps a sub-agent (`audit.AttenuatingSubAgent`): the sub-run's compensable writes were left in place and the delegation reported uncompensated. A resumed tree's token budget missed such a sub-run's spend the same way ([#117]).
- A saga rollback into an `audit.AttenuatingSubAgent`'s sub-run now compensates under the child grant and identity the sub-run journaled (none, if the delegation ran without a grant), never the parent's broader authority ([#117]).
- A saga rollback into a resumed `audit.AttenuatingSubAgent` delegation whose `AttenuateFunc` gave each grant its own ID stopped: the sub-run held two grants. The delegation now keeps its first grant ([#117]).
- A retry-safe saga step whose outcome was unknown (`ErrToolOutcomeUnknown`) was taken by the rollback for a step that changed nothing, and was neither undone nor reported; it is now reported in `SagaAborted.UnknownOutcome` ([#117]).
- A saga whose `audit.AttenuatingSubAgent` delegations were minted under two grants (the root grant expired or was rotated between drives) could never finish its rollback: each journaled grant was verified against the one grant bound, and the walk stopped at the first minted from the other, whichever was bound. Bind every such grant (`WithGrant` and `WithRollbackGrants`); a journaled grant whose parent is none of them is still refused (`ErrNotVerified`). Minting from an expired bound grant now says to keep it bound for the rollback beside the live one (model 11, finding D1) ([#133]).
- A saga rollback into an `audit.AttenuatingSubAgent`'s sub-run ran a retry-safe write of the sub-run again after the delegation's grant had expired: the rollback rebound the child grant without the mark the expiry guard checks. The mark is kept, and a re-run the guard refuses is listed in `SagaAborted.UnknownOutcome` and the rollback goes on (model 11, finding D2) ([#133]).
- An `audit.AttenuatingSubAgent` delegation under a grant bound with a nil or typed-nil signer (a nil pointer in the `Signer` interface) panicked: a typed nil when it minted or resumed, a nil one when it resumed, and a nil one was recorded as a failure when it minted. `WithGrant` now binds the grant with no signer, and the delegation is refused with `ErrConfig` and records nothing; `WithRollbackGrants` binds nothing without a signer ([#133]).
- A saga rollback skipped a sub-agent call with an error result (in a plain run inside the saga's tree, a sub-agent whose run failed after a compensable write), so the sub-run's writes were neither compensated nor listed. The rollback now walks a sub-agent call's sub-run whatever its result (model 11, finding D3) ([#133]).
- `RecoverLoop` took over a dead holder's run late when halted runs were listed before it: each pass visited every unfinished run in order, halted ones included, so the takeover waited about one visit per halted run, and a lease that lapsed just after the pass tried the run waited for the whole next pass (finding L1 of the run-lifecycle model, [#124]). It now runs a second loop on the same interval, with slots of its own, that drives only the runs whose lease lapsed, so a dead holder's run is taken over within about one interval of its lease lapsing however many halted runs the store holds. The full pass is unchanged and still re-drives halted runs and runs that held no lease ([#126]).

### Documentation

- The gsm convergence claims now say what holds today. The Coq/Rocq theorem is unchanged and correct, but gsm v0.11.0, which `govern` pins, has a `Build` gap: its commute shortcut does not check what event guards and effects read, so a machine where one event's guard or effect reads a variable another event writes (pay/ship) can be certified convergent when it is not ([gsm#2](https://github.com/blackwell-systems/gsm/pull/2) is the fix in progress). The README (and its four translations), the governance, audit and testing guides, `CONCEPTS.md`, the `govern` godoc and the `examples/govern` comments no longer say that `Build` proves every interleaving converges, or that the two checkers extracted from the proof re-certify every machine: they can re-check an exported machine but run neither in gsm's CI nor at runtime today, and a proof-derived gate on every build is planned. `KNOWN-LIMITATIONS.md` has a new "Governed state (gsm)" entry: do not rely on a v0.11.0 convergence verdict for such a machine until bide moves to the fixed gsm.

## [0.9.0] - 2026-10-01

### Added

#### Formal models

- A TLA+ model of the claim protocol (attempt claims, not-started records, numbered retries, remembered claims, the resume gate, the Step flight and halt resolution), checked with TLC in CI by the new Models workflow, a required check on every pull request; see [spec/tla](spec/tla/README.md) ([#100]).
- The claim model also covers `ClaimAttempt` on a single key (plan flows), the `pendingClaims` eviction, and a halt resolution running in a driver's process ([#106]); it follows the Store/Journal core as merged, including the claim on the lease path ([#107]).
- The claim model covers the approval gate (1-of-1 `Approve` and m-of-n `SubmitDecision` tallies) together with halt resolution ([#108]).
- A TLA+ model of flow semantics (switch and loop replay, per-iteration keys, `run:complete`, `Flow.ResolveHalt`), checked in CI ([#110]).
- A TLA+ model of spend accounting (`@llm`, `@spend`, `@spend-late`, hedged losers, two drivers), checked in CI ([#111]).
- A TLA+ model of the bide protocol's claim rules for remote tool calls (claim at assignment, begin records, abandons), checked in CI ([#112]).
- The approval model covers approvers' key sets (refused on any overlap) and a verifier resolver that changes between the gate's check and its count ([#115]).
- The design and plan for formal models of the coordination protocols, [docs/design/formal-models.md](docs/design/formal-models.md) ([#99]).

#### Store and Journal

- `agent.Store`, the storage port (`Insert`, `Get`, `Load` of an `Entry` with an opaque, commit-ordered `Seq`), with its requirements A1 to A8 documented on the type, and `agent.Journal` over it (`NewJournal`, `Get`, `History`, `Records`, `Format`): the journal owns memoization, the record encoding and salt, the journal format header, attempt claims, not-started records and recording an outcome after the caller's context is cancelled ([#92]).
- The journal format header: every run's journal starts with an `@journal` record (`agent.StepHeader`) naming `agent.JournalFormat` (`bide.journal.v1-dev`, the one tag every pre-release writes until 1.0, which switches to `bide.journal.v1` and refuses every pre-release journal); a run in another format, or with no header, is refused with `*agent.JournalVersionError` (wrapping `agent.ErrJournalVersion`) before anything is read or written ([#92]).
- `agent.RunFilter` (`After`, `Prefix`, `ExcludeHolding`), which SQL stores evaluate in their query ([#92]).
- `agent/storetest`: every store requirement (64 goroutines racing one name through three handles, prefix-closed reads, byte fidelity, context, iterator hygiene), the header checks, shared in-flight steps, claim reuse, record fidelity, and `CheckWrapper`, which checks, given at least two contexts that differ in what the wrapper reads from a context, that a store wrapper's mapping of run IDs and names does not depend on the context (A1), and its use of `Unwrap` ([#92]).
- `agent/agenttest.CountingStore`, and a test that holds the engine to an exact budget of store round trips per operation ([#92]).
- `Record.Raw` (the stored bytes), `Record.Salt()`, `Record.ClaimID()`, `Record.Format`, `Record.Redacted` (the reserved redaction tombstone), and `Record.MarshalJSON`, which writes the journal encoding ([#92]).
- `store/sqlite` implements `agent.Leaser`, on a lease connection of its own with a short busy timeout and expiry from the database's clock; `Open` opens a writer, readers and a lease pool ([#92]).
- `store/sqlite.New` and `store/postgres.New` over a `*sql.DB`, `WithTablePrefix`, and a schema version row: a database with a newer schema is refused ([#92]).
- Journals in one process over one store share in-flight steps. A claim whose insert failed (it may have committed) records that its attempt did not start, keyed by its claim (`attempt:not-started:<claim>:<marker>`), so a re-drive in any process re-attempts the effect instead of halting over one that never ran. If that record cannot be written either (or is written and reported failed), the process remembers the claim, and the next claim of the marker in the process, or a resume that meets it, writes the record again; every claim takes a fresh id, so an effect never runs under a marker that is, or can become, recorded as not started. `plan`'s conformance check ignores not-started records ([#92]).
- Benchmarks `BenchmarkRunTurns`, `BenchmarkToolCallSideEffect`, `BenchmarkStep`, `BenchmarkRecoverPass10k`, `BenchmarkAnchoredInsert`, `BenchmarkSQLiteInsert` and `BenchmarkPostgresInsert` ([#92]).

#### Pauses and halt resolution

- `agent.Pause`, the sealed interface every pause satisfies, with `agent.RunRef`, `IsPause` and `AsPause`. Its five kinds are `ApprovalPending`, `InterruptPending`, `SignalPending`, `TimerPending` and `OutcomeUnknown`; code outside the package cannot add one ([#90]).
- `agent.ResolveHaltRef(ctx, store, HaltRef, Outcome, ...)`, one resolution for tool and `Step` halts, with `OutcomeUnknown.Ref`, `HaltRef`, `OpRef`, `OpKind` (`OpTool`, `OpStep`), `HaltCause` (`HaltCrashed`, `HaltContended`) and `Outcome` (whose `Evidence` marks the resolution reconciled). The 1.0 rewrite renames it `ResolveHalt` ([#90]).
- Verbs named for the pause they answer: `SubmitDecision` with `Decision`, `AnswerInterrupt` and `Enqueue`; `agent.Wake` ([#90]).
- `agent.HaltInFlight`, `agent.HaltAlreadyResolved`, `agent.ErrAlreadyResolved` (wraps `ErrConfig`) and `agent.WithoutLiveDriverCheck` ([#90]).

#### Flows

- `agent.RunStart.Kind` (`agent.RunKind`: `RunKindAgent`, `RunKindFlow`) and `agent.RunStart.Flow` (`agent.FlowRef`): a `plan` flow's run records `run:start` with kind `flow`, its flow's name and its input as JSON, and `RecordedStart` reads them back. An agent run's `run:start` records no kind, which reads as `RunKindAgent` ([#103]).
- `plan.Flow.ResolveHalt`: `agent.ResolveHaltRef` for a node of the flow, which first checks that the halt names a node of this flow, that the run is a run of this flow (its recorded start names the flow and its recorded topology digest is the flow's), and that a successful outcome decodes as the node's output type, since a resolution is final and one the flow could not read would leave the run unable to continue ([#103]).
- `agent.ErrNoLiveAttempt` (wraps `ErrConfig`) ([#103]).
- `plan`'s fault-schedule exploration of flow lowering (crashes, store errors that did or did not commit, and cancellations at every write, across processes) as a permanent test, bounded by default (`BIDE_EXPLORE=1` explores every process plan) ([#103]).

#### Model calls and spend

- `agent.ModelCall` (`Request`, `Model`, `RunID`, `Turn`), `ModelCall.AddHook` and `ModelCall.Attempt`, `agent.ModelResponse` (`Message`, `Usage`, `Finish`, `RawFinish`) and `agent.ModelAttempt` (what a hook's `After` sees, including `Discarded`, the discarded spend a replayed turn reports) ([#104]).
- `agent.CallModel(ctx, model, req, mw...)` sends one model call outside an agent through the same model handler an agent uses: hooks, clipped request slices and the response checks ([#104]).
- Model records journal `Record.Finish`, `Record.RawFinish`, `Record.Model` (the `ModelInfo` of the model that answered) and per-turn `Record.PromptDigest` and `Record.ToolsDigest` (`agent.PromptDigest`, `agent.ToolsDigest`: SHA-256 of the system prompt and of the tool set the turn was sent, after middleware). `ModelInfo` has JSON tags ([#104]).
- `middleware.CostMeter.Snapshot()` returns a `CostSnapshot` (`Answer`, `Spend`, `AnswerUSD`, `SpendUSD`) read under one lock ([#104]).
- `trace.Model` records `gen_ai.response.finish_reasons`; an empty reason is recorded as `stop`, as the journal records it ([#104]).
- `agent.ModelCall.OnAnswer(key, fn)`: `fn` runs once per recorded turn with the turn's answer, after the journal holds it, however many targets or attempts a middleware sees; it reports whether the call belongs to a turn ([#104]).
- A run that ends (completes, pauses or fails) waits, for at most two seconds in all and not past its context, for model requests still in flight (a hedge loser, a request a middleware left running) and journals their usage in a late spend record, `@spend-late/<id>`, which `Result.Spend`, the budget and `Replay` count. A driver whose model record another driver of the run recorded first journals its request's spend there too ([#104]).

#### Proofs and signatures

- `audit.Signer` and `audit.Verifier` name their scheme with a typed `audit.Alg` (an alias of the new `agent.Alg`) and expose `PublicKey()`, and a `Verifier` reports its `KeyIDs()`, so every `audit.Verifier` is an `agent.ApproverVerifier`; `audit.NewVerifier` and `ParsePublicKey` (and `verify.NewVerifier`) refuse a weak Ed25519 key (`audit.ErrWeakKey`); `audit.NewVerifier(alg, pub)`, `audit.VerifierOf(signer)`, and the `<alg>:<hex>` key text form (`audit.FormatPublicKey`, `audit.ParsePublicKey`). The standalone `audit/verify` package gains `verify.Head`, `verify.Verifier` and `verify.NewVerifier`, and checks tree heads under ed25519, ML-DSA-65 and the hybrid ([#105], [#109]).
- `audit.ErrNotVerified` and `audit.ErrMalformed` (wraps `agent.ErrProtocol`; `audit.ErrFormat` now wraps it), the sentinels every verifier's error wraps ([#105]).
- `audit.JournalExport` (format `bide.audit.journal-export.v1`) and `audit.ExportJournal`: a run's journal as each record's stored bytes, the input of `bide-audit prove` and `prove-absent`. `audit.JournalLeafHash`, the leaf hash a redaction tombstone records; a redacted record keeps its place in every tree ([#105]).
- Format constants `audit.STHFormat`, `audit.AnchorEntryFormat`, `audit.GrantFormat`, `audit.JournalExportFormat`; `audit.EvidenceKind` (a typed string) with `KindRunCertificate` and `KindConsistency`; `agent.ReasonAlg` ([#105]).
- `agent.Decision.Alg` and `agent.Record.ApproverAlg`: an m-of-n decision is journaled with the scheme it was signed under ([#105]).

#### Approval key identity

- `agent.ApprovalPolicy.ValidateKeys`, `agent.ApprovalTally.Excluded`, `agent.ReasonSharedKey`, `agent.ReasonNoKeyID`, `audit.KeyID`, `audit.CheckEd25519PublicKey` and `audit.ErrWeakKey`; `KeyIDs` on `audit.Ed25519Verifier`, `MLDSAVerifier` and `HybridVerifier` ([#109]).

#### Documentation, testing and tooling

- The `RecoverLoop` documentation (godoc, the recovery guide, known limitations) states that a dead holder's run is picked up within about one interval of its lease expiring only while a pass is short: a pass costs about five store round trips per unfinished run it lists, halted runs included, so a long pass delays pickup. Bounding a pass's cost follows v0.9.0.
- The bide protocol (`bide.protocol.v1`), an accepted design for SDKs in other languages, with its claim rules checked by model 2; not implemented yet ([docs/design/protocol.md](docs/design/protocol.md), [#95]).
- The pre-1.0 API redesign proposal, [docs/design/api-v1.md](docs/design/api-v1.md) ([#64]); the roadmap adds bide underneath other agent frameworks ([#102]).
- `scripts/release.sh` pushes at most three tags per push, `release-modules.yml` checks a module tag on manual dispatch, and `bench.yml` has an A/B mode that runs two refs interleaved in one job ([#101]).
- Tooling: a nightly Explore workflow (`.github/workflows/explore.yml`, also on demand) runs the claim and flow-lowering fault-schedule explorations at their full bound (`BIDE_EXPLORE=1`) and keeps the schedule signatures (`BIDE_EXPLORE_SIGS`) as an artifact kept 30 days; it is not a required check ([#119], [#121]).
- The concurrent claim exploration is deterministic: its scheduler finds quiescence with `testing/synctest` instead of timers, runs one driver at a time, and branches on the races between drivers that share a step call in flight, so every run explores the same schedules (more than before, with each one the timing-based scheduler reached) and no longer slows or times out under CPU load ([#118]).
- `bench.yml` and `cmd/bench/README.md` report latency as the mean (concurrency / throughput), p90 and p99; the p50 stays in the raw output only, since the closed-loop harness's p50 is bimodal ([#116]).

### Changed

#### Store and Journal

- **Breaking:** every journal starts with the `@journal` header, so `History` returns it first and record indices (audit leaves, `ProveRecord`) shift by one; journals written by v0.8.0 and earlier have no header and are refused, not resumed ([#92]).
- **Breaking:** `agent.Lister.Runs` takes a `RunFilter` and returns an iterator of run IDs in ascending order; `Recover` and `RecoverLoop` ask the store for the runs holding no terminal marker and read none of the finished ones ([#92]).
- **Breaking:** `agent.Capability` takes a `Store` and follows `Unwrap() Store`; the engine still finds capabilities behind a `Durable` ([#92]).
- **Breaking:** `Record.Salt` and `Record.Claim` are read-only: use `Salt()` and `ClaimID()`; the journal sets them ([#92]).
- **Breaking:** a `Step` that is not retry-safe and returns a pause (`Interrupt`, `Sleep`, `Await`, a pending approval) is `ErrConfig`; its marker stays, so its next attempt halts. Inside a tool call the error is not recorded as a tool failure, so the call halts on resume rather than being retried under a new id. Put the pause in a retry-safe step of its own ([#92]).
- **Breaking:** `store/sqlite` names its tables `bide_steps`, `bide_leases` and `bide_schema_version`, and `store/postgres` its leases table `bide_leases`. `sqlite.Open` refuses a file whose v0.8.0-or-earlier journal table (`steps`) holds rows, with `ErrJournalVersion`, rather than open it as empty; Postgres refuses each such run on its first read, without writing to it. Stop every v0.8.0 (or earlier) node before starting this version: the two do not share leases, and v0.8.0 cannot read the new journals ([#92]).
- **Breaking:** a store wrapper whose `Do` and `History` come from `MemStore`, a SQL store or an embedded `agent.Durable` (directly or through another wrapper) while its `Insert`, `Get` or `Load` comes from elsewhere is `ErrConfig` where a `Durable` goes, since those `Do` and `History` would write past its methods; pass `agent.NewJournal(wrapper)` instead ([#92]).
- `agent.Durable` and the `Do` and `History` methods of `MemStore` and the SQL stores are transitional: they go through a Journal over the store, so existing code keeps working. `*Journal` implements `Durable`. `agent/durabletest` is `agent/storetest` under its former name ([#92]).
- `MemStore` honors its context, and lists runs in order ([#92]).
- A live tool result, a claim and a completion are written with one Insert, without a read first ([#92]).
- A `Step` that loses its claim to a call of the step in flight in this process, and finds that call failed, halts with `HaltContended` (its claimant was live); a `Step` that loses its claim with no call in flight still halts with `HaltCrashed`. `ResolveHaltRef` finds a `Leaser` through a `Journal` and through store wrappers, so its live-driver check uses the lease on `store/sqlite` as on `MemStore` and `store/postgres` ([#92]).
- `ResolveHaltRef` (and its wrappers) claims the attempt after the live one before it records the outcome, on a store that leases runs as on one that does not, and returns `*HaltInFlight` (whose new `Attempt` field names that attempt) if a driver holds it already, so a resolution cannot override a driver that revived a remembered claim and ran the effect: after `WithMinHaltAge`, or under the lease, which a plain `Run` does not hold. If recording the outcome then fails, the resolution's claim stays live, and the operation halts until it is resolved again ([#92]).
- A `SagaAborted` error lists its uncompensated writes without saying each lacked a compensator: the list also holds a call whose outcome is unknown and a call whose tool is gone ([#92]).
- The DST and reference-model crash suites inject their crashes at the storage port, under a Journal, so they cover the journal header, claims and not-started records; `agent` also carries an exhaustive fault-schedule exploration of the claim protocol, bounded by default (`BIDE_EXPLORE=1` runs the full exploration) ([#92]).

#### Pauses and halt resolution

- **Breaking:** the pause types are renamed: `PendingApproval` to `ApprovalPending`, `Interrupted` to `InterruptPending`, `Awaiting` to `SignalPending`, `Sleeping` to `TimerPending` and `ResumeHalt` to `OutcomeUnknown`. The old names stay as deprecated aliases until the 1.0 rewrite, so code that names the types or matches them with `errors.As` keeps compiling, but composite literals and the removed fields break: each type embeds `RunRef` (`RunID`, `RootRunID`), so a composite literal names `RunRef`; `Interrupted.Key` is `InterruptPending.Name`, `ResumeHalt.ToolUseID` and `ToolName` are `OutcomeUnknown.Op.ID` and `Op.ToolName`, and the never-set `Awaiting.Prompt` is gone ([#90]).
- **Breaking:** `Waker.Schedule(ctx, Wake) error`. A failed schedule fails the run with an error wrapping `ErrStorage` and records nothing for the sleeping call (it used to pause with no wake registered), and `RecoverLoop` schedules it again on its next pass. `MemWaker` keys a wake by its `RunID` and `Name` and resumes its `RootRunID` ([#90]).
- `OutcomeUnknown.Cause` says why a run halted: `HaltContended` when another driver won the claim during this drive, otherwise `HaltCrashed` (no live claimant known to the halting driver, which is not proof). **Breaking:** `ResolveHaltRef`, and the `ResolveHalt` and `ResolveStepHalt` wrappers, refuse to resolve an effect a driver may still be running: on a store that leases runs they hold the root run's lease while resolving and return `*HaltInFlight` while a driver holds it; on a store that cannot (a custom store with no `Leaser`) they require `WithMinHaltAge` (measured from the live attempt), unless `WithoutLiveDriverCheck` is given; a `HaltContended` halt always requires `WithMinHaltAge`. A resolution that conflicts with an outcome already recorded returns `*HaltAlreadyResolved` instead of nil ([#90]).
- `ResolveHalt`, `ResolveStepHalt`, `Resume` and the channel `Send` are deprecated wrappers of `ResolveHaltRef`, `AnswerInterrupt` and `Enqueue` (`ApproveAs`, the wrapper of `SubmitDecision`, is removed below); `ResolveHalt` and `ResolveStepHalt` take the cause as `HaltCrashed` ([#90]).
- The loop, `Recover`, `RecoverLoop` and `MemWaker` detect pauses with `IsPause` ([#90]).

#### Flows

- **Breaking:** `plan` lowers every node onto `agent.Step` through the engine's step hook: a node runs as the Step named by its node key, `node:<name>` (`node:iter:<n>:<name>` in a loop body), so it has the Step's claim protocol (a fresh claim id, the not-started record, numbered re-attempts) and pause guard. A node that halts returns `*agent.OutcomeUnknown` with `Op: agent.OpRef{Kind: agent.OpStep, ID: "node:<name>"}`, a pause that `RecoverLoop` does not report as a failure, and `agent.ResolveHaltRef` clears it with the node's output. A node cancelled after its claim and before its body is re-attempted on the next drive instead of halting. A node's body that returns a pause from a node that is not retry-safe is `ErrConfig`. A node reads the journal by point reads, never by loading the run. `plan.HaltAmbiguous` is removed ([#103]).
- **Breaking:** the keys a flow writes change: a node's result is `node:<name>` (was `<name>`), its attempt marker the Step's `attempt:step:node:<name>` (was `attempt:<name>`, a `StepValue` recording `{"retry_safe":...}`, which is no longer written or read: the Step marker is itself the attempt's recorded safety), and a loop iteration's keys are `node:iter:<n>:<name>` (was `iter:<n>:<name>`). A retry-safe node writes no marker, so a node attempted as retry-safe and relabelled a side effect since runs again under a claim, as a `Step` does; a node attempted as a side effect still halts if it is relabelled retry-safe ([#103]).
- **Breaking:** a flow's `Run` records `run:start` before `flow:digest`; resuming a flow run with an input whose JSON differs, under another flow's name, or driving it with an `Agent` (or a flow over an agent's run) is `ErrConfig`, and records nothing. `Conform` requires `run:start` to name the flow and recognizes the Step markers; it ignores the journal header and not-started records ([#103]).
- **Breaking:** a flow's run records `run:complete` with the flow's name and its output when its terminal node finishes, so `Recover` and `RecoverLoop` skip it (they re-drove every finished flow run on every pass) and `agent.IsComplete` reports it; a later drive with the run's input returns the recorded output with one point read, whatever the flow's topology is now. An `Agent` refuses a finished flow run, and a flow a finished agent run ([#103]).
- **Breaking:** a flow's input is compared with the recorded one as canonical JSON (keys sorted, each number as the exact decimal value it denotes, with no rounding through a float, so `1`, `1.0` and `1e0` match and 2^53 and 2^53+1 do not), so decoding `RecordedStart`'s `Input` without loss and passing it to `Run` resumes the run ([#103]).
- **Breaking:** a flow input whose JSON has a repeated object key, an escaped lone surrogate or invalid UTF-8 is refused with `ErrConfig`, since two different texts would compare as one input; numbers compare exactly whatever the size of their exponent ([#103]).
- **Breaking:** a `Step`'s value, and every other value the engine journals (an interrupt answer, a signal, a channel message, a timer's wake time, an `AwaitFor` deadline and outcome, a session's turn records, a saga's failure text, a resolution's evidence), is journaled without HTML escapes (`<`, not `\u003c`), as the journal encoding and `ResolveHaltRef` write values, and `ResolveHaltRef` compares an outcome with the recorded one as JSON values (whitespace, key order, number spelling and string escaping aside), so resolving the recorded outcome again is not a conflict whichever way it was escaped ([#103]).
- **Breaking:** `agent.Step`, a `Parallel` task and a Step's resolution refuse an empty step name with `ErrConfig`, inside a flow node or not ([#103]).
- **Breaking:** `ResolveHaltRef` (and `ResolveHalt`, `ResolveStepHalt`) refuse an operation with no live attempt marker, one that never halted, with `ErrNoLiveAttempt`, so a resolution cannot record an outcome for an effect that never ran ([#103]).
- **Breaking:** an `agent.Step` or `agent.Parallel` task a flow node's body runs for the flow's run is recorded under the node's key (`node:<node>:step:<name>`, and per iteration in a loop body): a loop body's Step used to replay its first iteration's result in every later iteration, so its effect ran once ([#103]).
- **Breaking:** `Conform` replays the run's routing from its recorded choices: a record of a node on an untaken arm, of a loop iteration the run did not reach, or of a node under the wrong kind of key (an iteration outside a loop, or a loop body node outside one) is a divergence, and so is a journal that records nodes without `run:start` and `flow:digest`, a completed journal that lacks a node, choice or loop iteration its routing reaches, and a `run:complete` whose output is not the recorded output of the terminal node the run reached (compared as JSON values, so HTML escaping does not matter); a Step a node's body ran is recognized as the node's ([#103]).
- **Breaking:** `Flow.Run` refuses (`ErrConfig`) the run IDs `agent.Run` refuses: an empty one, or one in the form reserved for sub-agents and session turns ([#103]).
- A loop whose body is one node (the head is the switched node) feeds the head its forward predecessor's value; it used to get a nil input ([#103]).
- **Breaking:** `node:`, `switch:` and `flow:` are reserved step-name prefixes, so a `Step` inside a flow body cannot collide with a flow's records. `ResolveHaltRef` (and `ResolveStepHalt`) accept a flow node's key as a step name ([#103]).

#### Model calls and spend

- **Breaking:** `agent.ModelHandler` is `func(ctx, ModelCall) (ModelResponse, error)`, and `Middleware` wraps it. A middleware passes on the call it received or a copy with fields changed; the model handler refuses a `ModelCall` built from scratch with `ErrConfig`, since it would drop the hooks outer middleware added. `Request.Messages` and `Request.Tools` arrive clipped at every handler, so an append in one hedged branch never writes into another's backing array ([#104]).
- **Breaking:** hooks are added with `ModelCall.AddHook` (append-only) and have the signatures `Before(ctx, ModelCall) error` and `After(ctx, ModelCall, ModelAttempt)`; a hook whose `Before` returned nil gets exactly one `After`, also when a later hook's `Before` refuses the request. The run's spend meter is not a hook, so no middleware can hide a request from `WithTokenBudget` or `Result.Spend`. Requests are numbered per turn across every retried attempt and hedged target (`ModelCall.Attempt`); a request is numbered when it reaches the model handler, before its Before hooks, so a request a Before hook refused keeps its number ([#104]).
- **Breaking:** the stream sink is claimed by one request per turn at a time. A failed claimer releases it and the next claimer emits `TurnRestarted`; when the turn's response is not the one that streamed (a hedge target that was not the claimer, a fallback, a cache), the agent emits `TurnRestarted` and replays the response; nothing a request sends after its turn returned reaches the caller. With `Hedge`, the first target to start streams live, where before a hedged turn was delivered only once the winner was chosen ([#104]).
- **Breaking:** an empty finish reason from a custom `Model`, or in a response a middleware built, is recorded as `FinishStop`; a response a middleware built with `FinishLength`, `FinishFiltered`, an unknown reason, or `FinishToolUse` without a call is the error the model's own would be. The loop still decides whether to run tools from the message's calls, never from the reason ([#104]).
- `agent.Replay` ends each turn with the finish reason and raw reason its record journaled, and reports the turn's discarded spend in `Finish.Discarded`, where it used to reach the run through the context; `middleware.Cost` on a replaying agent counts it too ([#104]).
- `middleware.Hedge` is `c := call; c.Model = backup` and has no streaming code; `Retry` loops `next(ctx, call)`; `RateLimit` and `Cost` add hooks, and count requests outside an agent only through `agent.CallModel`; `trace.Model` names the provider and model from `agent.ModelInfoOf(call.Model)`; `WithRetrieval` journals through the call's run, not the context ([#104]).
- `middleware.Cost` counts each call's answer once, the response the agent records, wherever it sits: inside a `Hedge` a losing target's response is no longer counted as an answer ([#104]).
- `agent.Replay` journals the replayed turn with the model its original record named (`Record.Model`), not the replaying Model, also through a Model that wraps the replay model and forwards its events; late spend journaled before any turn is reported with the first ([#104]).
- A model turn whose record write reports an error is settled from the journal: a record that landed is the turn's (its answer functions run, and the rest of the spend is late), one that did not is a failed call's `@spend/<id>`, and when the journal cannot be read the spend and the answer functions are kept by the process for the run's next drive in it, which decides from the journal; a spend record whose write failed is kept and written by that drive too. Nothing is counted twice ([#104]).
- The engine tells its own model record from another driver's by the salt the journal stamps (it draws that salt itself; `JournalEntry` keeps it), not by comparing content, so a tool call whose arguments are not compact JSON, text with invalid UTF-8, or two drivers' records equal in every field no longer make it take its own record for another's ([#104]).
- Spend kept for a run's next drive is keyed by the store's identity, so any Journal over the store in the process sees it, and bounded at 4096 entries, oldest runs dropped first. It follows a Durable wrapper's `Unwrap() Durable` (as `audit.AuditedStore` offers) to the store beneath it, so the run's next drive through another wrapper or Journal over the same store sees it; the list of runs holding kept spend never outgrows the entries ([#104]).
- A claim through a Durable the engine drives by its `Do` (a wrapper such as `audit.AuditedStore`) now behaves as through a Journal: a claim whose marker write fails records that its attempt did not start, and if that record cannot be written either the process remembers it, under the identity of the store beneath the wrapper, so the next claim or resume in the process writes it and re-attempts the effect instead of halting over one that never ran ([#104]).
- A `Step` driven through a Durable wrapper (such as `audit.AuditedStore`) over a Journal follows the Journal's loser rule: a driver that lost the step's claim only joins a call of the step in flight and otherwise reads the result, and never starts a call, so the claim's winner in the same process cannot take the loser's halt as its own and halt (the claim model's `WinnerNeverHalts`); a joined call that fails halts with `HaltContended`. ([#104]).
- `Unwrap() Durable` is a contract, as `Unwrap() Store` is: only a Durable wrapper that passes run IDs and step names through unchanged may implement it, since the engine follows it for capabilities and for the per-run state the process keeps; a key-rewriting wrapper must not. `storetest.CheckDurableWrapper` checks a Durable wrapper against it ([#104]).
- The `Durable` and `Store` contracts state what the engine's accounting relies on: a record is written at most once under a name, `fn` is called at most once per record, and every record's bytes, its salt among them, are kept exactly ([#104]).
- **Breaking:** `agent.Finish` has an unexported field (the replayed turn's marker), so an unkeyed composite literal of it outside the package no longer compiles; use field names ([#104]).
- **Breaking:** spend records are keyed by a fresh id, `@spend/<id>` (a replayed run keeps the original's), so two drivers of one run never write their spend under one key ([#104]).
- **Breaking:** a request of a model turn that is over (its call already returned, so nothing would record it) is refused with `ErrConfig` ([#104]).

#### Proofs and signatures

- **Breaking:** proofs commit to raw stored bytes. A journal leaf is its tag followed by the bytes the journal stores for the record (`Record.Raw`), never a re-encoding, so a record written by a later release with fields this one does not know verifies. `ProofBundle.Record` is replaced by `RecordBytes` (`record_bytes`, base64) and a `Record()` method that decodes them leniently for display and role checks only; `EvidencePackage` and `CurrentGrantProof.Leaf` carry record bytes through their bundles. `VerifyInclusion` takes the record bytes. A record built in memory has no leaf ([#105]).
- **Breaking:** new artifact formats, and before 1.0 a verifier reads only the current one (an older artifact is `ErrFormat`; re-create it from the journal): `bide.audit.proof.v3`, `bide.audit.absence.v3`, `bide.audit.runcert.v3`, `bide.audit.current-grant.v3`, `bide.audit.event-inclusion.v3`, `bide.audit.evidence.v5`, `bide.audit.sth.v5`, `bide.audit.anchor-entry.v1`. Grant canonical bytes are `bide.audit.grant.v2` (tag included in the signed and digested bytes), and event leaves `bide.audit.event-leaf.v3`. `audit.UnmarshalStrict` checks the format of every artifact it meets, including one carried inside another, and wraps any other decoding error in `ErrMalformed` ([#105]).
- **Breaking:** STH v5: a `SignedTreeHead` carries `format` (`bide.audit.sth.v5`) and always its `alg`, and the scheme is part of the signed bytes. `TreeHead.Timestamp` is `TimestampNanos` (`timestamp_nanos`). Each component of a hybrid signature signs its own label (`bide.hybrid.ed25519.v1`, `bide.hybrid.mldsa65.v1`) before the message, so a stripped ed25519 half verifies neither as hybrid nor as plain ed25519 ([#105]).
- **Breaking:** every ed25519-only entry point takes a `Signer` or `Verifier`: `SignTreeHead(th, Signer) (SignedTreeHead, error)` (`SignTreeHeadWith` and every `VerifyWith` are gone), `Sign(head, Signer) ([]byte, error)`, `VerifySignature(head, sig, Verifier) error`, `SignAbsenceRoot(..., Signer, ts)`, `Evidence(ctx, store, runID, Signer, ts, ...)`, `EvidencePackage.Seal(Signer)` and `Verify(Verifier, ...)`, `CertifyRun(ctx, store, runID, sth, RunCertSpec)` with `RunCertSpec.Signer` and `TimestampNanos` (the signer must hold the key that signed `sth`), `VerifyRun(cert, approved, Verifier)`, `VerifyApprovals(..., Verifier)`, `VerifyCurrentGrant(..., Verifier)`, and `NewAuditedStore(inner, Signer, anchor) (*AuditedStore, error)` (it no longer panics on a bad key). An `EvidencePackage` names its key as `{alg, public_key}` instead of `public_key_hex`, and `Verify` refuses a verifier of another scheme or key ([#105]).
- **Breaking:** verifiers return `error` (nil means verified): `SignedTreeHead.Verify`, `ProofBundle.Verify`, `AbsenceBundle.Verify`, `VerifyInclusion`, `VerifyConsistency`, `VerifyAbsence`, `VerifyEventInclusion`, `VerifyAnchorInclusion`, `SignedGrant.Verify`, `VerifyDelegationChain`, `VerifyCurrentGrant`. The report verifiers (`EvidencePackage.Verify`, `VerifyRun`, `VerifyApprovals`) return their report and an error wrapping `ErrNotVerified` when it is not OK. `CheckTimestamp` and `CheckTimestampOrder` errors wrap `ErrNotVerified` ([#105]).
- **Breaking:** `agent.ApproverVerifier` has `Alg()`, `SubmitDecision` requires `Decision.Alg`, and the m-of-n gate (and `TallyApprovals`, so `VerifyApprovals`) counts a decision only when its journaled scheme is its approver key's. The deprecated `ApproveAs` is removed; use `SubmitDecision` ([#105]).
- **Breaking:** `Grant.NotAfter` is `NotAfterUnix` (`not_after_unix`), and `Grant.Expired` takes Unix seconds ([#105]).
- **Breaking:** the stream event types (`TurnStarted`, `ModelEvent`, `AssistantTurn`, `ToolStarted`, `ToolCompleted`, `ApprovalRequired`, `Finished`, `TurnRestarted`) and the model events (`TextDelta`, `ReasoningDelta`, `ToolCallDelta`, `Finish`) have snake_case JSON tags, and event leaves name their kind in snake_case (`turn_started`, `model_event/text_delta`); an event of an unknown type is refused ([#105]).
- **Breaking:** `audit.PoliciesUsed(records) ([]string, error)` and `audit.AbsenceRoot(records, set) (root []byte, size int, err error)` return an error (`audit.ErrRedacted`, `audit.ErrMalformed`) for a journal they cannot project, instead of a set that silently omits a redacted record, so an auditor's recomputation cannot confirm a head that omits what the journal commits. `audit.KeyFunc` returns the record's keys (`[]string`, nil for none), so one model turn adds a key per call it requested; `ToolUseKey` and `PolicyUsedKey` follow ([#105]).
- Producers and verifiers of proofs read every record by its stored bytes ([#105]): producers that project a journal (the absence key sets, `CertifyRun`'s used-policy set, `EventLogFromJournal` and `PersistJournal`) refuse one holding a redacted record with the new `audit.ErrRedacted`, instead of omitting it (an absence proof of a redacted call, a certificate missing a redacted action's policy); a proof's `record_bytes` must read one way to every JSON reader (no duplicate or case-variant names, invalid UTF-8 or escaped lone surrogates; unknown fields tolerated) or they are `audit.ErrMalformed`; the tool-use key set also holds every call a model turn requested, attempt markers and saga failures, so a call that was started cannot be proven absent, a retry-safe call whose result was lost included; every producer and recomputation of a key set (`NewAbsenceTreeHead`, `SignAbsenceRoot`, `ProveAbsent`, `ProveAbsentBundle`, `AbsenceRoot`, `PoliciesUsed`, `CertifyRun`) checks each record's stored bytes as a proof does, and so do `EventLogFromJournal` and `PersistJournal`, so a record that reads two ways is `audit.ErrMalformed` rather than projected one way; each projects what a record's stored bytes (`Record.Raw`, which the journal tree binds) decode to, and refuses a record whose fields do not say what its stored bytes say (`audit.ErrMalformed`) (event leaves `PersistJournal` wrote before a redaction keep the redacted content: redact the event trail too); `VerifyAnchorInclusion`, `VerifyRun` and `EvidencePackage.Verify` check the format of every artifact they carry, at any depth, on a Go value as `UnmarshalStrict` does on JSON (a run certificate's heads included, when the certificate is for another run and `VerifyRun` is never reached); `VerifyApprovals` and `ApprovalEvidence` read the journaled tally with `UnmarshalStrict`, so a tally that reads two ways is `audit.ErrMalformed`, and the gate reads a recorded tally by the same rule (a tally it cannot read strictly is `ErrStorage`, and the tool does not run); `bide-audit verify-approvals` refuses an approver-key file that gives two ids one key, compared by key identity (`KeyIDs`), whether or not both ids are in the policy (exit 4).
- `bide-audit verify-approvals` takes `-approved` / `-approved-file`, the allowlist `verify-evidence` takes, so a package that carries a run certificate can pass ([#105]).
- **Breaking:** `bide-audit` reads keys as `<alg>:<hex>` (or bare ed25519 hex) for `-pubkey` and the approver keys, reads a `JournalExport` for `-journal`, and maps the audit sentinels to its exit codes: `ErrNotVerified` is 1, `ErrFormat` and `ErrMalformed` are 4 ([#105]).

#### Approval key identity

- **Breaking:** `agent.ApproverVerifier` has a second method, `KeyIDs() []string`: the identities of the signing keys behind `Verify`, derived from the public key's bytes (one per key; a hybrid reports each component). A custom verifier must implement it. An approver whose verifier reports no key identity, including an `audit.Ed25519Verifier` over a wrong-length key, used to count as unable to sign and is now `ErrConfig` at the gate ([#109]).

### Removed

- **Breaking:** `agent.DetachModelSink`, `agent.EmitMessage`, `agent.WithModel(ctx, m)` and `agent.WithModelCallHook(ctx, h)`: the model call path carries no engine data in the context. Use `ModelCall.Model` to retarget a call and `ModelCall.AddHook` to add a hook ([#104]).
- **Breaking:** `middleware.CostMeter.Total`, `Usage`, `Spent` and `SpentTotal`; use `Snapshot` ([#104]).
- **Breaking:** `trace.WithSystem` and `trace.WithModel`; the chat span reads the provider and model from the call's `Model` (`agent.Describer`) ([#104]).

### Fixed

- `store/postgres` sends each lease call (`AcquireLease`, `RenewLease`, `ReleaseLease`) and each `Insert` as one statement that Postgres commits before it replies, instead of a transaction of `BEGIN`, the statement and `COMMIT` in separate round trips. A holder stalled between its statement and its `COMMIT` (a SIGSTOP, a suspended VM, a long GC pause) kept the lease row, or the run's insert lock, locked, so another node's `AcquireLease` of the expired lease, or its next insert into the run, blocked for as long as the stall lasted instead of taking the run over one TTL after the last committed renewal. An insert now takes its position from `<prefix>next_seq_v1(run_id)`, a VOLATILE plpgsql function that `Open` creates when it is missing: called inside the insert's one statement, it takes the run's transaction-level advisory lock (the key earlier versions used, so both queue on one lock during an upgrade) and then reads `MAX(seq)+1` with a snapshot taken after the lock, so inserts into one run queue instead of racing for a position. `Open` never replaces the function and refuses one with another definition. A statement that a repeatable read or serializable deployment fails with a serialization failure (40001) is run again, reads included, so the default isolation still does not change the outcome. Retries wait a capped, jittered exponential backoff (1ms up to 100ms) and continue until the context ends, so a caller that needs a bound on a call sets a deadline. `Open` refuses, with `ErrConfig`, existing tables that lack a uniqueness the statements depend on (`(run_id, seq)` and `(run_id, name)` on the journal, `run_id` on the leases), since it never alters an existing table. The schema migration, the one remaining multi-statement transaction, sets `idle_in_transaction_session_timeout` so a node stalled inside it cannot hold the migration lock ([#113]).
- `govern/postgreslog.Append` is one statement for the same reason: a process stalled inside an append held the entity's advisory lock, and every other append to the entity waited for as long as the stall lasted. It takes its position from `governed_events_next_seq_v1(entity)` the same way, its retries back off the same way, and `Open` refuses a `governed_events` table without unique `(entity, seq)` and `(entity, append_id)` indexes, or a next_seq function with another definition ([#113]).
- `store/postgres` and `govern/postgreslog` open in a schema whose name has an upper-case letter: the next_seq lookup cast `current_schema()` to `regnamespace`, which folded the name to lower case. The lookups now read `pg_catalog`'s tables by name with the statement's snapshot, never through `to_regclass` or `to_regprocedure` ([#120]).
- `Open` in both packages uses one schema for every statement: the one `WithSchema` (new in `store/postgres` and, as an option of `govern/postgreslog.Open`, in the log) pins, or else the first schema on the search path holding the store's steps table (or `governed_events`), else the first schema on the path, with a warning logged through `log/slog`, since discovery runs again at every `Open` and a role that can create a schema earlier on the path can redirect a restarting node. `WithSchema` refuses `information_schema` and every `pg_` name, and a pinned schema that does not exist fails `Open` with `ErrConfig`; `Open` never creates a schema. Every table reference, the next_seq call and the migration's DDL name that schema. A `governed_events` table in a later schema than an empty one is now migrated in place: the migration's unqualified `CREATE TABLE IF NOT EXISTS` used to create a second, empty table in the first schema and move the log to it. `Open` refuses, with `ErrConfig`, a first relation of that name that is not an ordinary or partitioned table: a view named `governed_events` before the table had appends go through it to another table ([#120]).
- `Open` in both packages refuses a next_seq function owned by another role than the table's owner, or one without `SET search_path = pg_catalog, pg_temp`, with `ErrConfig`. The statement check now tokenizes every statement and holds it to allowlists: calls only to listed `pg_catalog` built-ins (a function named like an unreserved keyword, `conflict`, counts as a call) or to next_seq under its exact schema-qualified name, once, in a one-row `INSERT` into its own table; operators only as `OPERATOR(pg_catalog.<op>)` from a list, and no keyword operator or `ARRAY` constructor; relations only the store's own tables or `pg_catalog`'s; casts only to listed `pg_catalog` types (a domain's `CHECK` can call anything); ASCII only outside literals; no comments, dollar quotes, backslashes, typed literals or semicolons ([#120]).
- `Recover` and `RecoverLoop` call `resume` only for a run that is still unfinished: holding the run's lease, the pass checks the terminal markers (`run:complete`, `run:aborted`, `run:cancelled`) again before it calls `resume`, so a run another driver finished after the pass listed it is left alone and not counted as re-driven (nor is a run whose check failed, which is reported). The check covers every driver that holds the run's lease; a finish by a driver with no lease, or during a stall past the lease TTL between the check and `resume`, can still reach `resume`, which replays the finished run. **Budget:** a recovery pass makes three point reads (`Store.Get`) for each run it drives; the finished runs the listing excludes still cost nothing ([#114]).
- Tests: the multi-process HA harness in `store/postgres` gives each cluster a schema of its own. Its workers' recovery passes walked every incomplete run the package's other tests left in the shared schema, so taking over a stalled worker's run slowed with that count rather than the lease TTL, and `TestHA_MultiProcessStallPastTTL` timed out in the Integration job's second pass. Its wait for the take-over is now derived from the lease TTL, the recover interval and one drive ([#122]).

### Security

- An m-of-n approval gate counts one seat per signing key. Two approvers whose verifiers resolve to one key let that key's holder meet the quorum alone; the gate now refuses such a policy with `ErrConfig` on every evaluation, `TallyApprovals` never counts either approver, `audit.VerifyApprovals` returns an error, and `bide-audit verify-approvals` exits 4. Found by the TLA+ approvals model (finding F5) ([#109]).
  **Upgrading:** the check covers tallies this version counts. A terminal tally already in a journal is reused, not recounted, so a passed tally recorded by an earlier version stands even if two of its approvers shared a key, and its tool runs when the run resumes. Journals are not promised to resume across pre-releases; before upgrading, finish the runs paused on an m-of-n gate, or audit each one that holds a recorded tally (`audit.VerifyApprovals` under the new rules refuses a shared key) and resolve it by hand if two approvers shared a key.
- Weak Ed25519 public keys are refused. `crypto/ed25519` accepts keys that are not canonically encoded, small order, or mixed order: under a small-order key (such as any of the identity point's four accepted encodings) anyone can forge a signature for any message, and a mixed-order key `A + T` is a second public key for `A`'s secret, which gave one secret two approval seats. `audit.Ed25519Verifier` (and so `HybridVerifier`), `audit.VerifySignature` and `audit/verify.TreeHead` now verify nothing under such a key, `Ed25519Verifier.KeyIDs` reports no identity for it (so the approval gate refuses it with `ErrConfig`), and `bide-audit` refuses it as `-pubkey` or in `-approver-keys` (exit 4). The check costs 1 to 4 ms of CPU per new key; results are cached (a 1,024-key LRU, single-flight), so an application that resolves Ed25519 keys from untrusted input should bound or rate-limit those lookups. Found in the adversarial review of [#109].
- `store/postgres` and `govern/postgreslog`, in v0.8.0 and earlier as well: three kinds of role could run their code with the store's privileges, and some could choose the run's lock key. A role with `CREATE` on the database could create a schema named like the store's role, which the default search path (`"$user", public`) puts first, at any time after `Open`, with a `next_seq` function (or, for v0.8.0, tables) that the store's unqualified names then resolved to. A role that can create functions in any schema on the search path, even one after the store's own (`public` on Postgres 14 and earlier lets every role create there), could define `hashtextextended(text, integer)`, which matched the lock call `hashtextextended(r, 0)` better than `pg_catalog`'s. And a role that can create operators in any schema on the path could define `=` for `text[]` or for `oid` and `regtype`, which `Open`'s catalog checks compared and which `pg_catalog` has no exact match for, so the operator ran at every `Open`, `pg_catalog` searched first or not. Now every name in every statement is qualified: the tables and next_seq with the store's schema, and every function, type, collation and operator with `pg_catalog` (operators as `OPERATOR(pg_catalog.<op>)`); the statement check refuses anything else, keyword operators (`LIKE`, `IS DISTINCT FROM` and the like) and `ARRAY` constructors included; the next_seq functions qualify every name and operator in their body and also run with `SET search_path = pg_catalog, pg_temp`; and `Open` checks the function's owner and definition. With `WithSchema`, the search path plays no part in which schema the store uses; without it, discovery still trusts every schema on the path, so pin the schema. The lock key's value is unchanged, so v0.8.0 nodes still queue on it during an upgrade. The stores trust the owner of their schema and every role that can create objects in it, as they trust the tables' owner; any role that can connect can still stall a run's inserts by holding its advisory key, as before ([#120]).

## [0.8.0] - 2026-09-30

### Added

- `agent.RecoverLoop` with `WithRecoverInterval`, `WithRecoverConcurrency` and `WithRecoverErrors`: runs recovery passes until its context ends, so a dead holder's runs are taken over without another call ([#58]).
- `agent.ErrLeaseLost`: a lost lease cancels the drive with a cause that wraps it; failed renewals retry every ttl/20 and give up at 3/4 of the TTL ([#58]).
- `agent.ErrToolOutcomeUnknown`: a call lost mid-flight on a tool that is not retry-safe records no result and halts on resume ([#57]).
- `mcp.WithSafety`, `mcp.WithCallTimeout`, `mcp.WithMaxResultBytes`, `mcp.WithMaxDescriptionBytes`, with defaults `DefaultMaxResultBytes` (1 MiB) and `DefaultMaxDescriptionBytes` (8 KiB) and the error `mcp.ErrResultTooLarge` ([#57]).
- `modeltest.ToolNames` conformance check for provider tool-name rules ([#57]).
- `agent.StepNotStarted`: a tool call or `Step` cancelled after winning its claim but before its effect ran is recorded as not started and re-attempted, instead of halting ([#65]).
- `agent.Result.Spend`, `agent.Record.DiscardedUsage`, `middleware.CostMeter.Spent` and `SpentTotal`: usage of retried, hedged and failed requests is journaled and reported ([#55]).
- `agent.TurnRestarted` stream event, sent before a retried attempt's output when the previous attempt streamed any ([#55]).
- `agent.ModelCallHook`, `agent.WithModelCallHook` and `agent.WithModel` for hooks around every request and per-call model routing ([#55]).
- `agent.ErrNegativeUsage` and `agent.Usage.Validate` ([#55]).
- `agent.ToolErrorText` and `agent.RedactURLs`, so middleware can record exactly the error text the journal holds ([#54]).
- `agent.Capability[T]` finds an optional store capability through wrappers; `audit.AuditedStore` implements `Unwrap` ([#66]).
- `agent.ResolveStepHalt`, `agent.IsReservedStepName`, `agent.SubRunID` and `agent.ToolResultStep` ([#60]).
- `agent.FinishStop`, `FinishToolUse`, `FinishLength`, `FinishFiltered`, `ErrOutputTruncated` and `ErrOutputFiltered` ([#68]).
- `provider.DefaultMaxResponseBytes` (32 MiB), `provider.LimitResponse` and `WithMaxResponseBytes` on each model adapter ([#68], [#76]).
- `agent.Describer`, `agent.ModelInfo` and `agent.ModelInfoOf`, implemented by the Anthropic, OpenAI and Gemini adapters ([#76]).
- `Finish.Raw` carries the provider's own finish reason, and `Finish.Discarded` the usage of discarded responses ([#76]).
- `modeltest.CheckFinish` ([#76]).
- `agent.DecodeStoredRecord` for `Durable` implementations to read rows back ([#68]).
- `audit.CheckTimestamp`, `CheckTimestampOrder`, `DefaultClockSkew`, `WithVerifyTime`, `WithClockSkew`, and `bide-audit -max-clock-skew` and `-max-input-bytes` ([#68]).
- `audit.PolicyLeafName`, `ConvergenceLeafName`, `GovernedPolicyDigest` and `audit.ErrFormat` ([#66], [#68]).
- `plan.BlockName`, `Arm.Named`, `Flow.DigestV1` and the config safety value `side_effect` ([#68]).
- `RetrievalTool` options `RetrievalName` and `RetrievalDescription`, so one agent can search several stores ([#62]).
- `eval.ReportFormat`, `eval.ErrFormat`, `eval.MetricDirection` and `RunOutput.TraceErr` ([#74]).
- A multi-process HA test harness on Postgres that kills, stalls and restarts workers ([#58]).
- `bide-audit -json` and `-version` ([#79]).
- `eval.Compare` options `WithUnscoredTolerance` and `WithCaseSetMismatchAllowed`, `Comparison.Gate`, `ErrRegression`, `ErrInconclusive` and `DirectionInconclusive` ([#78]).
- CI enforces doc comments on every exported identifier with `internal/tools/doccheck` ([#73]).
- `agent.RunStart` and `agent.RecordedStart`: a run's input and entry point are journaled at its first drive and can be read back, for example by a `Recover` callback ([#70]).
- `agent.Record.ReadOnly` (`read_only`): a tool result records whether its call ran ReadOnly ([#70]).
- The library modules (`govern`, `store/sqlite`, `store/postgres`, `mcp`, `trace`, `codec/gcf`, `govern/sqlitelog`, `govern/redislog`, `govern/postgreslog`) are tagged `<dir>/vX.Y.Z` with each release by `scripts/release.sh`, so they install with `go get` ([#85]).
- CI checks that every Go block in the README and `docs/` compiles against the current code, and that API listings match the packages, with `internal/tools/docsnip` ([#88]).

### Changed

- **Breaking:** journal keys for tool results, attempt markers, approvals, compensations and sub-agent runs encode the tool-use id (`tool:<id>`, sub-run `<parent>><id>`); runs journaled by v0.7.0 do not resume ([#60]).
- **Breaking:** `ResolveHalt` resolves only tool calls; use `ResolveStepHalt` for a `Step` ([#60]).
- **Breaking:** step names with a reserved engine prefix are `ErrConfig`, run and session ids may not contain `>`, `plan` step names may not contain `:`, and `Recover` skips sub-runs ([#60]).
- **Breaking:** `Finish.Reason` uses the neutral values `stop`, `tool_use`, `length` and `filtered`; a cut-off or filtered turn fails with `ErrOutputTruncated` or `ErrOutputFiltered`, and any other reason with `ErrStreamProtocol` ([#68]).
- **Breaking:** a reply over the response cap fails with `ErrResponseTooLarge`, and a `tool_use` turn with no call fails ([#68]).
- **Breaking:** tool arguments for `Func`, `SubAgent` and `final_answer` decode strictly: a missing required field, an unknown or case-variant name, a duplicate or trailing data is `ErrToolArgs` ([#61]).
- **Breaking:** `schema.For` describes what `encoding/json` decodes (`Unmarshaler`, `TextUnmarshaler`, integer bounds) and returns `ErrUnsupportedType` for chan, func, complex and similar kinds ([#61]).
- **Breaking:** `RunTyped[T]` returns `ErrConfig` when `T` is not an object, and `RunTypedNative` on Anthropic returns `ErrConfig` ([#61]).
- **Breaking:** `WithRetrieval` sends context as a user message just before the latest user turn, one JSON object per document, journaled once per run as `@retrieval/<layer>`; `RetrievalTool` and `WithRetrieval` panic for k < 1 ([#62]).
- **Breaking:** `WithTokenBudget` on a parent covers its sub-agents, and `Result.Usage` and `Result.Spend` report the whole run, including earlier invocations and sub-agents ([#69]).
- **Breaking:** `agent.EmitMessage` takes the turn's `Usage` ([#52]).
- **Breaking:** `plan` step and join functions (`Builder.Step`, `Join2`, `Join3`, `RegisterStep`, `RegisterJoin2`, `RegisterJoin3`) take a `context.Context` ([#66]).
- **Breaking:** audit proof JSON uses snake_case names and a `format` field (`bide.audit.proof.v2` and siblings, `bide.audit.evidence.v4`); verifiers reject other formats with `audit.ErrFormat` ([#66]).
- **Breaking:** `plan` digests move to `bide.plan.topology.v2`, so flows started under v1 do not resume; config JSON decodes strictly, and a config `safety` can lower retry safety but not raise it or clear approval ([#68]).
- **Breaking:** `plan` configs must state `"version": 1` (`plan.ConfigVersion`) and use snake_case keys (`loop_max`); `Topology` JSON keys are snake_case and `TopologyNode.Kind` is `TopologyNodeKind`; config parse errors wrap `ErrConfig` ([#75]).
- **Breaking:** signed tree heads need a positive nanosecond timestamp within the clock skew, empty evidence packages fail, and `bide-audit` input is capped at 256 MiB ([#68]).
- **Breaking:** `ApprovalPolicy.Validate` refuses approver ids that are equal under Unicode normalization and case folding, and invalid UTF-8 ([#68]).
- **Breaking:** `middleware.Retry` and `ToolRetry` with n < 0 return `ErrConfig`; `Quorum` with k above the voter count is `ErrConfig`; `EventLog.Events` returns `ErrProtocol` on a gap in positions ([#68]).
- **Breaking:** `eval.Judge` passes only on an exact `PASS`, and a failed run never passes ([#68]).
- **Breaking:** `eval.RequiredRuns` returns `(int, error)`, and `eval.Run` returns an error for duplicate metric names ([#53]).
- **Breaking:** `eval.Matches` takes a `*regexp.Regexp`, `eval.AgentRunner` returns `(RunFunc, error)`, `eval.Compare` returns `(Comparison, error)`, and `GovernanceHeld` predicates take a context ([#74]).
- **Breaking:** the provider HTTP kit moves from `agent` to the new package `model/provider` (`ClassifyHTTPError`, `ClassifyStreamError`, `NewSSEScanner`, `ParseRetryAfter`, `ToolResultCodec`, `JSONToolResultCodec`, `EncodeToolResultOr` and others); `WithToolResultCodec` takes a `provider.ToolResultCodec` ([#76]).
- **Breaking:** `Finish.Reason` is the typed `agent.FinishReason` ([#76]).
- **Breaking:** `eval` `Metric.Fn` returns `(bool, error)`; a metric that cannot score a run leaves it unscored, `MetricStat` reports `Scored` and `Unscored`, and reports are `bide.eval.report.v2` ([#78]).
- **Breaking:** `bide-audit` exit statuses follow one scheme: 0 verified, 1 not verified, 2 usage, 3 no verdict, 4 input unreadable or unusable ([#79]).
- **Breaking:** a session's journal and turn runs move to `"<id>>@session"`, `"<id>>@turn/<n>"` and `"<id>>@event/<encoded key>"` (from `"<id>"`, `"<id>/t<n>"` and `"<id>/e/<key>"`), so no run ID passed to `Run` can name one: `Run("chat/t0")` and session `"chat"`'s first turn shared a journal, and whichever finished first handed the other its answer. A session journaled before this change opens empty. Session ids may contain `/` (#56 had refused it) and any `SendOnce` key is allowed; `agent.IsSessionRun` reports a session's run IDs, and `Recover` skips them, since the session resumes a turn when its message is sent again ([#86]).
- **Breaking:** every run journals `run:start` (its input and whether it is a saga); resuming an unfinished run with another input, or through `Run` for a saga (or `RunSaga` for a run), is `ErrConfig`, and so is `SendOnce` with a different input on a key whose turn is still open ([#70]).
- **Breaking:** `plan` attempt markers record whether the node was retry-safe; a node re-runs on resume only if it was retry-safe when attempted and is now, and a marker written before this halts ([#70]).
- **Breaking:** a recorded approval denial is final even if the tool's gate is later removed, loosened or made m-of-n ([#70]).
- **Breaking:** saga rollback treats a completed call as a write unless its result records that it ran ReadOnly, and reports calls whose tool is no longer registered as uncompensated (or halts on one attempted with no result) ([#70]).
- `WithTokenBudget` counts discarded and failed requests; `RateLimit` and `Cost` count every request sent; `Hedge` runs each backup through the inner middleware chain; `Retryable` no longer retries `ErrTruncatedToolArgs` ([#55]).
- A resume halts on any attempt marker without a result, whatever the tool's current safety; a tool no longer registered halts instead of failing with `ErrUnknownTool` ([#57]).
- `mcp.Tools` returns `ErrProtocol` for malformed or duplicate tool names, non-object schemas and oversized descriptions; an agent with two tools of the same name returns `ErrConfig` ([#57]).
- `Lease` claims, renews and releases under a per-call token (`<holder>#<token>`), so drivers sharing a holder name do not share a lease ([#58]).
- `ToolStarted` fires immediately before the tool is called ([#65]).
- The OpenAI adapter merges consecutive user messages into one turn ([#62]).
- `bide-audit` exits 2 for stray arguments, unknown flags, `-h`, or both `-tool` and `-index`, and 3 when a `-checker` gives no verdict ([#54]).
- `middleware.LogErrorText` logs the journaled, redacted error text ([#54]).
- `docs/KNOWN-LIMITATIONS.md` is rewritten for users, grouped by area with impact and workaround ([#49]).
- The approval guide is now [Human approval (human-in-the-loop)](docs/guides/hitl-approval.md) (`docs/guides/hitl-approval.md`); the old `docs/guides/approval.md` and its site URL point to it.

### Removed

- **Breaking:** `plan.Retryable` and the config safety spelling `"retryable"`; use `plan.Idempotent` and `"idempotent"` ([#75]).

### Fixed

- `store/postgres` and `govern/postgreslog` run every write transaction at read committed, whatever the deployment's `default_transaction_isolation`. At repeatable read or serializable, concurrent steps of one run failed with a duplicate `(run_id, seq)` (23505) because each insert computed its position from a snapshot taken before it held the run's lock, so a tool result could go unrecorded and the run halt on resume; racing nodes and lease calls failed with serialization errors (40001), and concurrent governed appends failed with 23505 or 40001 ([#93]).
- A timer (`Sleep`, `WaitUntil`, `AwaitFor`) inside a sub-agent registers its wake under a name qualified by the sub-run, so two sub-agents of one root waiting on timers of the same name no longer share one wake that the later replaced, which left the first sleeping past its wake time ([#91]).
- `examples/signals`: the Await scene's resumed run no longer fails with a reused tool-use id; the example's scripted model now decides from the conversation ([#91]).
- Recording a step costs about half what it did: `MemStore.Do` decodes a record it writes once, and a message part is tagged with its type without a decode and re-encode. The journal bytes do not change. This restores the `cmd/bench` throughput lost since v0.7.0 ([#89]).
- Resuming a run that crashed after its final answer returns the recorded answer without another model call ([#59]).
- `Replay` keeps redacted reasoning, empty thinking blocks and Gemini thought signatures ([#51]).
- `Replay` carries each turn's recorded usage, and a finish reason derived from the turn, so a replayed run stops at the same token budget ([#52]).
- A hedged stream's `Finish` carries the winner's usage ([#52]).
- A response substituted by middleware or a hedge backup is checked for missing or reused tool-use ids ([#55]).
- `NewRateLimiter` with a non-positive interval does not block, and a cancelled call keeps its token ([#55]).
- One `*Session` shared by concurrent callers is safe, and a resumed session turn keeps the transcript it started with ([#56]).
- An MCP result carrying only `structuredContent` reaches the model ([#57]).
- A retry-safe `Step` halts when an earlier side-effect attempt left a marker ([#57]).
- SQLite and Postgres record a step's outcome even when the driver's context is cancelled while the step runs ([#58]).
- `Lease` and `Recover` return `ErrConfig` for a non-positive TTL instead of panicking ([#58]).
- The lease renewer stops before the lease is released ([#58]).
- A tool-use id containing `/`, `:` or an engine key name no longer collides with other journal records, and a `Step` cannot mark a run complete by its name ([#60]).
- `Recover` no longer hands sub-runs to the resume callback ([#60]).
- `Lease`, `Recover` and `RecoverLoop` find the `Leaser` and `Lister` of a store wrapped in `AuditedStore` ([#66]).
- Saga compensation undoes the arguments the tool accepted after middleware, including in rollback re-runs ([#67]).
- `RunTyped` returns the arguments `final_answer` accepted, and its text fallback decodes the final turn ([#61]).
- `schema.For` terminates on recursive named maps and slices and on self-referential pointer types ([#61]).
- Retrieved context stays in every model call of a run, including after resume, and a retrieved document cannot forge another entry ([#62]).
- A non-finite retrieval score is reported as 0 ([#62]).
- A panicking model or tool call marks its span failed without exporting the panic value ([#54]).
- A turn cut off at its token limit or stopped by a filter is an error, not the run's answer ([#68]).
- Stream parsers reject a second call at the same index, Anthropic deltas for unknown or stopped blocks, and OpenAI choices other than index 0 ([#68]).
- A negative or overflowing `Retry-After` is clamped ([#68]).
- Model text quoted in errors is bounded ([#68]).
- In-process step keys are length-prefixed in `MemStore`, `MemWaker` and the SQL stores, so run ids and step names cannot collide ([#68]).
- Event-log reads detect a missing position, and a negative `Append` position is `ErrProtocol` ([#68]).
- `Quorum` checks that each recorded vote names its slot's voter and recomputes the tally ([#68]).
- `plan` refuses empty node and join names, and edges into a join other than its declared inputs ([#68]).
- Mermaid labels are entity-encoded ([#68]).
- `WithMinHaltAge` treats a non-positive attempt timestamp as missing ([#68]).
- Signing with an ed25519 key of the wrong length fails before anything is written ([#68]).
- `eval`: a repeated tag no longer double-counts, Wilson bounds are exact at 0/n and n/n, and `HashCases` reports encoding errors ([#53]).
- `eval.AgentRunner` reports a failed journal read, and trajectory metrics fail such a run ([#74]).
- `chaos.Verify` also requires a completed run to fire exactly once (`Report.Missed`), and `Bide().Writes()` counts the completion marker ([#53]).
- `cmd/bench` rejects non-positive `-runs` and `-concurrency` ([#53]).
- The example store in the extension-points reference records through `agent.JournalEntry`, so it salts every record and passes `agent/durabletest` ([#87]).

### Security

- `bide-audit` checks that each bundle is a record of the role it is read as ([#68]).
- `bide-audit -pubkey` always reads a 32-byte hex value as the key, never as a file name ([#68]).
- `audit.VerifyAnchorInclusion` binds an entry's sequence and run to its proof, and `verify.TreeHead` refuses the head shapes the `audit` package refuses ([#68]).
- `EventLog.Add` and `VerifyEventInclusion` refuse invalid UTF-8 ([#68]).
- One strict reader (`audit.GovernedPolicyDigest`) decodes the governed-action digest for both the agent and `bide-audit` ([#68]).
- `bide-audit verify-quorum` rejects a tally that lists one voter twice ([#54]).
- With content capture on, trace spans and `ToolLog` record the redacted error text the journal holds ([#54]).

## [0.7.0] - 2026-09-29

### Added

- `agent/durabletest` conformance suite for `Durable` stores, and one journal encoding (`agent.EncodeRecord`, `DecodeRecord`) used by every store ([#36]).
- Salted journal records (`Record.Salt`, `agent.JournalEntry`) and salted event-log leaves (`bide.audit.event-leaf.v2`, `agent.ProjectEvents`), so a proof discloses nothing about neighbouring records ([#42], [#46]).
- `Agent.WithToolErrorRedactor`; URL credentials are redacted from tool error text before it is journaled ([#42]).
- `middleware.ErrorSummary` and the `ToolLog` option `LogErrorText` ([#42]).
- `audit.VerifyCurrentGrant` and `ProveCurrentGrant` for a current-grant ledger ([#35], [#40]).
- `govern` `ApplyOnce` on every governor, and `agent.NextOnceKey` for exactly-once operations inside a call ([#34], [#40]).
- `ErrQuotaExhausted`, `ErrResponseTooLarge`, `ErrStreamProtocol`, `ErrToolUseIDReused` and `agent.ClassifyStreamError` ([#41], [#44]).
- `schema.Gemini`, `schema.ErrStrictUnsupported`, `openai.WithMaxCompletionTokens` and `modeltest.ToolConfig` ([#36], [#41]).
- Fuzz targets for the SSE scanner, adapter stream parsing, journal encoding, strict JSON, every audit verifier, the `plan` config loader and the GCF codec ([#44]).
- `govulncheck` in CI ([#38]) and a manual Benchmark workflow on a standard runner ([#45], [#47]).
- [How bide is verified](docs/testing/verification.md) ([#32]).

### Changed

- **Breaking:** audit heads sign `bide.audit.sth.v4` with versioned, salted leaves and commit to their tree kind and run; re-anchor heads made with earlier versions ([#35], [#40], [#42]).
- **Breaking:** new signatures for `VerifyRun`, `VerifyDelegationChain` (with `ScopeRules`), `SignAbsenceRoot` and the absence proofs, `EvidencePackage` (`Seal`, `WithConsistencyFrom`), `ProveCurrentGrant` and `VerifyCurrentGrant` ([#35], [#40]).
- **Breaking:** `EventLog.Prove` returns `EventInclusion`, and `VerifyEventInclusion` takes it ([#46]).
- **Breaking:** `govern.EventLog.Append` takes an append id, `Governor.Apply` can return an error, `Quorum` takes a name, and `bide-audit verify-quorum` requires `-name` ([#34]).
- **Breaking:** Redis event-log keys are `govern:{<len>:<entity>}`, and channel and quorum step names change; drain in-flight runs before upgrading ([#34], [#40]).
- **Breaking:** Postgres keeps its journal in a `bide_steps` table stored as `bytea`, with one position per step ([#25], [#36], [#40]).
- **Breaking:** `agent.MemWaker.Start` returns a channel that closes when the loop has stopped ([#24]).
- **Breaking:** a live model turn with a missing or reused tool-call id is an error, and Gemini tool-call ids are the provider's own or random ([#41]).
- **Breaking:** `schema.For` and `OpenAIStrict` can return errors, strict schemas make optional fields nullable, and `agent.Func` panics on an argument type `For` cannot describe ([#36], [#40]).
- **Breaking:** `RunTyped` ends at the first accepted `final_answer` ([#36]).
- **Breaking:** `plan.Build` rejects shapes `Run` cannot execute as declared, and approval-gated nodes ([#30]).
- **Breaking:** `Stream.Events` fails on any event after a `Finish`, and an Anthropic stream without `message_stop` is incomplete ([#44]).
- Quota and billing exhaustion is `ErrQuotaExhausted` and not retried, and `Retryable` no longer retries `ErrConfig` ([#41]).
- OpenAI sends `max_completion_tokens` on api.openai.com and for o-series and gpt-5 models ([#41]).
- With content capture off, trace spans and `ToolLog` record an error's category, not its text ([#42]).
- Signers, `AuditedStore` and the model adapters print a redacted string for every format verb ([#42]).
- `audit.UnmarshalStrict` uses only `encoding/json`, so bide builds with `GOEXPERIMENT=nojsonv2` ([#37]).
- The SQLite store and event log wait up to 30s for another writer's lock ([#29]).
- `golang.org/x/text` is v0.39.0 in `store/postgres` and `govern/postgreslog` ([#38]).
- Published benchmark numbers come from the Benchmark workflow and a labelled Mac run ([#47]).

### Fixed

- A losing driver is never told it won a step when its reload fails ([#25]).
- Postgres keeps records byte for byte, accepts NUL and invalid UTF-8, and gives each step its own position ([#25], [#36]).
- `Lease` releases on shutdown instead of holding until the TTL ([#25]).
- A resumed run sends the model the same bytes the live run did, on every store ([#36]).
- A cancelled `Step` does not start ([#24]).
- A pause in one parallel tool call lets its siblings finish and record their outcomes ([#26]).
- Saga rollback covers calls its abort cut off, including sub-agents, and reports idempotent writes without a compensator ([#27]).
- A pause or halt inside a sub-agent carries `RootRunID`, and a `Sleep` or `AwaitFor` in a sub-agent wakes the root ([#28], [#34]).
- `Recover` skips a saga whose rollback finished ([#31]).
- `plan` resumes only under the flow a run started with ([#30]).
- A tie for the most votes is never quorum agreement ([#33]).
- An unknown governed event name is rejected before it is appended ([#34]).
- Governed appends and `EventTool` calls land once when retried ([#34]).
- `AwaitFor` records its outcome once per call ([#34]).
- Channel names containing `:` no longer read each other's messages ([#34]).
- Each `Quorum` in a run keeps its own tally ([#34]).
- Two applies inside one tool call are both recorded ([#40]).
- SQLite and Postgres event-log migrations are safe under concurrent opens, and Postgres opens take no table lock when the schema is current ([#40]).
- The Redis event-log script works on Redis Cluster ([#40]).
- A negative Merkle leaf index is rejected ([#35]).
- Wrong-length keys verify nothing instead of panicking ([#35]).
- `codec/gcf` carries integers above 2^53 exactly and rejects trailing data ([#36], [#44]).
- `schema.For` agrees with `encoding/json` on promoted fields, `,string`, arrays and embedded pointers ([#36]).
- `RunTyped` ends at `final_answer` and does not fall back to unrelated prose ([#36]).
- Gemini requests group parallel tool results, skip empty parts and carry thought signatures ([#41]).
- Anthropic requests keep `redacted_thinking` blocks and tools under `tool_choice` "none" ([#41]).
- Empty OpenAI tool arguments are sent as `{}` ([#41]).
- An OpenAI or Gemini turn ends only on the provider's finish signal, not on a chunk carrying usage ([#44]).
- `bide-audit` decodes committed leaf content strictly ([#44]).

### Security

- Journal heads and absence heads are signed under distinct domains, so a head for one tree cannot prove absence from another ([#35]).
- `VerifyRun` requires the used-policy head to belong to the certificate's run, with an auditor-supplied allowlist ([#35]).
- `EvidencePackage.Verify` checks every field and authenticates the run id ([#35]).
- Delegation chains enforce issuer continuity, expiry and full scope attenuation; demoted earned-authority grants stop verifying ([#35], [#40]).
- Records and grants with invalid UTF-8 are refused, and audit bundles are decoded strictly ([#35], [#40]).
- `golang.org/x/text` v0.39.0 fixes GO-2026-5970, reachable from `store/postgres` and `govern/postgreslog` ([#38]).

## [0.6.0] - 2026-09-28

### Added

- `agent.StepSafety` and `Task.Safety` to declare a durable step retry-safe ([#18]).
- `Session.SendOnce` answers each inbound message once per key ([#20]).
- `Agent.WithTokenBudget`, rebuilt from the journal so it holds across resume; each model call's usage is recorded with its turn (`Record.Usage`) ([#15]).
- `Usage.TotalInputTokens` and `Usage.TotalTokens` ([#14]).
- `agent.ToolSafety`, `agent.WithToolSafety`, `Safety.RetrySafe` and `ErrToolReinvoked` ([#16]).
- `agent.NewStreamFunc` and `Stream.Close` ([#12]).
- `agent.ErrIncompleteResponse` ([#13]).
- `model/modeltest` conformance checks for adapter streaming ([#12], [#13]).
- `mcp.TrustAnnotations` ([#21]).

### Changed

- **Breaking:** `middleware.TokenBudget` is removed; use `Agent.WithTokenBudget` ([#15]).
- **Breaking:** `agent.Step` is a side effect unless `StepSafety` says otherwise: it claims an attempt marker and halts after a crash ([#18]).
- **Breaking:** `Parallel` requires unique task names ([#18]).
- **Breaking:** `Usage.InputTokens` is the uncached input on every provider ([#14]).
- **Breaking:** `eval.Run` returns `(Report, error)` ([#19]).
- **Breaking:** `mcp.Tools` applies server annotations only with `TrustAnnotations()` ([#21]).
- `ToolRetry` retries only retry-safe tools, `ToolCache` caches only read-only tools, and a tool that is not retry-safe runs once per call whatever the middleware does ([#16]).
- Breaking out of `Stream.Events` closes the stream ([#12]).
- `Session.Send` records which message started each turn; a different message while a turn is open is `ErrConfig` ([#22]).

### Fixed

- `MemWaker` retries a wake whose resume failed ([#10]).
- A cancelled tool call records no outcome, and a cancelled run stops before its next turn ([#11]).
- A stream the consumer abandons releases its HTTP response ([#12]).
- A response that ends without a `Finish` is an error, not the answer ([#13]).
- Cached tokens are counted once on OpenAI and Gemini, so cost matches across providers ([#14]).
- `trace.Model` reports total input tokens, including cached input ([#14]).
- A token budget applies per run and has no data race ([#15]).
- A cancelled parent waits for its sub-agent to finish ([#17]).
- A crash after a `Step`'s effect does not repeat it ([#18]).
- Each eval samples the model afresh, and a cancelled eval returns an error ([#19]).
- A session turn answers its own message, and concurrent handles lose no turn ([#22]).
- A run's anchored tree heads only grow, and a failed anchor publish is retried ([01a9afd]).

## [0.5.0] - 2026-09-28

### Added

- `agent.ClaimAttempt` and `Record.Claim`: an exclusive attempt claim keeps side effects at most once when two drivers overlap ([f71efd6]).
- `govern/eventlogtest` conformance suite for `EventLog` implementations ([f3fd665]).
- Log-backed governors gain `Sync(ctx)` ([f3fd665]).
- `agent.DetachModelSink` and `agent.EmitMessage` for middleware that delivers a chosen response to a streaming caller ([317c98b]).
- CI runs the Postgres and Redis integration suites ([afc0fe6]).

### Changed

- **Breaking:** `EventLog.Append` returns the event's position, and `Events` takes a starting position ([f3fd665]).
- **Breaking:** `Applier.Apply` returns `Applied{State, Position}`, and `FederatedApplier.Apply` returns `FedApplied` ([f3fd665]).
- **Breaking:** the Postgres event log uses a `governed_events` table; write Redis event-log streams only through the adapter ([f3fd665]).
- `MemStore` stores records as JSON and returns independent copies, like the SQL stores ([4912fbf]).
- With backup models, `Hedge` streams only the winning response ([317c98b]).

### Fixed

- Governors sharing an event log fold in each other's events as they act, and each governed action records its log position ([f3fd665]).
- The Postgres event log keeps each entity's events in a fixed order under concurrent writers ([7efece5]).
- A hedged stream cannot panic the process after the run ends ([317c98b]).

## [0.4.0] - 2026-09-28

### Added

- m-of-n signed approval: `Safety.Approval` with `ApprovalPolicy`, `ApproveAs`, `ApprovalDecisionBytes`, `Agent.WithApproverVerifiers` and `PendingApproval.Quorum` ([78f8db6], [3262cd1]).
- `WithDecisionCheck` on `ApproveAs`, with `ErrInvalidApproval` and `ErrAlreadyDecided` ([3262cd1]).
- `audit.ProveApproval`, `audit.ApprovalEvidence` and `audit.VerifyApprovals`, and `bide-audit verify-approvals` ([78f8db6], [994721b], [3262cd1]).
- A `plan` config `approval` block ([78f8db6]).
- `examples/approval`, a cross-process walk-through ([994721b]).
- `SECURITY.md` with a private reporting contact.

### Changed

- **Breaking:** calling `Run` on a finished run id returns its recorded answer; use `Session` to continue a conversation ([c6deb76]).

### Fixed

- Re-invoking a finished run returns its recorded answer without calling the model, so a repeated tool request cannot run again ([c6deb76]).

## [0.3.0] - 2026-09-28

### Added

- `ResolveHalt` options `WithMinHaltAge` and `WithNow`, the error `*HaltTooYoung`, and `ResumeHalt.AttemptedAt` ([#2]).
- `ResolveHalt` option `WithEvidence`, recorded as `Record.Reconciled` and `Record.Evidence` ([#2]).
- README translations in Simplified Chinese, Russian, Hindi and Arabic under `docs/i18n`.

### Changed

- The `bide-audit` Homebrew formula in `bide-ai/homebrew-tap` is maintained by hand instead of by the release pipeline.

## [0.2.0] - 2026-09-28

### Added

- `agent.ToolResultCodec`, `JSONToolResultCodec` and `EncodeToolResultOr`, and `WithToolResultCodec` on the OpenAI, Anthropic and Gemini adapters ([#1]).
- `codec/gcf` module: opt-in GCF encoding of model-facing tool results ([#1]).
- `bide-audit` Homebrew formula (`brew install bide-ai/tap/bide-audit`).

## [0.1.0] - 2026-09-28

First public release.

### Added

- `agent`: durable agent loop on an append-only journal, with at-most-once side effects keyed by tool-use id (`Agent.Run`, `RunResult`, `Stream`, `RunSaga`, `StreamSaga`, `Session`).
- `agent`: `Safety` classification, `ResumeHalt` and `ResolveHalt` for unknown outcomes.
- `agent`: durable `Step`, `Parallel`, `SubAgent`, and sagas with `CompensatedFunc`.
- `agent`: typed output with `RunTyped[T]` and `RunTypedNative`.
- `agent`: human-in-the-loop (`Interrupt`, `Resume`, `Approve`), durable timers (`Sleep`, `WaitUntil`, `MemWaker`), and durable signals (`Signal`, `Await`, `AwaitFor`, ordered channels with `Send`, `Receive`, `Ack`).
- `agent`: crash recovery and leasing (`Recover`, `Lease`, `WithLeaseTTL`, `WithLeaseHolder`).
- `agent`: `WithSystemPrompt`, `WithSystemPromptFunc`, `WithMaxTurns`, `WithSampling`, `WithToolChoice`, prompt caching, and multimodal image input.
- `agent`: retrieval seams (`WithRetrieval`, `RetrievalTool`), `Replay`, `RenderMermaid`, and sentinel error classification.
- `model/anthropic`, `model/openai` (any OpenAI-compatible endpoint) and `model/gemini` adapters.
- `store/sqlite` and `store/postgres` durable stores, with `Lister` and `Leaser` on Postgres.
- `middleware`: `Retry`, `Hedge`, `RateLimit`, `Cost`, `TokenBudget`, per-attempt timeouts, and tool middleware (`UseTool`, `ToolLog`, `ToolCache`, `ToolRetry`, `ToolRateLimit`).
- `trace`: OpenTelemetry `gen_ai` spans and `trace.Instrument`.
- `mcp`: Model Context Protocol tools, with pagination, elicitation and tool-list changes.
- `schema`: reflection-based JSON Schema and `OpenAIStrict`.
- `plan`: typed flow builder (`Step`, `Tool`, `Model`, `Edge`, `Switch`, `Join2`, `Join3`, `LoopBack`), declarative config (`Load`), conformance checks and a topology digest.
- `govern`: convergent governed state on gsm, durable event logs (`govern/sqlitelog`, `govern/redislog`, `govern/postgreslog`), federation, identity binding and `Quorum`.
- `audit`: tamper-evident journal, RFC 6962 inclusion and consistency proofs, signed tree heads, proof bundles, absence proofs, ML-DSA signatures, anchoring (`AuditedStore`), run certificates (`VerifyRun`), `EvidencePackage`, signed grants and delegation (`AttenuatingSubAgent`, earned authority).
- `bide-audit` standalone verifier, prebuilt for Linux, macOS and Windows on amd64 and arm64.
- `eval` statistical evaluation harness, `chaos` crash-injection harness, and `cmd/bench`.

[Unreleased]: https://github.com/bide-ai/bide/compare/v0.9.0...HEAD
[0.9.0]: https://github.com/bide-ai/bide/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/bide-ai/bide/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/bide-ai/bide/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/bide-ai/bide/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/bide-ai/bide/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/bide-ai/bide/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/bide-ai/bide/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/bide-ai/bide/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/bide-ai/bide/releases/tag/v0.1.0

[#1]: https://github.com/bide-ai/bide/pull/1
[#2]: https://github.com/bide-ai/bide/pull/2
[#10]: https://github.com/bide-ai/bide/pull/10
[#11]: https://github.com/bide-ai/bide/pull/11
[#12]: https://github.com/bide-ai/bide/pull/12
[#13]: https://github.com/bide-ai/bide/pull/13
[#14]: https://github.com/bide-ai/bide/pull/14
[#15]: https://github.com/bide-ai/bide/pull/15
[#16]: https://github.com/bide-ai/bide/pull/16
[#17]: https://github.com/bide-ai/bide/pull/17
[#18]: https://github.com/bide-ai/bide/pull/18
[#19]: https://github.com/bide-ai/bide/pull/19
[#20]: https://github.com/bide-ai/bide/pull/20
[#21]: https://github.com/bide-ai/bide/pull/21
[#22]: https://github.com/bide-ai/bide/pull/22
[#24]: https://github.com/bide-ai/bide/pull/24
[#25]: https://github.com/bide-ai/bide/pull/25
[#26]: https://github.com/bide-ai/bide/pull/26
[#27]: https://github.com/bide-ai/bide/pull/27
[#28]: https://github.com/bide-ai/bide/pull/28
[#29]: https://github.com/bide-ai/bide/pull/29
[#30]: https://github.com/bide-ai/bide/pull/30
[#31]: https://github.com/bide-ai/bide/pull/31
[#32]: https://github.com/bide-ai/bide/pull/32
[#33]: https://github.com/bide-ai/bide/pull/33
[#34]: https://github.com/bide-ai/bide/pull/34
[#35]: https://github.com/bide-ai/bide/pull/35
[#36]: https://github.com/bide-ai/bide/pull/36
[#37]: https://github.com/bide-ai/bide/pull/37
[#38]: https://github.com/bide-ai/bide/pull/38
[#40]: https://github.com/bide-ai/bide/pull/40
[#41]: https://github.com/bide-ai/bide/pull/41
[#42]: https://github.com/bide-ai/bide/pull/42
[#44]: https://github.com/bide-ai/bide/pull/44
[#45]: https://github.com/bide-ai/bide/pull/45
[#46]: https://github.com/bide-ai/bide/pull/46
[#47]: https://github.com/bide-ai/bide/pull/47
[#49]: https://github.com/bide-ai/bide/pull/49
[#51]: https://github.com/bide-ai/bide/pull/51
[#52]: https://github.com/bide-ai/bide/pull/52
[#53]: https://github.com/bide-ai/bide/pull/53
[#54]: https://github.com/bide-ai/bide/pull/54
[#55]: https://github.com/bide-ai/bide/pull/55
[#56]: https://github.com/bide-ai/bide/pull/56
[#57]: https://github.com/bide-ai/bide/pull/57
[#58]: https://github.com/bide-ai/bide/pull/58
[#59]: https://github.com/bide-ai/bide/pull/59
[#60]: https://github.com/bide-ai/bide/pull/60
[#61]: https://github.com/bide-ai/bide/pull/61
[#62]: https://github.com/bide-ai/bide/pull/62
[#64]: https://github.com/bide-ai/bide/pull/64
[#65]: https://github.com/bide-ai/bide/pull/65
[#66]: https://github.com/bide-ai/bide/pull/66
[#67]: https://github.com/bide-ai/bide/pull/67
[#68]: https://github.com/bide-ai/bide/pull/68
[#69]: https://github.com/bide-ai/bide/pull/69
[#70]: https://github.com/bide-ai/bide/pull/70
[#73]: https://github.com/bide-ai/bide/pull/73
[#74]: https://github.com/bide-ai/bide/pull/74
[#75]: https://github.com/bide-ai/bide/pull/75
[#76]: https://github.com/bide-ai/bide/pull/76
[#78]: https://github.com/bide-ai/bide/pull/78
[#79]: https://github.com/bide-ai/bide/pull/79
[#85]: https://github.com/bide-ai/bide/pull/85
[#86]: https://github.com/bide-ai/bide/pull/86
[#87]: https://github.com/bide-ai/bide/pull/87
[#88]: https://github.com/bide-ai/bide/pull/88
[#89]: https://github.com/bide-ai/bide/pull/89
[#90]: https://github.com/bide-ai/bide/pull/90
[#91]: https://github.com/bide-ai/bide/pull/91
[#92]: https://github.com/bide-ai/bide/pull/92
[#93]: https://github.com/bide-ai/bide/pull/93
[#95]: https://github.com/bide-ai/bide/pull/95
[#99]: https://github.com/bide-ai/bide/pull/99
[#100]: https://github.com/bide-ai/bide/pull/100
[#101]: https://github.com/bide-ai/bide/pull/101
[#102]: https://github.com/bide-ai/bide/pull/102
[#103]: https://github.com/bide-ai/bide/pull/103
[#104]: https://github.com/bide-ai/bide/pull/104
[#105]: https://github.com/bide-ai/bide/pull/105
[#106]: https://github.com/bide-ai/bide/pull/106
[#107]: https://github.com/bide-ai/bide/pull/107
[#108]: https://github.com/bide-ai/bide/pull/108
[#109]: https://github.com/bide-ai/bide/pull/109
[#110]: https://github.com/bide-ai/bide/pull/110
[#111]: https://github.com/bide-ai/bide/pull/111
[#112]: https://github.com/bide-ai/bide/pull/112
[#113]: https://github.com/bide-ai/bide/pull/113
[#114]: https://github.com/bide-ai/bide/pull/114
[#115]: https://github.com/bide-ai/bide/pull/115
[#116]: https://github.com/bide-ai/bide/pull/116
[#117]: https://github.com/bide-ai/bide/pull/117
[#118]: https://github.com/bide-ai/bide/pull/118
[#119]: https://github.com/bide-ai/bide/pull/119
[#120]: https://github.com/bide-ai/bide/pull/120
[#121]: https://github.com/bide-ai/bide/pull/121
[#122]: https://github.com/bide-ai/bide/pull/122
[#124]: https://github.com/bide-ai/bide/pull/124
[#126]: https://github.com/bide-ai/bide/pull/126
[#127]: https://github.com/bide-ai/bide/pull/127
[#128]: https://github.com/bide-ai/bide/pull/128
[#131]: https://github.com/bide-ai/bide/pull/131
[#132]: https://github.com/bide-ai/bide/pull/132
[#133]: https://github.com/bide-ai/bide/pull/133
[#137]: https://github.com/bide-ai/bide/pull/137
[#140]: https://github.com/bide-ai/bide/pull/140
[#143]: https://github.com/bide-ai/bide/pull/143
[#145]: https://github.com/bide-ai/bide/pull/145
[#146]: https://github.com/bide-ai/bide/pull/146
[#147]: https://github.com/bide-ai/bide/pull/147

[78f8db6]: https://github.com/bide-ai/bide/commit/78f8db6
[994721b]: https://github.com/bide-ai/bide/commit/994721b
[3262cd1]: https://github.com/bide-ai/bide/commit/3262cd1
[c6deb76]: https://github.com/bide-ai/bide/commit/c6deb76
[4912fbf]: https://github.com/bide-ai/bide/commit/4912fbf
[f71efd6]: https://github.com/bide-ai/bide/commit/f71efd6
[317c98b]: https://github.com/bide-ai/bide/commit/317c98b
[afc0fe6]: https://github.com/bide-ai/bide/commit/afc0fe6
[7efece5]: https://github.com/bide-ai/bide/commit/7efece5
[f3fd665]: https://github.com/bide-ai/bide/commit/f3fd665
[01a9afd]: https://github.com/bide-ai/bide/commit/01a9afd
