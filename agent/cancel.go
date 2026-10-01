// cancel.go holds Cancel (D1) and Status (D8): the two calls that act on a run from outside its
// drives (docs/design/api-v1.md, item 1).

package agent

import (
	"context"
	"errors"
)

// RunState is a run's state as Status reads it from the run's journal.
type RunState string

const (
	// RunNotStarted is a run with no run:start record.
	RunNotStarted RunState = "not_started"
	// RunStarted is a run with a run:start record and no end marker: running, paused, halted, or
	// stopped at a limit. Pauses are not journaled, so a paused run is RunStarted.
	RunStarted RunState = "started"
	// RunCompleted is a run whose first end marker is run:complete.
	RunCompleted RunState = "completed"
	// RunAborted is a saga whose first end marker is run:aborted: a step failed and its rollback
	// finished.
	RunAborted RunState = "aborted"
	// RunCancelled is a run whose first end marker is run:cancelled.
	RunCancelled RunState = "cancelled"
)

// RunStatus is a run's state, as Status reads it.
type RunStatus struct {
	State RunState
	// Terminal is the text the run's first end marker records: the reason given to Cancel for a
	// cancelled run, the failure that aborted a saga; empty otherwise.
	Terminal string
	// Records is the number of records the run's journal held when Status read it, the journal
	// header included.
	Records int
}

var errP14NotBuilt = errors.New("p14: not built yet")

// Cancel cancels runID's run (D1).
func Cancel(ctx context.Context, j *Journal, runID, reason string) error {
	return errP14NotBuilt
}

// Status reads runID's state from its journal (D8).
func Status(ctx context.Context, j *Journal, runID string) (RunStatus, error) {
	return RunStatus{}, errP14NotBuilt
}
