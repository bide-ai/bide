package agent

import (
	"context"
	"testing"
)

// NextOnceKey numbers a tool call's operations, restarts the numbering when the call runs
// again, gives every step its own numbering, and keeps steps with look-alike names apart.
func TestNextOnceKey(t *testing.T) {
	ctx := context.Background()
	if k := NextOnceKey(ctx); k != "" {
		t.Fatalf("outside a tool call NextOnceKey = %q, want empty", k)
	}
	call := func() []string {
		cctx := withOnceScope(ctx, "r/c1")
		keys := []string{NextOnceKey(cctx), NextOnceKey(cctx)}
		d := memJournal()
		for _, st := range [][2]string{{"a:1", "b"}, {"a", "1:b"}} {
			k, err := Step(cctx, d, st[0], st[1], func(sctx context.Context) (string, error) { return NextOnceKey(sctx), nil })
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
		}
		return keys
	}
	first, again := call(), call()
	seen := map[string]bool{}
	for i, k := range first {
		if k == "" || seen[k] {
			t.Fatalf("keys of one call are empty or repeat: %q", first)
		}
		seen[k] = true
		if again[i] != k {
			t.Fatalf("the call running again got key %q for operation %d, first run %q", again[i], i, k)
		}
	}
}
