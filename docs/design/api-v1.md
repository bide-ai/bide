# Pre-1.0 API redesign (proposal, v2)

Status: accepted, in progress. The maintainer approved this plan as the work to do; it is not a 1.0 freeze, and any part may still change before 1.0. Waves 1 to 3 (P1 to P11) are merged and ship in v0.9.0; Waves 4 to 8 (P12 to P16) follow (see the CHANGELOG). v2 folds in an independent adversarial critique.

## 0. Baseline, what is already done, and how v2 changes v1

### 0.1 Baseline
- **Merged:** main at b64a688, which includes #55, #57 and #60.
- **Merge before the redesign starts (Wave 0), in this order:** #66, #67, #70, #65, #68, #69.
- **Invariants that must hold at every PR boundary:**
  - side effects happen at most once, through attempt claims;
  - resume after a crash is safe;
  - replay is exact;
  - proofs verify offline;
  - no core package imports an adapter.

### 0.2 Already done by in-flight PRs (not redesigned here)

**#66**
- Adds `agent.Capability[T](Durable)`, which follows `Unwrap() Durable` the way `errors.As` follows `Unwrap`. A wrapper's own capability takes precedence over the one it wraps.
- `AuditedStore` implements `Unwrap`.
- Plan step bodies take `ctx`. Predicates stay ctx-free by design.
- Every audit artifact field is snake_case.
- `format` fields are added:
  - `bide.audit.proof.v2`
  - `bide.audit.absence.v2`
  - `bide.audit.runcert.v2`
  - `bide.audit.current-grant.v2`
  - `bide.audit.event-inclusion.v2`
  - `EvidenceFormat` becomes `bide.audit.evidence.v4`
- Adds `audit.ErrFormat`.
- Still open after #66: STHs carry no `format`, and event leaves still commit to Go-case event JSON.

**#67**
- Saga compensation now uses the arguments the tool actually accepted. They are journaled under `@saga/args/<enc id>`, only when a middleware changed them.
- The rollback re-run goes through tool middleware.
- This was v1's "follow-up B". It is done.

**#70**
- Every run records `run:start` = `{"input", "saga"}`.
- Resuming with a different input, or through the other entry point, is `ErrConfig`.
- Exports `agent.RunStart` and `agent.RecordedStart`.
- A recorded denial is final.
- Rollback now reports or halts on calls whose tool is no longer registered.
- Plan attempt markers record `{"retry_safe": bool}`.
- Adds a 60-row "decision points" table and a "live by design" list in docs/GUARANTEE.md.
- Leaves four open decisions:
  - (1) rollback reads live `Safety`;
  - (2) identity and grant are live per drive;
  - (3) the system prompt is not journaled;
  - (4) RunTyped mode is not in `run:start`.
- All four are resolved below.

**#65**
- A claimed effect that provably never started is recorded as `StepNotStarted` under `attempt:not-started:<marker>`.
- It is re-attempted under `attempt:retry:<n>:<base>`.
- This was v1's "follow-up A". It is done for tools and Steps. Plan flows still use their own markers; lowering (item 6) brings them under #65.

**#68**
- `Finish.Reason` has neutral values: `stop`, `tool_use`, `length`, `filtered`.
- `ErrOutputTruncated` and `ErrOutputFiltered` are added. Any other reason is `ErrStreamProtocol`.
- Per-adapter mapping tables, including Gemini `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII` and `IMAGE_SAFETY`, and Anthropic `pause_turn` and `model_context_window_exceeded`. This covers most of critique B14.
- In-process keys are length-prefixed.
- Plan config is decoded strictly.
- `NewAuditedStore` now panics on a bad key length. Section 9.3 turns this into an error.

**#69**
- `Result.Usage` and `Result.Spend` cover the whole run, including sub-agents.
- One token budget covers the whole agent tree.
- This was v1's "follow-up C", and it answers v1's Q5.

### 0.3 Verification of the critique's code claims

| Claim | Verified | Evidence |
|---|---|---|
| Leaves are over re-encoded records (B2) | yes | `canonicalRecord` hashes `agent.EncodeRecord(r)` of a decoded record (audit/merkle.go). `DecodeRecord` is plain `json.Unmarshal` and drops unknown fields. |
| A pause inside a side-effect Step halts (B3) | yes | `step()` claims `attempt:step:<name>`. When fn returns `*Interrupted`, `Do` records nothing and the marker stays. The next `ClaimAttempt` loses, so the step returns `ResumeHalt`. #65 covers only "cancelled before the call". |
| Proof indices are `History` positions, and Seq is never read (A1) | yes | `Prove` and `journalPrefix` index into `History` order. |
| `flow:digest` already refuses a changed topology (B7) | yes | plan/flow.go:79-91. `flow:`, `switch:` and `iter:` are not in `reservedPrefixes`. |
| govern needs only `Step` (A4) | yes | `govern.Quorum` calls only `agent.Step`. Raw `Do` is used by audit only (`policy.go`, `grant.go`, `confluence.go`, `runcert.go`) and by plan (`flow:digest`, `switch:`). |
| Nine root-module examples import govern (B16) | yes | authority, compliance, compose, coordination, delegation, earned-authority, mesh, proof-carrying-run, quorum. |
| A result returned after cancellation is kept (B10) | yes | Discarded only when `callErr != nil && sctx.Err() != nil`. |
| `Alg` is outside the STH signed bytes, and hybrid signs the same message twice (B15) | yes | `SignTreeHeadWith` signs `th.canonical()`. `HybridSigner.Sign` signs `m` with both schemes and packs `len||ed||mldsa`. |
| `IdempotencyKey` is never called by the SDK (C, Safety as data) | yes | It is only tested for nil in `retriableOnResume` and in plan's copy. |

### 0.4 Main changes from v1

- **Run settings are journaled.** They extend #70's `run:start`, and there is still one run header.
- **Proofs commit to raw stored bytes.**
- **Step pause guard.**
- **Journal format:** pinned per run, plus a dev tag before 1.0.
- **Store ordering:** A2 relaxed to commit order with prefix-closed visibility.
- **SQLite and MemStore are Leasers.**
- **Stream sink:** exclusively claimed per request.
- **Hooks:** private.
- **`Journal.Do` is unexported,** behind an internal hook.
- **Sequencing:** transitional shims, then one consolidated scripted rewrite at the end.
- **Recovery dispatch** keys on the run kind.
- **Cancellation marker,** a redaction tombstone format, and a `Lister` filter.
- **Performance gates.**

---

## 1. Cross-cutting conventions

### 1.1 Naming rule
1. **`WithX` is a functional option and nothing else.** `WithoutX` is its negation. A setting that applies at several scopes is one function whose result satisfies each scope's option interface. The same `With` name may appear in different packages, because it is package-qualified.
2. **Context helpers.** `XFrom(ctx)` reads and `ContextWithX(ctx, v)` writes. Context carries only request-scoped data that must reach tool code through `Tool.Call(ctx, args)`:
   - `RunInfo`
   - the once-key counter
   - the audit grant's live signer

   The engine's own machinery never travels in context: waker, clock, sink, hooks, model override, tool safety, redactor.
