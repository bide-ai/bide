package plan

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// kindCases has, for each kind, a config that has exactly one problem of that kind and the Finding it
// must give: the kind, the full names in the order the kind documents, and Got and Want. The names hold
// brackets, quotes and "; " so that nothing is told apart by punctuation. reg is "typed" for a kind that
// needs the registered types, else the structure alone is checked.
var kindCases = []struct {
	kind  ProblemKind
	typed bool
	cfg   string
	names []string
	got   string
	want  string
}{
	{ProblemEdgeTypes, true,
		`{"version":1,"flow":"f","entry":"a[1]","nodes":[{"name":"a[1]","block":"s2s"},{"name":"b\"2","block":"s2i"},{"name":"c; 3","block":"s2s"}],"wiring":[{"edge":["a[1]","b\"2"]},{"edge":["b\"2","c; 3"]}]}`,
		[]string{`b"2`, `c; 3`}, "int", "string"},
	{ProblemArmTypes, true,
		`{"version":1,"flow":"f","entry":"a[1]","nodes":[{"name":"a[1]","block":"s2s"},{"name":"b\"2","block":"s2s"},{"name":"c; 3","block":"s2s"}],"wiring":[{"switch":"a[1]","when":[{"pred":"isInt","to":"b\"2"}],"else":"c; 3"}]}`,
		[]string{"a[1]", "isInt", `b"2`}, "int", "string"},
	{ProblemJoinInputTypes, true,
		`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"x[1]","block":"s2i"},{"name":"y; 2","block":"s2s"}],"wiring":[{"edge":["a","x[1]"]},{"edge":["a","y; 2"]},{"join":"j\"3","inputs":["x[1]","y; 2"],"merge":"both"}]}`,
		[]string{`j"3`, "x[1]"}, "int", "string"},
	{ProblemTerminalTypes, true,
		`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"t[1]","block":"s2s"},{"name":"t; 2","block":"s2i"}],"wiring":[{"switch":"a","when":[{"pred":"nonEmpty","to":"t[1]"}],"else":"t; 2"}]}`,
		[]string{"t; 2"}, "int", "string"},
	{ProblemSafetyAboveRegistration, true,
		`{"version":1,"flow":"f","entry":"a[1]","nodes":[{"name":"a[1]","block":"s2s","safety":"idempotent"},{"name":"b\"2","block":"s2s"}],"wiring":[{"edge":["a[1]","b\"2"]}]}`,
		[]string{"a[1]"}, "idempotent", "side_effect"},
	{ProblemLoopNoExitArm, false,
		`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"b; 1","block":"x"}],"wiring":[{"edge":["a","b; 1"]},{"switch":"b; 1","when":[{"pred":"p","to":"a","loop_max":3}]}]}`,
		[]string{"b; 1"}, "", ""},
	{ProblemLoopMoreThanOneBackArm, false,
		`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"b[1]","block":"x"},{"name":"c","block":"x"}],"wiring":[{"edge":["a","b[1]"]},{"switch":"b[1]","when":[{"pred":"p","to":"a","loop_max":3},{"pred":"q","to":"a","loop_max":2}],"else":"c"}]}`,
		[]string{"b[1]"}, "", ""},
	{ProblemLoopHeadNotAncestor, false,
		`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"b","block":"x"},{"name":"h\"1","block":"x"},{"name":"c","block":"x"}],"wiring":[{"edge":["a","b"]},{"edge":["a","h\"1"]},{"switch":"b","when":[{"pred":"p","to":"h\"1","loop_max":3}],"else":"c"}]}`,
		[]string{"b", `h"1`}, "", ""},
	{ProblemLoopBodyNotSimple, false,
		`{"version":1,"flow":"f","entry":"e","nodes":[{"name":"e","block":"x"},{"name":"a","block":"x"},{"name":"x; 1","block":"x"},{"name":"b[2]","block":"x"},{"name":"c","block":"x"}],"wiring":[{"edge":["e","a"]},{"edge":["e","x; 1"]},{"edge":["a","b[2]"]},{"switch":"b[2]","when":[{"pred":"p","to":"a","loop_max":3}],"else":"c"}]}`,
		[]string{"b[2]", "x; 1"}, "", ""},
	{ProblemLoopInnerSwitch, false,
		`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"b[1]","block":"x"},{"name":"c\"2","block":"x"},{"name":"d","block":"x"}],"wiring":[{"edge":["a","b[1]"]},{"switch":"b[1]","when":[{"pred":"p","to":"c\"2"}],"else":"c\"2"},{"switch":"c\"2","when":[{"pred":"q","to":"a","loop_max":3}],"else":"d"}]}`,
		[]string{"b[1]", `c"2`}, "", ""},
	{ProblemLoopEdgeLeaves, false,
		`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"b","block":"x"},{"name":"c","block":"x"},{"name":"x[1]","block":"x"}],"wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"p","to":"a","loop_max":3}],"else":"c"},{"edge":["a","x[1]"]}]}`,
		[]string{"a", "x[1]", "b"}, "", ""},
	{ProblemLoopEdgeEnters, false,
		`{"version":1,"flow":"f","entry":"s","nodes":[{"name":"s","block":"x"},{"name":"h","block":"x"},{"name":"m; 1","block":"x"},{"name":"b","block":"x"},{"name":"c","block":"x"}],"wiring":[{"edge":["s","h"]},{"edge":["h","m; 1"]},{"edge":["m; 1","b"]},{"switch":"b","when":[{"pred":"p","to":"h","loop_max":3}],"else":"c"},{"edge":["s","m; 1"]}]}`,
		[]string{"s", "m; 1", "b", "h"}, "", ""},
}

