package plan

import (
	"context"
	"strings"
	"testing"
)

// Gaps in the structural check (InspectConfig without a Registry, so bide check and bide view
// -config): a config that Load refuses showed no error. Each case here loads or builds with a
// Registry and is refused without one only after the fix.
func structuralReg(t *testing.T, names ...string) *Registry {
	t.Helper()
	id := func(_ context.Context, n int) (int, error) { return n, nil }
	sum := func(_ context.Context, a, c int) (int, error) { return a + c, nil }
	r := NewRegistry()
	for _, n := range names {
		if err := r.RegisterStep(n, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RegisterJoin2("sum", sum); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterPredicate("neg", func(n int) bool { return n < 0 }); err != nil {
		t.Fatal(err)
	}
	return r
}

func structuralNodes(names ...string) string {
	var out []string
	for _, n := range names {
		out = append(out, `{"name":"`+n+`","block":"`+n+`"}`)
	}
	return strings.Join(out, ",")
}

func problemsOf(cfg string) []string {
	var out []string
	for _, p := range InspectConfig([]byte(cfg), nil).Problems {
		if p.Severity == "error" {
			out = append(out, p.Message)
		}
	}
	return out
}

func TestStructuralCheckRefusesWhatLoadRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		names   []string
		control string // wiring that loads
		bad     string // wiring Load refuses
		want    string // the structural error
	}{
		// plan.Load registers a join when it reaches its wiring element, so a join that reads a join
		// defined after it reads an unknown input (and a cycle of joins is one case of it).
		"a join that reads a later join": {
			[]string{"a", "b", "c"},
			`{"edge":["a","b"]},{"edge":["a","c"]},{"join":"j2","inputs":["b","c"],"merge":"sum"},{"join":"j1","inputs":["b","j2"],"merge":"sum"}`,
			`{"edge":["a","b"]},{"edge":["a","c"]},{"join":"j1","inputs":["b","j2"],"merge":"sum"},{"join":"j2","inputs":["b","c"],"merge":"sum"}`,
			`join "j1" reads the join "j2", which is defined after it`,
		},
		"a cycle of joins": {
			[]string{"a", "b", "c"},
			`{"edge":["a","b"]},{"edge":["a","c"]},{"join":"j1","inputs":["b","c"],"merge":"sum"}`,
			`{"edge":["a","b"]},{"edge":["a","c"]},{"join":"j1","inputs":["b","j2"],"merge":"sum"},{"join":"j2","inputs":["j1","c"],"merge":"sum"}`,
			`defined after it`,
		},
		// A loop_max arm is a bounded back-edge to an earlier loop head: Build refuses a target that does not
		// reach the switch in the forward graph.
		"a loop arm to a node that is not an ancestor": {
			[]string{"a", "b", "c", "d"},
			`{"edge":["a","b"]},{"switch":"b","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"c"},{"edge":["c","d"]}`,
			`{"edge":["a","b"]},{"switch":"b","when":[{"pred":"neg","to":"c","loop_max":3}],"else":"d"}`,
			`is not an ancestor of the switch`,
		},
		"two loop arms on one switch": {
			[]string{"a", "b", "c"},
			`{"edge":["a","b"]},{"switch":"b","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"c"}`,
			`{"edge":["a","b"]},{"switch":"b","when":[{"pred":"neg","to":"a","loop_max":3},{"pred":"neg","to":"a","loop_max":2}],"else":"c"}`,
			`more than one loop-back arm`,
		},
		"a loop with no exit arm": {
			[]string{"a", "b", "c"},
			`{"edge":["a","b"]},{"switch":"b","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"c"}`,
			`{"edge":["a","b"]},{"edge":["b","c"]},{"switch":"c","when":[{"pred":"neg","to":"a","loop_max":3}]}`,
			`has no exit arm`,
		},
	} {
		cfg := func(w string) string {
			return `{"version":1,"flow":"f","nodes":[` + structuralNodes(c.names...) + `],"wiring":[` + w + `]}`
		}
		if _, err := Load[int, int]([]byte(cfg(c.control)), structuralReg(t, c.names...)); err != nil {
			t.Fatalf("%s: the control does not load: %v", name, err)
		}
		if errs := problemsOf(cfg(c.control)); len(errs) != 0 {
			t.Errorf("%s: the control has structural errors: %q", name, errs)
		}
		if _, err := Load[int, int]([]byte(cfg(c.bad)), structuralReg(t, c.names...)); err == nil {
			t.Fatalf("%s: Load accepted it; the test is wrong", name)
		}
		errs := problemsOf(cfg(c.bad))
		if !slicesContain(errs, c.want) {
			t.Errorf("%s: the structural check says %q, want an error containing %q", name, errs, c.want)
		}
	}
}

func slicesContain(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// What the structural check reads as the forward graph is what Build reads: a loop head that reaches its
// switch through a join is an ancestor, so this loads and the check must not refuse it; and a join that
// lists itself as an input is refused. (A path through an else needs a Switch inside the loop, which Build
// refuses, so no config that loads shows the else edge.)
func TestStructuralLoopHeadReachesThroughAJoin(t *testing.T) {
	for name, c := range map[string]struct {
		names  []string
		wiring string
	}{
		"through a join": {[]string{"a", "b", "c", "d", "e"},
			`{"edge":["a","b"]},{"edge":["a","c"]},{"join":"j","inputs":["b","c"],"merge":"sum"},{"edge":["j","d"]},{"switch":"d","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"e"}`},
	} {
		cfg := `{"version":1,"flow":"f","nodes":[` + structuralNodes(c.names...) + `],"wiring":[` + c.wiring + `]}`
		if _, err := Load[int, int]([]byte(cfg), structuralReg(t, c.names...)); err != nil {
			t.Fatalf("%s: Load refuses it, so the test is wrong: %v", name, err)
		}
		if errs := problemsOf(cfg); len(errs) != 0 {
			t.Errorf("%s: the structural check refuses what Load accepts: %q", name, errs)
		}
	}
	self := `{"version":1,"flow":"f","nodes":[` + structuralNodes("a", "b", "c") + `],"wiring":[{"edge":["a","b"]},{"edge":["a","c"]},{"join":"j","inputs":["b","j"],"merge":"sum"}]}`
	if _, err := Load[int, int]([]byte(self), structuralReg(t, "a", "b", "c")); err == nil {
		t.Fatal("Load accepted a join that reads itself")
	}
	if errs := problemsOf(self); !slicesContain(errs, "defined after it") {
		t.Errorf("a join that reads itself: %q", errs)
	}
}