3. **Constructors.** One that can fail returns `(T, error)`. `MustX` is the panicking twin, offered only for tool constructors that people call at init.
4. **Verbs** that answer a pause use the pause's noun: `Approve`, `SubmitDecision`, `AnswerInterrupt`, `Signal`, `Enqueue`, `Ack`, `ResolveHalt`, `Cancel`. Pause points are named by `name`.
5. **Closed sets are typed strings:**
   - `FinishReason` (the values from #68)
   - `ToolChoiceMode`
   - `Alg`
   - `OpKind`
   - `HaltCause`
   - `RunKind`
   - `EvidenceKind`
   - `TopologyNodeKind`
   - `MetricDirection`
6. **No stutter:** `RunEvent`, `RunStream`.
7. **Transitional names** exist only between the semantic PRs and the final rewrite (section 10.2). Each one is marked `// Deprecated: transitional; renamed by the 1.0 rewrite`.

### 1.2 Option mechanics
Options are interfaces with unexported methods. `apply` returns an error, so a bad value fails at construction.

<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
type Option interface{ applyAgent(*agentConfig) error }
type RunOption interface{ applyRun(*runConfig) error }
type ToolOption interface{ applyTool(*toolConfig) error }
type StepOption interface{ applyStep(*stepConfig) error }
type ParallelOption interface{ applyParallel(*parallelConfig) error }
type LeaseOption interface{ applyLease(*leaseConfig) error }
type RecoverOption interface{ applyRecover(*recoverConfig) error }
type RecoverLoopOption interface{ applyRecoverLoop(*recoverLoopConfig) error }
type ResolveOption interface{ applyResolve(*resolveConfig) error }
```

**The closed list of combination types.** Each constructor returns its narrowest type, so an option passed where it does not apply is a compile error.

| Combination | Scopes | Constructors |
|---|---|---|
| `AgentRunOption` | Option, RunOption | `WithMaxTurns`, `WithTokenBudget`, `WithSystemPrompt`, `WithSampling`, `WithToolChoice`, `WithWaker`, `WithIdentity` |
| `ConcurrencyOption` | Option, RunOption, ParallelOption | `WithMaxConcurrency` |
| `ClockOption` | Option, RunOption, ResolveOption | `WithClock` |
| `SafetyOption` | ToolOption, StepOption | `WithSafety` |
| `LeaseControl` | LeaseOption, RecoverOption, RecoverLoopOption | `WithLeaseHolder`, `WithLeaseTTL`, `WithoutLease` |

**Evolution.** A constructor's return type may widen to a larger combination in a minor release. This is additive, and documented as such.

**Precedence.** For any setting, the order is: a per-run value, then the last agent-level value, then the default. `WithSystemPrompt` (text) and `WithSystemPromptFunc` (agent-only) fill one slot, and the later one wins. A per-run text prompt beats an agent-level function.

**Building options conditionally.** Build a `[]agent.Option` or a `[]agent.RunOption`. An `AgentRunOption` value fits in either slice.

### 1.3 Errors
- Every error wraps exactly one category.
- `audit` uses #66's `ErrFormat`, plus:
  - `ErrMalformed`, which wraps `agent.ErrProtocol`;
  - `ErrNotVerified`, a condition with no category.
- Verifiers return `error`. `nil` means verified. `ErrNotVerified` means the artifact was read and understood but does not hold. Report verifiers return `(Report, error)` with the same sentinels.
- Library code never panics on caller input (section 9.3).

---

## 2. Item 1: Run API

### New API
<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
func (a *Agent) Run(ctx context.Context, runID string, input Message, opts ...RunOption) (*Result, error)
func (a *Agent) Resume(ctx context.Context, runID string, opts ...RunOption) (*Result, error)
func (a *Agent) Stream(ctx context.Context, runID string, input Message, opts ...RunOption) *RunStream

type RunStream struct{ /* unexported */ }
func (s *RunStream) Events() iter.Seq[RunEvent]
func (s *RunStream) Result() (*Result, error)

// Result is non-nil whenever runID passed validation, including on a Pause, failure,
// abort or cancellation. Usage and Spend are the whole run's, sub-agents included (#69).
type Result struct {
	RunID   string
	Message Message         // the final answer; zero unless err == nil
	Output  json.RawMessage // typed runs: the accepted answer as journaled
	Usage   Usage           // whole run and subtree: recorded responses (#69)
	Spend   Usage           // whole run and subtree: every request billed (#69)
	Turns   int             // live turns this invocation (#69)
	Duration time.Duration  // this invocation
}

func RunTyped[T any](ctx context.Context, a *Agent, runID string, input Message, opts ...RunOption) (T, *Result, error)
func WithOutputMode(m OutputMode) RunOption // OutputTool (default) | OutputNative

func ValidateRunID(id string) error
func Cancel(ctx context.Context, j *Journal, runID, reason string) error   // D1
func Status(ctx context.Context, j *Journal, runID string) (RunStatus, error) // D8

func (a *Agent) Session(ctx context.Context, id string) (*Session, error)
func (s *Session) Send(ctx context.Context, input Message, opts ...RunOption) (*Result, error)
func (s *Session) SendOnce(ctx context.Context, key string, input Message, opts ...RunOption) (*Result, error)
```

### The run header: #70's `run:start`, extended (no second header)
<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
type RunStart struct {
	Input     Message                    `json:"input"`               // #70 (string becomes Message)
	Saga      bool                       `json:"saga,omitempty"`      // #70
	Kind      RunKind                    `json:"kind"`                // agent | session_turn | flow
	Session   *SessionRef                `json:"session,omitempty"`   // session id, turn
	Flow      *FlowRef                   `json:"flow,omitempty"`      // flow name (digest stays in flow:digest)
	Typed     *TypedStart                `json:"typed,omitempty"`     // mode, schema digest, full schema
	Settings  RunSettings                `json:"settings,omitempty"`  // per-run values the caller chose
	Principal *Principal                 `json:"principal,omitempty"` // OnBehalfOf, AuthorityRef
	Tools     []string                   `json:"tools,omitempty"`     // per-run tool filter (D3)
	Ext       map[string]json.RawMessage `json:"ext,omitempty"`       // sibling packages (audit grant digest)
}
type RunSettings struct {
	MaxTurns, TokenBudget *int
	SystemPrompt          *string
	Sampling              *Sampling
	ToolChoice            *ToolChoice
}
func RecordedStart(ctx context.Context, j *Journal, runID string) (RunStart, bool, error) // #70, retyped
```

### Journaling rule
The rule resolves critique B1 and #70's decisions 2 to 4, using #70's table.

1. **Per-run values are part of the run.** A value the caller passes as a `RunOption` is journaled in `run:start` at the first drive. On every later drive the journaled value is authoritative, and a drive that passes nothing uses it.
2. **Limits.** A later drive may pass a different `WithMaxTurns` or `WithTokenBudget` to raise or lower a limit. That writes an auditable amendment, `run:limits:<n>`, and takes effect. Operators need to continue a run that hit its budget, and the change is never silent.
3. **Everything else must match.** A later drive that passes a different value for any other setting is `ErrConfig`: system prompt, sampling, tool choice, tool filter, output mode, saga, typed schema, or principal.
4. **Agent-level defaults stay live by design,** as #70 documents: options given to `New`, including `WithSystemPromptFunc`, sampling and tool choice defaults.
   - For audit, every model record journals digests of what that turn was sent: `PromptDigest` (the system prompt text) and `ToolsDigest` (the canonical `ToolSpec` set).
   - This closes #70 decision 3 for audit without freezing a deployment's defaults into running runs.
5. **Identity (#70 decision 2).**
   - `OnBehalfOf` and `AuthorityRef` are journaled in `Principal` and restored on resume. A different value is `ErrConfig`.
   - `Actor` stays live: after an upgrade, the new deployment is the actor.
   - `IdentityFrom(ctx)` in a tool returns the live `Actor` plus the journaled principal.
6. **Audit grant.**
   - `audit.WithGrant(sg, signer)` becomes a `RunOption`, built through the internal run-extension hook (section 5).
   - The grant is already a journaled leaf. `run:start.Ext["audit.grant"]` holds its digest.
   - The signer is a key, so it is live, supplied by the resuming deployment.
   - A resume that needs to attenuate but has no signer is `ErrConfig`.
7. **RunTyped (#70 decision 4).** `Typed` holds the mode, `T`'s schema digest and the full schema. Resuming a typed run through `Run`, or with a different `T`, is `ErrConfig` before any model call.
8. **Deployment-only values, never journaled:** clock, waker, lease holder.

### Recovery dispatch (critique B6, B8)
<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
type Resumer func(ctx context.Context, runID string, start RunStart) error
var ErrNotResumable = errors.New("run not resumable by this resumer") // no category, like ErrLeaseLost
var ErrNotStarted  = fmt.Errorf("run has no run:start record: %w", ErrConfig)

func ResumeAgent(a *Agent, opts ...RunOption) Resumer              // kinds agent, session_turn (untyped)
func ResumeTyped[T any](a *Agent, opts ...RunOption) Resumer       // typed runs whose schema digest is T's
func ResumeAny(rs ...Resumer) Resumer                              // first that does not return ErrNotResumable
// plan: func ResumeFlows(flows ...plan.Resumable) agent.Resumer
```

`Recover` and `RecoverLoop` read `run:start` for each candidate:
- A run with none (a `Signal` sent to a mistyped ID, for example) is skipped. It is reported once per process through `WithRecoverErrors` as `ErrNotStarted`.
- A run no resumer claims is reported once as `ErrNotResumable`.
- Neither is re-reported on every pass.

**Typed runs are resumable by registering a typed resumer.** I disagree with the critique's "validate against the journaled schema and complete without T". bide has no JSON Schema validator; strict acceptance is decoding into `T`. A run completed under a weaker check is final, so a wrong answer could not be corrected later. The journaled full schema serves audit and a clear error.

### Saga
- `WithSaga()` is a `RunOption`, journaled as #70's `saga` flag.
- `Cancel` on a saga run rolls it back, because cancelling a saga is an abort.

### Cancellation (D1)
- `Cancel` writes `run:cancelled` `{reason}` under the reserved `run:` prefix.
- A driver checks for it with `Get` when a drive starts and at every turn boundary, and never starts a new claim after seeing it. Calls already in flight finish and record their results.
- `Run` on a cancelled run returns a `Result` and `ErrRunCancelled`. It has no category, because it is a terminal status and not a fault.
- `Recover` excludes cancelled runs through the `Lister` filter.

### Status (D8)
`RunStatus{State RunState; Terminal string; Records int}` with states:
- `NotStarted`
- `Started`
- `Completed`
- `Aborted`
- `Cancelled`

Pauses are not journaled, so a paused run reports `Started`. This is documented.

### Replaces
`Run(string) (Message, error)`, `RunResult`, `RunSaga`, `RunSagaResult`, `Stream(string)`, `StreamSaga`, `AgentStream.Final`, `RunTypedNative`, `Session.Send/SendOnce(string)`, the context decorators `WithWaker/WithClock/WithIdentity`, and `AgentEvent`/`AgentStream`.

### Migration
- Semantic PR P14 adds the new API under transitional names:
  - `RunMessage`, `StreamMessage`, `ResumeRun`
  - `Session.SendMessage`, `Session.SendMessageOnce`
  - `RunTypedMessage`
- The old methods are kept as wrappers.
- The final rewrite (P15) renames the new API and deletes the old one, covering about 172 Run, 19 Stream, 48 typed and 8 session sites.

### Rationale
- Per-run choices survive a crash, so a tenant's budget cannot be bypassed by recovery.
- Recovery needs no side table of inputs.
- A mismatch is always loud.

### Risks
- The header grows with the tool filter and schema. The size is bounded by what the caller passes.
- Journaled input, including images, raises retention questions. The redaction model in item 4 covers them.

---

## 3. Item 2: Agent construction

Unchanged from v1 except where noted.

<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
func New(model Model, j *Journal, opts ...Option) (*Agent, error)
func (a *Agent) With(opts ...Option) (*Agent, error)
func (a *Agent) Journal() *Journal
```

**Agent-only options:**
- `WithTools(...Tool)`
- `WithMiddleware(...Middleware)`
- `WithToolMiddleware(...ToolMiddleware)`
- `WithApproverVerifiers(ApproverVerifierFor)`
- `WithToolErrorRedactor(fn)`
- `WithRetrieval(r Retriever, k int)`, which the loop runs as an engine step. That removes two context keys, and retrieved documents stay journaled as #62 does.
- `WithSystemPromptFunc(fn func(context.Context, RunInfo) (string, error))`
- `WithOptions(...Option)`

**Validation in `New` and `With`.** Each failure is `ErrConfig`:
- a nil model, journal or tool;
- duplicate `Spec().Name`;
- the reserved name `final_answer`. No adapter rewrites names (`toolcfg.Check` refuses bad ones), so collisions after normalization cannot happen.
- a non-object input schema;
- an invalid `ApprovalPolicy`, or an approval policy with no `WithApproverVerifiers`, or one two of whose approvers resolve to one signing key (`ApprovalPolicy.ValidateKeys`; the gate also re-checks it on every evaluation, since the resolver is a function, though a terminal tally already journaled is reused, not recounted);
- `k < 1`;
- negative limits.

**Immutability.**
- Unexported fields only.
- `With` deep-copies the agent.
- `Spec()` is read once and snapshotted.

**Tracing.** `trace.Instrument(tracer, opts...) agent.Option`. `trace.WithModel` and `trace.WithSystem` are removed; the model name comes from `ModelInfoOf` (item 5).

**Naming sweep:**
- context `WithIdentity/WithWaker/WithClock` become run options;
- `WithToolSafety/ToolSafety(ctx)` become `ToolCall.Spec`;
- `WithModel(ctx)`, `WithModelCallHook(ctx)`, `DetachModelSink` and `EmitMessage` are removed (item 5);
- `WithNow` becomes `WithClock`;
- the option types are split: `LeaseOption`, `RecoverOption`, `RecoverLoopOption`;
- `RunScope` and `InSaga` become `RunInfoFrom(ctx) (RunInfo, bool)`;
- plan's builder `WithModel` and `WithLoadedModel` become one `plan.WithModel` option.

**`RunInfo`** gains `(RunInfo) SubRunFor(name string) string` for programmatic sub-runs (D5). The scheme is `<parent>>step:<enc name>`, next to `SubRunID`'s `<parent>><enc tool id>`. `>` and `step:` never appear in an encoded tool ID, so the two cannot collide.

**Migration.**
- P13 lands the options and `Build(model, j, opts...) (*Agent, error)` as the transitional name. The old `New` and the builder methods become wrappers over `Build`.
- P15 renames `Build` to `New` at about 367 sites and deletes the wrappers.

**Risks.** As in v1: construction errors add boilerplate; `agenttest.MustNew` covers tests.

---

## 4. Item 3: Journal format versioning

### New API
<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
const JournalFormat = "bide.journal.v1-dev" // becomes "bide.journal.v1" at the 1.0 tag
var ErrJournalVersion = fmt.Errorf("unsupported journal format: %w", ErrProtocol)
type JournalVersionError struct{ RunID, Found string; Supported []string }
const StepHeader StepKind = "header"
func (j *Journal) Format(ctx context.Context, runID string) (string, error)
```

### Two records, two owners (justified)
- **`@journal {"format"}`** is written by the journal layer as the first entry of every journal. That includes Step-only runs, audit ledgers and govern quorum runs, which have no `run:start`. It must be readable before any key is interpreted.
- **`run:start`** is written by the engine and describes what the run is.

The format cannot live in `run:start`, because many journals have no engine header.

### Dev tag (critique Q1 and B4)
- Before 1.0 there is one dev tag, `bide.journal.v1-dev`, and it is never bumped per change (maintainer decision): a change to the key scheme or the record shape between pre-releases does not change it, so pre-release journals are not promised to resume across releases.
- A 1.0 binary accepts only `bide.journal.v1`, so no journal from a pre-release binary is read under final rules.
- v1's "edit v1 in place" is withdrawn. It defeated the header.

### The format is pinned per run
- The first write fixes a run's format, and every later drive writes records of that format.
- A binary refuses to write to a run whose format it cannot write, and refuses to read one it cannot read. Both return `*JournalVersionError`.
- **Rolling-deploy guarantee:** an older binary always refuses a newer format, loudly and before any write.
- The compatibility window for 1.x (how many older formats each binary can write) is a decision for the maintainer (section 12, D1).

### How the header is written
1. Before its first write to a run, a `Journal` calls `Insert(runID, "@journal", header)`.
2. It then requires the header to be the **first entry in `Load` order**. This replaces v1's `Seq == 0` test, because A2 is relaxed (item 4).
3. A header that is not first means the journal was unversioned: `*JournalVersionError{Found: ""}`.
4. A known-good cache records runs already checked. It is keyed by run and bounded (65,536 entries).
5. A `Recover` pass does no per-run header reads. The `Lister` filter pushes "exclude finished" down to the store, so the cache does not thrash.

### How it is read (critique B5)
- **`Get` reads the record first, then the header.** If the header lookup ran first and a concurrent first writer then landed both entries, the record would be wrongly flagged as headerless.
- **`History` and `Records`** check the first entry as they stream.
- **Deleted runs.** Run IDs must never be reused after deletion (documented). A `Journal` with a stale cache that writes into a deleted run produces entries with no header, which every reader refuses loudly.

### Audit and storetest
- **Audit.** The header is leaf 0 of every tree. `EvidencePackage` carries `journal_format` and the leaf-0 proof (item 8).
- **storetest checks:**
  - (a) the header is the first entry;
  - (b) concurrent first writers through two handles leave exactly one header, and it is first;
  - (c) a raw `v999` header is refused on `History`, `Get` and writes;
  - (d) a headerless journal is refused;
  - (e) a read that races a concurrent first write never reports a headerless record.

### Risks
- Indices in tests shift by one. The script updates them.
- Pinning the format per run means one dev-tag bump strands pre-release journals. There are no users, so this is accepted.

---

## 5. Item 4: Durable redesign

### The port
<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
type Store interface {
	// Insert stores data under (runID, name) if absent; returns the stored entry and whether
	// this call stored it.
	Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error)
	Get(ctx context.Context, runID, name string) (Entry, bool, error)
	// Load yields runID's entries with Seq > after (after = -1 for all), in ascending Seq.
	Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error]
}
type Entry struct {
	Seq  int64 // opaque; strictly increasing in commit order
	Name string
	Data []byte
}

