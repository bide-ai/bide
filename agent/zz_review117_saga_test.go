package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The tests here give a tool a short WithTimeout and need the call to reach the tool before the
// deadline. On the wall clock it may not: under load the deadline can pass while the call is
// dispatched, and the base handler then rightly refuses it as not started (errDoneBeforeCall),
// so the tool never runs. A synctest bubble makes it deterministic: its clock advances only once
// every goroutine in it is durably blocked, so the deadline passes only after the tool has begun
// and blocks on its context. A tool that waits out its deadline must therefore block (on
// ctx.Done or time.Sleep), never spin on time.Now.

// R117-2. WithTimeout says an error returned after the deadline "has an unknown outcome", and a
// retry-safe tool records it. In a saga that record is the StepSagaFail of the step that aborted
// the saga, and rollback skips that step as one that "made no change". A retry-safe compensable
// write whose commit landed before it was cut off is then neither compensated nor reported:
// SagaAborted says nothing was left behind.
func TestR117_SagaLateErrorOfARetrySafeWriteIsNeitherUndoneNorReported(t *testing.T) {
	synctest.Test(t, testR117SagaLateErrorOfARetrySafeWrite)
}

func testR117SagaLateErrorOfARetrySafeWrite(t *testing.T) {
	var committed, undone atomic.Int32
	hold := CompensatedFunc("hold", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			committed.Add(1) // the provider commits the hold ...
			<-ctx.Done()     // ... and its reply arrives after the deadline
			return "", ctx.Err()
		},
		func(context.Context, struct{}, string) error { undone.Add(1); return nil },
		WithTimeout(time.Millisecond))
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "hold", `{}`), TextTurn("done"))
	_, err := mustNew(m, store, WithTools(hold)).Run(context.Background(), "s1", UserText("book"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga: err = %v, want *SagaAborted", err)
	}
	if committed.Load() != 1 || undone.Load() != 0 || len(ab.UnknownOutcome) != 1 || ab.UnknownOutcome[0] != "hold" {
		t.Fatalf("a write with an unknown outcome: committed %d, undone %d, compensated %q, uncompensated %q, unknown %q; want it committed once, reported as unknown, not compensated",
			committed.Load(), undone.Load(), ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
	}
}

// R117-3 (documented behaviour). NextOnceKey is scoped to one tool call: a retry is a new call
// (a new tool-use ID) and gets a new key, so a retry-safe tool whose late error was recorded, and
// which the model then calls again, applies its effect under two keys. Dedup across the model's
// retries needs a business key derived from the arguments (see Safety.Idempotent and NextOnceKey).
func TestR117_NextOnceKeyIsScopedToOneCall(t *testing.T) {
	synctest.Test(t, testR117NextOnceKeyIsScopedToOneCall)
}

func testR117NextOnceKeyIsScopedToOneCall(t *testing.T) {
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
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "post", `{}`), ToolTurn("c2", "post", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(post)).Run(context.Background(), "r1", UserText("post it")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(applied) != 2 || !applied[SubRunID("r1", "c1")+"#0"] || !applied[SubRunID("r1", "c2")+"#0"] {
		t.Fatalf("keys %v; want two distinct keys, one per call: a retry is a new call", applied)
	}
	// Both calls reached the tool and ran past their deadline: each recorded a late error (an
	// unknown outcome, recorded since the tool is retry-safe), not a call refused as not started.
	for _, id := range []string{"c1", "c2"} {
		rec, ok := hasStep(t, store, "r1", ToolResultStep(id))
		if !ok || !rec.IsError || !strings.Contains(string(rec.Result), "after its 1ms timeout") {
			t.Fatalf("%s: result %s (recorded %v, is_error %v); want the late error recorded", id, rec.Result, ok, rec.IsError)
		}
	}
}
