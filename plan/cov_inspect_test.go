package plan

import (
	"strings"
	"testing"
)

// InspectConfig names, for each structural fault, the node, edge or wiring element it belongs to,
// without needing a Registry.
func TestInspectConfig_StructuralFaults(t *testing.T) {
	cfg := func(nodes, wiring string, extra ...string) []byte {
		return []byte(`{"version": 1, "flow": "f", ` + strings.Join(extra, "") + `"nodes": [` + nodes + `], "wiring": [` + wiring + `]}`)
	}
	two := `{"name": "a", "block": "x"}, {"name": "b", "block": "y"}`
	for _, c := range []struct {
		name           string
		data           []byte
		node, from, to string
		has            string
	}{
		{"no nodes", cfg(``, ``), "", "", "", "flow has no nodes"},
		{"a node with no name", cfg(`{"block": "x"}`, ``), "", "", "", `a node (block "x") has no name`},
		{"a node with no block", cfg(`{"name": "a"}`, ``), "a", "", "", `node "a" names no block`},
		{"a duplicate node", cfg(`{"name": "a", "block": "x"}, {"name": "a", "block": "x"}`, ``), "a", "", "", `node "a" is declared more than once`},
		{"unknown entry", cfg(two, ``, `"entry": "zz", `), "", "", "", `entry "zz" names no declared node`},
		{"a wiring element that sets nothing", cfg(two, `{}`), "", "", "", "wiring[0] sets 0 of edge/switch/join; set exactly one"},
		{"a wiring element that sets two", cfg(two, `{"edge": ["a", "b"], "switch": "a", "else": "b"}`), "", "", "", "wiring[0] sets 2 of edge/switch/join; set exactly one"},
		{"an edge of three ends", cfg(two, `{"edge": ["a", "b", "a"]}`), "", "", "", "wiring[0] edge must be [from, to], got 3 element(s)"},
		{"a switch over an unknown node", cfg(two, `{"switch": "zz", "else": "b"}`), "zz", "", "", `wiring[0] switch over unknown node "zz"`},
		{"an arm with no predicate", cfg(two, `{"switch": "a", "when": [{"to": "b"}], "else": "b"}`), "", "a", "b", "wiring[0].when[0] names no predicate"},
		{"an else to an unknown node", cfg(two, `{"switch": "a", "else": "zz"}`), "", "a", "zz", `wiring[0] else routes to unknown node "zz"`},
		{"a join with no name", cfg(two, `{"merge": "m", "inputs": ["a", "b"]}`), "", "", "", "wiring[0] join has no name"},
		{"a join with no merge", cfg(two, `{"join": "j", "inputs": ["a", "b"]}`), "j", "", "", `wiring[0] join "j" names no merge block`},
		{"a join reading an unknown input", cfg(two, `{"join": "j", "merge": "m", "inputs": ["a", "zz"]}`), "", "zz", "j", `wiring[0] join "j" reads unknown input "zz"`},
		{"a join named for a node", cfg(two, `{"join": "a", "merge": "m", "inputs": ["a", "b"]}`), "a", "", "", `join "a" is already a declared node`},
	} {
		r := InspectConfig(c.data, nil)
		found := false
		for _, p := range problemAt(r, c.node, c.from, c.to) {
			found = found || p.Severity == "error" && strings.Contains(p.Message, c.has)
		}
		if !found {
			t.Errorf("%s: no error containing %q at node %q edge %q->%q; problems %+v", c.name, c.has, c.node, c.from, c.to, r.Problems)
		}
		if !r.Errors() {
			t.Errorf("%s: Errors() is false", c.name)
		}
	}
}

// A join in a config is drawn in the topology as a join node reading its inputs, and its safety
// class is the one its config names.
func TestInspectConfig_JoinTopologyAndSafety(t *testing.T) {
	for safety, want := range map[string]NodeSafety{
		"readonly": SafetyReadOnly, "idempotent": SafetyIdempotent, "side_effect": SafetySideEffect, "": SafetyUnknown, "bogus": SafetyUnknown,
	} {
		sf := ""
		if safety != "" {
			sf = `, "safety": "` + safety + `"`
		}
		data := []byte(`{"version": 1, "flow": "f", "nodes": [{"name": "a", "block": "x"}, {"name": "b", "block": "y"}],
			"wiring": [{"join": "j", "merge": "m", "inputs": ["a", "b"]` + sf + `}]}`)
		r := InspectConfig(data, nil)
		if got := r.Safety["j"]; got != want {
			t.Errorf("join safety %q = %q, want %q", safety, got, want)
		}
		var join *TopologyNode
		for i, n := range r.Topology.Nodes {
			if n.Name == "j" {
				join = &r.Topology.Nodes[i]
			}
		}
		if join == nil || join.Kind != NodeKindJoin || join.Block != "m" {
			t.Fatalf("the join is not a join node of block m: %+v", r.Topology.Nodes)
		}
		var from []string
		for _, e := range r.Topology.Edges {
			if e.To == "j" {
				from = append(from, e.From)
			}
		}
		if strings.Join(from, ",") != "a,b" || len(r.Topology.Joins) != 1 || r.Topology.Joins[0].Name != "j" {
			t.Errorf("join edges %v, joins %+v; want a,b into j", from, r.Topology.Joins)
		}
	}
	// With no explicit entry, the first node is the entry.
	r := InspectConfig([]byte(`{"version": 1, "flow": "f", "nodes": [{"name": "first", "block": "x"}, {"name": "second", "block": "y"}], "wiring": []}`), nil)
	if r.Topology.Entry != "first" {
		t.Errorf("entry = %q, want the first node", r.Topology.Entry)
	}
}
