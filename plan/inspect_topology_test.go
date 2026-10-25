package plan

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// policyKey is the fields of a policy that a gate carries into a Topology, so a policy and the one a
// topology gives back compare equal.
func policyKey(p agent.ApprovalPolicy) string {
	b, _ := json.Marshal(struct {
		Need      int
		Approvers []string
		Exclude   bool
		Timeout   time.Duration
	}{p.Need, p.Approvers, p.ExcludePrincipal, p.Timeout})
	return string(b)
}

// InspectTopology over the topology a config declares must give the report InspectConfig gave, for the
// parts a topology holds: the digest, the gates, and Gated. It runs over the whole corpus of the
// differential test, valid and broken configs, with and without the Registry.
func TestInspectTopologyAgreesWithInspectConfig(t *testing.T) {
	total, typed, gated, withGate := 0, 0, 0, 0
	for _, c := range describedCases() {
		reg := c.reg(t)
		for _, cfg := range variants(t, c.cfg, reg) {
			for _, r := range []ConfigReport{InspectConfig([]byte(cfg), reg), InspectConfig([]byte(cfg), nil)} {
				got := InspectTopology(r.Topology)
				total++
				if !reflect.DeepEqual(got.Topology, r.Topology) && len(r.Topology.Nodes) > 0 {
					t.Fatalf("the topology changed for\n%s", cfg)
				}
				if r.Digest != "" {
					typed++
					if got.Digest != r.Digest {
						t.Fatalf("digest %q, InspectConfig's %q for\n%s", got.Digest, r.Digest, cfg)
					}
				}
				// InspectConfig lists the gates the config declares; the topology also carries the ones a
				// block declares in Go, so with a Registry the topology's are a superset.
				if r.Typed && len(got.Approvals) < len(r.Approvals) || !r.Typed && len(got.Approvals) != len(r.Approvals) {
					t.Fatalf("approvals %v, InspectConfig's %v for\n%s", got.Approvals, r.Approvals, cfg)
				}
				for n, p := range r.Approvals {
					withGate++
					if policyKey(got.Approvals[n]) != policyKey(p) {
						t.Fatalf("gate %s: %s, InspectConfig's %s", n, policyKey(got.Approvals[n]), policyKey(p))
					}
				}
				if len(r.Topology.Nodes) > 0 && !reflect.DeepEqual(got.Gated, r.Gated) {
					t.Fatalf("gated %v, InspectConfig's %v for\n%s", got.Gated, r.Gated, cfg)
				}
				for _, g := range r.Gated {
					if g {
						gated++
					}
				}
				if got.Safety != nil || got.Assertions != nil || got.Typed || len(got.Problems) > 0 || len(got.Findings) > 0 {
					t.Fatalf("a topology report holds only the topology's parts: %+v", got)
				}
			}
		}
	}
	if total < 500 || typed < 20 || gated == 0 || withGate == 0 {
		t.Errorf("the corpus is too thin: %d reports, %d with a digest, %d gated nodes, %d gates", total, typed, gated, withGate)
	}
}

