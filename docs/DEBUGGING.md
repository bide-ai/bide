# Deterministic replay and run visualization

Every run journals its steps to a `Durable` store (see [EXTENSION-POINTS.md](EXTENSION-POINTS.md)).
Because that journal is a complete, ordered history of what happened, three debugging and
observability tools fall out of it directly, each a pure function of the recorded records:

- **`agent.Replay`** re-executes a past run deterministically, offline, without a live LLM.
- **`agent.ReplayEvents`** reconstructs the durable semantic events from the journal.
- **`agent.RenderMermaid`** exports a run as a Mermaid flowchart.

None of these re-run tools or call a provider (with the single, explicit exception of
`Replay`, which re-drives the agent loop against recorded *model* outputs). They read the
journal and project it.

## 1 · Deterministic replay: `agent.Replay`

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

If the replay model is asked for more turns than were recorded, its `Stream` returns
`agent.ErrNoRecordedOutput`; that is the signal that the replayed loop diverged from the
original (it wanted a turn the recording never produced).

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

Live-loop-only signals (token-level `ModelEvent` deltas, `TurnStarted`, `ToolStarted`, and
the terminal `Finished`) are not journaled and so are **not** part of the durable projection.
The durable content is the assistant turns and the tool results.

Because the sequence is a pure function of the recorded steps, it is identical before and
after a crash, which is what makes it a resume-stable audit artifact. The `audit` package
builds its event trail on exactly this projection (`audit.PersistJournal`,
`audit.EventLogFromJournal`).

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
- `StepValue` -> `step: <name>` (a user-authored durable step)
- `StepAttempt` records are skipped: they are the internal side-effect-safety marker, not
  part of the visual flow.

The chart opens with a `start([user])` node and closes with a `done([done])` node.

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
  n0["LLM"]
  start --> n0
  n1["tool: charge"]
  n0 --> n1
  n2["LLM"]
  n1 --> n2
  n2 --> done([done])
```

## The telemetry envelope: `RunResult` / `Result`

Separate from the journal projections above, `Agent.RunResult` (and `RunSagaResult`) return
a `*Result` envelope carrying telemetry accumulated over the whole run. `Run` and `RunSaga`
are unchanged and remain the path for callers that only need the final message; the
`*Result` variants are additive counterparts for callers that want observability data.

```go
type Result struct {
	Message  Message       // the final assistant answer, identical to what Run returns
	Usage    Usage         // sum of every model-call usage across the run
	Turns    int           // number of LIVE model turns (replayed turns are not counted)
	Duration time.Duration // wall-clock elapsed time for the run
	RunID    string        // echoes the run identifier passed in
}
```

```go
res, err := a.RunResult(ctx, "run-42", "summarize the ledger")
if err != nil {
	log.Fatal(err)
}
log.Printf("run %s: %d turns, %d in / %d out tokens, %s\n",
	res.RunID, res.Turns, res.Usage.InputTokens, res.Usage.OutputTokens, res.Duration)
```

`Turns` counts only live model turns: turns replayed from the durable journal are not
counted, because their usage was already accounted for in the original run.
