package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// Parallel with a concurrency cap starts no task once its context is cancelled: the tasks that
// never started must not run, and each reports the cancellation.
func TestParallel_StartsNoTaskAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran atomic.Int32
	tasks := make([]Task[int], 5)
	for i := range tasks {
		tasks[i] = Task[int]{Name: fmt.Sprintf("t%d", i), Safety: Safety{ReadOnly: true}, Fn: func(context.Context) (int, error) {
			if ran.Add(1) == 1 {
				cancel() // the caller gives up while the first task runs
			}
			return i, nil
		}}
	}
	_, err := memJournal().Parallel(ctx, "r1", tasks, WithMaxConcurrency(1))
	if n := ran.Load(); n != 1 {
		t.Fatalf("%d tasks ran; want 1 (none started after the cancellation)", n)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled for the tasks that never started", err)
	}
}

// A Step called with a cancelled context does not start: fn never runs, and nothing is journaled.
func TestStep_DoesNotStartWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := memJournal()
	var ran bool
	_, err := store.Step(ctx, "r1", "charge", func(context.Context) (int, error) { ran = true; return 1, nil })
	if ran || !errors.Is(err, context.Canceled) {
		t.Fatalf("ran = %v, err = %v; want not run and context.Canceled", ran, err)
	}
	if recs, _ := store.History(context.Background(), "r1"); len(recs) != 0 {
		t.Fatalf("a cancelled Step journaled %d records", len(recs))
	}
}
