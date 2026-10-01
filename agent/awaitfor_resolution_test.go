package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A tool that re-runs after AwaitFor resolved (here it pauses on a later Interrupt) sees the
// same outcome every time: once the timeout has won, a signal delivered after the deadline does
// not flip the call to the signal branch.
func TestAwaitFor_TimeoutOutcomeSurvivesALateSignal(t *testing.T) {
	store := NewMemStore()
	var clk int64 = 1000
	ctx := ContextWithClock(context.Background(), func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) })
	var outcomes []bool
	tool := Func("watch", "", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		_, ok, err := AwaitFor[string](ctx, "webhook", time.Minute)
		if err != nil {
			return "", err
		}
		outcomes = append(outcomes, ok)
		return Interrupt[string](ctx, "confirm", nil) // a later pause in the same tool
	})
	a := New(NewScriptedModel(ToolTurn("c1", "watch", `{}`), TextTurn("done")), store, tool)
	var aw *Awaiting
	if _, err := a.Run(ctx, "r", "go"); !errors.As(err, &aw) {
		t.Fatalf("first run: %v, want *Awaiting", err)
	}
	atomic.StoreInt64(&clk, 2000) // the deadline passes: the timeout wins
	var in *Interrupted
	if _, err := a.Run(ctx, "r", "go"); !errors.As(err, &in) {
		t.Fatalf("second run: %v, want *Interrupted", err)
	}
	if err := Signal(context.Background(), store, "r", "webhook", "late"); err != nil {
		t.Fatal(err)
	}
	if err := Resume(context.Background(), store, "r", "confirm", "yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "r", "go"); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if len(outcomes) != 2 || outcomes[0] || outcomes[1] {
		t.Fatalf("AwaitFor outcomes across re-runs of one call = %v, want [false false] (timeout, then timeout)", outcomes)
	}
}

// AwaitFor inside a sub-agent schedules its wake for the root run, which the waker's resume
// callback (the root agent) can drive; the woken tree completes on the timeout branch.
func TestSubAgentAwaitFor_WakesTheRoot(t *testing.T) {
	store := NewMemStore()
	var clk int64 = 1000
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	watch := Func("watch", "wait for a webhook", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		_, ok, err := AwaitFor[string](ctx, "webhook", time.Minute)
		if err != nil {
			return "", err
		}
		if ok {
			return "signaled", nil
		}
		return "timed out", nil
	})
	root := clerkTree(store, watch)
	var woken []string
	var completed bool
	w := NewMemWaker(func(ctx context.Context, runID string) error {
		woken = append(woken, runID)
		msg, err := root.Run(ctx, runID, "go")
		completed = err == nil && msg.Text() == "parent done"
		return err
	})
	ctx := ContextWithWaker(ContextWithClock(context.Background(), now), w)
	_, err := root.Run(ctx, "p", "go")
	var aw *Awaiting
	if !errors.As(err, &aw) || aw.RunID != "p>s1" || aw.RootRunID != "p" {
		t.Fatalf("await = %+v (%v); want RunID p>s1 and RootRunID p", aw, err)
	}
	atomic.StoreInt64(&clk, 2000)
	if _, err := w.Fire(ctx, now()); err != nil {
		t.Fatalf("Fire: %v (woke %v)", err, woken)
	}
	if len(woken) != 1 || woken[0] != "p" || !completed {
		t.Fatalf("woke %v (completed=%v); want the root run p woken and completed", woken, completed)
	}
	if rec, ok := hasStep(t, store, "p>s1", ToolResultStep("c1")); !ok || string(rec.Result) != `"timed out"` {
		t.Fatalf("sub-run recorded %s for the await, want the timeout branch", rec.Result)
	}
}
