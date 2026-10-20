package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/planaccess"
)

// typedChainRegistry registers blocks of plain types: s2i takes a string and gives an int, i2s the
// reverse, s2s keeps a string. A chain of s2i blocks has a type misfit on every edge.
func typedChainRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, err := range []error{
		reg.RegisterStep("s2i", func(_ context.Context, s string) (int, error) { return len(s), nil }),
		reg.RegisterStep("i2s", func(_ context.Context, n int) (string, error) { return fmt.Sprint(n), nil }),
		reg.RegisterStep("s2s", func(_ context.Context, s string) (string, error) { return s, nil }),
		reg.RegisterPredicate("isInt", func(n int) bool { return n > 0 }),
		reg.RegisterPredicate("nonEmpty", func(s string) bool { return s != "" }),
		reg.RegisterJoin2("both", func(_ context.Context, a, b string) (string, error) { return a + b, nil }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func typeProblemMessages(r ConfigReport) []string {
	var out []string
	for _, p := range r.Problems {
		if strings.Contains(p.Message, "(types must match exactly)") || strings.Contains(p.Message, " but switched node ") ||
			strings.Contains(p.Message, " but the merge expects ") || strings.Contains(p.Message, " but the flow output is ") {
			out = append(out, p.Message)
		}
	}
	return out
}

func chainConfig(n int) string {
	var nodes, wiring []string
	for i := range n {
		nodes = append(nodes, fmt.Sprintf(`{"name":"n%d","block":"s2i"}`, i))
		if i > 0 {
			wiring = append(wiring, fmt.Sprintf(`{"edge":["n%d","n%d"]}`, i-1, i))
		}
	}
	return `{"version":1,"flow":"chain","entry":"n0","nodes":[` + strings.Join(nodes, ",") + `],"wiring":[` + strings.Join(wiring, ",") + `]}`
}

// A config with several type problems lists every one, not the first: an edge between blocks of
// other types, each of them, and a switch arm whose predicate tests another type than the node gives.
func TestInspectConfig_ReportsEveryTypeProblem(t *testing.T) {
	reg := typedChainRegistry(t)
	got := typeProblemMessages(InspectConfig([]byte(chainConfig(4)), reg, AllowUnusedBlocks()))
	if len(got) != 3 {
		t.Fatalf("a chain of 4 blocks with a misfit on each of its 3 edges: %d type problems: %q", len(got), got)
	}
	for i, m := range got {
		if want := fmt.Sprintf(`edge "n%d" -> "n%d" connects`, i, i+1); !strings.Contains(m, want) {
			t.Errorf("problem %d = %q, want %s", i, m, want)
		}
	}
	// An arm whose predicate tests an int, over a node that gives a string, beside an edge misfit and
	// the terminal that gives another type than the first terminal.
	arms := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s"},{"name":"c","block":"s2s"},{"name":"d","block":"s2i"},{"name":"e","block":"s2i"}],
	  "wiring":[{"switch":"a","when":[{"pred":"isInt","to":"b"}],"else":"c"},{"edge":["b","d"]},{"edge":["d","e"]}]}`
	got = typeProblemMessages(InspectConfig([]byte(arms), reg, AllowUnusedBlocks()))
	all := strings.Join(got, "\n")
	if len(got) != 3 || !strings.Contains(all, `predicate "isInt" tests int but switched node "a" produces string`) || !strings.Contains(all, `edge "d" -> "e" connects`) || !strings.Contains(all, `terminal step "e" produces int but the flow output is string`) {
		t.Errorf("an arm, an edge and a terminal: %q", got)
	}
}

// The checks Build makes beyond the edges are all reported too: every join input that does not fit
// its merge, and every terminal of another type than the flow's output, with the edge misfits.
func TestInspectConfig_ReportsEveryJoinAndTerminalProblem(t *testing.T) {
	reg := typedChainRegistry(t)
	join := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"x","block":"s2i"},{"name":"y","block":"s2i"}],
	  "wiring":[{"edge":["a","x"]},{"edge":["a","y"]},{"join":"j","inputs":["x","y"],"merge":"both"}]}`
	got := typeProblemMessages(InspectConfig([]byte(join), reg, AllowUnusedBlocks()))
	if len(got) != 2 || !strings.Contains(got[0], `join "j" input "x" produces int but the merge expects string`) || !strings.Contains(got[1], `join "j" input "y"`) {
		t.Errorf("two join inputs: %q", got)
	}
	// Three terminals: the first (string) sets the flow's output, the other two give an int.
	terms := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"t1","block":"s2s"},{"name":"t2","block":"s2i"},{"name":"t3","block":"s2i"}],
	  "wiring":[{"switch":"a","when":[{"pred":"nonEmpty","to":"t1"},{"pred":"nonEmpty","to":"t2"}],"else":"t3"}]}`
	got = typeProblemMessages(InspectConfig([]byte(terms), reg, AllowUnusedBlocks()))
	if len(got) != 2 || !strings.Contains(got[0], `terminal step "t2" produces int but the flow output is string`) || !strings.Contains(got[1], `terminal step "t3"`) {
		t.Errorf("two terminals: %q", got)
	}
}

// With more type problems than a report lists, the rest are counted in one line, never dropped.
func TestInspectConfig_CapsTypeProblemsWithACount(t *testing.T) {
	r := InspectConfig([]byte(chainConfig(123)), typedChainRegistry(t), AllowUnusedBlocks()) // 122 misfit edges
	got := typeProblemMessages(r)
	if len(got) != maxTypeProblems+1 {
		t.Fatalf("%d type problems listed, want %d and a count", len(got), maxTypeProblems+1)
	}
	if last := got[len(got)-1]; last != "and 22 more type problems (types must match exactly)" {
		t.Errorf("the count line = %q", last)
	}
	if n := capTypeProblems([]string{"a", "b"}); len(n) != 2 {
		t.Errorf("a short list was changed: %q", n)
	}
}

// Load refuses a config with several type problems with all of them in its error.
func TestLoad_ListsEveryTypeProblem(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterStep("s2i", func(_ context.Context, s string) (int, error) { return len(s), nil }); err != nil {
		t.Fatal(err)
	}
	_, err := Load[string, int]([]byte(chainConfig(4)), reg)
	if err == nil {
		t.Fatal("loaded")
	}
	for i := range 3 {
		if want := fmt.Sprintf(`edge "n%d" -> "n%d" connects`, i, i+1); !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not list %s: %v", want, err)
		}
	}
}

// The wiring's own checks report every problem too: two nodes not reached from the entry, and two
// loops that have no exit arm.
func TestInspectConfig_ReportsEveryWiringProblem(t *testing.T) {
	two := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"o1","block":"x"},{"name":"o2","block":"x"}],"wiring":[]}`
	for _, reg := range []*Registry{nil, typedChainRegistry(t)} {
		var n int
		for _, p := range InspectConfig([]byte(two), reg).Problems {
			if strings.Contains(p.Message, "is not reached from the entry") {
				n++
			}
		}
		if reg == nil && n != 2 {
			t.Errorf("unreached nodes reported: %d, want 2", n)
		}
	}
	loops := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"x"},{"name":"b","block":"x"},{"name":"c","block":"x"},{"name":"d","block":"x"}],
	  "wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"p","to":"a","loop_max":3}]},{"edge":["a","c"]},{"switch":"c","when":[{"pred":"p","to":"a","loop_max":3}]}]}`
	var found []string
	for _, p := range InspectConfig([]byte(loops), nil).Problems {
		if strings.Contains(p.Message, "has no exit arm") {
			found = append(found, p.Message)
		}
	}
	if len(found) < 2 {
		t.Errorf("loops without an exit arm reported: %q", found)
	}
}

