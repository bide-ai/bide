package agent

import (
	"context"
	"errors"
	"runtime"
	"strings"
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
//
// The busy-wait needs the wall clock (in a synctest bubble time.Now would not advance while the
// tool spins), and on the wall clock the 2ms deadline can pass while the call is dispatched: the
// base handler then rightly refuses the call as not started, the tool never runs, and the attempt
// says nothing about the late rule. Such an attempt is checked to be exactly that refusal and run
// again; the test fails if no attempt in 100 reaches the tool.
func TestR117_LateErrorBeforeTheTimerFiresIsRecordedAsAFailure(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for range 100 {
		var calls atomic.Int32
		charge := MustFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
			calls.Add(1)
			dl, _ := ctx.Deadline()
			for time.Now().Before(dl) { // the provider call is in flight until the deadline
			}
			return "", errors.New("gateway: client timeout awaiting response")
		}, WithTimeout(2*time.Millisecond))
		store := memJournal()
		m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
		_, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
		rec, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
		if calls.Load() == 0 {
			// Not reached: the deadline passed before dispatch. That is a known failure, recorded.
			if err != nil || !ok || !rec.IsError || !strings.Contains(string(rec.Result), "not started") {
				t.Fatalf("a call that never reached its tool: Run err = %v, result %s (recorded %v); want it recorded as not started", err, rec.Result, ok)
			}
			continue
		}
		if ok {
			t.Fatalf("the side effect's error after its deadline was recorded as a failure (%s, is_error %v); Run err = %v; want nothing recorded and ErrToolOutcomeUnknown",
				rec.Result, rec.IsError, err)
		}
		if !errors.Is(err, ErrToolOutcomeUnknown) {
			t.Fatalf("Run: err = %v, want ErrToolOutcomeUnknown", err)
		}
		return
	}
	t.Fatal("no attempt in 100 reached the tool before its deadline")
}
