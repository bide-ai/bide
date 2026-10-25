package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ClaimAttempt stamps a marker's AttemptedAt from the wall clock, whatever clock the context carries.
// The marker's age is what WithMinHaltAge and the notifier's halt age measure, so a context clock that
// lags the wall clock (a run driven with WithClock) must not make a marker claimed this instant look old.

var claimClockLag = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func laggingCtx() context.Context {
	return contextWithClock(context.Background(), func() time.Time { return claimClockLag })
}

func TestClaimAttempt_IsStampedFromTheWallClockUnderALaggingContextClock(t *testing.T) {
	j := memJournal()
	before := time.Now()
	won, got, err := ClaimAttempt(laggingCtx(), j, "r", "attempt:step:x", Record{Kind: StepAttempt, ToolUseID: "x"})
	if err != nil || !won {
		t.Fatal(won, err)
	}
	at := markerTime(got.AttemptedAt)
	if at.Before(before.Truncate(time.Millisecond)) || at.After(time.Now()) {
		t.Fatalf("AttemptedAt = %v, want the wall clock (now is %v)", at, time.Now())
	}
	if _, _, err := ClaimAttempt(laggingCtx(), j, "r", "attempt:step:y", Record{Kind: StepAttempt, ToolUseID: "y", AttemptedAt: 1}); !errors.Is(err, ErrConfig) {
		t.Fatalf("a caller-set AttemptedAt: %v, want ErrConfig", err)
	}
}

func TestClaimAttempt_ALaggingContextClockDoesNotShortenTheMinimumHaltAge(t *testing.T) {
	j := memJournal()
	if won, _, err := ClaimAttempt(laggingCtx(), j, "r", "attempt:step:x", Record{Kind: StepAttempt, ToolUseID: "x"}); err != nil || !won {
		t.Fatal(won, err)
	}
	ref := HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: "x"}, Cause: HaltCrashed}
	if err := ResolveHalt(context.Background(), j, ref, Outcome{Result: "x"}, WithMinHaltAge(time.Hour)); !errors.As(err, new(*HaltTooYoung)) {
		t.Fatalf("resolve right away with a minimum of 1h: %v, want *HaltTooYoung", err)
	}
	later := func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := ResolveHalt(context.Background(), j, ref, Outcome{Result: "x"}, WithMinHaltAge(time.Hour), WithClock(later)); err != nil {
		t.Fatalf("resolve two hours later: %v", err)
	}
}

func TestClaimAttempt_ALaggingContextClockDoesNotMakeTheSweepReportAFreshClaim(t *testing.T) {
	j := mustJournal(unwrapFault{newFaultStore()}) // Unwrap lets the sweep find the MemStore's Lister
	if won, _, err := ClaimAttempt(laggingCtx(), j, "r", "attempt:step:x", Record{Kind: StepAttempt, ToolUseID: "x"}); err != nil || !won {
		t.Fatal(won, err)
	}
	clk := &notifyClock{t: time.Now().Add(time.Minute)}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 0 {
		t.Fatalf("a claim one minute old was reported as a halt: %v, %d", err, sink.count())
	}
	clk.t = time.Now().Add(10 * time.Minute)
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 1 {
		t.Fatalf("a claim ten minutes old: %v, %d", err, sink.count())
	}
}

// unwrapFault exposes the MemStore under a faultStore, so Capability finds its Lister.
type unwrapFault struct{ *faultStore }

func (u unwrapFault) Unwrap() Store { return u.faultStore.Store }