// A config's problems come with the first one first, as they did when only the first was reported,
// and AllTypeProblems lifts the cap of 100.
func TestTypeProblems_OrderAndTheOption(t *testing.T) {
	reg := typedChainRegistry(t)
	r := InspectConfig([]byte(chainConfig(4)), reg, AllowUnusedBlocks())
	got := typeProblemMessages(r)
	if len(got) == 0 || !strings.Contains(got[0], `edge "n0" -> "n1"`) {
		t.Errorf("the first problem leads: %q", got)
	}
	_, err := Load[string, int]([]byte(chainConfig(4)), func() *Registry {
		reg := NewRegistry()
		if err := reg.RegisterStep("s2i", func(_ context.Context, s string) (int, error) { return len(s), nil }); err != nil {
			t.Fatal(err)
		}
		return reg
	}())
	if err == nil || !strings.HasPrefix(err.Error(), `plan: load "chain": edge "n0" -> "n1" connects`) {
		t.Errorf("Load's error leads with the first problem: %v", err)
	}
}

// With a Registry the wiring checks report every problem as well (they run before Build, which
// stops at the first): two loops that have no exit arm.
func TestInspectConfig_TypedReportsEveryLoopProblem(t *testing.T) {
	loops := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s"},{"name":"c","block":"s2s"}],
	  "wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"a","loop_max":3}]},{"edge":["a","c"]},{"switch":"c","when":[{"pred":"nonEmpty","to":"a","loop_max":3}]}]}`
	var found []string
	for _, p := range InspectConfig([]byte(loops), typedChainRegistry(t), AllowUnusedBlocks()).Problems {
		if strings.Contains(p.Message, "has no exit arm") {
			found = append(found, p.Message)
		}
	}
	if len(found) != 2 {
		t.Errorf("loops without an exit arm reported: %q", found)
	}
}

