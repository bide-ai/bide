# Pre-1.0 API redesign (proposal)

Status: proposal, under review. Nothing here is implemented yet.


## 0. Baseline and assumptions

- **Baseline** is main at 2ff6bf6 (#55 is merged), plus #57 and #60 merged, plus three separate fixes:
  - **F1:** `agent.Capability[T any](store Durable) (T, bool)`, found through `Unwrap() Durable`. It is uncommitted in `wt-apibugs`. `AuditedStore` implements `Unwrap`.
  - **F2:** plan step bodies take `context.Context`.
  - **F3:** snake_case JSON tags on the audit artifacts (`Inclusion`, `Consistency`, `EventInclusion`, and the rest).
- **Not redesigned here:** those three. The design below builds on them. F1 is retargeted from `Durable` to the new `Store` port in item 4, with the same semantics. F3's tag names are taken as given.
- **Other in-flight branches** land before this plan starts (Wave 0): `audit/ib-audit`, `audit/ib-plan`, `audit/ib-model`, `audit/ib-govern`, `audit/journal-completeness`.
  - `ib-model` carries failing tests "a turn cut off at its token limit or stopped by a filter is not an answer". Item 5 gives that fix a normalized reason to key on.
- **Journal compatibility:** #60 already breaks v0.7 journals. Every pre-v1 journal is refused (item 3).
- **Invariants at every PR boundary:** at-most-once side effects via attempt claims, crash-safe resume, exact replay, and offline-verifiable proofs. Core packages (`agent`, `schema`, `middleware`, `audit`, `plan`, `eval`) import no adapter.
- **Size scale:**

  | Size | Lines changed |
  |---|---|
  | S | under 300 |
  | M | 300 to 1000 |
  | L | 1000 to 2500 |
  | XL | over 2500, mostly mechanical call-site rewrites |

---

## 1. Cross-cutting conventions (these apply to every item)

### 1.1 Naming rule
1. **`WithX`** names a functional option, and nothing else. Some settings apply at several scopes (an agent default, one run, one tool, one step, one lease). The option for such a setting is one function whose result satisfies every scope's option interface, so `WithMaxTurns(5)` works in `New`, `Run` and `(*Agent).With`. `WithoutX` is the negated form (`WithoutLease()`).
   - The same `With` name may appear in several packages, because it is package-qualified: `anthropic.WithModel(id)` and `plan.WithModel(m)` are both fine.
   - An unqualified `agent.WithModel(ctx, m)` that is not an option is not allowed.
2. **Context helpers:** `XFrom(ctx) (X, bool)` reads a value and `ContextWithX(ctx, x) context.Context` writes one. Context carries only request-scoped data that has to cross the `Tool.Call(ctx, args)` boundary into tool code: run info, identity, the audit grant, and the once-key counter.
   - The agent's own machinery never travels in context: waker, clock, stream sink, hooks, model override, tool safety, and the error redactor.
3. **Constructors:** a constructor that can fail returns `(T, error)`. `MustX` is the panicking twin, offered only where a constructor is routinely called at package init (`MustFunc`, `MustCompensatedFunc`).
4. **Verbs** that deliver an answer to a paused run use the pause's noun (`AnswerInterrupt`, `Signal`, `Enqueue`, `Approve`, `ResolveHalt`). Every pause point is named by `name`, never by `key` or `channel` for the same role.
5. **Closed sets are typed strings with constants:** `FinishReason`, `ToolChoiceMode`, `Alg`, `OpKind`, `EvidenceKind`, `TopologyNodeKind`, `MetricDirection`.
6. **No stutter:** `RunEvent` rather than `AgentEvent`, and `RunStream` rather than `AgentStream`.

### 1.2 Option mechanics
Options are interfaces with unexported methods, so one option value can satisfy several scopes and so bad values fail at construction:

```go
type Option interface{ applyAgent(*agentConfig) error }       // New, (*Agent).With
type RunOption interface{ applyRun(*runConfig) error }        // Run, Stream, Resume, Session.Send, RunTyped
type ToolOption interface{ applyTool(*ToolSpec) error }       // Func, CompensatedFunc, SubAgent, RetrievalTool
type StepOption interface{ applyStep(*stepConfig) error }     // Step, Parallel tasks
type ParallelOption interface{ applyParallel(*parallelConfig) error }
type LeaseOption interface{ applyLease(*leaseConfig) error }
type RecoverOption interface{ applyRecover(*recoverConfig) error }
type RecoverLoopOption interface{ applyRecoverLoop(*recoverLoopConfig) error }
type ResolveOption interface{ applyResolve(*resolveConfig) error }

// AgentRunOption is an option valid both as an agent default and for one run.
type AgentRunOption interface{ Option; RunOption }
```

An option that is invalid for a scope does not compile there. For example, `WithRecoverInterval` passed to `Recover` is a compile error, which fixes review 6.1's "silently ignored" options.

### 1.3 Error rule
- Every error wraps exactly one category, as the `agent` doc already promises. `audit` gains `ErrMalformed` (wraps `agent.ErrProtocol`), `ErrNotVerified` (a condition, no category, like `ErrLeaseLost`), and `ErrUnsupportedFormat` (wraps `ErrMalformed`).
- Verifiers converge on one shape (section 9.2).
- Library code does not panic on caller input (section 9.3).

### 1.4 Documentation rule
Covered in section 9.5.

---

## 2. Item 1: Run API

### New API
```go
// Run drives runID to completion, a pause, or an error. Result is non-nil whenever the run
// passed argument validation, including when err is a Pause or a failure.
func (a *Agent) Run(ctx context.Context, runID string, input Message, opts ...RunOption) (*Result, error)

// Resume continues a started run from its journal, using the input and mode journaled in its
// run header. It is what Recover, RecoverLoop and MemWaker call.
func (a *Agent) Resume(ctx context.Context, runID string, opts ...RunOption) (*Result, error)

// Stream is Run with live events. Run is Stream(...).Result().
func (a *Agent) Stream(ctx context.Context, runID string, input Message, opts ...RunOption) *RunStream

type RunStream struct{ /* unexported */ }
func (s *RunStream) Events() iter.Seq[RunEvent]
func (s *RunStream) Result() (*Result, error) // drains events; replaces Final

type Result struct {
	RunID    string
	Message  Message         // the final answer; zero unless err == nil
	Output   json.RawMessage // typed runs: the accepted structured answer, as journaled
	Usage    Usage           // whole run (replayed + live): recorded responses
	Spend    Usage           // whole run: every request billed, discarded ones included
	Live     Invocation      // this invocation only
}
type Invocation struct {
	Usage, Spend Usage
	Turns        int
	Duration     time.Duration
}

// Typed runs (see open question Q7 for method vs function).
func RunTyped[T any](ctx context.Context, a *Agent, runID string, input Message, opts ...RunOption) (T, *Result, error)
func WithOutputMode(m OutputMode) RunOption // OutputTool (default) | OutputNative
type OutputMode string
const ( OutputTool OutputMode = "tool"; OutputNative OutputMode = "native" )

// ValidateRunID reports whether id may name a top-level run or session.
func ValidateRunID(id string) error

// Session
func (a *Agent) Session(ctx context.Context, id string) (*Session, error)
func (s *Session) Send(ctx context.Context, input Message, opts ...RunOption) (*Result, error)
func (s *Session) SendOnce(ctx context.Context, key string, input Message, opts ...RunOption) (*Result, error)
```

**Per-run options** (each is also an agent default, so each returns `AgentRunOption`):
- `WithMaxTurns(n int)`
- `WithTokenBudget(n int)`
- `WithSystemPrompt(s string)`
- `WithSystemPromptFunc(fn func(context.Context, RunInfo) (string, error))`
- `WithSampling(opts ...SamplingOption)`
- `WithToolChoice(ToolChoice)`
- `WithMaxConcurrency(n int)` (also a `ParallelOption`)
- `WithWaker(Waker)`
- `WithClock(now func() time.Time)` (also a `ResolveOption`)

**Run-only options:**
- `WithIdentity(Identity)`
- `WithSaga()`
- `WithOutputMode(OutputMode)` (RunTyped only; `Run` returns `ErrConfig` if it is present)

**Run header record.** The agent writes `@run` once per run, through insert-if-absent, before its first model turn. It holds `RunHeader{Input Message; Saga bool; Kind RunKind; Session string; Typed *TypedHeader}` as a `StepValue`.
- On every later invocation the journaled header is authoritative.
- A non-zero input that differs from the journaled one (compared by canonical encoding) is `ErrConfig`, the same rule `Session.Send` already applies.
- A saga flag that differs is `ErrConfig`.
- `Resume` uses the journaled input, mode and session reference. It returns `ErrConfig` for a typed run ("resume it with RunTyped[T]"), because strict decoding needs `T`.

**Sessions.** A turn is a run. `turnRecord.Input` becomes a `Message`, and the turn's `@run` header names the session and turn so `Resume` can rebuild the seed from the session journal.

**Saga.** Saga is folded into the `WithSaga()` option and journaled in the header, not a separate type.
- Reason: a saga is a property of the run's journal. Resuming a saga run without saga mode today silently skips rollback, because the `StepSagaFail` record is treated as done.
- Sub-agents inherit saga mode from `RunInfo` as they do now.

**RunTyped:**
- `OutputTool` keeps the `final_answer` tool.
- `OutputNative` requires `model.(Describer).Describe().ResponseFormat` (item 5) and fails with `ErrConfig` before any call otherwise.
- The header records the schema digest and the mode, so a later `RunTyped` with a different `T` is `ErrConfig` rather than a silently different contract.

### Replaces
`Run(ctx, id, string) (Message, error)`, `RunResult`, `RunSaga`, `RunSagaResult`, `Stream(string)`, `StreamSaga`, `AgentStream.Final`, `RunTypedNative`, `Session.Send/SendOnce(string)`, the ctx decorators `WithWaker`, `WithClock` and `WithIdentity`, the "zero Result on error" behavior, `AgentEvent` and `AgentStream`.

### Migration
- About 172 `.Run*` sites (36 non-test), 19 Stream sites, 48 typed sites and 8 Session sites move to the new signatures. This is done by a rewrite script shipped with the PR, so a rebase means rerunning it rather than hand-merging:

  | Old | New |
  |---|---|
  | `msg, err := a.Run(ctx, id, "x")` | `res, err := a.Run(ctx, id, agent.UserText("x"))`, then `res.Message` |
  | `RunSaga` | `Run(..., agent.WithSaga())` |
- In-repo callers that change by hand:
  - `SubAgent` calls `sub.Run(ctx, info.SubRun(), UserText(task))`.
  - `audit.AttenuatingSubAgent`, likewise.
  - `eval.AgentRunner`.
  - `audit.Record`, renamed `audit.RecordStream(log, *agent.RunStream, onEvent)` returning `(*agent.Result, error)`.
  - The doc examples in `recovery.go`, `pause.go` and `identity.go`, rewritten as `Example` tests.
  - The `RecoverLoop` and `MemWaker` closures in examples become `agent.ResumeFunc(a)` (item 6).

### Rationale
- One entry point with variadic run options can grow without breaking method values.
- `Message` input unlocks images.
- A Result on error fixes #55's hidden spend.
- The run header makes resume independent of what the caller remembers (exact replay requires the same seed) and lets recovery resume runs without an `inputFor(runID)` side table.

### Risks
- **Input comparison:** the input on resume is compared by canonical encoding, so a caller that rebuilds an equivalent message with different part order gets `ErrConfig`.
  - Mitigation: a zero `Message`, or `Resume`, means "use the journal".
- **Header content:** the `@run` record puts the input into the journal. It is salted and disclosed only by choice, but retention policies now cover it (open question Q16).
- **Typed runs:** they cannot be resumed generically. This is documented, and the resumer returns a clear `ErrConfig`.

---

## 3. Item 2: Agent construction

### New API
```go
func New(model Model, j *Journal, opts ...Option) (*Agent, error)
func (a *Agent) With(opts ...Option) (*Agent, error) // a new agent; a is unchanged
func (a *Agent) Journal() *Journal
```

**Agent-only options:**
- `WithTools(tools ...Tool)`
- `WithMiddleware(mw ...Middleware)`
- `WithToolMiddleware(mw ...ToolMiddleware)`
- `WithApproverVerifiers(ApproverVerifierFor)`
- `WithToolErrorRedactor(func(tool string, err error) string)`
- `WithRetrieval(r Retriever, k int)` (see below)
- `WithOptions(opts ...Option) Option`, which bundles options for packages like `trace`.

**Dual-scope options** are the `AgentRunOption`s listed in section 2.

**Validation in `New` and `With`.** Each failure is `ErrConfig` naming the culprit:
- a nil model, nil journal, or nil tool;
- two tools with the same `Spec().Name`;
- a tool named `final_answer` (reserved, Q11);
- a tool whose `Spec().Input` is not a JSON object schema;
- an `ApprovalPolicy` that fails `Validate`, or any tool with `Safety.Approval != nil` and no `WithApproverVerifiers`;
- `WithRetrieval` with `k < 1`;
- negative limits;
- an option applied twice where "twice" is ambiguous (two `WithSystemPrompt`: last wins, documented; two `WithTools`: appended).

**Immutability.** The Agent has only unexported fields and is never mutated after `New`.
- `With` deep-copies the tool map and middleware slices.
- `Spec()` of every tool is read once at `New` and snapshotted.
- A dynamic MCP tool list is picked up by calling `With(WithTools(...))`, not by mutating the agent.

**Retrieval** becomes an agent option instead of a middleware constructor.
- The loop runs each retrieval layer as a journaled engine step (`@retrieval/<layer>`).
- This removes `withModelRun` and `retrievalLayerKey` from ctx, and `WithRetrieval` becomes a true option under the naming rule.

**Tracing:**
- `trace.Instrument(tracer, opts ...trace.Option) agent.Option` returns `agent.WithOptions(agent.WithMiddleware(trace.Model(...)), agent.WithToolMiddleware(trace.Tool(...)))`.
- `trace.WithModel` and `trace.WithSystem` are removed. The span reads the model identity from `ModelCall.Model` through `Describer` (item 5).

### Replaces
`New(model, store, tools...) *Agent` (panicking), `(*Agent).WithMaxTurns`, `WithTokenBudget`, `WithSystemPrompt`, `WithSystemPromptFunc`, `WithSampling`, `WithToolChoice`, `WithApproverVerifiers`, `WithToolErrorRedactor`, `SetMaxConcurrency`, `Use`, `UseTool`, the unexported `clone` and `cloneWith`, and PR #57's `dupTool` field with its run-time `checkTools`.

### Naming sweep in `agent` (the rule applied)

| Old | New |
|---|---|
| ctx `WithIdentity`, `WithWaker`, `WithClock` | run options of the same names |
| `WithToolSafety`, `ToolSafety(ctx)` | removed: `ToolCall.Spec.Safety` (item 7) |
| `WithModel(ctx, m)` (#55) | removed: `ModelCall.Model` (item 5) |
| `WithModelCallHook(ctx, h)` | removed: `ModelCall.Hooks` (item 5) |
| `DetachModelSink`, `EmitMessage` | removed: `ModelCall.Sink` (item 5) |
| `WithRetrieval(r, k) Middleware` | `WithRetrieval(r, k) Option` |
| `WithNow` (ResolveOption) | `WithClock` (one clock shape everywhere) |
| `RecoverOption` covering lease and loop | `LeaseOption`, `RecoverOption`, `RecoverLoopOption` |
| `RunScope`, `InSaga` | `RunInfoFrom(ctx) (RunInfo, bool)` |
| `audit.(*AuditedStore).WithClock(func() int64)` | option `audit.WithClock(func() time.Time)` on its constructor |
| `plan.(*Builder).WithModel`, `plan.WithLoadedModel` | one `plan.WithModel(m)` option accepted by `plan.New` and `plan.Load` |

`RunInfo` is defined as:

```go
type RunInfo struct {
	RunID, RootRunID, ToolUseID string
	Saga                        bool
	Identity                    Identity
}
func RunInfoFrom(ctx context.Context) (RunInfo, bool)
func (RunInfo) SubRun() string // the child run ID for this call; replaces SubRunID/RunScope
```

### Migration
- About 367 `New(` sites (36 non-test) are rewritten by script:
  - `agent.New(m, s, t1, t2)` becomes `a, err := agent.New(m, j, agent.WithTools(t1, t2))`.
  - Builder chains become options.
  - Tests use `agenttest.MustNew(t, m, j, opts...)`, a helper that calls `t.Fatal`.
- About 56 builder-method calls (18 non-test).
- `trace.Instrument` callers.
- The session, typed and saga internals that called `clone`.

### Rationale
Construction errors surface at construction, and the documented aliasing hazard disappears. An agent is safe to share because nothing can mutate it.

### Risks
- `New` returning an error adds boilerplate at 36 non-test sites. `agenttest.MustNew` covers tests.
- The `Spec` snapshot changes semantics for a tool whose `Spec()` changes over time. That is deliberate: #57 already made a call keep the safety it fired under.

---

## 4. Item 3: Journal format versioning

### New API
```go
const JournalFormat = "bide.journal.v1"

var ErrJournalVersion = fmt.Errorf("unsupported journal format: %w", ErrProtocol)

type JournalVersionError struct {
	RunID     string
	Found     string   // "" for an unversioned (pre-v1) journal
	Supported []string
}
func (e *JournalVersionError) Error() string
func (e *JournalVersionError) Unwrap() error { return ErrJournalVersion }

const StepHeader StepKind = "header"

// Format returns the journal format of runID ("" for a run with no entries).
func (j *Journal) Format(ctx context.Context, runID string) (string, error)
```

**The version record:**
- Key `@journal`. The `@` prefix is already reserved by #60.
- `Kind: StepHeader`, `Result: {"format":"bide.journal.v1"}`.
- It must be entry 0 of every journal the `Journal` type writes, which covers agent runs, sessions, flows, `Step`-only runs, govern quorum runs and audit ledger runs.
- The agent's `@run` header (item 1) is a separate, later record. `@journal` is owned by the journal layer and says nothing about what the run is.

### How it is written (the `Journal` type, item 4)
1. **First write.** Before the first write to a run in this `Journal` value, the journal calls `Store.Insert(runID, "@journal", headerBytes)`.
2. **Known runs.** A bounded per-`Journal` cache (LRU, 4096 run IDs) records runs whose header is known good, so later writes skip the extra round trip.
3. **Checks on the stored header:**
   - `Seq != 0` means the header landed after existing entries. The journal was unversioned, so the write returns `*JournalVersionError{Found: ""}`. The stray header is harmless, because the run is refused either way.
   - A `format` not in `Supported` returns `*JournalVersionError{Found: f}`.
4. **Ordering across writers.** Every writer confirms the header before inserting its own entry, and `Store` guarantees that seq order follows completion order (item 4, requirement A2). So the header precedes every other entry even with concurrent first writers.

### How it is read
- **`History`/`Records`** check entry 0 as they stream. An empty run is fine; anything else without a valid header is refused.
- **`Get`** checks the header with a point lookup (cached) before returning a record. A found record with no header is `*JournalVersionError`.
- **Replay:** `Replay(ctx, j, runID)` reads through `History`, so an old journal cannot be replayed into a fresh run.
- **Audit:**
  - The header is a salted record like any other, so it is leaf 0 of every journal Merkle tree. Every STH therefore commits to the journal format, and the leaf tag stays `bide.audit.journal-leaf.v1` (the leaf encoding is unchanged).
  - `EvidencePackage` v4 carries `journal_format` and the inclusion proof of leaf 0, so an offline verifier knows which key scheme names such as `tool:<enc id>` follow before it interprets them.
  - bide-audit refuses an unknown journal format with exit code 4 (section 9.4).

### Support policy (documented in docs/GUARANTEE.md)
- 1.x binaries read every 1.x journal format and write the newest.
- A new format adds a reader path keyed on `Format` and never reinterprets an old key.
- Pre-1.0: `v1` stays the only format until the 1.0 tag. Later pre-1.0 changes edit v1 in place and do not bump it (Q1).

### How durabletest checks it (the package is renamed storetest, item 4)
`storetest.Run` drives the store under test through `agent.NewJournal` as well as directly. It asserts:
- **(a)** The first `Journal.Do` on a fresh run leaves `@journal` at seq 0 with the current format.
- **(b)** Concurrent first writes from two `Journal` values over two handles on the same backend leave exactly one header, still at seq 0.
- **(c)** A header whose format is `bide.journal.v999`, inserted raw, makes `History`, `Get` and `Do` return `*JournalVersionError{Found: "bide.journal.v999"}`.
- **(d)** A run whose raw entries lack a header is refused with `Found: ""`.

### Replaces and migration
Nothing is replaced; this is new. Tests that assert exact `History` lengths or indices shift by one. That is about 40 test sites, and the index-based audit proof tests use `Prove(..., index+1)`. Fixtures holding recorded journals, if any, are regenerated.

### Rationale and risks
- **Rationale:** the check lives in the one layer every journal passes through, so no engine path can skip it.
- **Risk:** an extra round trip on the first write per run per process, which is negligible.
- **Risk:** proof indices shift by one. The audit tests catch this.

---

## 5. Item 4: Durable redesign (storage port versus journal semantics)

### New API: the port stores implement
```go
// Store is the byte-level journal port. Implementations are dumb: they never decode an entry.
type Store interface {
	// Insert adds data under (runID, name) if no entry has that name, and returns the stored
	// entry (this call's, or the one that was already there) and whether this call stored it.
	Insert(ctx context.Context, runID, name string, data []byte) (stored Entry, inserted bool, err error)
	// Get returns runID's entry named name.
	Get(ctx context.Context, runID, name string) (e Entry, found bool, err error)
	// Load yields runID's entries with Seq >= from, in ascending Seq.
	Load(ctx context.Context, runID string, from int) iter.Seq2[Entry, error]
}

type Entry struct {
	Seq  int
	Name string
	Data []byte
}

// Optional capabilities, found with Capability through wrappers (F1, retargeted).
type Lister interface {
	// Runs yields run IDs in ascending byte order, starting after `after` ("" for the first).
	Runs(ctx context.Context, after string) iter.Seq2[string, error]
}
type Leaser interface { /* AcquireLease, RenewLease, ReleaseLease: unchanged */ }

func Capability[T any](s Store) (T, bool) // follows Unwrap() Store; checks s itself first
```

### Exact atomicity requirements on `Store` (documented on the type and enforced by storetest)
- **A1, unique names, linearizable insert.** For each `(runID, name)` at most one entry ever exists. Concurrent `Insert` calls, from any number of processes sharing the backend, have exactly one winner. Every caller, and every later `Get` or `Load`, observes the winner's bytes.
- **A2, dense, real-time-ordered sequence.** Each run's entries have `Seq` values 0, 1, 2 and so on, with no gaps. If `Insert` A returned before `Insert` B was called, A's `Seq` is lower than B's. A losing insert consumes no `Seq`.
- **A3, durable before return.** `Insert` returns `inserted == true` only after the entry is committed. On any error, the entry is either absent or fully present with its final `Seq`; it is never partial. The caller may retry `Insert` with the same bytes, and a retry that finds its own earlier commit returns `inserted == false` with its own bytes.
- **A4, read-your-writes, monotone visibility.** After `Insert` returns, for either result, `Get` and `Load` from any process see the entry. Once visible, an entry stays visible, unchanged and at the same `Seq`.
- **A5, byte fidelity.** `Data` round-trips exactly (NUL, invalid UTF-8, any length the backend supports). The store never canonicalizes.
- **A6, immutability.** The interface has no update or delete. Retention would be a future optional capability (`Deleter`) at run granularity.
- **A7, context.** `Insert` honors `ctx`. Keeping a record after the caller's cancellation is the journal's job (it passes `context.WithoutCancel`), not the store's.
- **A8, iterator hygiene.** Breaking out of `Load` or `Runs` releases every resource (rows, transaction, connection) before the yield returns false. This matters for SQLite's single connection.

### New API: the semantics the agent package owns
```go
type Journal struct{ /* store, singleflight, header cache */ }

func NewJournal(s Store, opts ...JournalOption) (*Journal, error) // ErrConfig on nil
func (j *Journal) Store() Store

// Do returns runID's record named name, running fn and recording its record only if absent.
// fn runs at most once per (runID, name) within the process (singleflight); once fn has
// returned a record, it is recorded even if ctx was cancelled. Refuses engine-reserved names
// (every #60 prefix except "audit:", which package audit owns).
func (j *Journal) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error)
func (j *Journal) Get(ctx context.Context, runID, name string) (Record, bool, error)
func (j *Journal) History(ctx context.Context, runID string) ([]Record, error)
func (j *Journal) Records(ctx context.Context, runID string, from int) iter.Seq2[Record, error]
```

The `Journal` owns:
- memoization and in-process single-flight;
- naming and salting (`JournalEntry` becomes unexported);
- canonical encoding (`EncodeRecord` and `DecodeRecord` stay exported for audit and verifiers);
- detached recording;
- the header (item 3);
- the claim protocol. `ClaimAttempt` becomes the unexported `(*Journal).claim`, because plan lowers to `Step` (item 6) and needs no public claim.

**`Record` (review 7.4).**
- `Salt` and `Claim` become unexported fields with read accessors `Salt() []byte` and `ClaimID() string`, kept in the JSON encoding through `MarshalJSON`/`UnmarshalJSON`. A user-built `Record` cannot carry a salt or claim, and a decoded one keeps both.
- `AttemptedAt` becomes `AttemptedAtMillis`, with JSON key `attempted_at_ms` (review 9.6).
- New additive fields: `Finish FinishReason` on model records (item 5) and `ApproverAlg` (section 9.2).

### How at-most-once is preserved (the argument the PR must state and test)
1. **Claim.** `claim(runID, marker, rec)` draws 16 random bytes as a claim id and inserts `rec` (named, salted, claim set) with `Insert`. The driver has won if and only if the decoded stored entry carries its own claim id. This also covers a retried `Insert` whose first attempt committed (A3). By A1, exactly one driver's marker exists, and every driver sees the same winner.
2. **Effect.** A side effect runs only after a `claim` returns "won". By A3, the marker is durable before the effect starts.
3. **Crash between effect and result.** The resume reads the marker with `Get` and finds no result, so it halts with `OutcomeUnknown`, and the effect is not re-run. A4 guarantees another process sees the marker.
4. **Result recording.** `Journal.Do` inserts the result under `context.WithoutCancel`. An insert that races another driver loses by A1, and both return the one stored record, so the conversation stays single-valued (exact replay).
5. **The retry-safe probe.** #57's "probe with a failing `Do`" becomes `j.Get(runID, stepAttemptStep(name))`: a pure read, with no chance of a store recording a probe.
6. **Plan.** Plan nodes lower to `Step` (item 6), so plan inherits steps 1 to 5 exactly and does no journal reads of its own (no O(n²) `History` per node).

The only properties relied on are A1 to A4. Leases remain an efficiency and liveness mechanism, as documented today.

### Capabilities and wrappers
- F1's `Capability` is retargeted to `Store`, with `Unwrap() Store`.
- Journal writes are the base interface and so can never be reached around a wrapper. A wrapper that audits or traces writes always sees them; only optional capabilities (`Leaser`, `Lister`) are discovered through `Unwrap`.
- `Lease` without a `Leaser` becomes `ErrConfig` unless `WithoutLease()` is passed (review 2.3's "never a silent fallback", Q12).

### Migration

**`MemStore`**
- It implements `Store`, `Lister` and `Leaser` with a mutex, and is linearizable.
- `agent.NewMemStore()` still exists. `agenttest.NewJournal(t)` returns a MemStore-backed journal.

**store/sqlite**
- `Open(ctx, path, opts...)`, and `New(ctx, db *sql.DB, opts...)` (review 5.3).
- Table `bide_steps(run_id, seq, name, data)` with PK `(run_id, name)` and UNIQUE `(run_id, seq)`.
- `bide_schema_version`, which is checked at Open (review 8.3), and `WithTablePrefix`.
- `Insert` is `INSERT OR IGNORE` with the seq subquery in one statement (SQLite serializes writers), then a `Get` when no row was inserted.
- Singleflight moves out of the store into `Journal`.

**store/postgres**
- The same shape: `Open(ctx, dsn, opts...)` and `New(ctx, db *sql.DB, opts...)`.
- `leases` becomes `bide_leases`, plus `bide_schema_version`.
- `Insert` is the existing advisory-lock transaction with `ON CONFLICT DO NOTHING`, returning the entry.
- `Runs` uses a keyset cursor (`WHERE run_id > $1 ORDER BY run_id LIMIT 500`).

**audit.AuditedStore**
- It becomes `audit.NewAnchoredStore(inner agent.Store, s Signer, anchor Anchor, opts ...AnchoredOption) (*AnchoredStore, error)` and implements `Store` and `Unwrap() agent.Store`.
- It anchors after an `Insert` that inserted, by reading the run's entries with `Load`. Leaves commit to the stored bytes, which are exactly the canonical record encoding, so anchoring needs no decoding.

**Test stores**
- The 17 `Do` implementations, 5 of them non-test (MemStore, sqlite, postgres, AuditedStore, chaos), become `Store` wrappers.
- Crash-injection stores wrap `Insert`: "crash after insert" returns an error after the inner commit.
- `chaos/bide.go`'s `crashStore` likewise.

**Callers.** About 171 `Durable` references (102 non-test) become `*Journal`:
- `Step`, `Parallel`, `Approve`, `SubmitDecision`, `Signal`, `Enqueue`, `Ack`, `AnswerInterrupt`, `ResolveHalt`, `IsComplete`, `Lease`, `Recover`, `RecoverLoop`, `Replay`, `ReplayEvents`, `RenderMermaid`;
- audit `Prove*`, `Evidence`, `NewTreeHead`, `CertifyRun`, `Record*`;
- govern `Quorum`, plan `Flow.Run`, eval.

**Internal readers** switch to point lookups:
- `IsComplete` and `recoverable` use two `Get` calls.
- `quorumTally`'s tally record, `resolve`, `Interrupt`, `Await` and `waitUntil` use `Get`.
- `History` stays only where the whole journal is needed: loop resume, rollback and proofs.

**Unexported (from #60, review 11):** `ToolResultStep`, `SubRunID`, `IsSubRun`, `ApprovalTallyStep`, `JournalEntry`, `ClaimAttempt`, `SaltSize` (audit uses `len(r.Salt())`). `IsReservedStepName` stays exported.

### Rationale
- The five semantic duties the review lists (memoize, salt, detach, decode, atomic claim) now have one implementation and one test suite.
- Stores shrink to about 100 lines each, and new needs become additive capabilities.

### Risks
- **Mechanical blast radius.** This is the largest mechanical change, so it lands first, while few PRs are open.
- **Seq density.** A third-party store that cannot give dense sequences (some distributed KVs) cannot satisfy A2. That is documented as a hard requirement, because Merkle indices depend on it.
- **Visibility gap.** In-process singleflight moves from store to journal. Two `Journal` values over one store in one process no longer share it. That is safe (claims decide), but a retry-safe `fn` could run twice in-process. This is documented.

---

## 6. Item 5: Model call signature

### New API
```go
type ModelHandler func(ctx context.Context, call ModelCall) (ModelResponse, error)
type Middleware func(next ModelHandler) ModelHandler

// ModelCall is one model call as middleware sees it. It is a value: a middleware changes its
// own copy and passes it down, so concurrent sub-calls (Hedge) never alias.
type ModelCall struct {
	Request Request
	Model   Model           // where the base handler sends each request; middleware may retarget
	RunID   string          // "" outside an agent
	Turn    int             // the model turn's sequence number within the run
	Attempt int             // incremented by the base handler for each request it sends
	Sink    *Sink           // live stream sink; nil when nothing streams
	Hooks   []ModelCallHook // run by the base handler around every request it sends
}

type ModelResponse struct {
	Message Message
	Usage   Usage        // on error: what the failed request reported before failing
	Finish  FinishReason // normalized
	RawFinish string     // the provider's own value, for logs
}

type ModelCallHook struct {
	Before func(ctx context.Context, call ModelCall) error
	After  func(ctx context.Context, call ModelCall, a ModelAttempt)
}
type ModelAttempt struct {
	Response ModelResponse // Usage is what the request was billed
	Err      error
}

// Sink forwards a live call's events. Methods are nil-safe.
type Sink struct{ /* unexported */ }
func (s *Sink) Emit(ev Event)
func (s *Sink) Restart()                  // marks a new attempt: the consumer sees TurnRestarted
func (s *Sink) EmitResponse(r ModelResponse) // replays r as events (replaces EmitMessage)

type FinishReason string
const (
	FinishStop          FinishReason = "stop"
	FinishToolUse       FinishReason = "tool_use"
	FinishMaxTokens     FinishReason = "max_tokens"
	FinishContentFilter FinishReason = "content_filter"
	FinishOther         FinishReason = "other"
)
type Finish struct {
	Reason    FinishReason
	Raw       string
	Usage     Usage
	Discarded Usage // billed usage of requests the model itself made and discarded (Replay; zero for adapters)
}

// Describer is an optional Model capability (review 2.7).
type Describer interface{ Describe() ModelInfo }
type ModelInfo struct {
	Provider, Model string
	ResponseFormat  bool
}

// CallModel runs one call through mw and the base handler outside an agent (plan model
// nodes, tests), with the same hooks, checks, and Finish handling the agent uses.
func CallModel(ctx context.Context, m Model, req Request, mw ...Middleware) (ModelResponse, error)
```

**The base handler**, built by `Agent` or `CallModel`:
1. For each request it sends, it runs `Hooks[i].Before` in order, increments `Attempt`, and calls `Sink.Restart()`.
2. It sends to `call.Model`, streaming into `Sink` when the sink is non-nil.
3. It drains into a `ModelResponse` with the normalized `Finish`.
4. It runs `Hooks[i].After`. A non-zero `Finish.Discarded` is reported to `After` as a separate billed attempt, which is how `Replay` replays discarded spend without ctx.

**The agent's own spend meter** is the first hook, as today. The agent, not middleware, computes `Record.DiscardedUsage` and `Result.Spend`. Middleware never does spend arithmetic.

**Journal.** `Record.Finish` is recorded on model records, so `Replay` reproduces the provider's reason exactly, instead of deriving `stop`/`tool_use` from the message.

**Adapters** normalize:
- **Anthropic:** `end_turn` and `stop_sequence` map to stop; `tool_use` to tool_use; `max_tokens` to max_tokens; `refusal` to content_filter.
- **OpenAI:** `stop`, `tool_calls` (and `function_call`), `length`, `content_filter`.
- **Gemini:** the existing `mapFinishReason`, retargeted.
- **ScriptedModel:** emits the constants.
- A new modeltest check asserts every adapter's mapping table.

**Loop policy.** The `ib-model` follow-up sets it: a live turn ending in `FinishMaxTokens` or `FinishContentFilter` with no tool calls is not an answer. With the normalized reason it becomes a one-line check in the loop.

### Replaces
`ModelHandler func(ctx, Request) (Message, Usage, error)`, `ModelCallHook{Before(ctx); After(Usage)}`, `WithModelCallHook(ctx) (ctx, ok)`, `WithModel(ctx, m) (ctx, ok)`, `DetachModelSink`, `EmitMessage`, the ctx keys `modelSinkKey`, `modelHooksKey` and `modelOverrideKey`, the untyped `Finish.Reason string`, `trace.WithModel` and `trace.WithSystem`.

### How middleware adapts
- **Retry:** loops `next(ctx, call)`. It has no ctx tricks. The base handler's `Restart` produces `TurnRestarted`.
- **Hedge:**

  ```go
  sink := call.Sink; call.Sink = nil
  // Backup i: c := call; c.Model = backups[i]; go next(hctx, c)
  // On the first success: sink.EmitResponse(r); return r, nil
  ```

  This removes the `ok == false` fallback that called `b.Stream` directly. Every target now goes through the same chain, so hooks and checks always run.
- **Cost:** appends `ModelCallHook{After: ...}` to `call.Hooks` (with `slices.Clip`) and counts the answer from its own return.
  - `CostMeter` accessors collapse to `Snapshot() CostSnapshot{Answer, Spend Usage; AnswerUSD, SpendUSD float64}` (review 7.6).
- **RateLimit:** appends `ModelCallHook{Before: func(ctx, _) error { return r.wait(ctx) }}`, so every request, retries and hedges included, takes a token.
- **Response checks:** `checkToolUseIDs` and `Usage.Validate` stay in the base handler and after the chain, as #55 placed them.
- **trace.Model:** reads `call.Model.(agent.Describer)` for `gen_ai.system` and the model name, and `resp.Finish` for `gen_ai.response.finish_reasons`.
- **RunTyped's `injectSystem`:** becomes the agent's system prompt composition (an internal run setting), not a middleware.

### Migration
- The 48 `ModelHandler` references (15 non-test): every middleware in `middleware/` and `trace/`, typed.go, retrieval.go (now an option, item 2), and the tests' custom middleware.
- The three adapters and modeltest for `Finish`.

### Rationale
- Every datum that was smuggled through ctx is now a field.
- Hooks always run, because the base handler is always the agent's or `CallModel`'s.
- The `WithModel` clash disappears: `agent.WithModel` no longer exists, and adapter and plan `WithModel` are ordinary options.

### Risks
- Value-copying `ModelCall` copies the `Request` header per middleware, but not the message slices. That is cheap.
- A middleware that forgets to clear `Sink` while fanning out streams losers' tokens. Hedge is the only in-repo fan-out, and a test covers it. `Sink` doc comments state the rule.

---

## 7. Item 6: Pause contract, halts, and plan

### New API
```go
// Pause is a durable stop that is not a failure: the run is waiting on something outside it.
// The set is sealed; new pause types may be added in minor releases.
type Pause interface {
	error
	Paused() RunRef
	pause()
}
type RunRef struct{ RunID, RootRunID string }

func IsPause(err error) bool
func AsPause(err error) (Pause, bool)

type ApprovalPending  struct{ RunRef; ToolUseID, ToolName string; Args json.RawMessage; Quorum *ApprovalTally }
type InterruptPending struct{ RunRef; Name string; Prompt any }
type SignalPending    struct{ RunRef; Name string }            // Await, AwaitFor, Receive
type TimerPending     struct{ RunRef; Name string; FireAt time.Time }
type OutcomeUnknown   struct{ RunRef; Op OpRef; AttemptedAt time.Time }

type OpKind string
const ( OpTool OpKind = "tool"; OpStep OpKind = "step" )
type OpRef struct {
	Kind     OpKind
	ID       string // tool-use ID, or step name
	ToolName string // OpTool only
}

// Halt resolution: one function, kind carried by the reference.
type HaltRef struct {
	RunID string
	Op    OpRef
}
func (e *OutcomeUnknown) Ref() HaltRef
type Outcome struct {
	Result   any
	IsError  bool
	Evidence any // non-nil: recorded as reconciled, with this evidence
}
func ResolveHalt(ctx context.Context, j *Journal, ref HaltRef, out Outcome, opts ...ResolveOption) error
// ResolveOption: WithMinHaltAge(d), WithClock(now). HaltTooYoung is unchanged.

// Verbs (review 6.2)
func Approve(ctx context.Context, j *Journal, runID, toolUseID string, approved bool) error
func SubmitDecision(ctx context.Context, j *Journal, d Decision, opts ...DecisionOption) error
type Decision struct {
	RunID, ToolUseID, ApproverID string
	Approved                     bool
	Alg                          Alg
	Signature                    []byte
}
func AnswerInterrupt[T any](ctx context.Context, j *Journal, runID, name string, v T) error
func Signal[T any](ctx context.Context, j *Journal, runID, name string, payload T) error
func Enqueue[T any](ctx context.Context, j *Journal, runID, name, key string, payload T) error
func Ack(ctx context.Context, j *Journal, runID, name, key string) error

// Waker (item 9 folded here, since it owns pause.go)
type Waker interface{ Schedule(ctx context.Context, w Wake) error }
type Wake struct {
	RunID, RootRunID, Name string
	FireAt                 time.Time
}
func ResumeFunc(a *Agent, opts ...RunOption) func(ctx context.Context, runID string) error
```

**Recovery.**
- `Recover(ctx, j, resume, opts ...RecoverOption)` and `RecoverLoop(ctx, j, resume, opts ...RecoverLoopOption)` treat `IsPause(err)` as success.
- `MemWaker.Fire` also uses `IsPause`, so user code no longer needs the five `errors.As` checks. Adding a pause type later cannot break a user's classification.

**Waker failure.** `Sleep` and `WaitUntil` journal the fire time first. If `Schedule` then fails:
- They return an error that wraps `ErrStorage` and the unexported `errWakeNotScheduled` marker.
- The tool loop treats that marker like a pause for recording purposes: it records nothing and propagates. The run fails with a genuine error, not a pause, so `RecoverLoop` reports it and retries.
- The retry re-enters the retry-safe tool, finds the journaled timer, and calls `Schedule` again. A failed schedule can no longer leave a run asleep forever.

**Plan lowering.**
- `runNodeKeyed` becomes `agent.Step(ctx, j, runID, key, body, StepSafety(nodeSafety))`, where `key` is `<node>` or `iter:<n>:<node>`.
- A node's attempt marker becomes `attempt:step:<key>` instead of `attempt:<key>`.
- `HaltAmbiguous` is deleted. A flow halt is `*OutcomeUnknown{Op: OpRef{Kind: OpStep, ID: key}}`, cleared by `ResolveHalt(ctx, j, halt.Ref(), out)`.
- `RecoverLoop` treats it as a pause.
- `plan/conformance.go` reads the new marker key.
- F2 (ctx for step bodies) is a prerequisite: `Step` passes its ctx, `NextOnceKey` scope included, into the body.
- The `@run` header for flows records the flow's topology digest, so resuming a run with a changed flow is `ErrConfig` (Q18, recommended).

### Replaces
`PendingApproval`, `Interrupted`, `Awaiting`, `Sleeping`, `ResumeHalt`, `plan.HaltAmbiguous`, the unexported `isPause` and its callers' ad-hoc lists, `ResolveStepHalt` (#60), `ResolveHalt(..., toolUseID, result any, isError bool)`, `WithEvidence`, `WithNow`, `Resume[T]`, `ApproveAs` (seven positional parameters), agent's channel `Send`, and `Waker.Schedule(runID, name, fireAt)`.

### Migration
- About 224 references to the pause type names (104 non-test).
- The `errors.As` sites in toolexec, subagent, session, recovery and MemWaker collapse to `IsPause` or `AsPause`.
- Examples: interrupt, approval, signals, recover, webhook, quorum.
- plan/flow.go and conformance.go.
- govern, which uses `ResolveHalt`.
- The `ResolveStepHalt` call sites from #60.

### Rationale
A sealed exported interface is the extension point users need. One resolution function whose kind is carried by the reference removes #60's two-function split. It also makes plan halts resolvable, which they are not today.

### Risks
- **Plan journals change keys.** This is covered by the v1 policy (no old journals are read).
- **`OpRef` value comparison** in user switch statements: document that the fields are the identity.

---

## 8. Item 7: Tool interface

### New API
```go
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

type ToolSpec struct {
	Name        string
	Title       string          // optional human label (MCP title)
	Description string
	Input       json.RawMessage // provider-neutral JSON Schema; type "object"
	Output      json.RawMessage // optional result schema (MCP outputSchema)
	Safety      Safety
	Timeout     time.Duration   // 0: none; enforced by the agent (below)
}

// Compensator stays the optional-interface pattern.
type Compensator interface {
	Compensate(ctx context.Context, args, result json.RawMessage) error
}

// Constructors
func Func[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) (Tool, error)
func MustFunc[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) Tool
func CompensatedFunc[In, Out any](name, description string, do func(context.Context, In) (Out, error), undo func(context.Context, In, Out) error, opts ...ToolOption) (Tool, error)
func MustCompensatedFunc[In, Out any](...) Tool
func SubAgent(name, description string, sub *Agent, opts ...ToolOption) (Tool, error)
func RetrievalTool(name, description string, r Retriever, k int, opts ...ToolOption) (Tool, error)

// ToolOptions: WithSafety(Safety) (also a StepOption), WithTimeout(d), WithTitle(s), WithOutputSchema(raw)

// Tool middleware
type ToolCall struct {
	Use   ToolUse  // middleware may rewrite Use.Args; the base handler journals the args it passes
	Spec  ToolSpec // the registered tool's spec
	RunID string
	/* unexported: redactor */
}
func (c ToolCall) ErrorText(err error) string // what the agent journals for err
type ToolHandler func(ctx context.Context, call ToolCall) (json.RawMessage, error)
type ToolMiddleware func(next ToolHandler) ToolHandler

// Request carries specs, not tools.
type Request struct {
	Messages       []Message
	Tools          []ToolSpec
	Sampling       Sampling
	ResponseFormat *ResponseFormat
	ToolChoice     *ToolChoice // ToolChoice.Mode is ToolChoiceMode
}
```

**`Timeout` is enforced by the agent.** The tool runs under `context.WithTimeout(sctx, Spec.Timeout)`. A call that returns after the deadline has passed takes the existing "cancelled before it reported back" path: nothing is recorded, a non-retry-safe tool's marker makes the resume halt, and a retry-safe tool re-runs. This is the same outcome-unknown semantics #57 gives MCP.

**The at-most-once guard** (`ErrToolReinvoked`) keys on the snapshotted `Spec.Safety`, never on the middleware-visible copy.

### Mapping of existing tools

| Implementation | `Spec` |
|---|---|
| `Func` | `Input` from `schema.For[In]`, which errors instead of panicking; `Safety` from `WithSafety` (default: side effect); `Output` from `schema.For[Out]` when it succeeds (best effort, not an error) |
| `CompensatedFunc` | as `Func`, plus `Compensator` |
| `SubAgent` | `Input` is the `{task}` schema; `Safety{Idempotent: true}` is fixed, because re-running resumes the sub-run. `WithSafety` is refused with `ErrConfig`, since the sub-run's own markers carry safety. `Timeout` allowed |
| `RetrievalTool` | `Safety{ReadOnly: true}`; `k < 1` or an empty name is `ErrConfig` (formerly panics) |
| RunTyped `answerTool` | `Name: final_answer`, `ReadOnly`, `Input` is T's schema |
| MCP (`mcp.Tools`) | `Name`; `Title` (MCP title); `Description`; `Input` (inputSchema); `Output` (outputSchema); `Safety` (`WithSafety` override, else trusted annotations, else side effect); `Timeout` from `WithCallTimeout`. The adapter no longer applies the timeout itself. `MaxResultBytes` and `MaxDescriptionBytes` stay MCP-local `ToolsOption`s, because they guard the protocol, not the loop |
| govern `EventTool`, `AttestedEventTool`, `FederatedEventTool` | `govern.EventTool(gov, EventToolConfig{Name, Description, Event, PolicyDigest, Registry string; Safety agent.Safety})`, which removes the adjacent-string hazard (review 7.1) |
| audit `AttenuatingSubAgent` | `AttenuatingSubAgent(name, description string, sub *agent.Agent, cfg AttenuationConfig) (agent.Tool, error)`, where `cfg` holds the narrow func and rules (review 7.1) |
| chaos and modeltest tools | `Spec()` literals |

**Middleware:**
- `middleware.ToolRetry` and `ToolCache` read `call.Spec.Safety`.
- `ErrorSummary` and `trace.Tool` use `call.ErrorText(err)`.

**Pending follow-up hook (saga arguments).** The base tool handler already knows the args it actually passes, `call.Use.Args` after middleware. The saga follow-up records those on the result record, so compensation can use the accepted arguments.

### Replaces
`Tool{Name, Description, ArgsSchema, Safety, Call}`, `ToolHandler func(ctx, ToolUse)`, `ToolSafety(ctx)`, `WithToolSafety`, `ToolErrorText(ctx, ...)`, the panicking `Func`, `Request.Tools []Tool`, and `ToolChoice.Mode string`.

### Migration
- About 120 `Func`/`CompensatedFunc` sites (19 non-test), rewritten by script: `Func(n, d, s, fn)` becomes `MustFunc(n, d, fn, agent.WithSafety(s))` in tests and the `(Tool, error)` form in examples and library code.
- 26 `Name() string` implementers (11 non-test).
- 32 `ToolHandler` sites (11 non-test).
- Adapters read `req.Tools[i].Name` etc. in place of methods.

### Rationale
Struct fields are additive, so new static properties such as title, output schema and timeouts no longer break implementers.

### Risks
Snapshotting `Spec()` at `New` means a tool that computes its spec lazily must be deterministic. This is documented.

---

## 9. Items 8 and 9

### 9.1 Proof artifacts (item 8)

**Every top-level artifact carries `format`,** checked before anything else. An unknown format returns `ErrUnsupportedFormat`, and bide-audit exits 4.

| Artifact | Format | Change |
|---|---|---|
| `SignedTreeHead` | `bide.audit.sth.v4` | JSON `format` mirrors the signed domain tag; `Alg` typed; `Timestamp` becomes `TimestampNanos` (`timestamp_ns`, review 9.6). The canonical signed bytes are unchanged |
| `ProofBundle` | `bide.audit.proof.v1` | new field |
| `AbsenceBundle` | `bide.audit.absence-proof.v1` | new field |
| `RunCertificate` | `bide.audit.run-cert.v1` | new field |
| `CurrentGrantProof` | `bide.audit.current-grant-proof.v1` | new field |
| `SignedGrant` | `bide.audit.grant.v1` | `Grant.NotAfter` becomes `NotAfterUnix` (`not_after_unix`) |
| `AnchorEntry` | `bide.audit.anchor-entry.v1` | new field |
| `EvidencePackage` | `bide.audit.evidence.v4` | `public_key_hex` replaced by `{"alg", "public_key"}`; adds `journal_format` and the leaf-0 header proof |
| plan config JSON | `"version": 1` | snake_case keys (`loop_max`, `in_types`, `loop_back`); review 9.4 |
| eval `Report` | `bide.eval.report.v1` | new field (NICE, cheap) |

`Inclusion`, `Consistency` and `EventInclusion` are nested only, so they get no format. F3 gives them snake_case.

**Compatibility policy (docs/GUARANTEE.md):** producers emit the current format, verifiers accept the current and the previous format within a major, and one casing (snake_case) applies everywhere.

**Signature agility:**
```go
type Alg string // in package agent (audit aliases it: type Alg = agent.Alg)
const ( AlgEd25519 Alg = "ed25519"; AlgMLDSA65 Alg = "ml-dsa-65"; AlgHybrid Alg = "ed25519+ml-dsa-65" )

type Signer interface {
	Alg() Alg
	PublicKey() []byte
	Sign(msg []byte) ([]byte, error)
}
type Verifier interface {
	Alg() Alg
	PublicKey() []byte
	Verify(msg, sig []byte) bool
}
func NewEd25519Signer(priv ed25519.PrivateKey) (Signer, error)
func NewEd25519Verifier(pub ed25519.PublicKey) (Verifier, error)
```
`agent.ApproverVerifier` gains `Alg()` and matches `audit.Verifier` structurally. `Record.ApproverAlg` (`approver_alg`) journals the approver's scheme, so a verifier can tell schemes apart after key rotation.

Every ed25519-only entry point takes `Signer` or `Verifier` instead:
- `SignTreeHead(th, Signer) (SignedTreeHead, error)` replaces both `SignTreeHead(th, priv)` and `SignTreeHeadWith`. `Sign` and `VerifySignature` are removed.
- `NewAnchoredStore(inner, Signer, anchor)`.
- `Evidence(ctx, j, runID, s Signer, opts...)`.
- `(*EvidencePackage).Seal(Signer)`.
- `(*EvidencePackage).Verify(v Verifier, opts...)`. It requires `v.PublicKey()` to equal the embedded key and never trusts the embedded key on its own.
- `CertifyRun(ctx, j, runID, sth, spec RunCertSpec{ApprovedPolicies []string; Signer Signer; TimestampNanos int64})`, which drops the two trailing positional parameters (review 7.1).
- `VerifyRun(cert, approved, v Verifier)`.
- `SignAbsenceRoot(..., s Signer)`.
- `VerifyCurrentGrant(..., logVerifier Verifier)`.
- `VerifyApprovals(..., logVerifier Verifier)`.
- `verify.TreeHead(sth, v Verifier)`.

**Migration:** about 75 ed25519 sites (43 non-test), the proof-carrying-run, compliance and authority examples, and cmd/bide-audit key loading (`-alg`, with a default of ed25519 for PEM or hex input).

**Risk:** the evidence v4 cutover. Verifiers accept v3 during the pre-1.0 window only if Q1 says so. Recommendation: accept v3 through one release, then drop it.

### 9.2 Remaining MUST items

**Waker `Schedule(ctx, Wake) error`.** In section 7.

**govern and gsm (review 8.1).**
- Move `govern` into its own module, `github.com/bide-ai/bide/govern`. It stays v0 while gsm is v0.
- The root go.mod drops gsm.
- The root-module tests that import govern move into a new test-only module, `integration/` (`github.com/bide-ai/bide/integration`), which requires root, govern and the stores:
  - agent/e2e_convergence_test.go
  - audit/runcert_test.go
  - cmd/bide-audit's quorum_names, main, leaf_strict, security and checker tests where they use govern
- The architecture tests drop the govern allow-entry.
- go.work adds `./govern` and `./integration`.
- No Go API change. Wrapping gsm's `Machine`, `State` and `Report` would mean re-exposing gsm's whole definition surface under new names, with no stability gain.

**`With*` naming.** In section 1.1 and section 3.

**Exit codes for bide-audit (review 9.5):**

| Code | Meaning |
|---|---|
| 0 | every requested check verified |
| 1 | input read and understood; at least one check did not verify (tampered, mismatch, policy violated) |
| 2 | usage: bad flags or arguments; nothing was read |
| 3 | no verdict: an external checker failed or timed out, or an internal error |
| 4 | input unreadable or unusable: missing file, permission, invalid JSON, unknown or unsupported `format`, wrong artifact type for the role |

- **Precedence** when checks disagree: 2 is exclusive; otherwise 1 wins over 4, 4 over 3, and 3 over 0. A script must never read "tampered" as a lesser error.
- **Classification** is by `errors.Is` against `audit.ErrNotVerified` (1), `audit.ErrMalformed`, `ErrUnsupportedFormat` and `fs` errors (4), and the checker's error (3), through one `exitFor(err)` function. The 60 `os.Exit(1)` sites become `return err` to a single `main` that maps the error.
- **Also added:** `-version` (tool version plus supported formats) and `-json`, which emits `EvidenceReport` or `RunVerification`.

### 9.3 Library panics that become errors

| Function | Today | New |
|---|---|---|
| `agent.New` | nil model or store; nil tool deref | `(*Agent, error)` |
| `agent.Func`, `CompensatedFunc` | `schema.For` fails | `(Tool, error)` plus `Must*` |
| `agent.RetrievalTool` | `k < 1`, empty name | `(Tool, error)` |
| `agent.WithRetrieval` | `k < 1` | the Option's apply error, returned by `New` |
| `agent.SubAgent` | ignores the `schema.For` error; nil sub derefs at call time | `(Tool, error)` |
| `audit.NewAuditedStore` | nil inner or key accepted, fails later | `NewAnchoredStore(...) (*AnchoredStore, error)` |
| `eval.Matches(pattern)` | `regexp.MustCompile` | `Matches(re *regexp.Regexp) Metric`; `eval.Run` returns `ErrConfig` for a nil re |
| `eval.AgentRunner` | panics on crypto/rand | branch deleted: since Go 1.24, `crypto/rand.Read` never returns an error. The same dead branches in `JournalEntry`, `claim` and `newClaim` are removed |
| `plan` wiring endpoint | a non-Handle endpoint, unreachable by construction | kept as an internal-invariant panic, documented |
| `trace.end` | re-raises a user panic | kept (correct) |

### 9.4 SHOULD items and where each lands

| Review | Fix | PR |
|---|---|---|
| 2.6 Lister | cursor iterator; `IsComplete` via `Get` | P6 |
| 2.7 identity | `Describer` / `ModelInfo` | P7 |
| 2.8 Alg | typed `Alg`; `approver_alg` recorded | P10, P11 |
| 3.2 audit errors | `ErrMalformed`, `ErrNotVerified`, `ErrUnsupportedFormat`; every boolean verifier becomes `func(...) error` (nil verified; `ErrNotVerified` otherwise); report verifiers become `(Report, error)` with the same sentinels | P11 |
| 4 panics | table above | P4, P6, P12, P13 |
| 5.3 Open(ctx) | sqlite/postgres `Open(ctx, ...)`, `New(ctx, *sql.DB)`; govern logs likewise | P6, P2 |
| 5.4 GovernanceHeld ctx | adds ctx | P4 |
| 6.2 verbs | section 7 | P10 |
| 6.3 NICE items taken because they are cheap now | `RunEvent`/`RunStream`; `audit.Record` renamed `RecordStream`; `plan.Retryable` alias dropped | P14, P9, P5 |
| 6.4 typed strings | `FinishReason`, `ToolChoiceMode`, `Alg`, `EvidenceKind`, `TopologyNodeKind`, `MetricDirection` | P7, P11, P5, P4 |
| 7.1 long lists | `Decision`, `Outcome`, `RunCertSpec`, `AttenuationConfig`, `EventToolConfig`; `Parallel(..., opts ...ParallelOption)`; `Func` options; `SendOnce` input is a Message | P10, P11, P12, P13 |
| 7.2 provider kit | `ClassifyHTTPError`, `ClassifyStreamError`, `NewSSEScanner`, `SSEPayload`, `SSEReadError`, `MaxSSELine`, `ParseRetryAfter`, `EncodeToolResultOr` and `JSONToolResultCodec` move to public `model/provider`. `agent` keeps `APIError`, `RateLimited`, the sentinels, `Stream`, `NewStream`, `NewStreamFunc`, `Emit` and the events | P7 |
| 7.3 exported internals | unexport `ClaimAttempt`, `JournalEntry`, `ToolResultStep`, `SubRunID`, `IsSubRun`, `ApprovalTallyStep`, `SaltSize`, and the `Reason*` constants (become a typed `DecisionReason`). Keep `TallyApprovals`, `ProjectEvents`, `FindToolCall`, `IsApprovalDecision`, `DecisionCheck` as the documented offline-verification API (Q10) | P6, P16 |
| 7.4 Record | unexported salt and claim with accessors | P6 |
| 7.5 test doubles | `ScriptedModel` and friends move to `agent/agenttest` | P16 |
| 7.6 cost | `CostMeter.Snapshot`; hook `After(ctx, call, ModelAttempt)` | P9 |
| 8.3 store schema | `bide_` prefix, schema version row, `WithTablePrefix` | P6, P2 (govern logs) |
| 9.4 plan config | version and snake_case | P5 |
| 9.6 units | `AttemptedAtMillis`, `TimestampNanos`, `NotAfterUnix`; one `func() time.Time` clock | P6, P11 |
| 10.1 to 10.5 docs | section 9.5 | P8, P16 |
| 11 #60 key constructors | unexported; `ValidateRunID` exported | P6, P14 |

### 9.5 Missing doc comments policy
- **Coverage.** Every exported identifier has a doc comment that starts with its name: types, functions, methods (including `Error`, `Unwrap`, `MarshalJSON`, `UnmarshalJSON`, `String`), consts, vars, option constructors, and struct fields whose meaning is not self-evident from name and type. Every package has a package comment.
- **Content.** A comment documents the contract: what the identifier guarantees, its units, and when it errors. It contains no competitor names, no PR or agent references ("Agent C"), and no mention of internal file names.
- **Examples.** Code that shows a call is an `Example` test (compiled and run), not a comment block. Doc blocks may show short fragments only when every identifier in them exists.
- **Stability notes.** Rendering output (`RenderMermaid`, `plan.Flow.RenderMermaid`) is documented as unstable text. `RunEvent` documents that new event types may be added, so consumers keep a `default` case.
- **Enforcement.** An in-repo analyzer, `internal/tools/doccheck` (go/analysis, about 150 lines, no new dependency), runs in CI over every module.
  - It lands in P8 with an allowlist of today's gaps.
  - Each later PR must not add allowlist entries.
  - P16 empties the allowlist.

---

## 10. Implementation plan

Ownership rule: within a wave, no two PRs edit the same file. Mechanical call-site rewrites ship with the script that produced them (in the PR description, or in `internal/tools/migrate`), so a rebase reruns the script instead of hand-merging.

### Wave 0: prerequisites (in flight; merge first, in this order)
1. #57, then #60, which rebases on #57 (both edit agent/store.go and saga.go).
2. F1 (Capability/Unwrap), F2 (plan step ctx), F3 (audit snake_case). These are independent of each other.
3. `ib-model`, `ib-plan`, `ib-audit`, `ib-govern`, `journal-completeness`.

### Wave 1: split and independent packages (all parallel)

**P1. agent file split (S). Pure moves, no code changes.**
- **Files:**
  - agent.go splits into agent.go (type and construction), loop.go (`run`), toolexec.go (the tool-execution block and `toolHandler`), generate.go (`generate`/`send`/`turnSink`) and runctx.go (ctx keys and accessors).
  - saga.go splits into saga.go (`runSaga`/`rollback`) and compensate.go (`Compensator`, `CompensatedFunc`, `decodeRecordedArgs`).
  - recovery.go splits into recovery.go, lease.go and replay.go.
  - store.go splits into record.go, step.go and halt.go (`ResolveHalt`, `Approve`).
- **Tests prove:** `go test ./...` is unchanged and `go doc -all ./agent` output is byte-identical.

**P2. govern module and integration module (M).**
- **Files:** go.mod, go.sum, go.work, govern/go.mod (new), govern/*.go, the govern/*log go.mod requires, the new integration/ module (moved tests), agent/architecture_test.go, plan/architecture_test.go, and the CI workflow.
- **Tests prove:** the root module builds with `GOWORK=off` and without gsm in `go mod graph`; every moved test still runs.

**P3. bide-audit exit codes, `-version`, `-json` (M).**
- **Files:** cmd/bide-audit/main.go and its tests (not the govern-dependent tests, which P2 moves).
- **Tests prove:** a table of (input, expected code) covering each code and the precedence cases (tampered plus unreadable gives 1; checker failure plus unreadable gives 4).

**P4. eval (S).**
- **Files:** eval/eval.go, eval/stats.go, and tests.
- **Tests prove:** `Matches(nil)` gives `ErrConfig` from `Run`; the `GovernanceHeld` ctx is passed; the report `format` is present.

**P5. plan config v1 (S to M).**
- **Files:** plan/config.go, plan/topology.go (`TopologyNodeKind`), plan/builder.go (drop the `Retryable` alias), and tests.
- **Tests prove:** a v1 config round-trips; a missing or unknown version is refused; the camelCase keys are refused.

### Wave 2: foundations (parallel)

**P6. Store/Journal split and journal version header (XL: about 1800 lines of semantics plus the mechanical rewrite of about 470 `NewMemStore` and 170 `Durable` sites).**
- **Files:**
  - agent: journal.go (new), memstore.go (new), record.go, step.go, halt.go (signature), keys.go (`@journal`, unexports), errors.go (`ErrJournalVersion`, `JournalVersionError`), recovery.go, lease.go (Capability over Store, `WithoutLease`, `LeaseOption`/`RecoverOption`/`RecoverLoopOption` split), replay.go (read through the journal).
  - agent/durabletest becomes agent/storetest; new agent/agenttest (`NewJournal`).
  - store/sqlite/*, store/postgres/*, audit/audited_store.go (becomes the anchored store), chaos/bide.go.
  - The mechanical `Durable` to `*Journal` change in agent/*.go, audit/*.go, plan/*.go, eval, cmd, examples and benchmarks.
- **Tests prove:**
  - `storetest.Run` passes for MemStore, sqlite and postgres: A1 with 64 goroutines times 3 handles; A2 density and real-time order; A5 bytes; A8 break-early followed by an `Insert` on SQLite.
  - The header checks (a) to (d) from item 3.
  - Every existing DST and crash test (dst_test, step_crash_test, overlap_test, mofn_dst_test, the plan flow_dst and overlap tests) passes unchanged in intent.
  - `StepAttemptSafety` uses `Get` and records nothing.
  - An `AnchoredStore` over a leasing store keeps the lease, which reuses F1's tests.
  - `Lease` without a `Leaser` is `ErrConfig` and passes with `WithoutLease`.
  - Plan does zero `History` calls per node, checked with a counting store.

**P7. Adapters: FinishReason, provider kit, Describer, typed ToolChoiceMode (M to L).**
- **Files:** agent/model.go, agent/provider_http.go (deleted), model/provider/* (new), model/anthropic|openai|gemini/*, model/modeltest/*, model/internal/toolcfg.
- **Tests prove:** a per-adapter reason mapping table (modeltest `FinishReasons`); `Describe()` values; the moved helpers' tests pass in their new package; no core package imports model/provider (architecture test).

**P8. doccheck analyzer with allowlist (S).**
- **Files:** internal/tools/doccheck/*, CI workflow, .doccheck-allow.
- **Tests prove:** analyzer unit tests with golden packages.

### Wave 3: model call and pause (parallel)

**P9. ModelCall/ModelResponse (L).**
- **Files:** agent/generate.go, agent/modelcall.go, agent/replay.go (`Finish.Discarded`), agent/record.go (`Record.Finish`), agent/retrieval.go (middleware part retargeted), agent/typed.go (`injectSystem` removal), middleware/{retry,hedge,cost,ratelimit,middleware}.go, trace/trace.go (the Model half), and tests.
- **Tests prove:**
  - Hooks run exactly once per sent request under Retry, under Hedge (every target), and in a nested Retry(Hedge); the spend meter equals the sum of billed usage.
  - The Hedge winner's tokens alone reach `RunStream`, with `TurnRestarted` semantics unchanged (reuse #55's tests).
  - `Replay` reproduces `Result.Spend` and `Finish` exactly.
  - `CallModel` runs hooks outside an agent.
  - No `context.WithValue` remains in generate.go or modelcall.go (AST test).

**P10. Pause contract, Waker, halt unification, verbs, plan lowering (L).**
- **Files:** agent/pause.go, awaitfor.go, channel.go, approval.go (`ApprovalPending`, `SubmitDecision`, `Alg`), halt.go, recovery.go (`IsPause` use), toolexec.go (`errors.As` sites become `IsPause`), session.go (error docs only), plan/flow.go, plan/conformance.go, plan/builder.go (docs), and the examples interrupt, approval, signals, recover, webhook.
- **Tests prove:**
  - The pause set is sealed (a compile-time test that an outside type cannot satisfy `Pause`).
  - `RecoverLoop` does not re-drive a halted flow as a failure.
  - `ResolveHalt` clears a flow node's halt and the flow resumes, running the body zero more times.
  - A step name that only the other kind attempted is refused, keeping #60's guarantee.
  - A failed `Waker.Schedule` fails the run, and a later pass reschedules it.
  - The plan conformance suite passes with the new marker keys.

### Wave 4: tools and proofs (parallel)

**P11. Proof artifacts (L).**
- **Files:** audit/* (not delegate.go or audited_store.go), audit/verify/*, cmd/bide-audit/main.go, examples proof-carrying-run, compliance, authority, earned-authority.
- **Tests prove:**
  - Every artifact round-trips with `format`; the previous format is accepted and an unknown one gives `ErrUnsupportedFormat`.
  - An ML-DSA and a hybrid signer work end to end through Evidence, CertifyRun, anchored store and CLI.
  - Evidence refuses a verifier whose key differs from the embedded one.
  - The CLI exit table is extended with format cases (4).

**P12. Tool interface (L plus mechanical: about 120 Func sites).**
- **Files:** agent/tool.go, tool_middleware.go, toolexec.go, subagent.go, retrieval.go (tool part), typed.go (`answerTool`), compensate.go, model/{anthropic,openai,gemini} (spec reads), mcp/*, middleware/tool.go, middleware/error_summary.go, trace/trace.go (the Tool half), audit/delegate.go, govern (in its module), chaos, modeltest, examples.
- **Tests prove:**
  - `Spec.Timeout` expiry halts a side-effecting tool on resume and re-runs a retry-safe one.
  - The at-most-once guard uses the snapshot, not a middleware-rewritten spec.
  - The MCP mapping table, including `Output`.
  - `Func` with an undescribable `In` returns `ErrConfig`.
  - `SubAgent` with `WithSafety` gives `ErrConfig`.

### Wave 5: construction, plus follow-up A (parallel)

**P13. Agent construction and options (XL mechanical: about 367 New sites).**
- **Files:** agent/agent.go, options.go (new), runctx.go (`RunInfo`), identity.go, retrieval.go (the `WithRetrieval` Option), typed.go (clone removal), trace/trace.go (`Instrument`), eval (AgentRunner construction), and every `New(` site.
- **Tests prove:**
  - Each validation error from item 2 is returned by `New`.
  - `With` does not affect the original (concurrent `With` plus `Run` under `-race`).
  - An option passed at both scopes resolves as run over agent over default.
  - No exported mutating method remains on `Agent` (`go doc` check).

**Follow-up A: "record not started" attempt semantics (M).**
- **Files:** agent/toolexec.go and agent/record.go (the new `StepAttempt` outcome field or kind).
- **Why now:** it needs P12's toolexec to be settled, and it must land before the v1 freeze.
- **Tests prove:** a call cancelled after its marker and before `Call` began resumes without a halt; a call that began still halts.

### Wave 6: run API, plus follow-up B (parallel)

**P14. Run API (XL mechanical: about 172 Run, 19 Stream, 48 typed and 8 session sites).**
- **Files:** agent/loop.go, result.go, saga.go (`runSaga` fold), stream_agent.go (becomes stream.go with `RunStream`/`RunEvent`), typed.go, session.go, subagent.go (call site), recovery.go (`ResumeFunc`), audit/eventsink.go (`RecordStream`), eval/eval.go (AgentRunner), and every Run site and example.
- **Tests prove:**
  - `Result` is non-nil with correct `Usage`/`Spend` on failure, on each pause type, and on a saga abort.
  - A resume with a different input or saga mode is `ErrConfig`; `Resume` with the journaled input completes a crashed run, a session turn, and a saga rollback.
  - A typed run with a changed `T` is `ErrConfig`.
  - `Run == Stream().Result()` over the scripted suite.
  - An image input reaches the adapter request.

**Follow-up B: saga compensation with accepted arguments (M).**
- **Files:** agent/compensate.go and agent/toolexec.go (journal `call.Use.Args` as passed). P14 must not edit toolexec.go; the saga flag keeps its existing parameter.
- **Tests prove:** a middleware that rewrites args makes compensation receive the rewritten args, and an old record (no accepted args) falls back to the model's.

### Wave 7: budget and cleanup (parallel)

**Follow-up C: whole-tree budget (M).**
- **Files:** agent/loop.go, subagent.go, result.go.
- **Why last among the follow-ups:** it defines how sub-run spend enters the parent's `Result.Usage`/`Spend` and budget, so it needs P14's Result.
- **Tests prove:** the budget stops a parent whose sub-agents spent it; totals are identical across crash and resume (the `budget-tree` branch's test).

**P16. Cleanup (L, mostly mechanical).**
- **Files:** `ScriptedModel` moves to agent/agenttest (16 test users), the review 7.3 unexports, doc sweep (allowlist emptied), `Example` tests replacing doc-comment calls, competitor claims moved to docs/, the stale references from review 10.3, `TurnRestarted`/`RunEvent` open-set docs, the optional mcp package rename (Q9), CHANGELOG and docs/guides/migrating-to-1.0.md.
- **Tests prove:** doccheck is clean with an empty allowlist; `go vet`; every example module builds.

### Critical path and parallelism
- **Critical path:** P1, then P6, then P9 and P10, then P12, then P13, then P14, then follow-up C: seven sequential steps.
- **Off the critical path:**
  - P2 to P5 run in wave 1.
  - P7 and P8 run beside P6.
  - P11 runs beside P12.
  - Follow-ups A and B ride waves 5 and 6.
- **Invariant check at every boundary:** each PR runs the full DST, crash and chaos suites and `storetest` (with postgres against a live server, as #57 and #60 did).

---

## 11. Open questions (recommendation first)

1. **Journal version bumps before 1.0.** Recommend keeping `bide.journal.v1` until the 1.0 tag and editing it in place (no users). After 1.0, any key or record change bumps it.
2. **Pre-v1 journals.** Recommend refusing them (`Found: ""`) with no migration tool. #60 already breaks v0.7.
3. **Names `Store` (port) and `Journal` (semantics)** instead of keeping `Durable`. Recommend `Store`/`Journal`: the name then says which side of the line a type is on.
4. **Export `Journal.Do`?** Recommend yes, for engine packages (audit, govern), refusing engine-reserved prefixes other than `audit:`. The alternative is hidden cross-package hooks, which cost more than they protect.
5. **`Result.Usage` meaning.** Recommend whole-run (journal-derived and resume-stable) as the primary field, with per-invocation numbers under `Result.Live`. This matches what budgets count and the `budget-tree` test's expectation.
6. **Journaled input is authoritative on resume, and `Agent.Resume` exists.** Recommend yes. It removes the `inputFor(runID)` burden from every recovery deployment and closes an exact-replay gap.
7. **`RunTyped` as a Go 1.27 generic method or a package function.** Recommend the package function, for consistency with `Step[T]`, `Signal[T]` and `AnswerInterrupt[T]`, and to keep a brand-new language feature out of the core's frozen surface until tooling settles. Revisit for 2.0.
8. **govern: move or wrap.** Recommend move (own module, v0) plus an `integration/` test module.
9. **Rename package `mcp`** (it shadows the SDK). Recommend renaming the directory and package to `mcptools` in P16, since breaking changes are free now.
10. **Scope of review 7.3.** Recommend unexporting engine plumbing and keeping `TallyApprovals`, `ProjectEvents`, `FindToolCall`, `IsApprovalDecision` and `DecisionCheck` as the documented offline-verification API. Third-party verifiers need exactly these, and moving `Message`/`Record` into a lower package to hide them costs a large alias churn.
11. **Reserve the tool name `final_answer` at `New`.** Recommend yes. It is simpler than detecting the conflict per `RunTyped` call.
12. **`Lease` with no `Leaser`.** Recommend `ErrConfig` by default, with `WithoutLease()` to opt out, as review 2.3 asked. F1 keeps the silent fallback, so this is a behavior change P6 makes.
13. **bide-audit exit precedence.** Recommend: 2 is exclusive, then 1 over 4 over 3 over 0.
14. **`ToolSpec.Timeout` enforced by the agent as an unknown outcome.** Recommend yes. It is one semantics for MCP and local tools.
15. **Retrieval as an agent option rather than a middleware.** Recommend the option. It removes two ctx keys, and the layer order becomes explicit.
16. **The `@run` header stores the full input,** images included. Recommend yes, because `Resume` needs it. Document it for retention, and consider a future `Deleter` capability for GDPR erasure at run granularity.
17. **Rename `AgentEvent`/`AgentStream` (NICE).** Recommend doing it in P14, since every consumer changes there anyway.
18. **The flow `@run` header with the topology digest** (refuse resuming a changed flow). Recommend yes, in P10. Its absence is a silent-divergence hazard of the same class as the saga-mode one.
19. **Evidence v3 acceptance window.** Recommend that verifiers accept v3 for one pre-1.0 release, then drop it.

---

### Critical Files for Implementation
- /Users/dayna/code/go-agents/agent/store.go (becomes journal.go, memstore.go, record.go, step.go and halt.go; `Store`/`Journal`, header, claims)
- /Users/dayna/code/go-agents/agent/agent.go (becomes agent.go, loop.go, toolexec.go, generate.go and runctx.go; construction, run loop, tool execution, model call)
- /Users/dayna/code/go-agents/agent/modelcall.go (`ModelCall`/`ModelResponse`, hooks, removal of ctx override)
- /Users/dayna/code/go-agents/agent/recovery.go (`IsPause`, `Capability` over `Store`, `Lister` cursor, `Lease` options, `Replay`)
- /Users/dayna/code/go-agents/plan/flow.go (lowering to `agent.Step`, removal of `HaltAmbiguous`)

Other load-bearing paths:
- /Users/dayna/code/go-agents/agent/durabletest/durabletest.go (becomes agent/storetest)
- /Users/dayna/code/go-agents/audit/audited_store.go
- /Users/dayna/code/go-agents/store/sqlite/sqlite.go
- /Users/dayna/code/go-agents/store/postgres/postgres.go
- /Users/dayna/code/go-agents/cmd/bide-audit/main.go
- /Users/dayna/code/go-agents/middleware/hedge.go
- /Users/dayna/code/go-agents/agent/session.go
- the F1 work in progress at /private/tmp/claude-501/-Users-dayna-code/a20c4422-caf8-4114-92dd-7b8fad7f7ecf/scratchpad/wt-apibugs/agent/store.go