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

	// Usage is the sum of the usage of the model responses the run recorded, one per turn
	// (input, output, and cache tokens). It is the whole run's, read from the journal: the same
	// whether the run finished in one invocation or was resumed after a crash, and the same again
	// when a finished run is re-entered.
	Usage Usage

	// Spend is every token the run's model requests used: Usage plus the usage of requests whose
	// responses were discarded (failed attempts a middleware retried, losing hedge targets) and of
	// model calls that failed. It is what the provider bills, and what WithTokenBudget counts.
	// Like Usage it is the whole run's, from the journal (see Record.DiscardedUsage). A request
	// still in flight when an invocation ends (a hedge loser, a request a middleware left
	// running) is waited for, for at most two seconds and not past the run's context, and counted;
	// one that runs longer than that is not in Spend.
	Spend Usage

	// Turns is the number of live model turns this invocation executed. Turns replayed from
	// the journal are not counted: unlike Usage and Spend, it describes this invocation.
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
		Usage:    usage.answer,
		Spend:    usage.spend,
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
		Usage:    usage.answer,
		Spend:    usage.spend,
		Turns:    turns,
		Duration: elapsed,
		RunID:    runID,
	}, nil
}