type Lister interface {
	Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error]
}
type RunFilter struct {
	After  string   // cursor: run IDs are yielded in ascending byte order after this one
	Prefix string   // tenant or namespace (D4)
	// ExcludeHolding drops runs holding an entry with any of these names; a store pushes it
	// down (NOT EXISTS). Recover passes run:complete, run:aborted, run:cancelled.
	ExcludeHolding []string
}
type Leaser interface{ /* unchanged */ }
type Redactor interface { // D2: the only permitted mutation
	Redact(ctx context.Context, runID, name string, tombstone []byte) error
}

func Capability[T any](s Store) (T, bool) // #66's function, retargeted to Store; Unwrap() Store
```

### Store requirements
The critique's replacement for v1's dense A2 is adopted.

- **A1: unique names, linearizable insert.** At most one entry per `(runID, name)`. Concurrent inserts from any number of processes have exactly one winner, and every caller and later reader sees the winner's bytes.
- **A2: commit-ordered, prefix-closed visibility.** `Seq` is an opaque `int64`, strictly increasing in commit order within a run. Every read of a run returns a prefix of the run's final order: no entry ever becomes visible with a lower `Seq` than an entry already visible.
  - Consequences: gaps are allowed, but a reader never sees 0 to 5 and 7 while 6 is still in flight.
  - Indices are computed at read time, as positions in `Load` order. Nothing reads `Seq` as a count.
- **A3: durable before return.** `inserted == true` only after commit. On an error, the entry is either absent or complete. The same bytes may be retried.
- **A4: read-your-writes, monotone visibility.** Once visible, an entry stays visible with the same bytes and position. The one exception is A6's redaction, which keeps the position.
- **A5: byte fidelity.**
- **A6: immutable except for redaction.** No update or delete in the port. `Redactor` may replace an entry's `Data` with a tombstone, only for a run that is terminal (complete, aborted or cancelled).
- **A7: honor ctx.** Detached recording is the journal's job.
- **A8: iterator hygiene.** Breaking out of an iterator releases every resource. A store must not hold a connection, transaction or lock across a `yield`, so nested writes inside a `Load` loop cannot deadlock (critique B17).

**Why A2 is enough for audit.** Inclusion proofs need a fixed position within the first `Size` entries. Consistency proofs and STH anchoring need prefix-closed visibility. Density added only O(1) named-record indexing, and the prover loads the tree anyway. An `Indexer` capability is therefore not added.

This admits commit-ordered stores (FoundationDB versionstamps, Spanner commit timestamps) as well as counter-based ones.

### The journal the agent owns
<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
type Journal struct{ /* unexported */ }
func NewJournal(s Store, opts ...JournalOption) (*Journal, error)
func (j *Journal) Store() Store
func (j *Journal) Get(ctx context.Context, runID, name string) (Record, bool, error)
func (j *Journal) History(ctx context.Context, runID string) ([]Record, error)
func (j *Journal) Records(ctx context.Context, runID string) iter.Seq2[Record, error]
func (r Record) Raw() []byte      // the stored bytes, verbatim (B2)
func (r Record) Salt() []byte     // read-only
func (r Record) ClaimID() string  // read-only
```