// A node still to be wired in (the editor adds one unwired) is reported as not reached, and its
// types are not reported as a terminal that gives another type than the flow's output.
func TestInspectConfig_AnUnwiredNodeIsNotATerminalMisfit(t *testing.T) {
	cfg := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s"},{"name":"extra","block":"s2i"}],"wiring":[{"edge":["a","b"]}]}`
	r := InspectConfig([]byte(cfg), typedChainRegistry(t), AllowUnusedBlocks())
	if got := typeProblemMessages(r); len(got) != 0 {
		t.Errorf("type problems for an unwired node: %q", got)
	}
	var unreached bool
	for _, p := range r.Problems {
		unreached = unreached || strings.Contains(p.Message, `step "extra" is not reached from the entry`)
	}
	if !unreached {
		t.Errorf("the unwired node is not reported as not reached: %+v", r.Problems)
	}
}

// The refusals only Build makes (not the structural check) are all reported too, with and without a
// Registry: two edges that leave a loop's body are two problems, not one.
func TestInspectConfig_ReportsEveryBuildOnlyLoopProblem(t *testing.T) {
	cfg := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s"},{"name":"c","block":"s2s"},{"name":"x","block":"s2s"},{"name":"y","block":"s2s"}],
	  "wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"a","loop_max":3}],"else":"c"},{"edge":["a","x"]},{"edge":["a","y"]}]}`
	for name, reg := range map[string]*Registry{"structural": nil, "typed": typedChainRegistry(t)} {
		var found []string
		for _, p := range InspectConfig([]byte(cfg), reg, AllowUnusedBlocks()).Problems {
			if strings.Contains(p.Message, "leaves the loop") {
				found = append(found, p.Message)
			}
		}
		if len(found) != 2 {
			t.Errorf("%s: edges that leave the loop reported: %q", name, found)
		}
	}
}

// Load's and Validate's refusals are agent.ErrConfig, as the other config errors are, whatever
// their text.
func TestLoadAndValidateRefusalsWrapErrConfig(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterStep("s2i", func(_ context.Context, s string) (int, error) { return len(s), nil }); err != nil {
		t.Fatal(err)
	}
	misfit := []byte(chainConfig(3)) // a type misfit on each edge
	_, err := Load[string, int](misfit, reg)
	if !errors.Is(err, agent.ErrConfig) || !strings.HasPrefix(err.Error(), `plan: load "chain": edge "n0" -> "n1" connects`) {
		t.Errorf("Load: %v", err)
	}
	if err := Validate(misfit, reg); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `edge "n0" -> "n1" connects`) {
		t.Errorf("Validate: %v", err)
	}
	var already error = fmt.Errorf("x: %w", agent.ErrConfig)
	if asConfigError(already) != already || asConfigError(nil) != nil {
		t.Error("asConfigError changed an error that needed no change")
	}
}

