package plan

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Run derives every journal key from node names with ':' ("node:<node>",
// "node:iter:<n>:<node>", "switch:<node>", and the reserved "flow:digest"), so a node name with a
// ':' could mimic one of them. Before names were checked (and when a node's key was its bare name),
// the node "attempt:x" found x's attempt marker and returned it as its own memoized result, and
// its body never ran.
func TestBuild_NodeNamedAsAnotherNodesAttemptMarkerIsRejected(t *testing.T) {
	var ran int
	b := New[int, int]("keys")
	x := b.Step("x", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	y := b.Step("attempt:x", func(_ context.Context, n int) (int, error) { ran++; return n * 10, nil })
	b.Edge(x, y)
	flow, err := b.Build()
	if err == nil {
		out, runErr := flow.Run(context.Background(), agenttest.MemJournal(), "r", 1)
		t.Fatalf("Build accepted node %q; the run returned %d, %v with its body run %d times", "attempt:x", out, runErr, ran)
	}
	if !strings.Contains(err.Error(), "attempt:x") {
		t.Fatalf("Build error %v does not name the node", err)
	}
}

// Every name that can mimic one of Run's derived keys is refused, whatever the key.
func TestBuild_NodeNameWithColonIsRejected(t *testing.T) {
	for _, name := range []string{"flow:digest", "switch:x", "iter:0:x", "attempt:x", "node:x", ":"} {
		b := New[int, int]("keys")
		x := b.Step("x", func(_ context.Context, n int) (int, error) { return n, nil })
		y := b.Step(name, func(_ context.Context, n int) (int, error) { return n, nil })
		b.Edge(x, y)
		want := fmt.Sprintf("step name %q contains ':'", name)
		if _, err := b.Build(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("name %q: Build err = %v, want %q", name, err, want)
		}
	}
}

// A loaded config goes through the same check, and Validate reports it too.
func TestValidate_NodeNameWithColonIsRejected(t *testing.T) {
	cfg := strings.NewReplacer(
		`{"name": "decline",  "block": "decline"}`, `{"name": "switch:classify",  "block": "decline"}`,
		`"else": "decline"`, `"else": "switch:classify"`,
	).Replace(triageConfig)
	want := `step name "switch:classify" contains ':'`
	if err := Validate([]byte(cfg), triageRegistry(t)); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Validate err = %v, want %q", err, want)
	}
	if _, err := Load[cfgOrder, cfgReceipt]([]byte(cfg), triageRegistry(t)); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Load err = %v, want %q", err, want)
	}
}

// A join creates its node from the wiring, and its name is checked the same way.
func TestValidate_JoinNameWithColonIsRejected(t *testing.T) {
	cfg := strings.Replace(diamondConfig, `"join": "merge"`, `"join": "attempt:y"`, 1)
	want := `step name "attempt:y" contains ':'`
	if err := Validate([]byte(cfg), diamondRegistry(t)); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Validate err = %v, want %q", err, want)
	}
}

// Every key Run writes is one agent reserves, so no agent.Step a node body runs can name it, and a
// node's key is one the engine's step hook and ResolveHaltRef accept as a plan node's.
func TestRunKeys_AreReserved(t *testing.T) {
	for _, k := range []string{nodeKey("x"), iterNodeKey(3, "x"), iterNodeKey(0, "iter"), nodeKey("iter"),
		"switch:x", iterSwitchKey(2, "x"), flowDigestStep, runStartStep} {
		if !agent.IsReservedStepName(k) {
			t.Errorf("key %q is not reserved by agent", k)
		}
	}
	if _, err := agent.Step(context.Background(), agenttest.MemJournal(), "r", nodeKey("x"),
		func(context.Context) (int, error) { return 1, nil }); err == nil {
		t.Fatalf("agent.Step accepted the node key %q", nodeKey("x"))
	}
}

// nodeOfKey inverts nodeKey and iterNodeKey, and refuses every other key.
func TestNodeOfKey(t *testing.T) {
	for key, want := range map[string]string{
		nodeKey("a"): "a", iterNodeKey(0, "a"): "a", iterNodeKey(12, "iter"): "iter", nodeKey("iter"): "iter",
	} {
		if got, ok := nodeOfKey(key); !ok || got != want {
			t.Errorf("nodeOfKey(%q) = %q, %v; want %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"a", "iter:0:a", "switch:a", "flow:digest", "attempt:step:node:a"} {
		if got, ok := nodeOfKey(key); ok {
			t.Errorf("nodeOfKey(%q) = %q, true; want not a node key", key, got)
		}
	}
	for name, want := range map[string]string{
		"attempt:step:node:a": "node:a", "attempt:retry:3:step:node:iter:1:a": "node:iter:1:a",
	} {
		if got, ok := attemptedStep(name); !ok || got != want {
			t.Errorf("attemptedStep(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"attempt:tool:a", "attempt:retry:x:step:a", "attempt:retry:1:tool:a", "attempt:not-started:ab:attempt:step:node:a", "node:a"} {
		if got, ok := attemptedStep(name); ok {
			t.Errorf("attemptedStep(%q) = %q, true; want not a step's marker", name, got)
		}
	}
}
