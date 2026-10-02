# Deterministic replay and run visualization

Every run journals its steps to a `Durable` store (see [extension points](../reference/extension-points.md)).
Because that journal is a complete, ordered history of what happened, three debugging and
observability tools fall out of it directly, each a pure function of the recorded records:

- **`agent.Replay`** re-executes a past run deterministically, offline, without a live LLM.
- **`agent.ReplayEvents`** reconstructs the durable semantic events from the journal.
- **`agent.RenderMermaid`** exports a run as a Mermaid flowchart.

None of these re-run tools or call a provider (with the single, explicit exception of
`Replay`, which re-drives the agent loop against recorded *model* outputs). They read the
journal and project it.

## 1 · Deterministic replay: `agent.Replay`

<!-- docsnip: api agent -->
```go
func Replay(ctx context.Context, source Durable, runID string) (Model, error)
```

`Replay` returns a `Model` that re-emits the model outputs recorded for `runID`, in order,
instead of calling a live LLM. It reads `source.History(ctx, runID)` and collects every
`StepModel` record's assembled message; the returned model streams them back one turn at a
time. Run an agent built on that model against a **fresh** store to deterministically
re-execute the past run.

Because the journal captures every model output across the whole (possibly nested) tree, the
replay is exact. Use it for:

- **Time-travel debugging**: step through exactly what happened, offline and free.
- **Regression tests**: capture a production run, replay it in CI (pairs with
  `testing/synctest`), assert behavior did not drift.
- **Evals over real traffic**: the journal *is* a golden dataset.

Each replayed turn also reports the token usage recorded with it, and the usage its turn
discarded (failed attempts, losing hedge targets) in its `Finish.Discarded`, and a model call
that failed for good fails again at the same point with the same usage. So the replayed run's
`RunResult` usage and spend and its journal match the original, a `middleware.Cost` on the
replaying agent counts the same spend, and a run that `WithTokenBudget` stopped stops at the same
point on replay. Each replayed turn ends with the finish reason and the provider's raw reason its
record journaled (`Record.Finish`, `Record.RawFinish`); a record written before they were
journaled gets the reason its message implies (`tool_use` or `stop`). The replayed turn's record
names the model the original record named (`Record.Model`), not the replay model. Spend the
original journaled in a late spend record (a request that ended after its turn) is reported with
the turn before it, so the totals match although the record layout differs.

If the replay model is asked for more turns than were recorded, its `Stream` returns
`agent.ErrNoRecordedOutput`; that is the signal that the replayed loop diverged from the
original (it wanted a turn the recording never produced).

<!-- docsnip: setup ctx context.Context; prod agent.Durable; runID string; tools []agent.Tool; originalInput string -->
```go
// `prod` is the store that captured the original run; runID identifies it.
replayModel, err := agent.Replay(ctx, prod, runID)
if err != nil {
	log.Fatal(err)
}

// Rebuild the agent with the SAME tools and a FRESH store, swapping the live
// model for the replay model. Feed the same input the original run started with.
fresh := agent.NewMemStore()
replayed := agent.New(replayModel, fresh, tools...)

msg, err := replayed.Run(ctx, runID, originalInput)
if err != nil {
	log.Fatal(err)
}
log.Println(msg.Text()) // identical to the original terminal answer
```

The replay model supplies the model turns; your tools still execute (against whatever
fixtures or mocks you wire in). This is what makes it useful for regression: the model side
is pinned to the recording, so any drift you see comes from your tool or loop changes.

## 2 · Reconstruct durable semantic events: `agent.ReplayEvents`

<!-- docsnip: api agent -->
```go
func ReplayEvents(ctx context.Context, store Durable, runID string) ([]AgentEvent, error)
```

`ReplayEvents` returns the semantic lifecycle events implied by a run's **durable journal**:
the same `AssistantTurn` and `ToolCompleted` events `Agent.Stream` re-emits when it resumes
from that journal, in persisted order. It reconstructs from the journal alone, without
re-running the model or tools.

Only journaled facts are reproduced:

- `StepModel` records become `AssistantTurn{Message: ..., Replayed: true}`.
- `StepToolResult` records become `ToolCompleted{ToolUseID, Name, Result, IsError}`.

