package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ResolveHaltRef's live-driver check finds the run's Leaser through a Journal and through store
// wrappers that implement Unwrap() Store, as Lease and Recover do: a resolution through a Journal
// over a wrapped MemStore is refused while a driver holds the root run's lease, and goes through
// once it is released. A Journal over a store that exposes no Leaser falls back to the rule for a
// store that cannot lease runs: WithMinHaltAge, or the explicit opt-out.
func TestResolveHaltRef_FindsTheLeaserThroughAJournal(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	j := newJournal(unwrapStore{unwrapStore{mem}})
	if won, _, err := ClaimAttempt(ctx, j, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	if ok, err := mem.AcquireLease(ctx, "r1", "worker-1", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	err := ResolveHalt(ctx, j, ref, Outcome{Result: "charged"})
	if _, ok := errors.AsType[*HaltInFlight](err); !ok {
		t.Fatalf("resolving through a Journal under a live lease = %v; want *HaltInFlight", err)
	}
	if err := mem.ReleaseLease(ctx, "r1", "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHalt(ctx, j, ref, Outcome{Result: "charged"}); err != nil {
		t.Fatalf("resolving once the lease is released = %v", err)
	}

	hidden := newJournal(plainStore{NewMemStore()}) // no Unwrap: its Leaser is not exposed
	if won, _, err := ClaimAttempt(ctx, hidden, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	if err := ResolveHalt(ctx, hidden, ref, Outcome{Result: "x"}); !errors.Is(err, ErrConfig) {
		t.Fatalf("a Journal over a store with no Leaser, no minimum age = %v; want ErrConfig", err)
	}
	if err := ResolveHalt(ctx, hidden, ref, Outcome{Result: "x"}, WithMinHaltAge(time.Second)); err != nil {
		t.Fatalf("with an old enough attempt = %v", err)
	}
}

// A resolution claims the attempt after the live one, on the lease path as on the min-age path,
// and journals that claim as an attempt marker of the operation; WithoutLiveDriverCheck skips it
// with the other checks.
func TestResolveHaltRef_ClaimsTheNextAttemptUnlessOptedOut(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		opts  []ResolveOption
		store func() *Journal
		claim bool
	}{
		"lease":   {nil, func() *Journal { return memJournal() }, true},
		"min age": {[]ResolveOption{WithMinHaltAge(time.Second)}, func() *Journal { return newJournal(plainStore{NewMemStore()}) }, true},
		"opt-out": {[]ResolveOption{WithoutLiveDriverCheck()}, func() *Journal { return memJournal() }, false},
	} {
		t.Run(name, func(t *testing.T) {
			d := tc.store()
			if won, _, err := ClaimAttempt(ctx, d, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
				t.Fatal(won, err)
			}
			ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
			if err := ResolveHalt(ctx, d, ref, Outcome{Result: "charged"}, tc.opts...); err != nil {
				t.Fatal(err)
			}
			rec, ok, err := d.Get(ctx, "r1", retryAttemptStep(toolAttemptStep("c1"), 1))
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.claim || ok && (rec.Kind != StepAttempt || !isToolAttempt(rec)) {
				t.Fatalf("the resolution's claim of the next attempt: present %v (%+v), want %v", ok, rec, tc.claim)
			}
		})
	}
}
