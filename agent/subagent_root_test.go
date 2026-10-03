package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// clerkTree builds a root agent whose one tool is a sub-agent ("clerk", call s1) that calls
// inner (call c1) and then answers "sub done".
func clerkTree(store *Journal, inner Tool) *Agent {
	sub := mustNew(
		NewScriptedModel(ToolTurn("c1", inner.Spec().Name, `{}`), TextTurn("sub done")),
		store,
		WithTools(inner),
	)
	return mustNew(
		NewScriptedModel(ToolTurn("s1", "clerk", `{"task":"do it"}`), TextTurn("parent done")),
		store,
		WithTools(MustSubAgent("clerk", "does it", sub)),
	)
}

// A halt inside a sub-agent is resolved against the sub-run and continued from the root: the
// signal says which run to re-invoke, and doing so completes the whole tree.
func TestSubAgentHalt_ResolvedAndContinuedFromTheRoot(t *testing.T) {
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	charge := MustFunc("charge", "charge", func(ctx context.Context, _ struct{}) (string, error) {
		cancel() // the charge went out; the run is cut off before it reports back
		return "", ctx.Err()
	})
	root := clerkTree(store, charge)
	_, _ = root.Run(ctx, "p", UserText("go"))
	_, err := root.Run(context.Background(), "p", UserText("go"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.RunID != "p>s1" || halt.RootRunID != "p" {
		t.Fatalf("halt = %+v (%v); want RunID p>s1 and RootRunID p", halt, err)
	}
	if err := ResolveHalt(context.Background(), store, HaltRef{RunID: halt.RunID, Op: OpRef{Kind: OpTool, ID: halt.Op.ID}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}); err != nil {
		t.Fatal(err)
	}
	res, err := root.Run(context.Background(), halt.RootRunID, UserText("go"))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "parent done" {
		t.Fatalf("continue from the root = %q, %v", msg.Text(), err)
	}
	if rec, ok := hasStep(t, store, "p", ToolResultStep("s1")); !ok || string(rec.Result) != `"sub done"` {
		t.Fatalf("the parent recorded %s for the sub-agent, want its own answer", rec.Result)
	}
}

// An Interrupt inside a sub-agent: answer it against the sub-run, continue from the root.
func TestSubAgentInterrupt_AnsweredAndContinuedFromTheRoot(t *testing.T) {
	store := memJournal()
	ask := MustFunc("ask", "ask", func(ctx context.Context, _ struct{}) (string, error) {
		return Interrupt[string](ctx, "confirm", "ok?")
	}, WithSafety(Safety{ReadOnly: true}))
	root := clerkTree(store, ask)
	_, err := root.Run(context.Background(), "p", UserText("go"))
	var intr *InterruptPending
	if !errors.As(err, &intr) || intr.RunID != "p>s1" || intr.RootRunID != "p" {
		t.Fatalf("interrupt = %+v (%v); want RunID p>s1 and RootRunID p", intr, err)
	}
	if err := store.AnswerInterrupt(context.Background(), intr.RunID, intr.Name, "yes"); err != nil {
		t.Fatal(err)
	}
	if msg, err := answerOf(root.Run(context.Background(), intr.RootRunID, UserText("go"))); err != nil || msg.Text() != "parent done" {
		t.Fatalf("continue from the root = %q, %v", msg.Text(), err)
	}
}

// A Sleep inside a sub-agent schedules a wake for the root run, which the waker's resume
// callback (the root agent) can drive; the woken run completes.
func TestSubAgentSleep_WakesTheRoot(t *testing.T) {
	store := memJournal()
	var fired bool
	nap := MustFunc("nap", "wait a moment", func(ctx context.Context, _ struct{}) (string, error) {
		if err := Sleep(ctx, "nap", 10*time.Millisecond); err != nil {
			return "", err
		}
		return "rested", nil
	}, WithSafety(Safety{ReadOnly: true}))
	root := clerkTree(store, nap)
	var woken []string
	w := NewMemWaker(func(ctx context.Context, runID string) error {
		woken = append(woken, runID)
		res, err := root.Run(ctx, runID, UserText("go"))
		var msg Message
		if res != nil {
			msg = res.Message
		}
		fired = err == nil && msg.Text() == "parent done"
		return err
	})
	_, err := root.Run(context.Background(), "p", UserText("go"), WithWaker(w))
	var slp *TimerPending
	if !errors.As(err, &slp) || slp.RootRunID != "p" {
		t.Fatalf("sleep = %+v (%v); want RootRunID p", slp, err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := w.Fire(contextWithWaker(context.Background(), w), time.Now()); err != nil {
		t.Fatalf("Fire: %v (woke %v)", err, woken)
	}
	if len(woken) != 1 || woken[0] != "p" || !fired {
		t.Fatalf("woke %v (completed=%v); want the root run p woken and completed", woken, fired)
	}
}