Live-loop-only signals (token-level `ModelEvent` deltas, `TurnStarted`, `TurnRestarted`, `ToolStarted`, and
the terminal `Finished`) are not journaled and so are **not** part of the durable projection.
The durable content is the assistant turns and the tool results.

Because the sequence is a pure function of the recorded steps, it is identical before and
after a crash, which is what makes it a resume-stable audit artifact. The `audit` package
builds its event trail on exactly this projection (`audit.PersistJournal`,
`audit.EventLogFromJournal`).

<!-- docsnip: setup ctx context.Context; store agent.Durable; runID string -->
```go
events, err := agent.ReplayEvents(ctx, store, runID)
if err != nil {
	log.Fatal(err)
}
for _, e := range events {
	switch ev := e.(type) {
	case agent.AssistantTurn:
		fmt.Printf("assistant turn (replayed=%v): %s\n", ev.Replayed, ev.Message.Text())
	case agent.ToolCompleted:
		fmt.Printf("tool %s -> error=%v result=%s\n", ev.Name, ev.IsError, ev.Result)
	}
}
```

## 3 · Export a run as a Mermaid diagram: `agent.RenderMermaid`

<!-- docsnip: api agent -->
```go
func RenderMermaid(ctx context.Context, d Durable, runID string) (string, error)
```

`RenderMermaid` returns a Mermaid `flowchart TD` of a run's journaled steps: the graph is
*derived output*, rendered from what actually ran, not hand-authored. Feed it to a dev UI, a
trace viewer, or a PR description.

Each record maps to a node in run order:

- `StepModel` -> `LLM`
- `StepToolResult` -> `tool: <name>` (a failed tool result gets a `✗` suffix)
- `StepApproval` -> `approved ✓` or `denied ✗`
- `StepValue` -> `step: <name>` (a user-authored durable step, or the run's `run:start` and
  `run:complete` records)
- `StepHeader`, `StepAttempt` and `StepNotStarted` records are skipped: they are the journal
  header, the internal side-effect-safety marker and the record that an attempt never started, not
  part of the visual flow.

The chart opens with a `start([user])` node and closes with a `done([done])` node.

<!-- docsnip: setup ctx context.Context; store agent.Durable; runID string -->
```go
diagram, err := agent.RenderMermaid(ctx, store, runID)
if err != nil {
	log.Fatal(err)
}
fmt.Println(diagram)
```

Example output for a run that made one model call, ran one tool, then answered:

```
flowchart TD
  start([user])
  n0["step: run:start"]
  start --> n0
  n1["LLM"]
  n0 --> n1
  n2["tool: charge"]
  n1 --> n2
  n3["LLM"]
  n2 --> n3
  n4["step: run:complete"]
  n3 --> n4
  n4 --> done([done])
```

## 4 · Crash recovery: `Lister` and `Recover`

Replay (section 1) re-drives one run you already have the ID for. After a real crash the
harder question is which runs were in flight at all: the durable store holds them, but the
base `Store` port is `Insert`, `Get` and `Load(runID)` only, with no way to enumerate. Recovery
adds that missing piece and a supervisor that uses it.

The durable store is the source of truth. A run's full state lives in its journal, so
recovery is enumerate-then-re-drive, nothing more:

<!-- docsnip: api agent -->
```go
type Lister interface {
	// Runs yields the IDs of the runs f admits, in ascending byte order.
	Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error]
}

type RunFilter struct {
	After          string   // cursor: only run IDs after this one
	Prefix         string   // a tenant or namespace, by run-ID prefix
	ExcludeHolding []string // drop runs holding an entry with any of these names
	LeaseLapsed    bool     // only runs whose lease has lapsed (its holder died or stalled)
}

func IsComplete(ctx context.Context, store Durable, runID string) (bool, error)
type Resumer func(ctx context.Context, runID string, start RunStart) error

func Recover(ctx context.Context, store Durable, resume Resumer, opts ...RecoverOption) (int, error)
func RecoverLoop(ctx context.Context, store Durable, resume Resumer, opts ...RecoverLoopOption) error
func ResumeAgent(a *Agent, opts ...RunOption) Resumer         // runs of kind agent, not typed
func ResumeTyped[T any](a *Agent, opts ...RunOption) Resumer  // typed runs whose answer type is T
func ResumeAny(rs ...Resumer) Resumer                         // the first that does not return ErrNotResumable
```

