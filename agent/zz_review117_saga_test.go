package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// R117-2. WithTimeout says an error returned after the deadline "has an unknown outcome", and a
// retry-safe tool records it. In a saga that record is the StepSagaFail of the step that aborted
// the saga, and rollback skips that step as one that "made no change". A retry-safe compensable
// write whose commit landed before it was cut off is then neither compensated nor reported:
// SagaAborted says nothing was left behind.
func TestR117_SagaLateErrorOfARetrySafeWriteIsNeitherUndoneNorReported(t *testing.T) {
	var committed, undone atomic.Int32
	hold := CompensatedFunc("hold", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			committed.Add(1) // the provider commits the hold ...
			<-ctx.Done()     // ... and its reply arrives after the deadline
			return "", ctx.Err()
		},
		func(context.Context, struct{}, string) error { undone.Add(1); return nil },
		WithTimeout(time.Millisecond))
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "hold", `{}`), TextTurn("done"))
	_, err := New(m, store, hold).RunSaga(context.Background(), "s1", "book")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga: err = %v, want *SagaAborted", err)
	}
	if undone.Load() != 0 || len(ab.UnknownOutcome) != 1 || ab.UnknownOutcome[0] != "hold" {
		t.Fatalf("a write with an unknown outcome: undone %d, compensated %q, uncompensated %q, unknown %q; want it reported as unknown, not compensated",
			undone.Load(), ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
	}
}

// R117-3 (documented behaviour). NextOnceKey is scoped to one tool call: a retry is a new call
// (a new tool-use ID) and gets a new key, so a retry-safe tool whose late error was recorded, and
// which the model then calls again, applies its effect under two keys. Dedup across the model's
// retries needs a business key derived from the arguments (see Safety.Idempotent and NextOnceKey).
func TestR117_NextOnceKeyIsScopedToOneCall(t *testing.T) {
	var mu sync.Mutex
	applied := map[string]bool{}
	post := Func("post", "", Safety{Idempotent: true}, func(ctx context.Context, _ struct{}) (string, error) {
		k := NextOnceKey(ctx)
		mu.Lock()
		applied[k] = true
		mu.Unlock()
		<-ctx.Done()
		return "", ctx.Err()
	}, WithTimeout(time.Millisecond))
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "post", `{}`), ToolTurn("c2", "post", `{}`), TextTurn("done"))
	if _, err := New(m, store, post).Run(context.Background(), "r1", "post it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("keys %v; want two distinct keys, one per call: a retry is a new call", applied)
	}
}
