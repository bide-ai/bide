package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Task is one unit of durable parallel work: a named step returning a T. Name is the durable
// memoization key within the run, so it must be unique across the tasks in one Parallel call.
// Because each task is a journaled step, a resumed run returns a completed task's recorded result
// without re-running it, and that result is an independently provable record (see audit.ProveStep).
type Task[T any] struct {
	Name string
	Fn   func(context.Context) (T, error)
	// Safety declares whether the task may re-run after a crash, as StepSafety does for a Step.
	// The zero value treats the task as a side effect: it runs at most once, and a crash after
	// its effect halts the resumed call with *OutcomeUnknown. Mark a check or a lookup ReadOnly.
	Safety Safety
}

// Parallel runs tasks concurrently, each as a durable named step (see Step), and returns their
// results in task order. It is the durable, auditable fan-in that compliance-style pipelines want:
// run several independent checks at once (sanctions, credit, fraud), each one crash-safe, each
// result committed to the journal and provable on its own, then aggregate.
//
// All tasks run even if some fail, so the full set of results is preserved (a failed check does not
// hide the others). The returned error joins every task's error and is nil only if all succeeded.
// Succeeded tasks are memoized on a later resume. A failed task was not journaled: one marked
// retry-safe (Task.Safety) re-runs, and a side effect halts with *OutcomeUnknown, as Step does, since a
// failed effect may still have landed. Task names must be unique and not empty; a repeated or empty
// name is ErrConfig.
// maxConcurrency caps in-flight tasks; <= 0 means one goroutine per task.
//
// This is deliberately a thin primitive over the durable journal, not a graph engine: dynamic,
// model-driven routing stays in plain Go and sub-agents; Parallel covers the static fan-out/fan-in
// that a governed workflow's parallel-checks-then-decide stage is made of.
//
// As with Step, each task result is journaled as JSON, so T must be JSON-serializable;
// a struct's unexported fields silently round-trip to their zero values.
func Parallel[T any](ctx context.Context, d Durable, runID string, maxConcurrency int, tasks ...Task[T]) ([]T, error) {
	seen := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		if err := checkStepName("agent: Parallel", t.Name); err != nil {
			return nil, err // refused before any task runs
		}
		if seen[t.Name] {
			// Two tasks sharing a journal key would share one result: one of them would never run.
			return nil, fmt.Errorf("agent: Parallel: task name %q is used twice: %w", t.Name, ErrConfig)
		}
		seen[t.Name] = true
	}
	results := make([]T, len(tasks))
	errs := make([]error, len(tasks))

	var sem chan struct{}
	if maxConcurrency > 0 {
		sem = make(chan struct{}, maxConcurrency)
	}
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		if sem != nil {
			sem <- struct{}{}
		}
		go func(i int) {
			defer wg.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			results[i], errs[i] = Step(ctx, d, runID, tasks[i].Name, tasks[i].Fn, StepSafety(tasks[i].Safety))
		}(i)
	}
	wg.Wait()
	return results, errors.Join(errs...)
}