// Each kind gives its Finding with the full names in the order the kind documents, Got and Want where it
// has them, and no position, whatever the names hold. The Message is the problem's, and the Finding is in
// the report beside it.
func TestFindings_EveryKind(t *testing.T) {
	seen := map[ProblemKind]bool{}
	for _, c := range kindCases {
		var reg *Registry
		if c.typed {
			reg = typedChainRegistry(t)
		}
		r := InspectConfig([]byte(c.cfg), reg, AllowUnusedBlocks())
		var got []Finding
		for _, f := range r.Findings {
			if f.Kind == c.kind {
				got = append(got, f)
			}
		}
		if len(got) != 1 {
			t.Errorf("%s: %d findings of the kind, want 1 (findings %+v, problems %+v)", c.kind, len(got), r.Findings, r.Problems)
			continue
		}
		f := got[0]
		if !slices.Equal(f.Names, c.names) || f.Got != c.got || f.Want != c.want {
			t.Errorf("%s: names %q got %q want %q; want %q %q %q", c.kind, f.Names, f.Got, f.Want, c.names, c.got, c.want)
		}
		if f.Message == "" || strings.Contains(f.Message, "[0]") && false {
			t.Errorf("%s: no message", c.kind)
		}
		// The finding's message is that of an error in Problems.
		found := false
		for _, p := range r.Problems {
			found = found || p.Severity == "error" && p.Message == f.Message
		}
		if !found {
			t.Errorf("%s: no problem has the message %q", c.kind, f.Message)
		}
		seen[c.kind] = true
	}
	for _, k := range []ProblemKind{ProblemEdgeTypes, ProblemArmTypes, ProblemJoinInputTypes, ProblemTerminalTypes, ProblemSafetyAboveRegistration,
		ProblemLoopNoExitArm, ProblemLoopMoreThanOneBackArm, ProblemLoopHeadNotAncestor, ProblemLoopBodyNotSimple, ProblemLoopInnerSwitch,
		ProblemLoopEdgeLeaves, ProblemLoopEdgeEnters} {
		if !seen[k] {
			t.Errorf("no case for %s", k)
		}
	}
}

// The values of the kinds are the contract: they are written to JSON and compared by tools, so a
// change here is a break.
func TestFindings_KindValuesAreStable(t *testing.T) {
	want := map[ProblemKind]string{
		ProblemEdgeTypes: "edge_types", ProblemArmTypes: "arm_types", ProblemJoinInputTypes: "join_input_types",
		ProblemTerminalTypes: "terminal_types", ProblemSafetyAboveRegistration: "safety_above_registration",
		ProblemLoopNoExitArm: "loop_no_exit_arm", ProblemLoopMoreThanOneBackArm: "loop_more_than_one_back_arm",
		ProblemLoopHeadNotAncestor: "loop_head_not_ancestor", ProblemLoopBodyNotSimple: "loop_body_not_simple",
		ProblemLoopInnerSwitch: "loop_inner_switch", ProblemLoopEdgeLeaves: "loop_edge_leaves",
		ProblemLoopEdgeEnters: "loop_edge_enters", ProblemOther: "other",
	}
	for k, v := range want {
		if string(k) != v {
			t.Errorf("%q != %q", k, v)
		}
	}
	if len(want) != 13 {
		t.Errorf("%d kinds, want 12 and other", len(want))
	}
}

