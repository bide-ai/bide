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
// for a run), is ErrConfig. A finished run returns its recorded answer to a drive with the input
// it answered, through either entry point; another input is ErrConfig, since the answer is not
// that input's (as a finished flow's run is held to its start).
//
// A run whose earlier drives predate this record gets it on its first drive under this version,
// with the input and entry point that drive is given.
//
// Kind says what drives the run. A plan flow's run records RunKindFlow, its flow's name in Flow,
// and its input as JSON text in Input; resuming it with another input (compared as canonical
// JSON, so the input decoded from Input and encoded again resumes it), under another flow's name,
// or driving it as an agent run (or an agent run as a flow) is ErrConfig. The flow's topology is
// held by its own record, flow:digest, not here.
type RunStart struct {
	Input string   `json:"input"`
	Saga  bool     `json:"saga,omitempty"`
	Kind  RunKind  `json:"kind,omitempty"`
	Flow  *FlowRef `json:"flow,omitempty"`
}

// RunKind is what drives a run, as its run:start records it (see RunStart).
type RunKind string

const (
	// RunKindAgent is a run an Agent drives (Run, RunSaga, Stream, a sub-agent, a session turn).
	// A run:start with no kind, as agent runs record it, is this kind.
	RunKindAgent RunKind = "agent"
	// RunKindFlow is a run a plan flow drives (plan.Flow.Run). RunStart.Flow names the flow.
	RunKindFlow RunKind = "flow"
)

// FlowRef names the plan flow that drives a run of kind RunKindFlow.
type FlowRef struct {
	Name string `json:"name"`
}

// kind is s's kind, with a run:start that records none read as RunKindAgent.
func (s RunStart) kind() RunKind {
	if s.Kind == "" {
		return RunKindAgent
	}
	return s.Kind
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
//	if start.Kind == agent.RunKindFlow {
//	    return driveFlow(ctx, start.Flow.Name, runID, start.Input) // the flow's Run, with the input decoded
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

// beginRun is journalhook.Begin: the completion of a finished run, or else want held as the run's
// start (see holdToStart).
func beginRun(ctx context.Context, d Durable, runID string, want RunStart) (json.RawMessage, bool, error) {
	if want.kind() == RunKindFlow {
		// A flow's input is held by its canonical JSON (see equalJSON); one that has none (a
		// repeated key, a lone surrogate) could not be told apart from another, so it is refused.
		if _, err := canonicalJSON(want.Input); err != nil {
			return nil, false, fmt.Errorf("run %s: the flow input: %w (%w)", runID, err, ErrConfig)
		}
	}
	b, err := marshalJournal(want)
	if err != nil {
		return nil, false, fmt.Errorf("encode %s (run %s): %w (%w)", runStartStep, runID, err, ErrConfig)
	}
	var recs []Record
	if j := journalOf(d); j != nil {
		rec, inserted, err := j.putNew(ctx, runID, runStartStep, Record{Kind: StepValue, Result: b})
		if err != nil {
			return nil, false, fmt.Errorf("record %s (run %s): %w", runStartStep, runID, err)
		}
		if inserted {
			return nil, false, nil // a new run: nothing to hold it to, and no completion
		}
		recs = []Record{rec}
	}
	done, ok, err := lookup(ctx, d, runID, runCompleteStep)
	if err != nil {
		return nil, false, err
	}
	if err := holdToStart(ctx, d, runID, recs, want); err != nil {
		return nil, false, err
	}
	if ok && done.Kind == StepValue {
		return done.Result, true, nil
	}
	return nil, false, nil
}

// checkFinishedStart refuses (ErrConfig) an agent's drive, with the given input, of a finished
// run whose recorded start in recs is of another kind or answered another input: the recorded
// answer is that input's, not this one's (#137's R137-2; an unfinished run is held to its start by
// holdToStart). A run with no recorded start passes, as a run journaled by a version that recorded
// none.
func checkFinishedStart(runID string, recs []Record, input string) error {
	for _, r := range recs {
		if r.Kind != StepValue || r.Name != runStartStep {
			continue
		}
		var got RunStart
		if err := json.Unmarshal(r.Result, &got); err != nil {
			return fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
		}
		if got.kind() != RunKindAgent {
			return fmt.Errorf("run %s was started as a run of kind %q, not %q; drive it the way it was started (see RecordedStart): %w", runID, got.kind(), RunKindAgent, ErrConfig)
		}
		if got.Input != input {
			return fmt.Errorf("run %s finished answering a different input (see RecordedStart); its answer is not this input's: %w", runID, ErrConfig)
		}
		return nil
	}
	return nil
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
	case got.kind() != want.kind():
		return fmt.Errorf("run %s was started as a run of kind %q, not %q; drive it the way it was started (see RecordedStart): %w", runID, got.kind(), want.kind(), ErrConfig)
	case got.kind() == RunKindFlow && (got.Flow == nil || want.Flow == nil || got.Flow.Name != want.Flow.Name):
		return fmt.Errorf("run %s was started by flow %s, not %s; resume it with the flow it started with: %w", runID, flowName(got.Flow), flowName(want.Flow), ErrConfig)
	case got.Saga && !want.Saga:
		return fmt.Errorf("run %s was started as a saga; resume it with RunSaga (or StreamSaga): %w", runID, ErrConfig)
	case !got.Saga && want.Saga:
		return fmt.Errorf("run %s was not started as a saga; resume it with Run (or Stream): %w", runID, ErrConfig)
	case got.kind() == RunKindFlow:
		same, err := equalJSON([]byte(got.Input), []byte(want.Input))
		if err != nil {
			return fmt.Errorf("run %s: compare its input with the recorded one: %w (%w)", runID, err, ErrConfig)
		}
		if !same {
			return fmt.Errorf("run %s was started with a different input (see RecordedStart); resume it with that input: %w", runID, ErrConfig)
		}
	case got.kind() != RunKindFlow && got.Input != want.Input:
		return fmt.Errorf("run %s was started with a different input (see RecordedStart); resume it with that input: %w", runID, ErrConfig)
	}
	return nil
}

// flowName quotes f's name for an error, or says there is none.
func flowName(f *FlowRef) string {
	if f == nil {
		return "(none recorded)"
	}
	return fmt.Sprintf("%q", f.Name)
}
