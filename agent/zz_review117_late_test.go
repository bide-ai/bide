package agent

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// R117-1. The late-error rule is "an error returned after the deadline has an unknown outcome",
// but callTool detects it by tctx.Err(), which is set only when the context's timer goroutine has
// run. A tool that honors its deadline from ctx.Deadline() (its own client timeout, a conn
// deadline, a poll loop) and returns an error once the deadline has passed, before that timer
// callback has run, is recorded as an ordinary failure. For a side effect that tells the model
// the call failed, inviting a second charge.
//
// GOMAXPROCS(1) makes it deterministic: the tool busy-waits past the deadline without yielding,
// so the timer callback cannot run before callTool reads tctx.Err().
func TestR117_LateErrorBeforeTheTimerFiresIsRecordedAsAFailure(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var calls atomic.Int32
	charge := Func("charge", "", Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		calls.Add(1)
		dl, _ := ctx.Deadline()
		for time.Now().Before(dl) { // the provider call is in flight until the deadline
		}
		return "", errors.New("gateway: client timeout awaiting response")
	}, WithTimeout(2*time.Millisecond))
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	_, err := New(m, store, charge).Run(context.Background(), "r1", "pay")
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); ok {
		t.Fatalf("the side effect's error after its deadline was recorded as a failure (%s, is_error %v); Run err = %v; want nothing recorded and ErrToolOutcomeUnknown",
			rec.Result, rec.IsError, err)
	}
	if !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("Run: err = %v, want ErrToolOutcomeUnknown", err)
	}
}
