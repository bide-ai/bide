package plan

import (
	"context"
	"strings"
	"testing"
)

// TestBuildValidFlow builds a coherent flow (entry consumes In, a Switch whose
// arms all terminate producing Out) and asserts Build succeeds and freezes a
// distinct spec.
func TestBuildValidFlow(t *testing.T) {
	b := New[int, string]("valid")
	start := b.Step("start", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	yes := b.Step("yes", func(context.Context, int) (string, error) { return "yes", nil })
	no := b.Step("no", func(context.Context, int) (string, error) { return "no", nil })
	b.Switch(start,
		When(func(n int) bool { return n > 0 }, yes),
		Else(no),
	)

	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if flow == nil {
		t.Fatal("Build returned nil flow")
	}

	// The spec is sealed: mutating the original builder must not change the flow.
	before := len(flow.core.nodes)
	b.Step("late", func(context.Context, int) (string, error) { return "late", nil })
	if len(flow.core.nodes) != before {
		t.Errorf("sealed flow saw a post-Build node: nodes = %d, want %d", len(flow.core.nodes), before)
	}
}

// TestBuildDuplicateName asserts a duplicate step name (recorded during
// construction) surfaces at Build naming the offending step.
func TestBuildDuplicateName(t *testing.T) {
	b := New[int, int]("dup")
	b.Step("same", func(context.Context, int) (int, error) { return 0, nil })
	b.Step("same", func(context.Context, int) (int, error) { return 1, nil })

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a duplicate step name")
	}
	if !strings.Contains(err.Error(), "same") {
		t.Errorf("error %q does not name the duplicate step %q", err, "same")
	}
}

// TestBuildEntryConsumesIn asserts Build rejects a flow whose entry step does not
// consume the pinned In, naming the entry.
func TestBuildEntryConsumesIn(t *testing.T) {
	b := New[int, int]("entry")
	// Entry consumes string, but the flow input is int.
	b.Step("bad-entry", func(context.Context, string) (int, error) { return 0, nil })

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted an entry that does not consume In")
	}
	if !strings.Contains(err.Error(), "bad-entry") {
		t.Errorf("error %q does not name the entry step", err)
	}
}

// TestBuildTerminalProducesOut asserts Build rejects a flow whose terminal step
// does not produce the pinned Out, naming the terminal.
func TestBuildTerminalProducesOut(t *testing.T) {
	b := New[int, string]("terminal")
	// Terminal (no outgoing edge, not switched over) produces int, not string.
	b.Step("bad-terminal", func(context.Context, int) (int, error) { return 0, nil })

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a terminal that does not produce Out")
	}
	if !strings.Contains(err.Error(), "bad-terminal") {
		t.Errorf("error %q does not name the terminal step", err)
	}
}

// TestBuildUnreachable asserts Build rejects a step unreachable from the entry,
// naming the orphan.
func TestBuildUnreachable(t *testing.T) {
	b := New[int, string]("unreachable")
	b.Step("entry", func(context.Context, int) (string, error) { return "", nil })  // entry, terminal, produces Out
	b.Step("orphan", func(context.Context, int) (string, error) { return "", nil }) // never wired

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted an unreachable step")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Errorf("error %q does not name the orphan step", err)
	}
}

// TestBuildTwoElse asserts Build rejects a Switch with more than one Else arm,
// naming the switched-over step.
func TestBuildTwoElse(t *testing.T) {
	b := New[int, string]("two-else")
	over := b.Step("over", func(_ context.Context, n int) (int, error) { return n, nil })
	a := b.Step("a", func(context.Context, int) (string, error) { return "a", nil })
	c := b.Step("c", func(context.Context, int) (string, error) { return "c", nil })
	b.Switch(over, Else(a), Else(c))

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a Switch with two Else arms")
	}
	if !strings.Contains(err.Error(), "over") {
		t.Errorf("error %q does not name the switched step", err)
	}
	if !strings.Contains(err.Error(), "Else") {
		t.Errorf("error %q does not mention the Else violation", err)
	}
}

// TestBuildEmptyFlow asserts Build rejects a flow with no steps: it can neither
// consume In nor produce Out.
func TestBuildEmptyFlow(t *testing.T) {
	b := New[int, string]("empty")
	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a flow with no steps")
	}
	if !strings.Contains(err.Error(), "no steps") {
		t.Errorf("error %q does not explain the empty flow", err)
	}
}

// TestBuildSwitchArmTerminalMustProduceOut asserts that when a Switch arm target
// is itself a terminal producing the wrong type, Build names it. This exercises
// the "every arm leads to a terminal producing Out" requirement.
func TestBuildSwitchArmTerminalMustProduceOut(t *testing.T) {
	b := New[int, string]("arm-out")
	over := b.Step("over", func(_ context.Context, n int) (int, error) { return n, nil })
	good := b.Step("good", func(context.Context, int) (string, error) { return "ok", nil })
	bad := b.Step("bad-arm", func(context.Context, int) (int, error) { return 0, nil }) // terminal, wrong Out
	b.Switch(over,
		When(func(n int) bool { return n > 0 }, good),
		Else(bad),
	)

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a Switch arm terminal producing the wrong Out")
	}
	if !strings.Contains(err.Error(), "bad-arm") {
		t.Errorf("error %q does not name the offending arm terminal", err)
	}
}