**No exported `Do`** (critique A4, accepted).
- Only the engine writes raw records.
- Two same-module packages need raw writes:
  - audit writes its `audit:` leaves;
  - plan writes `flow:digest`, `switch:*` and its node steps.
- They get them through `internal/journalhook`:
  - `agent` assigns function variables there in `init()`;
  - the functions use only `any`, `string` and `json.RawMessage`, so there is no import cycle.
- govern is a separate module. It uses only `agent.Step`, so it never imports root `internal`, which avoids the skew the critique warns about.

**Run extensions for sibling packages.** `internal/runext` lets audit build a `RunOption` whose journaled part goes into `RunStart.Ext`. It uses the same mechanism as `journalhook`.

**Other journal responsibilities:**
- **Singleflight is shared across `Journal` values** (critique B9).
  - The dedup is a package-level singleflight keyed by (store identity, run ID, name), using the store's interface value when it is comparable (true of every in-repo store, which are pointers).
  - An entry exists only while a call is in flight, so nothing leaks.
  - Two `NewJournal` calls over one store in one process share in-process dedup, as today.
- **Ambiguous claim commits** (critique B11). When a claim's `Insert` errors, the journal remembers the claim ID in memory (bounded, per store identity). An in-process re-drive reuses it and so recognizes its own committed marker, instead of halting a human over an effect that never ran. Claims that won are never remembered.
- **Salts, naming, encoding (`marshalJournal`), the header, claims, #65's not-started records and detached recording** all live here.
- **`Record.MarshalJSON`** must use `marshalJournal` (no HTML escaping). A golden test pins the bytes.

