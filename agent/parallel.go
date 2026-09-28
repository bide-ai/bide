package agent

import (
	"context"
	"errors"
	"sync"
)

// Task is one unit of durable parallel work: a named step returning a T. Name is the durable
// memoization key within the run, so it must be unique across the tasks in one Parallel call.
// Because each task is a journaled step, a resumed run returns a completed task's recorded result
// without re-running it, and that result is an independently provable record (see audit.ProveStep).
type Task[T any] struct {
	Name string
	Fn   func(context.Context) (T, error)
}

// Parallel runs tasks concurrently, each as a durable named step (see Step), and returns their
// results in task order. It is the durable, auditable fan-in that compliance-style pipelines want:
// run several independent checks at once (sanctions, credit, fraud), each one crash-safe and
// at-most-once, each result committed to the journal and provable on its own, then aggregate.
//
// All tasks run even if some fail, so the full set of results is preserved (a failed check does not
// hide the others). The returned error joins every task's error and is nil only if all succeeded; a
// task that failed was not journaled, so on a later resume it re-runs while succeeded tasks are
// memoized. maxConcurrency caps in-flight tasks; <= 0 means one goroutine per task.
//
// This is deliberately a thin primitive over the durable journal, not a graph engine: dynamic,
// model-driven routing stays in plain Go and sub-agents; Parallel covers the static fan-out/fan-in
// that a governed workflow's parallel-checks-then-decide stage is made of.
//
// As with Step, each task result is journaled as JSON, so T must be JSON-serializable;
// a struct's unexported fields silently round-trip to their zero values.
func Parallel[T any](ctx context.Context, d Durable, runID string, maxConcurrency int, tasks ...Task[T]) ([]T, error) {
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
			results[i], errs[i] = Step(ctx, d, runID, tasks[i].Name, tasks[i].Fn)
		}(i)
	}
	wg.Wait()
	return results, errors.Join(errs...)
}
