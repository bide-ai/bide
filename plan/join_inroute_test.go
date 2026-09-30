package plan

import (
	"context"
	"strings"
	"testing"
)

// A Join reads exactly its declared inputs. An edge or a switch arm into a join from anywhere
// else was accepted: the value it carried was dropped (the merge reads only its inputs), and a
// config edge's types were never checked, since a join has no single input type. Both are refused.
func TestBuild_RefusesAnExtraRouteIntoAJoin(t *testing.T) {
	id := func(_ context.Context, n int) (int, error) { return n, nil }
	sum := func(_ context.Context, a, c int) (int, error) { return a + c, nil }

	b := New[int, int]("f")
	a := b.Step("a", id)
	p := b.Step("p", id)
	q := b.Step("q", id)
	x := b.Step("x", id)
	b.Edge(a, p)
	b.Edge(a, q)
	b.Edge(a, x)
	j := b.Join2("j", p, q, sum)
	b.Edge(x, j)
	if _, err := b.Build(); err == nil {
		t.Errorf("Builder: an extra Edge into a join built")
	}

	// reg registers an int step for each name except "s" (int -> string) and "t" (string -> int), plus the merge
	// and a predicate. Blocks a config does not use are a load error, so it registers only these.
	reg := func(names ...string) *Registry {
		r := NewRegistry()
		for _, n := range names {
			var err error
			switch n {
			case "s":
				err = RegisterStep(r, n, func(context.Context, int) (string, error) { return "", nil })
			case "t":
				err = RegisterStep(r, n, func(context.Context, string) (int, error) { return 0, nil })
			default:
				err = RegisterStep(r, n, id)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := RegisterJoin2(r, "sum", sum); err != nil {
			t.Fatal(err)
		}
		if err := RegisterPredicate(r, "neg", func(n int) bool { return n < 0 }); err != nil {
			t.Fatal(err)
		}
		return r
	}
	cfg := func(names []string, wiring string) string {
		var nodes []string
		for _, n := range names {
			nodes = append(nodes, `{"name":"`+n+`","block":"`+n+`"}`)
		}
		return `{"version":1,"flow":"f","nodes":[` + strings.Join(nodes, ",") + `],"wiring":[{"edge":["a","p"]},{"edge":["a","q"]},{"inputs":["p","q"],"merge":"sum","join":"j"}` + wiring + `]}`
	}
	for name, tc := range map[string]struct {
		names          []string
		control, extra string // the wiring that loads, and the same wiring with the extra route
	}{
		// x's int is dropped at the join.
		"extra edge": {[]string{"a", "p", "q", "x", "y"},
			`,{"edge":["a","x"]},{"edge":["x","y"]}`,
			`,{"edge":["a","x"]},{"edge":["x","j"]},{"edge":["j","y"]}`},
		// s's string reaches a merge of ints: the edge's types are never checked.
		"mistyped extra edge": {[]string{"a", "p", "q", "s", "t"},
			`,{"edge":["a","s"]},{"edge":["s","t"]}`,
			`,{"edge":["a","s"]},{"edge":["s","t"]},{"edge":["s","j"]}`},
		// an input edge given a second time
		"duplicate input edge": {[]string{"a", "p", "q"},
			``,
			`,{"edge":["p","j"]}`},
		// a switch arm routing into the join
		"switch arm": {[]string{"a", "p", "q", "x", "y"},
			`,{"edge":["a","x"]},{"switch":"x","when":[{"pred":"neg","to":"y"}]}`,
			`,{"edge":["a","x"]},{"switch":"x","when":[{"pred":"neg","to":"j"}],"else":"y"}`},
	} {
		if _, err := Load[int, int]([]byte(cfg(tc.names, tc.control)), reg(tc.names...)); err != nil {
			t.Fatalf("%s: control config: %v", name, err)
		}
		if _, err := Load[int, int]([]byte(cfg(tc.names, tc.extra)), reg(tc.names...)); err == nil {
			t.Errorf("Load, %s into a join: loaded", name)
		}
	}
}