// Two loops that each have a body Build refuses (a node between the head and the switch that is not on
// the loop path) are two problems, with and without a Registry.
func TestInspectConfig_ReportsEveryNonContiguousLoop(t *testing.T) {
	cfg := `{"version":1,"flow":"f","entry":"e","nodes":[{"name":"e","block":"s2s"},{"name":"a1","block":"s2s"},{"name":"x1","block":"s2s"},{"name":"b1","block":"s2s"},{"name":"c1","block":"s2s"},{"name":"a2","block":"s2s"},{"name":"x2","block":"s2s"},{"name":"b2","block":"s2s"},{"name":"c2","block":"s2s"}],
	  "wiring":[{"edge":["e","a1"]},{"edge":["e","x1"]},{"edge":["a1","b1"]},{"switch":"b1","when":[{"pred":"nonEmpty","to":"a1","loop_max":3}],"else":"c1"},{"edge":["c1","a2"]},{"edge":["c1","x2"]},{"edge":["a2","b2"]},{"switch":"b2","when":[{"pred":"nonEmpty","to":"a2","loop_max":3}],"else":"c2"}]}`
	for name, reg := range map[string]*Registry{"structural": nil, "typed": typedChainRegistry(t)} {
		var found []string
		for _, p := range InspectConfig([]byte(cfg), reg, AllowUnusedBlocks()).Problems {
			if strings.Contains(p.Message, "has a non-contiguous body") {
				found = append(found, p.Message)
			}
		}
		if len(found) != 2 {
			t.Errorf("%s: non-contiguous loops reported: %q", name, found)
		}
	}
}

// Problems of different checks come together: with a Registry, a loop Build refuses and a node not
// reached from the entry are both reported (Build alone stops at the first step that fails).
func TestInspectConfig_TypedReportsProblemsOfDifferentWiringChecks(t *testing.T) {
	cfg := `{"version":1,"flow":"f","entry":"e","nodes":[{"name":"e","block":"s2s"},{"name":"a1","block":"s2s"},{"name":"x1","block":"s2s"},{"name":"b1","block":"s2s"},{"name":"c1","block":"s2s"},{"name":"orphan","block":"s2s"}],
	  "wiring":[{"edge":["e","a1"]},{"edge":["e","x1"]},{"edge":["a1","b1"]},{"switch":"b1","when":[{"pred":"nonEmpty","to":"a1","loop_max":3}],"else":"c1"}]}`
	var loop, orphan int
	for _, p := range InspectConfig([]byte(cfg), typedChainRegistry(t), AllowUnusedBlocks()).Problems {
		if strings.Contains(p.Message, "has a non-contiguous body") {
			loop++
		}
		if strings.Contains(p.Message, `step "orphan" is not reached from the entry`) {
			orphan++
		}
	}
	if loop != 1 || orphan != 1 {
		t.Errorf("non-contiguous loop problems %d, unreached orphan problems %d; want one each", loop, orphan)
	}
}

// A type problem is reported beside any other error: an edge to a name the config does not have, or a
// node whose stated safety the registered block does not allow, does not hide it.
func TestInspectConfig_TypeProblemsBesideOtherErrors(t *testing.T) {
	reg := typedChainRegistry(t)
	for name, cfg := range map[string]string{
		"unknown name":    `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2i"},{"name":"c","block":"s2s"}],"wiring":[{"edge":["a","b"]},{"edge":["b","c"]},{"edge":["c","ghost"]}]}`,
		"unknown block":   `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2i"},{"name":"c","block":"s2s"},{"name":"d","block":"nowhere"}],"wiring":[{"edge":["a","b"]},{"edge":["b","c"]}]}`,
		"safety above":    `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2i","safety":"readonly"},{"name":"c","block":"s2s"}],"wiring":[{"edge":["a","b"]},{"edge":["b","c"]}]}`,
		"unknown pred":    `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2i"},{"name":"b","block":"s2s"},{"name":"c","block":"i2s"}],"wiring":[{"switch":"a","when":[{"pred":"nope","to":"b"}],"else":"c"},{"edge":["b","c"]}]}`,
		"structural only": `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2i"},{"name":"c","block":"s2s"}],"wiring":[{"edge":["a","b"]},{"edge":["b","c"]},{"edge":["a"]}]}`,
	} {
		r := InspectConfig([]byte(cfg), reg, AllowUnusedBlocks())
		other := false
		for _, p := range r.Problems {
			other = other || (p.Severity == "error" && !strings.Contains(p.Message, "(types must match exactly)") && !strings.Contains(p.Message, " but switched node "))
		}
		got := typeProblemMessages(r)
		if !other || len(got) == 0 {
			t.Errorf("%s: another error %v and %d type problems %q; want both (problems %+v)", name, other, len(got), got, r.Problems)
		}
	}
}