The lease options (`WithLeaseHolder`, `WithLeaseTTL`, which `Lease`, `Recover` and `RecoverLoop` all
take; `WithRecoverInterval`, `WithRecoverConcurrency` and `WithRecoverErrors` are `RecoverLoop`'s
alone) apply when the store also implements `Leaser`:
`Recover` then drives each run under a per-run lease and skips runs another holder leases (see
[known limitations](../KNOWN-LIMITATIONS.md) for what the lease does and does not guarantee).

`Lister` is an OPTIONAL capability, kept off the base `Store` interface on purpose: memoization
and replay are the crash-safety core, and enumeration is a separate, backend-specific concern (a
SQL store lists with a query; the base contract stays minimal). `MemStore`, `store/sqlite` and
`store/postgres` implement it. A store opts in by implementing `Runs`; `Recover` finds it with
`agent.Capability`, which also looks through wrappers that implement `Unwrap() Store` (and, for the
transition, `Unwrap() Durable`, such as `audit.AuditedStore`), and returns an `ErrConfig`-wrapped
error if the store cannot enumerate.

`Recover` asks the store for the runs that are not over: its filter excludes every run holding a
terminal marker (`run:complete`, `run:aborted`, `run:cancelled`), which a SQL store evaluates in
its query, a page of 500 run IDs at a time, so a pass reads none of the finished runs. It skips
every sub-agent run (`agent.IsSubRun`; its root's re-run resumes it) and every session journal and
turn run (`agent.IsSessionRun`; the session resumes a turn when its message is sent again), and
hands each remaining run to `resume` with its `run:start` record, read under the run's lease.

A run with no `run:start` (one never driven, such as a `Signal` sent to a mistyped run ID, or one
whose first drive has not written it yet) is skipped and reported once per process as
`ErrNotStarted` (in `Recover`'s error, or to `WithRecoverErrors`). The process remembers the
report, not the skip: `run:start` is read again on every pass, so a run that starts later is
recovered. A run that `resume` declines (an error wrapping `ErrNotResumable`) is reported once
the same way. The process remembers the 65,536 runs it reported most recently; one it has
forgotten is reported again the next time a pass finds it so. `ErrNotStarted` wraps no category:
a run whose first drive has not written `run:start` yet is a race, not a configuration error.

Another driver can finish a listed run before the pass gets to it (while the pass drives the runs
listed before it, or, in `RecoverLoop`, waits for a free slot). So once the pass holds a run's
lease, it checks the three terminal markers again before it calls `resume`, and leaves a run that
is over alone (it does not count it as re-driven, nor a run whose check failed). The check cannot
miss a finish by a driver that holds the run's lease (`Lease`, `Recover`, `RecoverLoop`): such a
driver records the marker before it releases its lease. It can miss two others: a finish by a
driver that holds no lease (a plain `Run`), and a finish in the lost-lease window, when the pass
stalls past its lease TTL between the check and `resume` and another driver takes the run over and
finishes it. In both cases `resume` is handed a finished run, which `Run` or `RunSaga` replays
without firing anything again. The check and the `run:start` read cost four point reads
(`Store.Get`) for each run the pass drives, and none for the finished runs the filter excluded;
over a `Durable` that is not a `Journal`, `History` reads instead.

`ResumeAgent` is the usual `Resumer`: it drives each run under the options its `run:start`
journaled (a saga as a saga), and takes only the deployment's own options (a Waker, a clock, a
concurrency cap, an identity's Actor); a journaled setting passed to it is `ErrConfig`, since a
recovery drive must not change a run's options. It declines typed runs (use `ResumeTyped[T]`),
flows and session turns with `ErrNotResumable`, and so a run whose `run:start` an earlier version
wrote (no kind and no typed start: the record does not say whether the run is typed). Recover
those with a `Resumer` of your own, placed after `ResumeAgent`; `ResumeAny` combines several:

<!-- docsnip: setup ctx context.Context; store agent.Durable; a *agent.Agent; w agent.Waker -->
```go
n, err := agent.Recover(ctx, store, agent.ResumeAgent(a, agent.WithWaker(w)))
// n = runs re-driven; err = joined genuine failures (nil if the only "errors" were pauses)
```

A deployment's own `Resumer` is any `func(ctx, runID, start agent.RunStart) error`, and returns an
error wrapping `agent.ErrNotResumable` for a run it does not drive.

A run's first drive records its input, whether it runs as a saga, and the run options its caller
passed (the `run:start` step; see `agent.RunStart`), and every later drive runs under them: a
different limit (`WithMaxTurns`, `WithTokenBudget`) is journaled as an amendment
(`run:limits:<n>`), and any other different setting (the input, saga, tool filter, system prompt,
sampling, tool choice, typed schema or output mode, principal) is `ErrConfig`. A completed run
returns its recorded answer only to a drive with the input it answered; another input is
`ErrConfig` too. `RecordedStart` reads the record back (its `Input` is a `Message`;
`start.Input.Text()` is its text).

**Keep recovering for the life of the process.** `Recover` is one pass: a run whose holder died
a moment ago still has a live lease, so the pass skips it, and nothing re-drives it until someone
calls `Recover` again. `RecoverLoop` is that someone. Start it once per worker; it runs a pass every
`WithRecoverInterval` (half the lease TTL by default) until its context ends, in two loops:

- The **full pass** lists every unfinished run and drives each one it can lease: halted runs, runs
  nobody leases (a plain `Run` whose process died) and dead holders' runs. It costs about five store
  round trips for each unfinished run it lists, halted runs included, and the next full pass starts
  only after this one has started all of its drives, so with many unfinished runs or a slow store
  it can outlast the interval.
- The **lapsed loop**, over a store that implements `Leaser`, lists only the unfinished runs whose
  lease has lapsed (`RunFilter.LeaseLapsed`) and drives them with slots of its own
  (`WithRecoverLapsedConcurrency`, 16 by default). A store deletes a lease on release, so a lapsed
  lease means its holder died or stalled; halted runs hold no lease between visits, so this loop
  never waits behind them. Each lapsed pass first deletes, with `Leaser.ReapLeases`, the lapsed
  leases no pass takes over (a finished run's, left by a holder that died before its release, one
  on a run the store does not hold, or one on a session's or a sub-agent's run, whose ID contains
  `>`: a session leases each turn's run, and the session, not a pass, resumes it).

So a dead holder's run is taken over within about one interval of its lease expiring however many
halted runs the store holds, as long as the lapsed loop has a free slot. A run whose driver held no
lease (a plain `Run`) is left to the full pass, whose length still bounds its pickup, so resolve
halted runs rather than leaving them for every full pass to visit:

<!-- docsnip: setup ctx context.Context; store agent.Durable; resume agent.Resumer -->
```go
go func() {
	err := agent.RecoverLoop(ctx, store, resume,
		agent.WithLeaseHolder("worker-1"),
		agent.WithRecoverErrors(func(err error) { log.Printf("recover: %v", err) }))
	// err is ctx's error once ctx is done (or a configuration error at once)
}()
```

It drives runs concurrently, up to `WithRecoverConcurrency` in the full pass and
`WithRecoverLapsedConcurrency` in the lapsed loop (16 each by default), so one long drive does not
hold up the others, and neither loop starts a run the worker is already driving. A genuine failure goes to
the `WithRecoverErrors` handler and the run is tried again on the next pass; pauses and lost leases
are not failures. On shutdown it waits for the drives it started to return.

**The completion marker lets it skip finished runs.** When a run returns its final answer,
the loop records one terminal `StepValue` named `run:complete` (it renders as
`step: run:complete` in the Mermaid graph above). `IsComplete` reports whether it is the run's
first end marker (a run cancelled before it completed is not complete), and `Recover`'s filter
excludes any run that has an end marker. The marker is appended only at the terminal and is at-most-once by
name, so a replayed run never adds a second one and no earlier record's index shifts. A run
that crashed after journaling its final answer but before the marker is not skipped; re-driving
it writes the marker from the recorded answer without calling the model.

**Cancel and Status.** `agent.Cancel(ctx, j, runID, reason)` ends a run: it writes `run:cancelled`
(an end marker the filter excludes), or, on a saga, a rollback request `run:cancel-requested`, which
is not an end marker, so recovery still drives the saga, whose drive rolls it back and then writes
`run:cancelled`. A drive checks for the marker when it starts, at every turn boundary after its
first, and after each won side-effect claim, before the call (a claim it then records as not
started); calls already in flight finish and record their results, and a retry-safe call already
dispatched in the current turn may still run. A sub-run reads its tree root's cancellation at the
same checks (from the root's store, also when the sub-agent journals to another), so `Cancel` of
the root stops the whole tree, and a `plan` flow's run reads it before
each node that runs. A run with no `run:start` is
`ErrNotStarted`, and a run already over is `ErrRunEnded` (one already cancelled is `nil`). A drive
of a cancelled run returns `ErrRunCancelled`, with a `Result`. The keys `run:complete`,
`run:aborted` and `run:cancelled` are distinct, so two can land; the first in journal order is the
run's end for every reader, and every writer of one reads the markers back and reports the first.
`agent.Status(ctx, j, runID)` reads a run's `RunStatus` (`not_started`, `started`, `completed`,
`aborted`, `cancelled`, the end marker's text, the record count) from one `Load`; a paused, halted
or limit-stopped run is `started`, since pauses are not journaled.

**Pauses re-surface; they are not errors.** A re-driven run that is still waiting returns an
`agent.Pause` (`*ApprovalPending`, `*InterruptPending`, `*TimerPending`, `*SignalPending`,
`*OutcomeUnknown`). `Recover` detects these with `agent.IsPause` and treats them as SUCCESSFUL recoveries: the run is
back in memory and will resume when its condition is met (a human approves, an interrupt is
answered, a timer fires). Only a genuine model, storage, or tool fault is joined into the
returned error.

**A Waker-bound resume rebuilds the timer set** with no separate journal scan. A sleeping run
journals its wake time (see [Messaging](messaging.md) and `agent/pause.go`). When `Recover` re-drives it with a
`resume` whose agent has a `Waker` (the `agent.WithWaker` option), the run replays into its
durable `Sleep`, which sees the `Waker` and re-registers the journaled wake automatically. If the
`Waker` cannot schedule it, the run fails with an error wrapping `ErrStorage` (reported to
`WithRecoverErrors`) and records nothing, and the next `RecoverLoop` pass schedules it again.
Advancing the clock and firing the waker then resumes the run to completion. The rebuild
falls out of ordinary replay: no timer-specific recovery path exists or is needed.

**Run leasing coordinates recovery across processes.** When several processes recover against a
shared store they all enumerate the same in-flight runs. If the store implements the optional
`Leaser` (`AcquireLease` / `RenewLease` / `ReleaseLease` / `ReapLeases`), `Recover` claims an exclusive, renewed
lease per run before driving it and skips a run another holder currently leases, so competing
recoverers do not both re-drive one run (redundant, and a hazard when the store's `Do` is not
cross-process atomic). A crash lets the lease expire (default 30s, `WithLeaseTTL`) and another
process's `RecoverLoop` takes over; that expiry-and-takeover is the high-availability property. `MemStore`
implements `Leaser` in-process (the reference and for tests); `store/sqlite` implements it for the
processes sharing one database file, on a connection of its own with a short busy timeout, so a
writer holding the file's lock does not delay a renewal past its cutoff; `store/postgres`
implements it for any number of nodes. Each uses an atomic upsert over a leases table, with expiry
computed from the database's clock. Without `Leaser` (a custom store), `Recover` drives every
enumerated run, safe under at-most-once claims, just redundant.
A live primary driver wraps `Agent.Run` in `agent.Lease(ctx, store, runID, drive, ...)`, which holds
the same lease, so a recoverer never grabs a run a worker is actively driving; recovery and primary
driving coordinate through one mechanism (`Recover` itself drives each run via `Lease`).

**A lease is renewed on a fixed schedule, and given up before it can lapse.** `Lease` claims the
run under the holder name plus a token of the call's own, so two drivers that share a
`WithLeaseHolder` name still exclude each other. Measured from when the last successful acquisition
or renewal was issued, the lease cannot expire before one TTL has passed (the store sets the expiry
from its own clock while serving the call, and only the store compares expiries, so clock offsets
between nodes do not matter). The holder first renews at half the TTL, retries a renewal that fails
with an error every twentieth of the TTL, and abandons each attempt at three quarters of the TTL. If
the store reports the lease is no longer held, or no renewal succeeds by that cutoff, the drive's
context is cancelled with `agent.ErrLeaseLost` as its cause (`context.Cause`), a quarter of the TTL
before any other process could take the lease. `Lease` then returns the drive's error wrapped with
`ErrLeaseLost`, so `errors.Is(err, agent.ErrLeaseLost)` tells a lost lease from a shutdown, and
`Recover` does not count it as a failure. The quarter-TTL margin is for the cancellation to reach
the drive and for the store's clock rate to differ slightly from the worker's; it cannot cover a
worker that is stalled through the cutoff, which is why side effects rest on the attempt claim
instead (see [known limitations](../KNOWN-LIMITATIONS.md)).

**The idempotent retry path** reduces halt-for-a-human stops. On resume, a tool with an
unknown outcome (invoked, no result journaled) normally fires `*OutcomeUnknown` unless it is
retry-safe. A tool declared `Safety{Idempotent: true}` is retry-safe: it asserts that a retried
call de-duplicates downstream, so the run retries it instead of halting. The contract is the
tool's to keep: it derives a stable downstream key from its arguments (or uses
`agent.NextOnceKey`) and sends it with the request. This keeps autonomous and ambient agents
moving instead of stopping for a human on every uncertain call.

**Boundary: mechanism vs policy.** `Recover` is the mechanism (enumerate, skip finished,
re-drive the rest). `resume` is deployment POLICY: it knows which agent drives a run and any
`Waker` or clock to bind (the run's input and entry point are in its journal, see `RecordedStart`), and it should no-op a `runID` it does not own (a sub-agent run
is driven by its parent; re-driving one directly is redundant, though harmless under
at-most-once memoization). `MemStore` and `MemWaker` are in-memory references: a process exit
loses their state, so for durability across a real crash use the SQLite or Postgres store
(and an external scheduler or durable waker) whose runs survive the restart `Recover` reads
them back from.

## The telemetry envelope: `RunResult` / `Result`

Separate from the journal projections above, `Agent.RunResult` (and `RunSagaResult`) return
a `*Result` envelope carrying telemetry accumulated over the whole run. `Run` and `RunSaga`
are unchanged and remain the path for callers that only need the final message; the
`*Result` variants are additive counterparts for callers that want observability data.

<!-- docsnip: api agent -->
```go
type Result struct {
	Message  Message       // the final assistant answer, identical to what Run returns
	Usage    Usage         // usage of the model responses the whole run recorded
	Spend    Usage         // Usage plus discarded requests (retried attempts, hedge losers) and failed calls
	Turns    int           // number of LIVE model turns in this invocation (replayed turns are not counted)
	Duration time.Duration // wall-clock elapsed time for the run
	RunID    string        // echoes the run identifier passed in
}
```

<!-- docsnip: setup ctx context.Context; a *agent.Agent -->
```go
res, err := a.RunResult(ctx, "run-42", "summarize the ledger")
if err != nil {
	log.Fatal(err)
}
log.Printf("run %s: %d turns, %d in / %d out tokens, %s\n",
	res.RunID, res.Turns, res.Usage.InputTokens, res.Usage.OutputTokens, res.Duration)
```

`Usage` and `Spend` are the whole run's, read from the journal: a run resumed after a crash
reports the model calls made before the crash too, and re-entering a finished run reports the
same totals again. `Turns` describes this invocation only: it counts live model turns, not turns
replayed from the durable journal.
