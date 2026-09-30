package agent

import (
	"context"
	"testing"
)

// Two calls that add a hook to the same parent context (two hedged targets through the same
// middleware) each keep their own hook: adding one must not overwrite the other's.
func TestWithModelCallHook_SiblingsKeepTheirOwnHooks(t *testing.T) {
	ctx := inModelCall(context.Background(), &spendMeter{})
	for range 3 { // grow the parent's hook list so it has spare capacity
		ctx, _ = WithModelCallHook(ctx, ModelCallHook{})
	}
	var got []string
	mark := func(s string) ModelCallHook { return ModelCallHook{After: func(Usage) { got = append(got, s) }} }
	a, _ := WithModelCallHook(ctx, mark("a"))
	b, _ := WithModelCallHook(ctx, mark("b"))
	for _, c := range []context.Context{a, b} {
		for _, h := range modelHooks(c) {
			if h.After != nil {
				h.After(Usage{})
			}
		}
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("hooks ran %v, want [a b]", got)
	}
	if _, ok := WithModelCallHook(context.Background(), mark("x")); ok {
		t.Fatal("WithModelCallHook reported ok outside a model call")
	}
	if _, ok := WithModel(context.Background(), nil); ok {
		t.Fatal("WithModel reported ok outside a model call")
	}
}
