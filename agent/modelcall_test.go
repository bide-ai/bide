package agent

import (
	"context"
	"testing"
)

// Two calls that add a hook to the same parent call (two hedged targets through the same
// middleware) each keep their own hook: adding one must not overwrite the other's, even when the
// parent's hook slice has spare capacity.
func TestAddHook_SiblingsKeepTheirOwnHooks(t *testing.T) {
	var got []string
	mark := func(s string) ModelCallHook {
		return ModelCallHook{After: func(context.Context, ModelCall, ModelAttempt) { got = append(got, s) }}
	}
	parent := ModelCall{hooks: make([]ModelCallHook, 0, 8)}
	for range 3 {
		parent = parent.AddHook(ModelCallHook{})
	}
	parent.hooks = append(parent.hooks[:3:3], make([]ModelCallHook, 0, 8)...) // spare capacity
	a := parent.AddHook(mark("a"))
	b := parent.AddHook(mark("b"))
	for _, c := range []ModelCall{a, b} {
		for _, h := range c.hooks {
			if h.After != nil {
				h.After(context.Background(), c, ModelAttempt{})
			}
		}
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("hooks ran %v, want [a b]", got)
	}
	if len(parent.hooks) != 3 {
		t.Fatalf("the parent has %d hooks after its children added theirs, want 3", len(parent.hooks))
	}
}
