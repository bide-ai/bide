package agent

import (
	"context"
	"errors"
	"testing"
)

// crashOnce is a store whose first Do for crashName runs fn, so the side effect happens, and
// then fails before the record commits, as when the process dies between the two.
type crashOnce struct {
	Durable
	crashName string
	crashed   bool
}

var errDied = errors.New("process died")

func (c *crashOnce) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if name == c.crashName && !c.crashed {
		c.crashed = true
		_, _ = fn(ctx)
		return Record{}, errDied
	}
	return c.Durable.Do(ctx, runID, name, fn)
}

// The README's plain-Go order flow: "reserve" is non-idempotent. The process dies after the
// reservation is made and before it is journaled; on resume the step must not reserve again.
// It should halt for confirmation, as a tool call or a plan node in the same position does.
func TestStep_CrashAfterEffectDoesNotRepeatIt(t *testing.T) {
	store := &crashOnce{Durable: NewMemStore(), crashName: "reserve"}
	var reserved int
	reserve := func(context.Context) (string, error) { reserved++; return "res-1", nil }

	if _, err := Step(context.Background(), store, "order-42", "reserve", reserve); !errors.Is(err, errDied) {
		t.Fatalf("setup: %v", err)
	}
	_, err := Step(context.Background(), store, "order-42", "reserve", reserve) // resume
	if reserved != 1 {
		t.Fatalf("reserved %d times across a crash and resume, want 1 (resume err = %v)", reserved, err)
	}
	var halt *ResumeHalt
	if !errors.As(err, &halt) {
		t.Fatalf("resume err = %v, want *ResumeHalt for the unconfirmed reservation", err)
	}
}

// A halted step is cleared the way a halted tool call is: ResolveHalt records the confirmed
// outcome under the step's name (ResolveStepHalt), and the resumed step returns it without running fn.
func TestStep_HaltResolvedByResolveStepHalt(t *testing.T) {
	for _, failed := range []bool{false, true} {
		store := &crashOnce{Durable: NewMemStore(), crashName: "reserve"}
		var reserved int
		reserve := func(context.Context) (string, error) { reserved++; return "res-1", nil }
		_, _ = Step(context.Background(), store, "order-42", "reserve", reserve)
		if err := ResolveStepHalt(context.Background(), store, "order-42", "reserve", "res-1", failed, WithoutLiveDriverCheck()); err != nil { // the crash wrapper hides MemStore's Leaser; no driver is running
			t.Fatal(err)
		}
		got, err := Step(context.Background(), store, "order-42", "reserve", reserve)
		if reserved != 1 {
			t.Fatalf("failed=%v: reserved %d times, want 1", failed, reserved)
		}
		if failed && !errors.Is(err, ErrTool) {
			t.Fatalf("resolved as failed: err = %v, want an ErrTool", err)
		}
		if !failed && (err != nil || got != "res-1") {
			t.Fatalf("resolved: got %q, %v; want res-1", got, err)
		}
	}
}

// A step declared retry-safe skips the marker and simply re-runs after a crash.
func TestStep_RetrySafeStepReRunsAfterCrash(t *testing.T) {
	store := &crashOnce{Durable: NewMemStore(), crashName: "classify"}
	var runs int
	classify := func(context.Context) (string, error) { runs++; return "rush", nil }
	ro := WithSafety(Safety{ReadOnly: true})
	_, _ = Step(context.Background(), store, "order-42", "classify", classify, ro)
	got, err := Step(context.Background(), store, "order-42", "classify", classify, ro)
	if err != nil || got != "rush" || runs != 2 {
		t.Fatalf("got %q, %v after %d runs; want rush after 2", got, err, runs)
	}
}
