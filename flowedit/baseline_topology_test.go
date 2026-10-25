package flowedit

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bide-ai/bide/plan"
)

// renameFixture names the node "a" in every place a topology can: entry, node, both ends of an edge, a
// Switch's node, an arm's target, `else`, a join's inputs, and a loop's head, switch and body.
func renameFixture() plan.Topology {
	return plan.Topology{
		Flow: "f", Entry: "a",
		Nodes: []plan.TopologyNode{
			{Name: "a", Kind: plan.NodeKindStep, Block: "a"}, {Name: "b", Kind: plan.NodeKindSwitch, Block: "b"},
			{Name: "c", Kind: plan.NodeKindStep, Block: "c"}, {Name: "j", Kind: plan.NodeKindJoin, Block: "m"},
		},
		Edges: []plan.TopologyEdge{{From: "a", To: "b"}, {From: "c", To: "a"}, {From: "a", To: "j"}, {From: "c", To: "j"}},
		Branches: []plan.TopologyBranch{
			{Over: "a", Arms: []plan.TopologyBranchArm{{Target: "c"}, {Target: "a", LoopBack: true, LoopMax: 3}, {Target: "b", Else: true}}, Else: "b"},
			{Over: "b", Arms: []plan.TopologyBranchArm{{Target: "a", Else: true}}, Else: "a"},
		},
		Joins: []plan.TopologyJoin{{Name: "j", Inputs: []string{"a", "c"}, InTypes: []string{"int", "int"}, InCanonical: []string{"int", "int"}}},
		Loops: []plan.TopologyLoop{{Head: "a", Over: "a", Body: []string{"a", "c"}, Max: 3}},
	}
}

func js(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Renaming a node rewrites every place that names it, and only those. The expected topology is the
// fixture with every "a" spelled "a2" in the places it can be, written out, so a field the rename misses
// fails here (each field has a mutation that skips it).
func TestRenameTopologyRewritesEveryPlace(t *testing.T) {
	want := plan.Topology{
		Flow: "f", Entry: "a2",
		Nodes: []plan.TopologyNode{
			{Name: "a2", Kind: plan.NodeKindStep, Block: "a"}, {Name: "b", Kind: plan.NodeKindSwitch, Block: "b"},
			{Name: "c", Kind: plan.NodeKindStep, Block: "c"}, {Name: "j", Kind: plan.NodeKindJoin, Block: "m"},
		},
		Edges: []plan.TopologyEdge{{From: "a2", To: "b"}, {From: "c", To: "a2"}, {From: "a2", To: "j"}, {From: "c", To: "j"}},
		Branches: []plan.TopologyBranch{
			{Over: "a2", Arms: []plan.TopologyBranchArm{{Target: "c"}, {Target: "a2", LoopBack: true, LoopMax: 3}, {Target: "b", Else: true}}, Else: "b"},
			{Over: "b", Arms: []plan.TopologyBranchArm{{Target: "a2", Else: true}}, Else: "a2"},
		},
		Joins: []plan.TopologyJoin{{Name: "j", Inputs: []string{"a2", "c"}, InTypes: []string{"int", "int"}, InCanonical: []string{"int", "int"}}},
		Loops: []plan.TopologyLoop{{Head: "a2", Over: "a2", Body: []string{"a2", "c"}, Max: 3}},
	}
	in := renameFixture()
	before := js(t, in)
	got, made := renameTopology(in, []Rename{{From: "a", To: "a2"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%s\nwant\n%s", js(t, got), js(t, want))
	}
	if len(made) != 1 || made[0] != (Rename{From: "a", To: "a2"}) {
		t.Errorf("made %v", made)
	}
	if js(t, in) != before {
		t.Error("the input changed")
	}
	// Nothing in the result is the input's memory.
	got.Edges[0].To, got.Loops[0].Body[0], got.Joins[0].Inputs[0], got.Branches[0].Arms[0].Target = "x", "x", "x", "x"
	if js(t, in) != before {
		t.Error("the result shares memory with the input")
	}
}

// A swap (a and b trade names) and a chain (a -> b, b -> c, with c free) work in order, as the editor's
// operations did; the result is the fixture with the names traded.
func TestRenameTopologySwapAndChain(t *testing.T) {
	in := plan.Topology{Flow: "f", Entry: "a",
		Nodes: []plan.TopologyNode{{Name: "a", Block: "ba"}, {Name: "b", Block: "bb"}},
		Edges: []plan.TopologyEdge{{From: "a", To: "b"}}}
	got, made := renameTopology(in, []Rename{{From: "a", To: "tmp"}, {From: "b", To: "a"}, {From: "tmp", To: "b"}})
	if len(made) != 3 || got.Entry != "b" || got.Nodes[0].Name != "b" || got.Nodes[0].Block != "ba" || got.Nodes[1].Name != "a" || got.Edges[0] != (plan.TopologyEdge{From: "b", To: "a"}) {
		t.Errorf("swap: %s (made %v)", js(t, got), made)
	}
	got, made = renameTopology(in, []Rename{{From: "b", To: "c"}, {From: "a", To: "b"}})
	if len(made) != 2 || got.Entry != "b" || got.Nodes[0].Name != "b" || got.Nodes[1].Name != "c" || got.Edges[0] != (plan.TopologyEdge{From: "b", To: "c"}) {
		t.Errorf("chain: %s (made %v)", js(t, got), made)
	}
}

// A rename the editor's operation would refuse is skipped whole, and the next one is still tried.
func TestRenameTopologyRefusals(t *testing.T) {
	in := renameFixture()
	dup := renameFixture()
	dup.Nodes = append(dup.Nodes, plan.TopologyNode{Name: "c", Kind: plan.NodeKindStep})
	for name, c := range map[string]struct {
		t plan.Topology
		r Rename
	}{
		"missing":                {in, Rename{"zz", "y"}},
		"the new name is a node": {in, Rename{"a", "c"}},
		"the new name is a join": {in, Rename{"a", "j"}},
		"a join":                 {in, Rename{"j", "k"}},
		"the same name":          {in, Rename{"a", "a"}},
		"empty name":             {in, Rename{"a", ""}},
		"space":                  {in, Rename{"a", " a2"}},
		"control":                {in, Rename{"a", "a\x00"}},
		"bidi":                   {in, Rename{"a", "a‮2"}},
		"too long":               {in, Rename{"a", strings.Repeat("x", maxNameLen+1)}},
		"declared twice":         {dup, Rename{"c", "c2"}},
	} {
		got, made := renameTopology(c.t, []Rename{c.r})
		if len(made) != 0 || js(t, got) != js(t, c.t) {
			t.Errorf("%s: made %v, changed %s", name, made, js(t, got))
		}
	}
	got, made := renameTopology(in, []Rename{{"a", "c"}, {"a", "a2"}})
	if len(made) != 1 || got.Entry != "a2" {
		t.Errorf("a refused rename stops the list: %v %s", made, js(t, got))
	}
}

// With no renames, or more than the bound, the topology is returned as it was.
func TestRenameTopologyBounds(t *testing.T) {
	in := renameFixture()
	if got, made := renameTopology(in, nil); len(made) != 0 || js(t, got) != js(t, in) {
		t.Errorf("none: %v", made)
	}
	many := make([]Rename, maxRenames+1)
	for i := range many {
		many[i] = Rename{"a", "z"}
	}
	if got, made := renameTopology(in, many); len(made) != 0 || got.Entry != "a" {
		t.Errorf("past the bound: %v", made)
	}
}
