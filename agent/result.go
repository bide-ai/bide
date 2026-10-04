package agent

import (
	"encoding/json"
	"time"
)

// Result is what a run entry point returns (Run, Resume, RunStream.Result, RunTyped,
// Session.Send): the final answer and the run's telemetry. Each returns a
// non-nil Result whenever the run ID is valid, whatever the error: a pause, a halt, a failure, a
// saga's abort, or a cancellation.
type Result struct {
	// Message is the final assistant answer. It is zero unless the run returned no error.
	Message Message

	// Output is a typed run's answer as its journal holds it (RunTyped): the arguments the
	// final_answer tool accepted, or the model's native structured output. It is nil for an
	// untyped run, and for a typed run that returned an error.
	Output json.RawMessage

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

	// RunID echoes the run identifier the run was driven under.
	RunID string
}
