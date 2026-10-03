package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A Step driver that loses the claim to a call of the step in flight in this process has seen its
// claimant live: that driver owns the effect and may be running it now, so the halt is
// HaltContended, which ResolveHaltRef does not resolve without WithMinHaltAge. A driver that loses
// the claim with no call in flight knows of no live claimant (the owner may have died), so its
// halt is HaltCrashed, as for a marker found on resume.
//
// The flight is placed by hand so the interleaving is exact: it is what the loser finds when the
// owner's call is in flight and fails (its effect may have fired) while the loser waits on it.
func TestStep_LoserOfALiveClaimHaltsContended(t *testing.T) {
	ctx := context.Background()
	j := newJournal(NewMemStore())
	won, _, _, err := j.claimNext(ctx, "r", stepAttemptStep("s"),
		Record{Kind: StepAttempt, ToolUseID: "s", AttemptedAt: time.Now().UnixMilli()})
	if err != nil || !won {
		t.Fatalf("the owner's claim = %v, %v", won, err)
	}
	k := flightKey{j.id, "r", "s"}
	flights.mu.Lock()
	flights.m[k] = &flight{err: errors.New("provider timeout")}
	flights.mu.Unlock()
	ran := 0
	fn := func(context.Context) (int, error) { ran++; return 1, nil }
	_, err = j.Step(ctx, "r", "s", fn)
	flights.mu.Lock()
	delete(flights.m, k)
	flights.mu.Unlock()
	halt, ok := errors.AsType[*OutcomeUnknown](err)
	if !ok || halt.Op != (OpRef{Kind: OpStep, ID: "s"}) || halt.Cause != HaltContended {
		t.Fatalf("the loser of a live claim = %v (%+v); want an OpStep halt on s with Cause %q", err, halt, HaltContended)
	}
	if ran != 0 {
		t.Fatalf("the loser ran fn %d times, want 0", ran)
	}
	ref := halt.Ref()
	if err := ResolveHalt(ctx, j, ref, Outcome{Result: 1}); !errors.Is(err, ErrConfig) {
		t.Errorf("resolving the contended halt without WithMinHaltAge = %v; want ErrConfig", err)
	}

	// No call in flight: the owner may have died, and nothing says it is live.
	_, err = j.Step(ctx, "r", "s", fn)
	halt, ok = errors.AsType[*OutcomeUnknown](err)
	if !ok || halt.Cause != HaltCrashed {
		t.Fatalf("a loser with no call in flight = %v (%+v); want Cause %q", err, halt, HaltCrashed)
	}
	if ran != 0 {
		t.Fatalf("the loser ran fn %d times, want 0", ran)
	}
}

// P10's rule (#90, F3) for a tool whose error chain holds a halt or a pending approval: it
// propagates as itself, recorded as nothing, whatever the tool's safety. It holds when the chain
// also holds a side-effect Step's refusal to pause (*stepPauseError): the halt comes back, and the
// call records nothing, so the model is not told the call failed.
func TestStepPauseGuard_JoinedWithAHaltPropagatesTheHalt(t *testing.T) {
	store := memJournal()
	mixed := Func("mixed", "", Safety{Idempotent: true}, func(ctx context.Context, _ struct{}) (string, error) {
		d, runID, _ := runContext(ctx)
		_, guard := d.Step(ctx, runID, "confirm", func(ctx context.Context) (int, error) {
			return Interrupt[int](ctx, "q", nil)
		})
		_, _, _ = ClaimAttempt(ctx, d, runID, stepAttemptStep("inner"), Record{Kind: StepAttempt, ToolUseID: "inner", AttemptedAt: 1})
		_, halt := d.Step(ctx, runID, "inner", func(context.Context) (int, error) { return 1, nil })
		return "", errors.Join(guard, halt)
	})
	_, err := mustNew(
		&greedyModel{script: [][]Emit{toolTurn("c1", "mixed", `{}`), textTurn("done")}},
		store,
		WithTools(mixed),
	).Run(context.Background(), "r1", UserText("go"))
	halt, ok := errors.AsType[*OutcomeUnknown](err)
	if !ok || halt.Op.ID != "inner" {
		t.Fatalf("run = %v; want the inner step's halt", err)
	}
	if _, ok := errors.AsType[*stepPauseError](err); !ok {
		t.Errorf("run = %v; want the guard's error kept in the chain beside the halt", err)
	}
	recs, err := store.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == ToolResultStep("c1") {
			t.Fatalf("the call recorded %s; want nothing recorded for it", r.Result)
		}
	}
}
