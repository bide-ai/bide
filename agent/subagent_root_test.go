package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// clerkTree builds a root agent whose one tool is a sub-agent ("clerk", call s1) that calls
// inner (call c1) and then answers "sub done".
func clerkTree(store Durable, inner Tool) *Agent {
	sub := New(NewScriptedModel(ToolTurn("c1", inner.Name(), `{}`), TextTurn("sub done")), store, inner)
	return New(NewScriptedModel(ToolTurn("s1", "clerk", `{"task":"do it"}`), TextTurn("parent done")), store, SubAgent("clerk", "does it", sub))
}

// A halt inside a sub-agent is resolved against the sub-run and continued from the root: the
// signal says which run to re-invoke, and doing so completes the whole tree.
func TestSubAgentHalt_ResolvedAndContinuedFromTheRoot(t *testing.T) {
	store := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	charge := Func("charge", "charge", Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		cancel() // the charge went out; the run is cut off before it reports back
		return "", ctx.Err()
	})
	root := clerkTree(store, charge)
	_, _ = root.Run(ctx, "p", "go")
	_, err := root.Run(context.Background(), "p", "go")
	var halt *ResumeHalt
	if !errors.As(err, &halt) || halt.RunID != "p>s1" || halt.RootRunID != "p" {
		t.Fatalf("halt = %+v (%v); want RunID p>s1 and RootRunID p", halt, err)
	}
	if err := ResolveHalt(context.Background(), store, halt.RunID, halt.ToolUseID, "charged", false); err != nil {
		t.Fatal(err)
	}
	msg, err := root.Run(context.Background(), halt.RootRunID, "go")
	if err != nil || msg.Text() != "parent done" {
		t.Fatalf("continue from the root = %q, %v", msg.Text(), err)
	}
	if rec, ok := hasStep(t, store, "p", ToolResultStep("s1")); !ok || string(rec.Result) != `"sub done"` {
		t.Fatalf("the parent recorded %s for the sub-agent, want its own answer", rec.Result)
	}
}

// An Interrupt inside a sub-agent: answer it against the sub-run, continue from the root.
func TestSubAgentInterrupt_AnsweredAndContinuedFromTheRoot(t *testing.T) {
	store := NewMemStore()
	ask := Func("ask", "ask", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		return Interrupt[string](ctx, "confirm", "ok?")
	})
	root := clerkTree(store, ask)
	_, err := root.Run(context.Background(), "p", "go")
	var intr *Interrupted
	if !errors.As(err, &intr) || intr.RunID != "p>s1" || intr.RootRunID != "p" {
		t.Fatalf("interrupt = %+v (%v); want RunID p>s1 and RootRunID p", intr, err)
	}
	if err := Resume(context.Background(), store, intr.RunID, intr.Key, "yes"); err != nil {
		t.Fatal(err)
	}
	if msg, err := root.Run(context.Background(), intr.RootRunID, "go"); err != nil || msg.Text() != "parent done" {
		t.Fatalf("continue from the root = %q, %v", msg.Text(), err)
	}
}

// A Sleep inside a sub-agent schedules a wake for the root run, which the waker's resume
// callback (the root agent) can drive; the woken run completes.
func TestSubAgentSleep_WakesTheRoot(t *testing.T) {
	store := NewMemStore()
	var fired bool
	nap := Func("nap", "wait a moment", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		if err := Sleep(ctx, "nap", 10*time.Millisecond); err != nil {
			return "", err
		}
		return "rested", nil
	})
	root := clerkTree(store, nap)
	var woken []string
	w := NewMemWaker(func(ctx context.Context, runID string) error {
		woken = append(woken, runID)
		msg, err := root.Run(ctx, runID, "go")
		fired = err == nil && msg.Text() == "parent done"
		return err
	})
	_, err := root.Run(WithWaker(context.Background(), w), "p", "go")
	var slp *Sleeping
	if !errors.As(err, &slp) || slp.RootRunID != "p" {
		t.Fatalf("sleep = %+v (%v); want RootRunID p", slp, err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := w.Fire(WithWaker(context.Background(), w), time.Now()); err != nil {
		t.Fatalf("Fire: %v (woke %v)", err, woken)
	}
	if len(woken) != 1 || woken[0] != "p" || !fired {
		t.Fatalf("woke %v (completed=%v); want the root run p woken and completed", woken, fired)
	}
}
