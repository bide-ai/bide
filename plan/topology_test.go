package plan

import (
	"encoding/json"
	"reflect"
	"testing"
)

// topologyFixtureFlow builds a frozen Flow whose spec is assembled directly (the
// exported constructors live elsewhere and are exercised by their own tests) so
// Topology can be verified against a known shape covering a switch, a join, and a
// bounded loopback in one graph. The declared topology is:
//
//	parse -> gen -> gate(switch)
//	gate --When loop <=3--> gen        (bounded back-edge to the loop head)
//	gate --Else--> reject
//	gen, aux -> merge(join)            (a two-input fan-in)
//
// gen is the loop head; gate is the loop switch; the join merges gen and aux.
func topologyFixtureFlow() *Flow[string, string] {
	nParse := &node{name: "parse", kind: kindStep, inType: typeOf[string](), outType: typeOf[int]()}
	nGen := &node{name: "gen", kind: kindModel, inType: typeOf[int](), outType: typeOf[int]()}
	nGate := &node{name: "gate", kind: kindSwitch, inType: typeOf[int](), outType: typeOf[int]()}
	nReject := &node{name: "reject", kind: kindTool, inType: typeOf[int](), outType: typeOf[string]()}
	nAux := &node{name: "aux", kind: kindStep, inType: typeOf[int](), outType: typeOf[bool]()}
	nMerge := &node{
		name:        "merge",
		kind:        kindJoin,
		outType:     typeOf[string](),
		joinInputs:  []string{"gen", "aux"},
		joinInTypes: []reflect.Type{typeOf[int](), typeOf[bool]()},
	}

	c := &builderCore{
		flowName: "generate",
		entry:    "parse",
		inType:   typeOf[string](),
		outType:  typeOf[string](),
		nodes:    []*node{nParse, nGen, nGate, nReject, nAux, nMerge},
		byName: map[string]*node{
			"parse": nParse, "gen": nGen, "gate": nGate,
			"reject": nReject, "aux": nAux, "merge": nMerge,
		},
		edges: []edge{
			{from: "parse", to: "gen"},
			{from: "gen", to: "gate"},
			{from: "gen", to: "merge"},
			{from: "aux", to: "merge"},
		},
		branches: []branch{{
			over: "gate",
			arms: []arm{
				{isElse: false, pred: func(v any) bool { return true }, target: "gen", loopBack: true, loopMax: 3},
				{isElse: true, target: "reject"},
			},
		}},
		loops: []loopSpec{{
			head:    "gen",
			over:    "gate",
			body:    []string{"gen", "gate"},
			max:     3,
			headIdx: 1,
			overIdx: 2,
		}},
	}
	return &Flow[string, string]{core: c}
}

func TestTopology_NodesEdgesAndBoundary(t *testing.T) {
	top := topologyFixtureFlow().Topology()

	if top.Flow != "generate" {
		t.Errorf("Flow = %q, want %q", top.Flow, "generate")
	}
	if top.Entry != "parse" {
		t.Errorf("Entry = %q, want %q", top.Entry, "parse")
	}
	if top.In != "string" || top.Out != "string" {
		t.Errorf("boundary In/Out = %q/%q, want string/string", top.In, top.Out)
	}

	// Nodes, in insertion order, each with kind and I/O type names as strings. The
	// join has an empty In (no single input type); its ports live in Joins.
	want := []TopologyNode{
		{Name: "parse", Kind: "step", In: "string", Out: "int"},
		{Name: "gen", Kind: "model", In: "int", Out: "int"},
		{Name: "gate", Kind: "switch", In: "int", Out: "int"},
		{Name: "reject", Kind: "tool", In: "int", Out: "string"},
		{Name: "aux", Kind: "step", In: "int", Out: "bool"},
		{Name: "merge", Kind: "join", In: "", Out: "string"},
	}
	if !reflect.DeepEqual(top.Nodes, want) {
		t.Errorf("Nodes mismatch:\n got %#v\nwant %#v", top.Nodes, want)
	}

	// Edges in insertion order; the two edges feeding the join carry their port types.
	wantEdges := []TopologyEdge{
		{From: "parse", To: "gen"},
		{From: "gen", To: "gate"},
		{From: "gen", To: "merge", Type: "int"},
		{From: "aux", To: "merge", Type: "bool"},
	}
	if !reflect.DeepEqual(top.Edges, wantEdges) {
		t.Errorf("Edges mismatch:\n got %#v\nwant %#v", top.Edges, wantEdges)
	}
}

func TestTopology_BranchesJoinsAndLoops(t *testing.T) {
	top := topologyFixtureFlow().Topology()

	// One Switch over "gate": a loopback When arm (to the head "gen", max 3) and an
	// Else arm to "reject". The Else convenience field names the Else target.
	wantBranches := []TopologyBranch{{
		Over: "gate",
		Arms: []TopologyBranchArm{
			{Target: "gen", LoopBack: true, LoopMax: 3},
			{Target: "reject", Else: true},
		},
		Else: "reject",
	}}
	if !reflect.DeepEqual(top.Branches, wantBranches) {
		t.Errorf("Branches mismatch:\n got %#v\nwant %#v", top.Branches, wantBranches)
	}

	// One join "merge" with its ordered inputs and positionally-aligned port types.
	wantJoins := []TopologyJoin{{
		Name:    "merge",
		Inputs:  []string{"gen", "aux"},
		InTypes: []string{"int", "bool"},
		Out:     "string",
	}}
	if !reflect.DeepEqual(top.Joins, wantJoins) {
		t.Errorf("Joins mismatch:\n got %#v\nwant %#v", top.Joins, wantJoins)
	}

	// One derived loop: head gen, switch gate, body region gen..gate, bound 3.
	wantLoops := []TopologyLoop{{
		Head: "gen",
		Over: "gate",
		Body: []string{"gen", "gate"},
		Max:  3,
	}}
	if !reflect.DeepEqual(top.Loops, wantLoops) {
		t.Errorf("Loops mismatch:\n got %#v\nwant %#v", top.Loops, wantLoops)
	}
}

func TestTopology_JSONRoundTrips(t *testing.T) {
	top := topologyFixtureFlow().Topology()

	data, err := json.Marshal(top)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var back Topology
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// The snapshot is pure data, so it survives a JSON round-trip without loss.
	if !reflect.DeepEqual(top, back) {
		t.Errorf("Topology did not round-trip through JSON:\n got %#v\nwant %#v", back, top)
	}
}