### At-most-once, restated over A1 to A4
1. A claim inserts a marker with a fresh claim ID. The driver has won if and only if the stored marker carries that ID (A1, and A3 covers retried inserts).
2. The effect fires only after the claim is won, and the claim is durable first (A3).
3. After a crash, the resume finds the marker without a result through `Get` (A4) and halts, unless the claimant recorded not-started (#65).
4. Recording the result is detached and single-winner (A1).
5. The retry-safe probe is a pure `Get`.
6. Plan lowers to Step (item 6), so it inherits all of the above.

### Capabilities and wrappers
The critique's key-rewriting-wrapper concern (C) is handled by contract.

- **`Unwrap` is only for key-preserving wrappers.** The `Store` docs require that a wrapper implement `Unwrap` only if it passes run IDs and names through unchanged. A wrapper that rewrites keys (for example, a tenant prefix) must not implement `Unwrap`, and must implement each capability itself.
- **storetest provides `CheckWrapper(t, wrap)`,** which fails if a wrapper that rewrites keys exposes an inner capability.
- **Why not a declared capability list.** I disagree with the critique's `Capabilities() []reflect.Type`. A key-rewriting wrapper that implements `Unwrap` is already wrong for every capability, and a reflected type list is hard to use and easy to leave stale.
- **Tenants** use the documented run-ID prefix convention plus `RunFilter.Prefix` (D4), so a tenant store needs no custom `Lister`.

### Leasing (coordinator point 2, critique A2)
- **MemStore and SQLite implement `Leaser`**, so `Lease`, `Recover` and `RecoverLoop` return `ErrConfig` (unless `WithoutLease()` is passed) only for custom stores. `Recover` and `Lease` follow one rule.
- **SQLite's leaser:**
  - uses its own connection (a lease pool of one);
  - uses single-statement upserts;
  - computes expiry in SQL with `unixepoch('subsec')`;
  - uses a short busy timeout, `min(ttl/8, 2s)`, so a lease statement never blocks past the renewal cutoff at 3/4 of the TTL.
- **SQLite `Open` opens three pools:**
  - writer: one connection, all `Insert`s;
  - reader: N connections, `Get`/`Load`/`Runs`;
  - lease: one connection.
- **`sqlite.New(ctx, db)`** documents that it needs WAL and at least two connections.
- **Documented limits:** NFS is unsupported. A forward jump of the wall clock expires leases early, which claims keep safe.

### Redaction (D2)
The tombstone format is reserved now.

- **Tombstone shape:** `{"redacted":{"leaf_hash":"<hex>","at_ms":<ms>}}`.
- **Proofs.** The prover uses the tombstone's leaf hash instead of hashing the entry. Proofs of every other record still verify. The redacted record decodes as `Record{Redacted: true}`.
- **Resume.** A run with a redacted record is terminal and cannot be resumed.
- **Scope.** Whether 1.0 ships the capability in the three stores is a decision for the maintainer (section 12, D2).

### Migration with shims (coordinator point 5)
- **P6a (semantic):**
  - adds `Store` and `Journal`;
  - moves every semantic duty into `Journal`;
  - gives MemStore, sqlite and postgres deprecated `Do` and `History` methods that delegate to an internal `Journal` over themselves. `*Journal` also satisfies `Durable`.
- **Engine code** that needs `Get` calls the unexported `journalOf(d Durable)`:
  - it returns the `*Journal` a shim exposes;
  - for a raw custom `Durable` (tests only), it falls back to a `History` scan.
- **About 640 call sites compile unchanged.** P15 deletes `Durable` and the shims.
- **Stores also gain:**
  - `Open(ctx, ...)`, `New(ctx, *sql.DB)`;
  - `bide_` table names, a `bide_schema_version` row, `WithTablePrefix`;
  - paged `Load`: keyset on `seq`, 256 rows per page, no connection held across a yield.
- **`audit.NewAnchoredStore(inner agent.Store, s Signer, anchor, opts...) (*AnchoredStore, error)`** replaces `AuditedStore`.
  - It hashes stored bytes incrementally, using a Merkle frontier keyed by position.
  - It does one `Load(after = last seq)` per insert. This is O(log n) per insert, down from O(n²) per run today.

### Online schema migration (D6)
- Formats coexist in one table, because each run's header names its format.
- Migrations are additive (new columns or tables) and keep A1 to A8 while they run.
- A binary refuses to open a schema version newer than it knows.

### Risks
- **Shim period.** Two code paths exist during the shim period; P15 ends it.
- **Singleflight keyed by comparable store identity.** A non-comparable custom store falls back to per-`Journal` dedup. This is documented, and claims keep it safe.

---

## 6. Item 5: Model call signature

<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
type ModelHandler func(ctx context.Context, call ModelCall) (ModelResponse, error)
type Middleware func(next ModelHandler) ModelHandler

type ModelCall struct {
	Request Request // Messages and Tools arrive slices.Clip'ped: appends never share a backing array
	Model   Model   // where the base handler sends requests; middleware may retarget
	RunID   string
	Turn    int
	/* unexported: sink, hooks, meter, attempt counter */
}
func (c ModelCall) AddHook(h ModelCallHook) ModelCall // returns a copy with h appended
func (c ModelCall) Attempt() int                      // set for the request a hook observes

type ModelCallHook struct {
	Before func(ctx context.Context, call ModelCall) error
	After  func(ctx context.Context, call ModelCall, a ModelAttempt)
}
type ModelAttempt struct {
	Response ModelResponse
	Err      error
}
type ModelResponse struct {
	Message   Message
	Usage     Usage
	Finish    FinishReason
	RawFinish string
	/* unexported: which request produced it */
}

type FinishReason string // #68's values, typed
const (
	FinishStop     FinishReason = "stop"
	FinishToolUse  FinishReason = "tool_use"
	FinishLength   FinishReason = "length"
	FinishFiltered FinishReason = "filtered"
)
type Finish struct {
	Reason    FinishReason
	Raw       string
	Usage     Usage
	Discarded Usage
}

type Describer interface{ Describe() ModelInfo }
type ModelInfo struct {
	Provider, Model string
	ResponseFormat  bool
}
func ModelInfoOf(m Model) (ModelInfo, bool) // follows Unwrap() Model
func CallModel(ctx context.Context, m Model, req Request, mw ...Middleware) (ModelResponse, error)
```

**A default-safe sink** (coordinator point 3, critique A3).
- The sink is claimed exclusively per request. When the base handler starts a request, it tries to claim the sink. If another request holds it, this request runs without streaming. A failed claimer releases the claim, and the next claimer triggers `TurnRestarted`.
- At the top of the chain, the agent checks whether the response it got came from the request that streamed. If not (a hedge winner that was not the claimer, or a cached or middleware-built response), the agent emits a restart and replays the response.
- `DetachModelSink` and `EmitMessage` are removed. Hedge contains no sink code, and correctness does not depend on middleware remembering anything.

**Private hooks** (critique B13).
- The agent's spend meter sits outside the hook list.
- Middleware can only add hooks, through `AddHook`, never remove or reorder them. No middleware can break budgets or `Result.Spend`.

**Attempt** (critique A3).
- A counter shared per turn numbers each request the base handler sends.
- Hooks see the true attempt number, including across parallel Hedge branches.

**Finish** (critique B14; #68 did the mapping).
- v2 adds the type, `Raw`, and `Finish.Discarded`, which lets `Replay` report the discarded spend it recorded without using context.
- An empty `Reason` stays accepted from custom models. That is #68's choice, and the `Finish` event itself marks the end of the turn; a missing `Finish` is already `ErrIncompleteResponse`.
- modeltest asserts that first-party adapters never emit an empty reason.
- **Invariant:** the loop decides whether to run tools from message content, never from `Finish`. An OpenAI forced `tool_choice` can report `stop` alongside tool calls.

**Journaled on each model record.** These are additive, and safe under raw-byte leaves (item 8):
- `Finish` and `RawFinish`;
- `Model` (`ModelInfo`), for audit and cross-provider fallback;
- `PromptDigest` and `ToolsDigest` (item 1).

**How middleware adapts:**
- **Retry:** loops `next(ctx, call)`.
- **Hedge:** `c := call; c.Model = backup`, and nothing else.
- **Cost:** `call.AddHook(After...)`. `CostMeter.Snapshot() CostSnapshot{Answer, Spend Usage; AnswerUSD, SpendUSD float64}`.
- **RateLimit:** `call.AddHook(Before: wait)`.
- **trace.Model:** reads `ModelInfoOf(call.Model)` and `resp.Finish`.

**Replaces:**
- the old `ModelHandler`;
- `WithModel(ctx)`, `WithModelCallHook(ctx)`;
- `DetachModelSink`, `EmitMessage`;
- the public `Hooks` field from v1;
- `trace.WithModel`, `trace.WithSystem`.

**Migration.** P9 edits the 48 `ModelHandler` references directly. That is not a mass rewrite, so there is no shim.

**Risks.**
- A middleware that builds its own response always gets a replay. That is intended.
- Exclusive claiming serializes streaming to one request per turn, which is also intended.

---

## 7. Item 6: Pause contract, halts, plan

<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
type Pause interface {
	error
	Paused() RunRef
	pause()
}
type RunRef struct{ RunID, RootRunID string }
func IsPause(err error) bool
func AsPause(err error) (Pause, bool)

// pause() is declared on each concrete type, never on RunRef (critique B12); a test outside
// the package asserts that a struct embedding RunRef does not satisfy Pause.
type ApprovalPending  struct{ RunRef; ToolUseID, ToolName string; Args json.RawMessage; Quorum *ApprovalTally }
type InterruptPending struct{ RunRef; Name string; Prompt any }
type SignalPending    struct{ RunRef; Name string }
type TimerPending     struct{ RunRef; Name string; FireAt time.Time }
type OutcomeUnknown   struct{ RunRef; Op OpRef; AttemptedAt time.Time; Cause HaltCause }

type HaltCause string
const (
	HaltCrashed   HaltCause = "crashed"   // marker, no result, no live claimant known
	HaltContended HaltCause = "contended" // another driver won the claim; it may be running the effect
)
type OpKind string
const ( OpTool OpKind = "tool"; OpStep OpKind = "step" )
type OpRef struct{ Kind OpKind; ID, ToolName string }
type HaltRef struct{ RunID string; Op OpRef }
func (e *OutcomeUnknown) Ref() HaltRef
type Outcome struct {
	Result   any
	IsError  bool
	Evidence any
}
func ResolveHalt(ctx context.Context, j *Journal, ref HaltRef, out Outcome, opts ...ResolveOption) error
```

**`ResolveHalt`.**
- It refuses to resolve a halt whose `Cause` is `HaltContended` and whose marker is younger than `WithMinHaltAge`. A reconciler cannot resolve an effect that another driver may still be running.
- `WithMinHaltAge` measures from the live attempt, as in #65.

**Verbs:**
- `Approve`
- `SubmitDecision(ctx, j, Decision{RunID, ToolUseID, ApproverID, Approved, Alg, Signature})`
- `AnswerInterrupt[T]`
- `Signal[T]`
- `Enqueue[T]`
- `Ack`

**Waker.** `Schedule(ctx, Wake) error`, with `Wake{RunID, RootRunID, Name, FireAt}`.
- If scheduling fails, the run fails with an error wrapping `ErrStorage` and records nothing.
- `RecoverLoop` reports it and retries, and the retry-safe path schedules again.

**Step pause guard** (critique B3). A `Step` without `StepSafety` retry-safe that returns a `Pause` from its body is `ErrConfig`.
- This is the same rule tools already follow.
- The marker stays, so the step halts, which is the safe reading, because the body may have done something before it paused.
- The error tells the developer to put the pause in its own retry-safe step.

**The "released, not fired" state is #65's `StepNotStarted`,** applied to flows through lowering.

**Why not release on pause.** I disagree with the critique's "release the marker when the body paused". That would require the body to have done nothing unrepeatable before the pause, which cannot be enforced, and a charge placed before an `Interrupt` would fire again. Declaring the step retry-safe is the enforceable form of the same contract.

**Plan lowering** (critique B7; this supersedes #70's plan marker change).
- Each node runs through the engine step hook as `agent.Step` semantics, under reserved plan prefixes:
  - node keys `node:<name>` and `node:iter:<n>:<name>`;
  - branch choices `switch:*`;
  - topology `flow:digest`.
- `node:`, `switch:` and `flow:` are added to `reservedPrefixes` in P5b, so user `Step` names inside a flow body can never collide with them.
- Node markers become Step markers. After #57 and #65, a Step marker is itself the recorded safety, so #70's `{"retry_safe": bool}` is no longer needed. #65's not-started records now cover flow nodes.
- `HaltAmbiguous` is deleted. Flow halts are `*OutcomeUnknown{Op: OpRef{Kind: OpStep}}`, and `ResolveHalt` clears them.
- Flows write `run:start{Kind: flow, Flow: {Name}, Input}`. Resuming with a different `in` is `ErrConfig`. v1's Q18 is withdrawn, because `flow:digest` already guards the topology.

**Migration (P10).**
- The new types land beside the old ones as aliases, `type PendingApproval = ApprovalPending`, and the old verbs become wrappers.
- `ResolveHalt` with the new signature lands as the transitional `ResolveHaltRef`.
- P15 renames and deletes the old names (about 224 references).

**Risks.**
- Plan journals change keys. Covered by the dev-tag bump.
- The Step guard turns today's probed halt into a loud `ErrConfig`. That is the intended outcome.

---

## 8. Item 7: Tool interface

<!-- docsnip: skip design proposal: this API is not implemented yet -->
```go
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}
type ToolSpec struct {
	Name, Title, Description string
	Input, Output            json.RawMessage
	Safety                   Safety          // plain data now
	Approval                 *ApprovalPolicy // split from Safety (critique C)
	Timeout                  time.Duration
}
type Safety struct{ ReadOnly, Idempotent bool } // comparable, serializable
type ApprovalPolicy struct {
	Need      int
	Approvers []string
}
func SingleApproval() *ApprovalPolicy // Need 1, no approver set: today's RequiresApproval

func Func[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) (Tool, error)
func MustFunc[In, Out any](...) Tool
func CompensatedFunc[In, Out any](name, description string, do ..., undo ..., opts ...ToolOption) (Tool, error)
func SubAgent(name, description string, sub *Agent, opts ...ToolOption) (Tool, error)
func RetrievalTool(name, description string, r Retriever, k int, opts ...ToolOption) (Tool, error)
// ToolOptions: WithSafety (SafetyOption), WithApproval(*ApprovalPolicy), WithTimeout, WithTitle, WithOutputSchema

type ToolCall struct {
	Use   ToolUse
	Spec  ToolSpec
	RunID string
}
func (c ToolCall) ErrorText(err error) string
type ToolHandler func(ctx context.Context, call ToolCall) (json.RawMessage, error)
type Request struct{ Messages []Message; Tools []ToolSpec; /* ... */ }
```

**`IdempotencyKey` is removed.**
- The SDK never calls it. It only counted as retry-safe when non-nil, so declaring `Idempotent` expresses the same thing.
- Tools derive their own downstream keys, or use `NextOnceKey`.
- `Safety` becomes plain data, so it can be journaled.

**The call's safety is recorded** (#70 decision 1). Tool-result and saga-fail records carry the `Safety` and `Approval` the call ran under. Rollback reads the recorded safety, so a write whose tool was later relabelled `ReadOnly` is no longer skipped.
- #57's remaining gap stays as #57 decided: a call that was retry-safe when it fired, then relabelled a side effect, has no marker. Closing it would cost a marker write on every call, and the maintainer declined that.

**`SubAgent`:**
- accepts `WithApproval`, so the parent can require approval before delegating;
- refuses `WithSafety`, because the sub-run's own markers carry safety.

**`Timeout`** (critique B10). The call runs under `context.WithTimeout`.
- A call that returns a result is recorded, even if the deadline has passed. A known outcome is never discarded.
- A call that returns an error after its context is done takes the unknown-outcome path (#57).
- `Timeout` bounds only tools that honor ctx. The loop still waits for the goroutine. This is documented.

**Tool mapping (MCP, govern, audit, chaos).** As in v1:
- `mcp.Tools` fills `Title`, `Output` and `Timeout`;
- `govern.EventTool(gov, EventToolConfig{...})`;
- `audit.AttenuatingSubAgent(name, desc, sub, AttenuationConfig)`.

#67's accepted-argument record stays with the base tool handler.

**Migration.**
- P12 makes the internals spec-based, with `specOf(t)` accepting both method sets.
- P15 changes the interface: `Func` at about 120 sites and 26 implementers.

---

## 9. Items 8 and 9

### 9.1 Proof artifacts

What remains after #66:

**Raw-byte leaves** (critique B2).
- Leaves are `tag || stored bytes`, taken from `Record.Raw()`, never re-encoded.
- `ProofBundle`, `EvidencePackage` and `CurrentGrantProof.Leaf` carry `record_bytes` (base64) verbatim.
- The verifier hashes `record_bytes`. It decodes them only for display and role checks, leniently: unknown fields are ignored for display and never for hashing.
- A 1.0 verifier therefore verifies records written by 1.1 with new fields.
- New formats: `bide.audit.proof.v3`, `bide.audit.current-grant.v3`, `bide.audit.evidence.v5`.

**STH v5** (critique B15).
- Canonical bytes include `alg`, so the signed domain is `bide.audit.sth.v5`, and `SignedTreeHead` gets `format: "bide.audit.sth.v5"`.
- Hybrid signatures separate their components by domain: each component signs `label || M'`, with distinct labels `bide.hybrid.ed25519.v1` and `bide.hybrid.mldsa65.v1`, following the IETF composite-signature construction.
- A stripped ed25519 half then verifies neither as hybrid nor as plain ed25519 over the STH.

**Signature agility** (unchanged from v1):
- `Signer` and `Verifier` gain `PublicKey()`, and `Alg` becomes a typed string.
- Every ed25519-only entry point takes `Signer` or `Verifier`.
- Evidence carries `{alg, public_key}`, and `Verify(v Verifier)` requires the key to match.
- `ApproverAlg` is journaled with each decision.

**Other formats.**
- Grant: `NotAfterUnix` in `bide.audit.grant.v2`.
- Event leaves: `bide.audit.event-leaf.v3`, with snake_case tags on `RunEvent` types (#66's open item).
- Anchor entry: `bide.audit.anchor-entry.v1`.

**Policy.**
- Producers emit the current format.
- Before 1.0, verifiers accept only the current format. Evidence v3 and v4 are dropped at once, as critique Q19 suggests; this is consistent with "no users".
- From 1.0 on, verifiers accept the current format and the one before it.

### 9.2 Remaining MUST items

**govern into its own module** (critique B16 accepted).
- govern becomes `github.com/bide-ai/bide/govern` (v0).
- The nine root-module examples that import govern move into `examples/govern/<name>`, a module that requires root and govern.
- The govern-dependent tests move to `integration/`.
- The root module drops gsm.

**bide-audit exit codes:**

| Code | Meaning |
|---|---|
| 0 | verified |
| 1 | read and understood, not verified |
| 2 | usage error |
| 3 | no verdict (checker failure or internal error) |
| 4 | input unreadable or unusable, including an unknown `format` |

- Precedence: 2 is exclusive, then 1, then 4, then 3.
- Documented rule: **only 0 means verified.** Scripts must treat 4 as a failure, never as retryable, because a tampered `format` yields 4.
- Also adds `-version` and `-json`.

### 9.3 Library panics that become errors
- `agent.New`
- `Func` and `CompensatedFunc`
- `RetrievalTool` and `WithRetrieval`
- `SubAgent`
- `audit.NewAuditedStore`'s nil and key-length panic (added by #68), which becomes `NewAnchoredStore(...) (..., error)`
- `eval.Matches(*regexp.Regexp)`
- the dead `crypto/rand` branches: since Go 1.24, `crypto/rand.Read` never returns an error

Kept, and documented:
- the plan wiring-invariant panic;
- `trace`'s re-panic of a user panic.

### 9.4 SHOULD, C and D items

| Item | Resolution | PR |
|---|---|---|
| 2.6 Lister | `RunFilter` with pushdown | P6a |
| 2.7 identity | `Describer`, `ModelInfoOf` through `Unwrap`, journaled per turn | P7, P9 |
| 2.8 Alg | typed; journaled per decision | P6a (field), P10 |
| 3.2 audit errors | sentinels; `error`-shaped verifiers | P11 |
| 5.3 Open(ctx) | stores and govern logs | P6a, P2 |
| 5.4 GovernanceHeld ctx | | P4 |
| 6.2 verbs | item 6 | P10 |
| 6.3 renames | `RunEvent`/`RunStream`, `audit.RecordStream`, drop `plan.Retryable` alias, **mcp becomes `mcptools`** (critique Q9: in the rewrite, not optional) | P15, P5 |
| 6.4 typed strings | list in section 1.1 | P7, P5, P4, P11 |
| 7.1 long parameter lists | `Decision`, `Outcome`, `RunCertSpec`, `AttenuationConfig`, `EventToolConfig`, `ParallelOption` | P10 to P12 |
| 7.2 provider kit | moves to `model/provider` | P7 |
| 7.3 exported internals | unexport engine plumbing; keep `TallyApprovals`, `ProjectEvents`, `FindToolCall`, `IsApprovalDecision` and `DecisionCheck` as the verification API; changing their verdict rule is a format bump (critique Q10) | P6a, P16 |
| 7.4 Record | unexported salt and claim; `Raw()` | P6a |
| 7.5 test doubles | `agent/agenttest` | P15 |
| 7.6 cost | `Snapshot`; `ModelAttempt` | P9 |
| 8.3 store schema | prefix, version | P6a |
| 9.4 plan config | version and snake_case (strict reading done by #68) | P5 |
| 9.6 units | `AttemptedAtMillis`, `TimestampNanos`, `NotAfterUnix`; the clock option reaches attempt timestamps (D7). Salts and claim IDs stay random by design | P6a, P11 |
| C: split approval from safety | adopted | P12 |
| C: model identity and what the model was shown | adopted | P9 |
| C: `OutcomeUnknown.Cause` | adopted | P10 |
| D1 cancel | adopted (reserve `run:cancelled`) | P14 (key in P6a) |
| D2 redaction | tombstone format reserved | P6a; the capability depends on maintainer decision D2 |
| D3 per-run tool filter | `WithToolFilter(names...)` `RunOption`, journaled | P14 |
| D4 multi-tenancy | run-ID prefix convention plus `RunFilter.Prefix` | P6a |
| D5 programmatic sub-runs | `SubRunFor` | P13 |
| D6 schema migration | statement in item 4 | P6a docs |
| D8 status | `Status` | P14 |

### 9.5 Documentation policy
Unchanged from v1:
- a doc comment on every exported identifier, starting with its name;
- `Example` tests in place of call snippets in comments;
- no competitor or PR references in godoc;
- `internal/tools/doccheck` in CI (P8), with an allowlist that must stay empty from P16 on.

---

## 10. Implementation plan

### 10.1 Waves
Within a wave, no two PRs edit the same file. Sizes:
- S: under 300 lines changed
- M: 300 to 1000
- L: 1000 to 2500
- XL: over 2500, mostly mechanical

**Wave 0 (in flight, merge first):** #66, #67, #70, #65, #68, #69.

**Wave 1 (parallel)**

| PR | Scope | Files | Size | Tests must prove |
|---|---|---|---|---|
| P1 | agent file split, pure moves | agent.go into agent/loop/toolexec/generate/runctx.go; saga.go into saga/compensate.go; recovery.go into recovery/lease/replay.go, with `isPause` moved to pause.go; store.go into record/step/halt.go | S | `go test` unchanged; `go doc -all ./agent` identical |
| P2 | govern module, examples/govern, integration module | go.mod, go.work, govern/**, examples/{nine}, integration/**, the two architecture tests, CI | M | root builds with `GOWORK=off` and without gsm; moved tests pass |
| P3 | bide-audit exit codes, `-version`, `-json` | cmd/bide-audit/main.go and its tests | M | exit table including precedence |
| P4 | eval | eval/* | S | `Matches(nil)` returns `ErrConfig`; GovernanceHeld ctx; report format |
| P5 | plan config v1 | plan/config.go, plan/topology.go, plan/builder.go (the alias) | S | version required; snake_case keys |
| P7 | adapters: typed `FinishReason`, `Finish.Raw`, `Discarded`, provider kit, `Describer` | agent/model.go, agent/provider_http.go (deleted), model/provider/**, model/{anthropic,openai,gemini}/**, model/modeltest/** | M-L | mapping tables; no empty reason from first-party adapters; core does not import `model/provider` |
| P8 | doccheck plus allowlist | internal/tools/doccheck/**, CI | S | analyzer golden tests |

**Wave 2 (parallel)**

**P6a: Store/Journal semantics, header, shims (L, about 2200 lines).**
- **Files:**
  - agent: journal.go (new), memstore.go (new), record.go (all new `Record` fields for the whole redesign: `Raw`, `Salt()`, `ClaimID()`, `AttemptedAtMillis`, `Finish`, `RawFinish`, `Model`, `PromptDigest`, `ToolsDigest`, `ApproverAlg`, `Safety`/`Approval` on results, `Redacted`), step.go (probe via `Get`, B3 pause guard), keys.go (`@journal`, `run:cancelled`, `run:limits:`), errors.go, lease.go, recovery.go (Capability over Store, `RunFilter`, skip not-started)
  - internal/journalhook/**, internal/runext/**
  - agent/storetest/** (durabletest kept as a thin deprecated wrapper), agent/agenttest/**
  - store/sqlite/** (Leaser, three pools, paged Load), store/postgres/**
  - audit/audited_store.go (anchored store, incremental), chaos/bide.go
- **Tests must prove:**
  - storetest over MemStore, sqlite and postgres:
    - A1 with 64 goroutines times 3 handles;
    - A2 prefix-closure: readers polling during concurrent inserts never observe a gap below a visible entry;
    - A5;
    - A8, both break-early and a write nested inside `Load`;
    - header checks (a) to (e);
    - singleflight shared across two Journals;
    - ambiguous-claim reuse;
    - `CheckWrapper`.
  - All DST, crash and chaos suites pass through the shims.
  - Step pause guard.
  - SQLite lease survives a 5s writer hold without lease loss.
  - Golden bytes for `Record.MarshalJSON`.

**P10: pause contract, Waker, halt unify, verbs, as aliases and wrappers (L).**
- **Files:** agent/pause.go, awaitfor.go, channel.go, approval.go, halt.go, toolexec.go (`IsPause` sites), session.go (docs), and the examples interrupt, approval, signals, recover, webhook.
- **Tests must prove:**
  - the seal, including the embedding test;
  - `ResolveHaltRef` for tools and steps;
  - a contended halt is not resolved while young;
  - Waker failure is retried;
  - `MemWaker` uses `IsPause`.

**Wave 3 (parallel)**

**P9: ModelCall (L).**
- **Files:** agent/generate.go, modelcall.go, replay.go, middleware/{retry,hedge,cost,ratelimit,middleware}.go, trace/trace.go, agent/typed.go (`injectSystem`), agent/retrieval.go.
- **Tests must prove:**
  - hooks run exactly once per request under Retry, Hedge and Retry(Hedge);
  - a middleware cannot remove the meter;
  - only the winner's tokens reach the stream, with no sink code in Hedge;
  - the Hedge race detector test passes with a middleware that appends to `Messages`;
  - `Attempt` is unique across branches;
  - `Replay` reproduces `Finish` and `Spend`;
  - per-turn digests are journaled;
  - no `context.WithValue` in generate.go or modelcall.go (AST test).

**P5b: plan lowering (M).**
- **Files:** plan/flow.go, plan/conformance.go, plan/builder.go (docs), agent/keys.go (plan prefixes; not touched by any other wave-3 PR).
- **Tests must prove:**
  - `ResolveHaltRef` clears a node halt;
  - a node cancelled before its body is re-attempted (#65 through lowering);
  - a different flow input is `ErrConfig`;
  - zero `History` calls per node (counting store);
  - conformance passes with the new keys.

**P11: proof artifacts (L).**
- **Files:** audit/* except audited_store.go and delegate.go, audit/verify/**, cmd/bide-audit/**, examples/govern/proof-carrying-run.
- **Tests must prove:**
  - a record with an unknown future field verifies under `record_bytes`;
  - a stripped hybrid half fails;
  - an STH v4 artifact gets `ErrFormat`;
  - ML-DSA end to end;
  - the exit table covers format cases.

**Wave 4**

**P12: tool internals (L).**
- **Files:** agent/tool.go, tool_middleware.go, toolexec.go, subagent.go, retrieval.go (tool part), typed.go (`answerTool`), compensate.go, model.go (`Request.Tools`), model/* (spec reads), mcp/**, middleware/tool.go, error_summary.go, trace/trace.go (Tool half), audit/delegate.go, govern (its module).
- **Tests must prove:**
  - a result returned after the deadline is recorded;
  - a late error halts a side-effect tool;
  - rollback uses the recorded safety;
  - `SubAgent` with `WithApproval` pauses;
  - the MCP mapping.

**Wave 5**

**P13: construction under `Build` (M-L).**
- **Files:** agent/agent.go, options.go (new), runctx.go (`RunInfo`, `SubRunFor`), identity.go, trace/trace.go (`Instrument`), typed.go (clone), retrieval.go (option).
- **Tests must prove:**
  - every validation error;
  - `With` isolation under `-race`;
  - precedence rules;
  - combination types reject the wrong scope at compile time (a vet-style test using go/types).

**Wave 6**

**P14: Run API under transitional names (L).**
- **Files:** agent/loop.go, result.go, saga.go, stream_agent.go (becomes stream.go), typed.go, session.go, subagent.go (call), recovery.go (dispatch), audit/eventsink.go, eval/eval.go.
- **Tests must prove:**
  - a Result on every error kind;
  - B1: a budget and prompt passed per run survive `RecoverLoop`;
  - a limit amendment is journaled;
  - other mismatches are `ErrConfig`;
  - principal restored and `Actor` live;
  - typed runs resumed by `ResumeTyped`;
  - a not-started run is skipped and reported once;
  - `Cancel` on a live run and on a saga (rollback);
  - `Status`;
  - image input.

**Wave 7**

**P15: the consolidated mechanical rewrite and shim removal (XL).**
- **Files:** every call site in every module, plus deletion of the shims and transitional names.
- **Script:** `internal/tools/migrate` (go/ast, committed), which does all of the following:
  - `Durable` becomes `*Journal`, and store construction becomes `NewJournal(...)`;
  - `Build` becomes `New`, and builder chains become options;
  - `RunMessage` becomes `Run`, the old `Run` and `RunSaga` calls become `Run(..., UserText(x))`/`WithSaga()`, and results are rewritten;
  - `Func` sites use options and `Must*`, and implementers' method sets become `Spec()`;
  - pause and verb aliases are resolved, and `ResolveHaltRef` becomes `ResolveHalt`;
  - `RunEvent`/`RunStream`;
  - `mcp` becomes `mcptools`;
  - `ScriptedModel` moves to `agenttest`.
- A rebase reruns the script.
- **Tests must prove:** `go vet` and `go test ./...` pass in every module; no deprecated identifier remains (grep gate); `go doc` shows no transitional name.

**Wave 8**

**P16: docs and cleanup (M).**
- **Files:** doccheck allowlist emptied, `Example` tests, stale and competitor text moved out of godoc, docs/GUARANTEE.md (format policy, journaling rule, only-0-verified), migration guide, CHANGELOG, and the journal format tag set to its final value only at the release commit.
- **Tests must prove:** doccheck is clean.

**Critical path:** P1, then P6a, then P9, P5b and P11 (in parallel), then P12, then P13, then P14, then P15, then P16. That is eight sequential steps. P2 to P5, P7 and P8 ride wave 1.

### 10.2 Transitional names (removed by P15)

| Transitional | Final |
|---|---|
| `Build` | `New` |
| `RunMessage`, `StreamMessage`, `ResumeRun`, `RunTypedMessage`, `Session.SendMessage`, `Session.SendMessageOnce` | `Run`, `Stream`, `Resume`, `RunTyped`, `Session.Send`, `Session.SendOnce` |
| `ResolveHaltRef` | `ResolveHalt` |
| aliases `PendingApproval`, `Interrupted`, `Awaiting`, `Sleeping`, `ResumeHalt` | deleted |
| wrappers `Resume[T]`, `ApproveAs`, channel `Send`, `ResolveStepHalt` | deleted |
| `Durable` interface and store `Do`/`History` shims | deleted |
| old `Tool` method set (accepted through `specOf`) | `Spec()` only |

### 10.3 Performance gates
1. **Counting-store round-trip test.** `agenttest.CountingStore` counts `Insert`, `Get`, `Load` calls and entries read. The test lands in P6a and is kept green by every later PR, which may only lower the budget. It asserts these exact per-operation budgets (deterministic, no timing):

   | Operation | Budget |
   |---|---|
   | first drive of a new run | 1 `Load`, `Insert @journal`, `Insert run:start` |
   | each live model turn | 1 `Insert`, plus 1 `Get` (`run:cancelled` check) |
   | side-effect tool call | 2 `Insert` (claim, result); +1 only under #67's condition |
   | retry-safe tool call | 1 `Insert` |
   | retry-safe `Step` | 1 `Get`, 1 `Insert` |
   | completion | 1 `Insert` |
   | resume of a run with n records | 1 `Load` of n entries, no point reads for markers |
   | `Recover` pass over R runs, D of them driven | ceil(R/500) `Runs` pages on SQL stores (MemStore the same), no `Load`, and 3 `Get` per driven run (the terminal markers, re-checked under its lease): 3D in all, none for the finished runs the filter excludes |
   | anchored insert | 1 `Load` of the new entries only, O(log n) hashes |

   The `Recover` row was raised after P6a, with the maintainer's approval: a pass read no run at all until it was found to call `resume` for a run another driver finished between the listing and the lease, and the three point reads close that gap.

2. **Benchstat on the CI runner.**
   - Benchmarks land in P6a: `BenchmarkRunTurns`, `BenchmarkToolCallSideEffect`, `BenchmarkStep`, `BenchmarkRecoverPass10k`, `BenchmarkAnchoredInsert`, `BenchmarkSQLiteInsert`, `BenchmarkPostgresInsert`.
   - The existing bench workflow (#45/#47, standard runner) runs base against head with `-count=10`.
   - P6a, P9, P12, P14 and P15 must show no benchstat regression over 5% in time or allocs at p < 0.05, and must paste the table into the PR.
   - For the `cmd/bench` A/B (bench.yml), "time" means the mean (wall-clock, and throughput as its inverse), p90 and p99 of run latency, never p50: under the closed-loop harness's contention the overhead scenario's p50 is bimodal, a scheduling artifact that shifts with changes that add no work (maintainer decision, recorded with #116, which moves bench reporting to mean, p90 and p99).
   - `BenchmarkAnchoredInsert` must show the O(n) to O(log n) improvement.

### 10.4 Order relative to the follow-ups
- Follow-ups A, B and C are done by #65, #67 and #69.
- Their remaining edges are folded into the plan:
  - #65 reaches flows through P5b;
  - #67's record stays with the base tool handler in P12;
  - #69's totals are the Result semantics in P14.

---

## 11. Disposition of the critique

| Item | Verdict | Where / why |
|---|---|---|
| A1 relax A2 | Agree; adopted the stronger prefix-closed form | item 4 |
| A2 SQLite Leaser | Agree, with all the pitfalls | item 4 |
| A3 sink, clip, attempt | Agree | item 5 |
| A4 unexport Do | Agree; govern needs only Step, audit and plan use the hook | item 4 |
| A5 shims, one rewrite | Agree | section 10 |
| B1 | Agree; limits change through an auditable amendment rather than `ErrConfig` | item 1 |
| B2 | Agree | item 8 |
| B3 | Partly: guard adopted; release on pause rejected (unenforceable); release on not-started is #65 | item 6 |
| B4 | Agree; the window is decision D1 | item 3 |
| B5 | Agree; deletion handled by a no-reuse rule plus loud refusal | item 3 |
| B6 | Agree | item 1 |
| B7 | Agree | item 6 |
| B8 | Partly: typed resumers instead of schema-only completion | item 1 |
| B9 | Agree (global singleflight keyed by store identity) | item 4 |
| B10 | Agree | item 7 |
| B11 | Agree | item 4 |
| B12 | Agree | item 6 |
| B13 | Agree | item 5 |
| B14 | Mostly done by #68; typed plus the content-decides invariant added | item 5 |
| B15 | Agree | item 8 |
| B16 | Agree | 9.2 |
| B17 | Agree | item 4 |
| C approval split | Agree; `IdempotencyKey` dropped as a func that is never called | item 7 |
| C capability declaration | Disagree; contract plus `CheckWrapper` | item 4 |
| C option combinations | Agree; closed list | 1.2 |
| C `OutcomeUnknown.Cause`, `Lister` filter, model identity, shown-context digests | Agree | items 4 to 6 |
| D1 to D8 | Agree | 9.4 |
| E: Q1 | Agree | item 3 |
| E: Q4 | Agree | item 4 |
| E: Q12 | Agree | item 4 |
| E: Q18 | Moot; flow input journaled instead | item 6 |
| E: Q19 | Agree | item 8 |
| E: Q9 | Agree (rename in P15) | 9.4 |
| E: Q13 | Agree (documented) | 9.2 |

v1 questions now resolved, and not asked again:
- **Q2** refuse pre-v1 journals;
- **Q3** `Store`/`Journal` names;
- **Q5** by #69;
- **Q6** journaled input;
- **Q7** `RunTyped` stays a function;
- **Q8** move govern;
- **Q10** keep the verification API;
- **Q11** reserve `final_answer`;
- **Q14** `Timeout` semantics;
- **Q15** retrieval as an option;
- **Q17** renames.

---

## 12. Decisions for the maintainer

**D1. Journal format compatibility across 1.x.** A run's format is pinned (item 3). The choice is how a 1.x binary treats runs of an older 1.x format.
- Options:
  - (a) keep reader and writer paths for the previous format;
  - (b) read only, and require draining runs before upgrading;
  - (c) keep every 1.x format forever.
- Runs can pause for weeks awaiting approval, so draining is often not possible.
- **Recommendation:** (a), with a window of N-1. Each format change carries its predecessor's writer for one minor release, and the release notes say to finish or resolve runs that are two formats old before upgrading twice.

**D2. How much redaction ships in 1.0.** The tombstone format and `Record.Redacted` are reserved either way, which is the part that would break later.
- Options:
  - (a) implement `Redactor` in MemStore, SQLite and Postgres, plus `agent.Redact(ctx, j, runID, name)`, before 1.0;
  - (b) reserve only, and ship it in 1.x.
- (a) is about 500 lines and makes GDPR erasure of journaled inputs and retrieved documents possible on day one.
- **Recommendation:** (a). Inputs, images and retrieved documents are now journaled by design (item 1), so erasure is part of the promise.

**D3. Release shape for govern.** Core would be 1.0 while `govern` stays a v0 module until gsm reaches v1. The alternative is holding the 1.0 tag until gsm is stable.
- **Recommendation:** ship core 1.0 with govern at v0.x, stated in the README. Governance users then accept v0 churn explicitly, and the core promise does not wait on another project's schedule.

**Critical files for implementation**
- /Users/dayna/code/go-agents/agent/store.go (becomes journal.go, memstore.go, record.go, step.go and halt.go)
- /Users/dayna/code/go-agents/agent/agent.go (becomes loop.go, toolexec.go, generate.go and runctx.go)
- /Users/dayna/code/go-agents/agent/modelcall.go
- /Users/dayna/code/go-agents/audit/merkle.go (raw-byte leaves) and /Users/dayna/code/go-agents/audit/sth.go (STH v5)
- /Users/dayna/code/go-agents/plan/flow.go
## 13. Decision: generic methods (maintainer, 2026-09-30)

Use Go 1.27 generic methods wherever a generic function has a natural receiver of a concrete type:
- `plan.RegisterStep`, `RegisterJoin2`, `RegisterJoin3`, `RegisterTool`, `RegisterModel`, `RegisterPredicate` become methods on `*plan.Registry` (`r.RegisterStep[I, O](name, fn, opts...)`).
- `RunTyped[T]` becomes `(*Agent).RunTyped[T](ctx, runID, input, opts...)` (this supersedes Q7's "package function").
- The journal verbs become methods on `*Journal`: `j.Step[T]`, `j.Parallel[T]`, `j.Signal[T]`, `j.Enqueue[T]`, `j.AnswerInterrupt[T]`. `*Journal` is a concrete struct, so the rule that generic methods cannot satisfy interfaces costs nothing here; storage is mocked at the `Store` interface.
Functions with no receiver stay functions: `Interrupt`, `Await`, `AwaitFor`, `Receive` (they read the run from ctx), `Func`, `CompensatedFunc`, `schema.For`, `plan.New`, `Load`, `When`, `Else`, `LoopBack`, `Capability`.
Implemented in P15's scripted rewrite (P14 lands `RunTyped` under its transitional name). Tooling note: homebrew gofmt and polywave-tools do not parse generic methods; CI and agents use the Go toolchain's gofmt.