// A report shares no memory with the topology it was made from, so a caller that changes the topology
// afterwards, or the report, changes neither.
func TestInspectTopologyDoesNotAliasItsInput(t *testing.T) {
	t0 := Topology{Flow: "f", Entry: "a",
		Nodes:    []TopologyNode{{Name: "a", Kind: NodeKindStep, Gate: &TopologyGate{Need: 1, Approvers: []string{"x"}}}, {Name: "j", Kind: NodeKindJoin}},
		Edges:    []TopologyEdge{{From: "a", To: "j"}},
		Branches: []TopologyBranch{{Over: "a", Arms: []TopologyBranchArm{{Target: "j"}}}},
		Joins:    []TopologyJoin{{Name: "j", Inputs: []string{"a"}, InTypes: []string{"int"}, InCanonical: []string{"int"}}},
		Loops:    []TopologyLoop{{Head: "a", Over: "a", Body: []string{"a"}, Max: 2}},
	}
	want, _ := json.Marshal(t0)
	r := InspectTopology(t0)
	r.Topology.Nodes[0].Gate.Approvers[0] = "changed"
	r.Topology.Nodes[0].Gate.Need = 9
	r.Topology.Nodes[0].Name = "changed"
	r.Topology.Edges[0].To = "changed"
	r.Topology.Branches[0].Arms[0].Target = "changed"
	r.Topology.Joins[0].Inputs[0] = "changed"
	r.Topology.Joins[0].InTypes[0] = "changed"
	r.Topology.Joins[0].InCanonical[0] = "changed"
	r.Topology.Loops[0].Body[0] = "changed"
	if p := r.Approvals["a"]; p.Approvers != nil && len(p.Approvers) > 0 && p.Approvers[0] == "changed" {
		t.Error("the policy shares the gate's approvers")
	}
	if got, _ := json.Marshal(t0); string(got) != string(want) {
		t.Errorf("the input changed:\n%s\nwas\n%s", got, want)
	}
}

// A one-decision gate is the policy agent.SingleApproval returns.
func TestInspectTopologySingleGate(t *testing.T) {
	r := InspectTopology(Topology{Entry: "a", Nodes: []TopologyNode{{Name: "a", Gate: &TopologyGate{Single: true}}}})
	if p, ok := r.Approvals["a"]; !ok || p.Need != 1 || !r.Gated["a"] {
		t.Errorf("approvals %+v gated %v", r.Approvals, r.Gated)
	}
}

// Whatever the topology holds, InspectTopology returns: no node, a dangling edge, a duplicate name, an
// entry that names no node, a self loop, a join with no TopologyJoin.
func TestInspectTopologyToleratesHostileTopologies(t *testing.T) {
	for name, t0 := range map[string]Topology{
		"empty":          {},
		"no entry node":  {Entry: "x", Nodes: []TopologyNode{{Name: "a"}}},
		"dangling edge":  {Entry: "a", Nodes: []TopologyNode{{Name: "a"}}, Edges: []TopologyEdge{{From: "a", To: "nowhere"}, {From: "ghost", To: "a"}}},
		"duplicate name": {Entry: "a", Nodes: []TopologyNode{{Name: "a", Gate: &TopologyGate{Single: true}}, {Name: "a"}}},
		"self loop":      {Entry: "a", Nodes: []TopologyNode{{Name: "a"}}, Edges: []TopologyEdge{{From: "a", To: "a"}}},
		"orphan join":    {Entry: "a", Nodes: []TopologyNode{{Name: "a"}, {Name: "j", Kind: NodeKindJoin}}},
		"nil arm target": {Entry: "a", Nodes: []TopologyNode{{Name: "a"}}, Branches: []TopologyBranch{{Arms: []TopologyBranchArm{{}}}}},
	} {
		r := InspectTopology(t0)
		if len(t0.Nodes) != len(r.Gated) && name != "duplicate name" {
			t.Errorf("%s: gated %v for %d nodes", name, r.Gated, len(t0.Nodes))
		}
	}
}

func FuzzInspectTopology(f *testing.F) {
	f.Add([]byte(`{"flow":"f","entry":"a","nodes":[{"name":"a","kind":"step","gate":{"need":1,"approvers":["x"]}},{"name":"b","kind":"step"}],"edges":[{"from":"a","to":"b"},{"from":"b","to":"zz"}],"branches":[{"over":"b","arms":[{"target":"a","loop_back":true,"loop_max":2}]}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var t0 Topology
		if json.Unmarshal(data, &t0) != nil {
			return
		}
		r := InspectTopology(t0)
		if len(r.Gated) > len(t0.Nodes) {
			t.Fatalf("gated %d of %d nodes", len(r.Gated), len(t0.Nodes))
		}
		again := InspectTopology(r.Topology)
		if !reflect.DeepEqual(again.Gated, r.Gated) || again.Digest != r.Digest {
			t.Fatalf("not stable: %v vs %v", again.Gated, r.Gated)
		}
	})
}
