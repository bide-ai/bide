package plan

import (
	"strings"
	"testing"
)

// TestInspectConfig_Valid: a valid config, with the Registry, has no errors and reports the digest
// Load's flow journals; without it, the same config is structurally clean and has no digest.
func TestInspectConfig_Valid(t *testing.T) {
	reg := triageRegistry(t)
	f, err := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), reg)
	if err != nil {
		t.Fatal(err)
	}
	r := InspectConfig([]byte(triageConfig), reg)
	if r.Errors() || !r.Typed || r.Digest != f.Digest() {
		t.Fatalf("typed: errors %v, digest %s, want %s; problems %+v", r.Errors(), r.Digest, f.Digest(), r.Problems)
	}
	if d, err := TopologyDigest(r.Topology); err != nil || d != f.Digest() {
		t.Fatalf("the reported topology's digest = %s, %v; want %s", d, err, f.Digest())
	}
	s := InspectConfig([]byte(triageConfig), nil)
	if s.Errors() || s.Typed || s.Digest != "" || len(s.Topology.Nodes) != 4 || len(s.Topology.Branches) != 1 {
		t.Fatalf("structural: %+v", s)
	}
}

// problemAt returns the problems attached at node (or at the edge from -> to when node is "").
func problemAt(r ConfigReport, node, from, to string) []ConfigProblem {
	var out []ConfigProblem
	for _, p := range r.Problems {
		if p.Node == node && p.From == from && p.To == to {
			out = append(out, p)
		}
	}
	return out
}

// TestInspectConfig_ProblemsWhereTheyBelong: each problem class is attached to its node, edge or
// arm, structurally and with the Registry.
func TestInspectConfig_ProblemsWhereTheyBelong(t *testing.T) {
	edit := func(old, new string) []byte {
		if !strings.Contains(triageConfig, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return []byte(strings.Replace(triageConfig, old, new, 1))
	}
	cases := []struct {
		name           string
		data           []byte
		typed          bool
		node, from, to string
		severity, has  string
	}{
		{"dangling edge", edit(`{"edge": ["reserve", "finalize"]}`, `{"edge": ["reserve", "finalise"]}`), false, "", "reserve", "finalise", "error", `"finalise" is not a declared node`},
		{"arm to nowhere", edit(`"to": "reserve"`, `"to": "reserv"`), false, "", "classify", "reserv", "error", "routes to unknown node"},
		{"missing else", edit(`, "else": "decline"`, ``), false, "classify", "", "", "warning", "has no else"},
		{"invalid approval", edit(`{"name": "reserve",  "block": "reserve"}`, `{"name": "reserve",  "block": "reserve", "approval": {"need": 3, "approvers": ["ops", "finance"]}}`), false, "reserve", "", "", "error", "approval"},
		{"unknown safety", edit(`{"name": "decline",  "block": "decline"}`, `{"name": "decline",  "block": "decline", "safety": "maybe"}`), false, "decline", "", "", "error", "unknown safety"},
		{"unregistered block", edit(`{"name": "finalize", "block": "finalize"}`, `{"name": "finalize", "block": "finalise"}`), true, "finalize", "", "", "error", `unknown block "finalise"`},
		{"unregistered predicate", edit(`"pred": "rush"`, `"pred": "rusht"`), true, "", "classify", "reserve", "error", `unknown predicate "rusht"`},
		{"type mismatch", edit(`{"edge": ["reserve", "finalize"]}`, `{"edge": ["reserve", "decline"]}`), true, "", "reserve", "decline", "error", "types must match"},
	}
	for _, c := range cases {
		var reg *Registry
		if c.typed {
			reg = triageRegistry(t)
		}
		r := InspectConfig(c.data, reg)
		ps := problemAt(r, c.node, c.from, c.to)
		found := false
		for _, p := range ps {
			found = found || p.Severity == c.severity && strings.Contains(p.Message, c.has)
		}
		if !found {
			t.Errorf("%s: no %s containing %q at node %q edge %q->%q; problems %+v", c.name, c.severity, c.has, c.node, c.from, c.to, r.Problems)
		}
	}
}

// TestInspectConfig_RegistryFindsWhatStructureCannot: an unregistered block and a type mismatch are
// found only with the Registry; the structural check passes them.
func TestInspectConfig_RegistryFindsWhatStructureCannot(t *testing.T) {
	data := []byte(strings.Replace(triageConfig, `{"edge": ["reserve", "finalize"]}`, `{"edge": ["reserve", "decline"]}`, 1))
	if r := InspectConfig(data, nil); r.Errors() {
		t.Fatalf("the structural check found %+v; it cannot see types", r.Problems)
	}
	if r := InspectConfig(data, triageRegistry(t)); !r.Errors() {
		t.Fatal("the Registry check missed the type mismatch")
	}
}

// TestInspectConfig_Unparseable: a config that does not parse is one config-level error.
func TestInspectConfig_Unparseable(t *testing.T) {
	r := InspectConfig([]byte(`{"version": 1, "flow": "x", "nodes": [], "wiring": [], "aproval": 1}`), nil)
	if !r.Errors() || r.Problems[0].Node != "" || !strings.Contains(r.Problems[0].Message, "aproval") {
		t.Fatalf("problems = %+v", r.Problems)
	}
}

// A valid approval block is no problem: plan flows enforce it. It is listed in Approvals and on its
// node in Topology, structurally and with the Registry (whose digest commits to it).
func TestInspectConfig_ApprovalGate(t *testing.T) {
	data := []byte(strings.Replace(triageConfig, `{"name": "reserve",  "block": "reserve"}`, `{"name": "reserve",  "block": "reserve", "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}}`, 1))
	plain := InspectConfig([]byte(triageConfig), triageRegistry(t))
	for _, reg := range []*Registry{nil, triageRegistry(t)} {
		r := InspectConfig(data, reg)
		if r.Errors() {
			t.Fatalf("typed=%v: problems %+v; want none", reg != nil, r.Problems)
		}
		if p := r.Approvals["reserve"]; p.Need != 2 || len(p.Approvers) != 3 {
			t.Fatalf("typed=%v: Approvals = %+v", reg != nil, r.Approvals)
		}
		var gate *TopologyGate
		for _, n := range r.Topology.Nodes {
			if n.Name == "reserve" {
				gate = n.Gate
			}
		}
		if gate == nil || gate.Need != 2 {
			t.Fatalf("typed=%v: reserve's topology gate = %+v", reg != nil, gate)
		}
		if reg != nil && (r.Digest == "" || r.Digest == plain.Digest) {
			t.Fatalf("the gated config's digest %q does not differ from the ungated %q", r.Digest, plain.Digest)
		}
	}
}
