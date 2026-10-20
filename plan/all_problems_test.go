package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
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
	all := typeProblemMessages(InspectConfig([]byte(chainConfig(123)), reg, AllowUnusedBlocks(), allTypeProblems()))
	if len(all) != 122 {
		t.Errorf("AllTypeProblems: %d problems, want 122", len(all))
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
