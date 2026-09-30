package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// runCompleteStep is the journal name of the terminal completion marker. The agent loop
// appends one StepValue Record under this name when a run returns its final answer, so a
// crash-recovery supervisor can tell a finished run from an in-flight one (see IsComplete
// and Recover) without inspecting the model output.
const runCompleteStep = "run:complete"

// runAbortedStep is the journal name of the terminal marker a saga records once its rollback
// has finished, so a recovery supervisor treats the aborted run as over.
const runAbortedStep = "run:aborted"

// runStartStep is the journal name of the record a run's first drive writes: how the run was
// started (see RunStart).
const runStartStep = "run:start"

// RunStart is how a run was started, as its first drive records it: the input it answers (for a
// Session turn, the turn's message) and whether it runs as a saga (RunSaga, StreamSaga,
// RunSagaResult, or a sub-agent called inside a saga). A run's model turns and tool calls answer
// that input under that entry point's rules, so every later drive is held to it: resuming an
// unfinished run with another input, or through the other entry point (Run for a saga, RunSaga
// for a run), is ErrConfig. A finished run returns its recorded answer whatever it is passed, as
// before.
//
// A run whose earlier drives predate this record gets it on its first drive under this version,
// with the input and entry point that drive is given.
type RunStart struct {
	Input string `json:"input"`
	Saga  bool   `json:"saga,omitempty"`
}

// RecordedStart returns how runID was started (see RunStart), and ok=false for a run whose
// journal holds no such record: one never driven, or one not driven since before the record
// existed. A session's turn run (IsSessionRun) records its message, but only the session can
// drive it (it seeds the turn with the transcript before that message), so Recover never hands
// one to its callback. A recovery callback uses it to re-drive a run with its own input and
// entry point:
//
//	start, ok, err := agent.RecordedStart(ctx, store, runID)
//	if err != nil {
//	    return err
//	}
//	if !ok {
//	    start = startFor(runID) // the deployment's own record, for a run not driven under this version
//	}
//	if start.Saga {
//	    _, err = a.RunSaga(ctx, runID, start.Input)
//	} else {
//	    _, err = a.Run(ctx, runID, start.Input)
//	}
func RecordedStart(ctx context.Context, d Durable, runID string) (RunStart, bool, error) {
	r, ok, err := lookup(ctx, d, runID, runStartStep)
	if err != nil || !ok || r.Kind != StepValue {
		return RunStart{}, false, err
	}
	var s RunStart
	if err := json.Unmarshal(r.Result, &s); err != nil {
		return RunStart{}, false, fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
	}
	return s, true, nil
}

// holdToStart records want as runID's start if the run has none, and otherwise checks want
// against the recorded one: a drive that differs is ErrConfig, since the run's journal answers
// the recorded input under the recorded entry point's rules. recs is the run's journal as the
// drive read it; a start recorded there is checked without another read.
func holdToStart(ctx context.Context, d Durable, runID string, recs []Record, want RunStart) error {
	var rec Record
	found := false
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == runStartStep {
			rec, found = r, true
			break
		}
	}
	if !found {
		b, err := marshalJournal(want)
		if err != nil {
			return fmt.Errorf("encode %s (run %s): %w (%w)", runStartStep, runID, err, ErrConfig)
		}
		rec, err = putRecord(ctx, d, runID, runStartStep, Record{Kind: StepValue, Result: b})
		if err != nil {
			return fmt.Errorf("record %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
		}
	}
	var got RunStart
	if err := json.Unmarshal(rec.Result, &got); err != nil {
		return fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
	}
	switch {
	case got.Saga && !want.Saga:
		return fmt.Errorf("run %s was started as a saga; resume it with RunSaga (or StreamSaga): %w", runID, ErrConfig)
	case !got.Saga && want.Saga:
		return fmt.Errorf("run %s was not started as a saga; resume it with Run (or Stream): %w", runID, ErrConfig)
	case got.Input != want.Input:
		return fmt.Errorf("run %s was started with a different input (see RecordedStart); resume it with that input: %w", runID, ErrConfig)
	}
	return nil
}
