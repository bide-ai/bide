package plan

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A node's name is its journal key, and Run derives its other keys from node names with ':'
// ("attempt:<key>", "iter:<n>:<node>", "switch:<node>", and the reserved "flow:digest"). A
// node named like one of those keys shares a record with another step. Here "attempt:x" is
// the key of x's attempt marker: before names were checked, the node "attempt:x" found the
// marker and returned it as its own memoized result, and its body never ran.
func TestBuild_NodeNamedAsAnotherNodesAttemptMarkerIsRejected(t *testing.T) {
	var ran int
	b := New[int, int]("keys")
	x := b.Step("x", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	y := b.Step("attempt:x", func(_ context.Context, n int) (int, error) { ran++; return n * 10, nil })
	b.Edge(x, y)
	flow, err := b.Build()
	if err == nil {
		out, runErr := flow.Run(context.Background(), agent.NewMemStore(), "r", 1)
		t.Fatalf("Build accepted node %q; the run returned %d, %v with its body run %d times", "attempt:x", out, runErr, ran)
	}
	if !strings.Contains(err.Error(), "attempt:x") {
		t.Fatalf("Build error %v does not name the node", err)
	}
}

// Every name that can mimic one of Run's derived keys is refused, whatever the key.
func TestBuild_NodeNameWithColonIsRejected(t *testing.T) {
	for _, name := range []string{"flow:digest", "switch:x", "iter:0:x", "attempt:x", ":"} {
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
