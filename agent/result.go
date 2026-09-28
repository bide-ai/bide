package agent

import (
	"context"
	"time"
)

// Result is a rich envelope returned by RunResult and RunSagaResult. It carries the
// terminal assistant message plus telemetry accumulated over the whole run: total token
// usage (sum of every model-call Usage), the number of model turns, wall-clock duration,
// and the run ID.
//
// Run and RunSaga are unchanged and remain the idiomatic path for callers that only need
// the final message; RunResult / RunSagaResult are the additive counterparts for callers
// that need observability data.
type Result struct {
	// Message is the final assistant answer, identical to what Run / RunSaga return.
	Message Message

	// Usage is the sum of all model-call usages across the run (input, output, and
	// cache tokens accumulated across every turn).
	Usage Usage

	// Turns is the number of live model turns executed during this run (replayed turns
	// from the durable journal are not counted, since their usage was already accounted
	// for in the original run).
	Turns int

	// Duration is the wall-clock elapsed time for the run (from entry to return).
	Duration time.Duration

	// RunID echoes the run identifier passed to RunResult / RunSagaResult.
	RunID string
}

// RunResult is the envelope-returning counterpart of Run. It drives the agent to
// completion and returns a *Result carrying the final message plus accumulated telemetry
// (usage totals, turn count, duration). Run remains unchanged; existing callers need not
// change.
func (a *Agent) RunResult(ctx context.Context, runID, input string) (*Result, error) {
	start := time.Now()
	msg, usage, turns, err := a.run(ctx, runID, []Message{UserText(input)}, false, nil)
	elapsed := time.Since(start)
	if err != nil {
		return nil, err
	}
	return &Result{
		Message:  msg,
		Usage:    usage,
		Turns:    turns,
		Duration: elapsed,
		RunID:    runID,
	}, nil
}

// RunSagaResult is the envelope-returning counterpart of RunSaga. It behaves identically
// to RunSaga (transactional run with reverse-order compensation on failure) but returns
// the richer *Result envelope on success.
func (a *Agent) RunSagaResult(ctx context.Context, runID, input string) (*Result, error) {
	start := time.Now()
	msg, usage, turns, err := a.runSagaWithTelemetry(ctx, runID, input, nil)
	elapsed := time.Since(start)
	if err != nil {
		return nil, err
	}
	return &Result{
		Message:  msg,
		Usage:    usage,
		Turns:    turns,
		Duration: elapsed,
		RunID:    runID,
	}, nil
}