// The records the typed editor compares carry the full names as the config has them (the message
// shortens them), whatever they contain, and no position: names with brackets, quotes and "[#]" stay
// as they are, and two problems about names that differ only past the 64th character are two.
func TestInspectRecords_CarryFullNamesWhateverTheyContain(t *testing.T) {
	reg := typedChainRegistry(t)
	cfg := func(second string) string {
		return `{"version":1,"flow":"f","entry":"v","nodes":[{"name":"v","block":"s2s"},{"name":"n[1]","block":"s2s"},{"name":"n[2]","block":"` + second + `"},{"name":"n[3]","block":"s2i"}],
		  "wiring":[{"edge":["v","n[1]"]},{"edge":["n[1]","n[2]"]},{"edge":["n[2]","n[3]"]}]}`
	}
	// s2i takes a string: n[1]->n[2] fits; n[2] gives an int into n[3], which takes a string: a misfit.
	recs := inspectRecords([]byte(cfg("s2i")), reg, nil)
	if len(recs) != 1 || recs[0].Class != "type" || recs[0].Kind != "edge" || strings.Join(recs[0].Names, "|") != "n[2]|n[3]" {
		t.Fatalf("records = %+v", recs)
	}
	// Setting n[2] to s2s moves the misfit away: no record, not a record with an equal key.
	if recs := inspectRecords([]byte(cfg("s2s")), reg, nil); len(recs) != 0 {
		t.Errorf("records = %+v", recs)
	}
	l1, l2 := strings.Repeat("a", 70)+"1", strings.Repeat("a", 70)+"2"
	long := `{"version":1,"flow":"f","entry":"v","nodes":[{"name":"v","block":"s2i"},{"name":"` + l1 + `","block":"s2i"},{"name":"` + l2 + `","block":"s2i"}],
	  "wiring":[{"edge":["v","` + l1 + `"]},{"edge":["v","` + l2 + `"]}]}`
	recs = inspectRecords([]byte(long), reg, nil)
	if len(recs) < 2 || recs[0].Names[1] != l1 || recs[1].Names[1] != l2 {
		t.Fatalf("records do not keep the full names: %+v", recs)
	}
	if strings.Contains(recs[0].Message, l1) {
		t.Errorf("the message should be the shortened display text: %q", recs[0].Message)
	}
	// An arm names its switched node, predicate and target, and no position.
	arm := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s"},{"name":"c","block":"s2s"}],"wiring":[{"edge":["b","c"]},{"switch":"a","when":[{"pred":"isInt","to":"b"}],"else":"c"}]}`
	recs = inspectRecords([]byte(arm), reg, nil)
	if len(recs) != 1 || recs[0].Kind != "arm" || strings.Join(recs[0].Names, "|") != "a|isInt|b" {
		t.Errorf("arm records = %+v", recs)
	}
}

// A safety above what the block registers is reported beside a structural error too (assemble, which
// reports it otherwise, does not run then), and once when assemble does run.
func TestInspectConfig_SafetyAboveTheRegistrationIsAlwaysReported(t *testing.T) {
	reg := typedChainRegistry(t) // s2i and the others register no options: a side effect
	for name, cfg := range map[string]string{
		"with a structural error": `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s","safety":"idempotent"}],"wiring":[{"edge":["a","b"]},{"edge":["b","ghost"]}]}`,
		"alone":                   `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s","safety":"idempotent"}],"wiring":[{"edge":["a","b"]}]}`,
	} {
		n := 0
		for _, p := range InspectConfig([]byte(cfg), reg, AllowUnusedBlocks()).Problems {
			if strings.Contains(p.Message, `node "b" has safety "idempotent", above the "side_effect" its Go registration declares`) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: the safety problem is reported %d times, want once", name, n)
		}
	}
}

