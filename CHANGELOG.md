# Changelog

All notable changes to bide are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While bide is pre-1.0, a
minor version (0.x.0) may include breaking API or journal-format changes; each one is marked
**Breaking:** below. Curated highlights for each release are in [docs/releases](docs/releases).

## [Unreleased]

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

### Removed

- **Breaking:** `plan.Retryable` and the config safety spelling `"retryable"`; use `plan.Idempotent` and `"idempotent"` ([#75]).

### Fixed

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

[Unreleased]: https://github.com/bide-ai/bide/compare/v0.7.0...HEAD
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
[#91]: https://github.com/bide-ai/bide/pull/91
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