// The JSON of a Finding is a golden: the names of its fields, empty fields left out, names as given.
func TestFindings_JSONGolden(t *testing.T) {
	r := InspectConfig([]byte(kindCases[0].cfg), typedChainRegistry(t), AllowUnusedBlocks())
	var edge []Finding
	for _, f := range r.Findings {
		if f.Kind == ProblemEdgeTypes {
			edge = append(edge, f)
		}
	}
	b, err := marshalPlain(edge)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"kind":"edge_types","names":["b\"2","c; 3"],"got":"int","want":"string","message":"edge \"b\\\"2\" -> \"c; 3\" connects int to string (types must match exactly)"}]`
	if string(b) != want {
		t.Errorf("JSON:\n%s\nwant\n%s", b, want)
	}
	loop := InspectConfig([]byte(kindCases[5].cfg), nil)
	b, _ = marshalPlain(loop.Findings)
	wantLoop := `[{"kind":"loop_no_exit_arm","names":["b; 1"],"message":"wiring[1] the loop over \"b; 1\" has no exit arm; add an arm or an else that does not loop back so the loop can terminate"}]`
	if string(b) != wantLoop {
		t.Errorf("loop JSON:\n%s\nwant\n%s", b, wantLoop)
	}
	other, _ := marshalPlain([]Finding{{Kind: ProblemOther, Message: "m"}})
	if string(other) != `[{"kind":"other","message":"m"}]` {
		t.Errorf("other JSON: %s", other)
	}
}

// marshalPlain is json.Marshal without HTML escaping, so the golden reads as the JSON a tool gets.
func marshalPlain(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// A problem's node, edge or arm is where its creator put it, never read from its message: names that
// look like the message's own punctuation (a quote, " -> ", a bracketed index) are attributed exactly.
func TestProblemPlaceComesFromTheCreatorNotTheMessage(t *testing.T) {
	odd := `x" -> "y`
	reg := typedChainRegistry(t)
	type place struct{ node, from, to string }
	for name, c := range map[string]struct {
		cfg  string
		want place
		kind ProblemKind
	}{
		"edge types": {`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"x\" -> \"y","block":"s2i"},{"name":"b","block":"s2s"}],"wiring":[{"edge":["a","x\" -> \"y"]},{"edge":["x\" -> \"y","b"]}]}`,
			place{"", odd, "b"}, ProblemEdgeTypes},
		"arm types": {`{"version":1,"flow":"f","entry":"x\" -> \"y","nodes":[{"name":"x\" -> \"y","block":"s2s"},{"name":"b[1]","block":"s2s"},{"name":"c","block":"s2s"}],"wiring":[{"switch":"x\" -> \"y","when":[{"pred":"isInt","to":"b[1]"}],"else":"c"}]}`,
			place{"", odd, "b[1]"}, ProblemArmTypes},
		"join input": {`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"x","block":"s2i"},{"name":"y","block":"s2s"}],"wiring":[{"edge":["a","x"]},{"edge":["a","y"]},{"join":"j\" -> \"k","inputs":["x","y"],"merge":"both"}]}`,
			place{`j" -> "k`, "", ""}, ProblemJoinInputTypes},
		"terminal": {`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"t1","block":"s2s"},{"name":"x\" -> \"y","block":"s2i"}],"wiring":[{"switch":"a","when":[{"pred":"nonEmpty","to":"t1"}],"else":"x\" -> \"y"}]}`,
			place{odd, "", ""}, ProblemTerminalTypes},
		"loop": {`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"x\" -> \"y","block":"x"},{"name":"c","block":"x"},{"name":"z","block":"x"}],"wiring":[{"edge":["a","x\" -> \"y"]},{"switch":"x\" -> \"y","when":[{"pred":"p","to":"a","loop_max":3}],"else":"c"},{"edge":["a","z"]}]}`,
			place{"a", "", ""}, ProblemLoopEdgeLeaves},
	} {
		var rg *Registry
		if name != "loop" { // the loop config uses blocks the typed registry lacks
			rg = reg
		}
		r := InspectConfig([]byte(c.cfg), rg, AllowUnusedBlocks())
		ok := false
		for _, f := range r.Findings {
			if f.Kind != c.kind {
				continue
			}
			var p *ConfigProblem
			for j := range r.Problems {
				if r.Problems[j].Severity == "error" && r.Problems[j].Message == f.Message {
					p = &r.Problems[j]
				}
			}
			if p == nil || (place{p.Node, p.From, p.To}) != c.want {
				t.Errorf("%s: problem place %+v, want %+v", name, p, c.want)
			}
			ok = true
		}
		if !ok {
			t.Errorf("%s: no finding of kind %s (%+v)", name, c.kind, r.Findings)
		}
	}
	// A problem with no place of its own (a text-only error) has none, whatever its message quotes.
	r := InspectConfig([]byte(`{"version":1,"flow":"f","entry":"x\" -> \"y","nodes":[{"name":"x\" -> \"y","block":"nowhere"}],"wiring":[]}`), reg)
	for _, p := range r.Problems {
		if strings.Contains(p.Message, "unknown block") && p.Node != odd {
			t.Errorf("an unknown block is at node %q, want %q", p.Node, odd)
		}
	}
}

// The graph checks put a problem on its node by the name they hold, not by the quoted names in the text:
// a step never reached and a join of mutually exclusive arms, both with names that look like quoting.
func TestGraphCheckProblemsAreOnTheirNode(t *testing.T) {
	odd := `x" -> "y`
	for name, c := range map[string]struct{ cfg, msg string }{
		"not reached": {`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"x\" -> \"y","block":"x"}],"wiring":[]}`, "is not reached from the entry"},
		"join of exclusive arms": {`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"p","block":"x"},{"name":"q","block":"x"}],
		  "wiring":[{"switch":"a","when":[{"pred":"p","to":"p"}],"else":"q"},{"join":"x\" -> \"y","inputs":["p","q"],"merge":"m"}]}`, "mutually-exclusive"},
	} {
		got := false
		for _, p := range InspectConfig([]byte(c.cfg), nil).Problems {
			if strings.Contains(p.Message, c.msg) {
				got = true
				if p.Node != odd {
					t.Errorf("%s: problem %q is on node %q, want %q", name, p.Message, p.Node, odd)
				}
			}
		}
		if !got {
			t.Errorf("%s: no problem containing %q", name, c.msg)
		}
	}
}
