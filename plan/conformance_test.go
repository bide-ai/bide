package plan

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// TestConformCleanRun builds a small switched flow, runs it against a MemStore to
// produce a real journal, then asserts Conform reports the run followed the
// declared topology: ok is true and diffs is empty. This is the positive
// accountability case: the journal names only declared nodes and the recorded
// Switch choice picked a declared arm.
func TestConformCleanRun(t *testing.T) {
	b := New[int, string]("conform-clean")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, high),
		Else(low),
	)

	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mem := agent.NewMemStore()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "clean", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ok, diffs, err := flow.Conform(ctx, mem, "clean")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Errorf("Conform reported divergence on a clean run: %v", diffs)
	}
	if len(diffs) != 0 {
		t.Errorf("Conform returned diffs %v on a clean run, want none", diffs)
	}
}

// TestConformUnexpectedStep hand-crafts a journal (via store.Do) that contains a
// step name NOT in the declared topology, alongside the run's real records, and
// asserts Conform reports the intruder as a divergence: ok is false and diffs
// names the unexpected step. This is the negative accountability case.
func TestConformUnexpectedStep(t *testing.T) {
	b := New[int, string]("conform-intruder")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, high),
		Else(low),
	)

	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mem := agent.NewMemStore()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "intruder", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Inject a step name that no declared node owns. store.Do memoizes it into the
	// same run's journal, so History will surface it to Conform.
	if _, err := mem.Do(ctx, "intruder", "ghost", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"boo"`)}, nil
	}); err != nil {
		t.Fatalf("inject ghost step: %v", err)
	}

	ok, diffs, err := flow.Conform(ctx, mem, "intruder")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if ok {
		t.Fatal("Conform reported ok on a journal with an undeclared step")
	}
	found := false
	for _, d := range diffs {
		if contains(d, "ghost") {
			found = true
		}
	}
	if !found {
		t.Errorf("Conform diffs %v do not name the undeclared step %q", diffs, "ghost")
	}
}

// TestConformUnreachableArm hand-crafts a "switch:<over>" record whose recorded
// chosen target is not a declared arm of that Switch, and asserts Conform flags
// the unreachable arm as a divergence. It uses a runID with no prior records so
// the only record present is the crafted choice.
func TestConformUnreachableArm(t *testing.T) {
	b := New[int, string]("conform-arm")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, high),
		Else(low),
	)

	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mem := agent.NewMemStore()
	ctx := context.Background()

	// A Switch choice record whose target is not a declared arm of the entry Switch.
	if _, err := mem.Do(ctx, "arm", "switch:entry", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"nowhere"`)}, nil
	}); err != nil {
		t.Fatalf("inject switch choice: %v", err)
	}

	ok, diffs, err := flow.Conform(ctx, mem, "arm")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if ok {
		t.Fatal("Conform reported ok on a Switch choice to an undeclared arm")
	}
	found := false
	for _, d := range diffs {
		if contains(d, "switch:entry") && contains(d, "nowhere") {
			found = true
		}
	}
	if !found {
		t.Errorf("Conform diffs %v do not flag the unreachable arm choice", diffs)
	}
}

// TestConformHaltedRunIsObservable asserts a partial journal (an attempt marker
// present without its result, as a *HaltAmbiguous run leaves behind) is NOT a
// divergence: the attempt maps to its declared node, and a missing result is an
// in-flight state, not an unexpected step. Conform must not panic and must report
// ok on such a journal.
func TestConformHaltedRunIsObservable(t *testing.T) {
	b := New[int, string]("conform-halt")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, high),
		Else(low),
	)

	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mem := agent.NewMemStore()
	ctx := context.Background()

	// Simulate a run halted mid-node: the attempt marker for the entry node is
	// recorded, but its result never was.
	if _, err := mem.Do(ctx, "halt", attemptMarker("entry"), func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue}, nil
	}); err != nil {
		t.Fatalf("inject attempt marker: %v", err)
	}

	ok, diffs, err := flow.Conform(ctx, mem, "halt")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Errorf("Conform flagged a halted in-flight journal as divergent: %v", diffs)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
