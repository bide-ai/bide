package plan

import (
	"strings"
	"testing"
)

// The rest of what Build refuses that needs no Registry (it is about the wiring alone): the structural
// check (InspectConfig without a Registry, so bide check and bide view -config) must report each. A
// control loads with a Registry and has no structural error; the variant is refused by Load, and the
// structural check must say so in the words of the refusal.
func TestStructuralCheckReportsWhatBuildRefusesOnTheGraph(t *testing.T) {
	const join = `{"join":"j","inputs":["b","c"],"merge":"sum"}`
	for name, c := range map[string]struct {
		names        []string
		control, bad string
		want         string
	}{
		"a node not reached from the entry": {[]string{"a", "b", "c"},
			`{"edge":["a","b"]},{"edge":["b","c"]}`,
			`{"edge":["a","b"]}`,
			`step "c" is not reached from the entry "a"`},
		"a cycle that is not a loop": {[]string{"a", "b", "c"},
			`{"edge":["a","b"]},{"edge":["b","c"]}`,
			`{"edge":["a","b"]},{"edge":["b","c"]},{"edge":["c","b"]}`,
			`topology has a cycle "b" -> "c" -> "b"`},
		"a cycle with a node after it": {[]string{"a", "b", "c", "d"},
			`{"edge":["a","b"]},{"edge":["b","c"]},{"edge":["c","d"]}`,
			`{"edge":["a","b"]},{"edge":["b","c"]},{"edge":["c","b"]},{"edge":["c","d"]}`,
			`topology has a cycle "b" -> "c" -> "b"`},
		"a node fed by several producers without a join": {[]string{"a", "b", "c", "d"},
			`{"edge":["a","b"]},{"edge":["a","c"]},` + join + `,{"edge":["j","d"]}`,
			`{"edge":["a","b"]},{"edge":["a","c"]},{"edge":["b","d"]},{"edge":["c","d"]}`,
			`step "d" is fed by`},
		"two join inputs on mutually exclusive arms": {[]string{"a", "b", "c", "d"},
			`{"edge":["a","b"]},{"edge":["a","c"]},` + join + `,{"edge":["j","d"]}`,
			`{"switch":"a","when":[{"pred":"neg","to":"b"}],"else":"c"},` + join + `,{"edge":["j","d"]}`,
			`mutually-exclusive arms of the Switch over "a"`},
		"a join input that a switch can skip": {[]string{"a", "b", "x", "c", "y", "d"},
			`{"edge":["a","b"]},{"edge":["a","x"]},{"edge":["x","c"]},{"edge":["a","y"]},` + join + `,{"edge":["j","d"]}`,
			`{"edge":["a","b"]},{"edge":["a","x"]},{"switch":"x","when":[{"pred":"neg","to":"c"}],"else":"y"},` + join + `,{"edge":["j","d"]}`,
			`can be skipped while input`},
		"an inner switch in a loop body": {[]string{"a", "b", "c", "d"},
			`{"edge":["a","b"]},{"edge":["b","c"]},{"switch":"c","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"d"}`,
			`{"edge":["a","b"]},{"switch":"b","when":[{"pred":"neg","to":"c"}],"else":"c"},{"switch":"c","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"d"}`,
			`has its own Switch`},
		"an edge that leaves a loop": {[]string{"a", "b", "c", "d", "e"},
			`{"edge":["a","b"]},{"edge":["b","c"]},{"switch":"c","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"d"},{"edge":["d","e"]}`,
			`{"edge":["a","b"]},{"edge":["b","c"]},{"edge":["b","e"]},{"switch":"c","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"d"}`,
			`leaves the loop`},
		"a loop body that is not contiguous": {[]string{"a", "b", "c", "d", "e"},
			`{"edge":["a","b"]},{"edge":["b","c"]},{"switch":"c","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"d"},{"edge":["d","e"]}`,
			`{"edge":["a","b"]},{"edge":["b","c"]},{"edge":["a","e"]},{"switch":"c","when":[{"pred":"neg","to":"a","loop_max":3}],"else":"d"}`,
			`non-contiguous body`},
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
		_, loadErr := Load[int, int]([]byte(cfg(c.bad)), structuralReg(t, c.names...))
		if loadErr == nil {
			t.Fatalf("%s: Load accepted it; the test is wrong", name)
		}
		// Load says the same, in the same words (a node not reached from the entry is worded for work
		// in progress, so Load's "unreachable" is the one difference).
		if c.want[:4] != "step" || !strings.Contains(c.want, "not reached") {
			if !strings.Contains(loadErr.Error(), c.want) {
				t.Errorf("%s: Load says %q, the test expects %q", name, loadErr, c.want)
			}
		}
		if errs := problemsOf(cfg(c.bad)); !slicesContain(errs, c.want) {
			t.Errorf("%s: the structural check says %q, want an error containing %q", name, errs, c.want)
		}
	}
}