// The refusals only Build makes are found beside a structural error too (an edge to a name the config
// does not have), with and without a Registry: the wiring is checked among the parts that resolve.
func TestInspectConfig_BuildOnlyLoopProblemsBesideAStructuralError(t *testing.T) {
	cfg := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s"},{"name":"c","block":"s2s"},{"name":"x","block":"s2s"},{"name":"y","block":"s2s"}],
	  "wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"a","loop_max":3}],"else":"c"},{"edge":["a","x"]},{"edge":["a","y"]},{"edge":["c","ghost"]}]}`
	for name, reg := range map[string]*Registry{"structural": nil, "typed": typedChainRegistry(t)} {
		var leaves, ghost int
		for _, p := range InspectConfig([]byte(cfg), reg, AllowUnusedBlocks()).Problems {
			if strings.Contains(p.Message, "leaves the loop") {
				leaves++
			}
			if strings.Contains(p.Message, `"ghost"`) {
				ghost++
			}
		}
		if leaves != 2 || ghost == 0 {
			t.Errorf("%s: %d edges that leave the loop and %d problems about the unknown name; want 2 and at least 1", name, leaves, ghost)
		}
	}
}

// A config with ten thousand type problems does not print ten thousand lines: a report lists the first
// hundred and one line counting the rest (what bide check and the viewer show), while the editor
// compares the complete records.
func TestInspectConfig_TenThousandTypeProblemsAreListedAHundredAtATime(t *testing.T) {
	data := []byte(chainConfig(10001)) // 10,000 edges, each a misfit
	r := InspectConfig(data, typedChainRegistry(t), AllowUnusedBlocks())
	got := typeProblemMessages(r)
	if len(got) != maxTypeProblems+1 || got[len(got)-1] != "and 9900 more type problems (types must match exactly)" {
		t.Fatalf("%d type problems listed, last %q", len(got), got[len(got)-1])
	}
	if len(r.Problems) > maxTypeProblems+5 {
		t.Errorf("the report lists %d problems", len(r.Problems))
	}
	var types int
	for _, rec := range inspectRecords(data, typedChainRegistry(t), nil) {
		if rec.Class == "type" {
			types++
		}
	}
	if types != 10000 {
		t.Errorf("the editor's records hold %d type problems, want all 10000", types)
	}
}

// The records of the loop and wiring shapes carry their kind and the full names: Build's (a body that is
// not simple, an inner switch, an edge that leaves a loop) as well as the structural check's.
func TestInspectRecords_LoopShapesCarryTheirNames(t *testing.T) {
	has := func(recs []planaccess.Record, kind string, names ...string) bool {
		for _, r := range recs {
			if r.Class == "loop" && r.Kind == kind && strings.Join(r.Names, "|") == strings.Join(names, "|") {
				return true
			}
		}
		return false
	}
	// A body that is not simple: x lies between the head a and the switch b but is not on the loop path.
	body := `{"version":1,"flow":"f","entry":"e","nodes":[{"name":"e","block":"s2s"},{"name":"a","block":"s2s"},{"name":"x","block":"s2s"},{"name":"b","block":"s2s"},{"name":"c","block":"s2s"}],
	  "wiring":[{"edge":["e","a"]},{"edge":["e","x"]},{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"a","loop_max":3}],"else":"c"}]}`
	if recs := inspectRecords([]byte(body), nil, nil); !has(recs, "non-contiguous-body", "b", "x") {
		t.Errorf("non-contiguous body records: %+v", recs)
	}
	// Edges that leave the loop, and an inner switch.
	leave := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s"},{"name":"c","block":"s2s"},{"name":"x","block":"s2s"}],
	  "wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"a","loop_max":3}],"else":"c"},{"edge":["a","x"]}]}`
	if recs := inspectRecords([]byte(leave), nil, nil); !has(recs, "leaves-the-loop", "a", "x", "b") {
		t.Errorf("leaves-the-loop records: %+v", recs)
	}
	inner := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s"},{"name":"c","block":"s2s"},{"name":"d","block":"s2s"}],
	  "wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"c"}],"else":"c"},{"switch":"c","when":[{"pred":"nonEmpty","to":"a","loop_max":3}],"else":"d"}]}`
	if recs := inspectRecords([]byte(inner), nil, nil); !has(recs, "inner-switch", "b", "c") {
		t.Errorf("inner-switch records: %+v", recs)
	}
	// The structural check's: a loop with no exit arm.
	noExit := `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"s2s"},{"name":"b","block":"s2s"}],"wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"nonEmpty","to":"a","loop_max":3}]}]}`
	if recs := inspectRecords([]byte(noExit), nil, nil); !has(recs, "structural-no-exit-arm", "b") {
		t.Errorf("no-exit-arm records: %+v", recs)
	}
}
