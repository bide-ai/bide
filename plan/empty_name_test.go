package plan

import (
	"context"
	"testing"
)

// A node's name is its journal key, and register makes the first named node the entry. An empty
// name therefore both keys a step to "" and silently moves the entry to the next node. Every empty
// name, of a step or a join, from Go or from a config, is refused.
func TestBuild_RefusesAnEmptyStepName(t *testing.T) {
	id := func(_ context.Context, n int) (int, error) { return n, nil }

	b := New[int, int]("f")
	x := b.Step("x", id)
	y := b.Step("", id)
	b.Edge(x, y)
	if _, err := b.Build(); err == nil {
		t.Errorf("Builder: a step named \"\" built")
	}

	b = New[int, int]("f")
	a := b.Step("a", id)
	p := b.Step("p", id)
	q := b.Step("q", id)
	b.Edge(a, p)
	b.Edge(a, q)
	b.Join2("", p, q, func(_ context.Context, a, c int) (int, error) { return a + c, nil })
	if _, err := b.Build(); err == nil {
		t.Errorf("Builder: a join named \"\" built")
	}

	reg := NewRegistry()
	for _, n := range []string{"a", "b"} {
		if err := RegisterStep(reg, n, id); err != nil {
			t.Fatal(err)
		}
	}
	cfg := `{"flow":"f","nodes":[{"name":"","block":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["b",""]}]}`
	if f, err := Load[int, int]([]byte(cfg), reg); err == nil {
		t.Errorf("Load: a node named \"\" loaded (entry %q)", f.core.entry)
	}

	reg = NewRegistry()
	for _, n := range []string{"a", "b", "c"} {
		if err := RegisterStep(reg, n, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := RegisterJoin2(reg, "sum", func(_ context.Context, a, c int) (int, error) { return a + c, nil }); err != nil {
		t.Fatal(err)
	}
	cfg = `{"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"},{"name":"c","block":"c"}],` +
		`"wiring":[{"edge":["a","b"]},{"edge":["a","c"]},{"inputs":["b","c"],"merge":"sum"}]}`
	if _, err := Load[int, int]([]byte(cfg), reg); err == nil {
		t.Errorf("Load: a join named \"\" loaded")
	}
}
