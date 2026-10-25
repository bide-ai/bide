package runview

import (
	"fmt"
	"slices"

	"github.com/bide-ai/bide/plan"
)

// MaxTopologyNodes and MaxTopologyEdges bound the topology a diff against a journaled run reads: its
// nodes, and its edges plus Switch arms. The loader bounds a config by bytes only, and the diff costs
// more than linear time (about 0.66 s at 1000 nodes with disjoint names, 3.2 s at 4000), so a topology
// a journal supplies is refused past these.
const (
	MaxTopologyNodes = 1000
	MaxTopologyEdges = 4000
)

// TopologyDiff is two sides compared as what a run's resume compares: the topology digest.
//
// A journaled run holds a plan.Topology and nothing else, so each side is a report (plan.InspectTopology
// for the run, plan.InspectConfig for the file). The comparison counts only what the digest covers
// and nothing it does not: safety and assert lines are in neither a journal nor the digest, so they
// are never compared, and the order of nodes, edges and arms, which the digest commits to and the
// config diff does not itemise, is.
type TopologyDiff struct {
	// Old and New are the sides; Old is the run's.
	Old, New plan.ConfigReport
	// OldName and NewName label them.
	OldName, NewName string
	// Typed says both sides know their kinds, types and gates: the file was checked against a
	// plan.Registry (or a description of one). Without it the comparison is structural: flow name,
	// entry, node names and blocks, edges, Switch arms and their order. A journaled gate is the
	// resolved policy (a block's own Go gate, a one-decision gate), which a structural file cannot
	// show, so gates, and every verdict about them, are compared only when Typed.
	Typed bool
	// Basis is what the file's typed result rests on (SourceRegistry or SourceDescription), empty
	// when structural.
	Basis string
}

// NormalizeForRun returns r as it is compared with a run: a deep copy (a run's report is cached and
// shared, so the original is never changed), with Safety and Assertions removed, and every node
// counted as gated and every report as checked, so the config diff's path and safety verdicts, which
// need what a topology lacks, cannot arise. Without typed it also drops what only a Registry knows:
// node kinds (but a join's), types and gates.
func NormalizeForRun(r plan.ConfigReport, typed bool) plan.ConfigReport {
	n := plan.InspectTopology(r.Topology) // a deep copy
	n.Digest = r.Digest
	n.Typed = true
	n.Approvals = nil
	n.Gated = map[string]bool{}
	t := &n.Topology
	for i := range t.Nodes {
		n.Gated[t.Nodes[i].Name] = true
		if typed {
			continue
		}
		x := &t.Nodes[i]
		if x.Kind != plan.NodeKindJoin {
			x.Kind = ""
		}
		x.In, x.Out, x.InCanonical, x.OutCanonical, x.Gate = "", "", "", "", nil
	}
	if !typed {
		t.In, t.Out, t.InCanonical, t.OutCanonical = "", "", "", ""
		for i := range t.Edges {
			t.Edges[i].Type = ""
		}
		for i := range t.Joins {
			t.Joins[i].InTypes, t.Joins[i].InCanonical, t.Joins[i].Out = nil, nil, ""
		}
		n.Digest = ""
	}
	return n
}

// DiffTopologies compares a journaled run's topology (Old) with a file's (New) by what the digest
// covers. See TopologyDiff.
func DiffTopologies(d TopologyDiff) (*DiffView, error) {
	for _, s := range []struct {
		name string
		r    plan.ConfigReport
	}{{d.OldName, d.Old}, {d.NewName, d.New}} {
		t := s.r.Topology
		if len(t.Nodes) == 0 && s.r.Errors() {
			return nil, fmt.Errorf("%s: %s", s.name, s.r.Problems[0].Message)
		}
		arms := 0
		for _, b := range t.Branches {
			arms += len(b.Arms)
		}
		if len(t.Nodes) > MaxTopologyNodes || len(t.Edges)+arms > MaxTopologyEdges {
			return nil, fmt.Errorf("%s: the topology has %d nodes and %d edges and arms; the comparison reads at most %d and %d",
				s.name, len(t.Nodes), len(t.Edges)+arms, MaxTopologyNodes, MaxTopologyEdges)
		}
	}
	v := &DiffView{Name: d.NewName, OldName: d.OldName, Flow: d.New.Topology.Flow, Typed: d.Typed, Basis: d.Basis,
		OldBasis: "journal", NewBasis: "structural", NewErrors: errorCount(d.New)}
	if d.Typed {
		v.NewBasis, v.OldDigest, v.NewDigest = d.Basis, d.Old.Digest, d.New.Digest
		if d.New.Digest == "" {
			v.Cannot = fmt.Sprintf("the file has errors (%d), so it has no digest", v.NewErrors)
			v.Changes = []Change{}
			v.Verdict = topologyVerdict(v)
			return v, nil
		}
	}
	o, n := NormalizeForRun(d.Old, d.Typed), NormalizeForRun(d.New, d.Typed)
	v.finish(o, n, nil, nil)
	v.Changes = append(v.Changes, orderChanges(o, n)...)
	if v.Changes == nil {
		v.Changes = []Change{}
	}
	if d.Typed && d.Old.Digest != "" && d.Old.Digest != d.New.Digest && len(v.Changes) == 0 {
		v.DigestNote = "the digest differs in a way the diff does not itemise (for example a type spelled with another package path)"
	}
	v.Verdict = topologyVerdict(v)
	return v, nil
}

// orderChanges lists where the digest's order differs: the digest commits to the order of nodes,
// edges, Switches, arms and join inputs, and the config diff matches by name. Only what both sides
// hold is ordered, so an added or removed element, which the diff already lists, is not counted twice.
func orderChanges(o, n plan.ConfigReport) []Change {
	ren := renamedNodes(o, n)
	name := func(s string) string {
		if to, ok := ren[s]; ok {
			return to
		}
		return s
	}
	var cs []Change
	add := func(what string, details ...string) {
		cs = append(cs, Change{Change: "changed", What: "order", Node: what, Details: details})
	}
	var on, nn []string
	for _, x := range o.Topology.Nodes {
		on = append(on, name(x.Name))
	}
	for _, x := range n.Topology.Nodes {
		nn = append(nn, x.Name)
	}
	if !sameOrder(on, nn) {
		add("nodes", "the order of the nodes changed")
	}
	var oe, ne []string
	for _, e := range o.Topology.Edges {
		oe = append(oe, name(e.From)+"\x00"+name(e.To))
	}
	for _, e := range n.Topology.Edges {
		ne = append(ne, e.From+"\x00"+e.To)
	}
	if !sameOrder(oe, ne) {
		add("edges", "the order of the edges changed")
	}
	var ob, nb []string
	for _, b := range o.Topology.Branches {
		ob = append(ob, name(b.Over))
	}
	for _, b := range n.Topology.Branches {
		nb = append(nb, b.Over)
	}
	if !sameOrder(ob, nb) {
		add("switches", "the order of the switches changed")
	}
	newBranch := map[string]plan.TopologyBranch{}
	for _, b := range n.Topology.Branches {
		newBranch[b.Over] = b
	}
	for _, b := range o.Topology.Branches {
		nbr, ok := newBranch[name(b.Over)]
		if !ok {
			continue
		}
		var oa, na []string
		for _, a := range b.Arms {
			oa = append(oa, name(a.Target))
		}
		for _, a := range nbr.Arms {
			na = append(na, a.Target)
		}
		if !sameOrder(oa, na) {
			add("arms of "+nbr.Over, "the order of the arms of "+nbr.Over+" changed")
		}
	}
	newJoin := map[string]plan.TopologyJoin{}
	for _, j := range n.Topology.Joins {
		newJoin[j.Name] = j
	}
	for _, j := range o.Topology.Joins {
		nj, ok := newJoin[name(j.Name)]
		if !ok {
			continue
		}
		var oi []string
		for _, in := range j.Inputs {
			oi = append(oi, name(in))
		}
		if !sameOrder(oi, nj.Inputs) {
			add("inputs of "+nj.Name, "the order of the inputs of join "+nj.Name+" changed")
		}
	}
	return cs
}

// sameOrder reports whether a and b list their common elements in the same order.
func sameOrder(a, b []string) bool {
	in := func(set []string) map[string]bool {
		m := map[string]bool{}
		for _, s := range set {
			m[s] = true
		}
		return m
	}
	inA, inB := in(a), in(b)
	keep := func(s []string, other map[string]bool) []string {
		return slices.DeleteFunc(slices.Clone(s), func(x string) bool { return !other[x] })
	}
	return slices.Equal(keep(a, inB), keep(b, inA))
}

// topologyVerdict is the headline of a comparison with a run. The config diff's words ("NO CHANGES",
// "the two configs declare the same flow") claim more than a run comparison can: a file with errors
// was not compared, a digest can differ with nothing itemised, and a structural comparison does not
// see kinds, types or gates. So those cases say what they know. Weakened gates and itemised changes
// keep the config diff's words and its stats, whose digest figure comes from the view's digests.
func topologyVerdict(v *DiffView) Verdict {
	d := diffVerdict(v)
	switch {
	case v.Cannot != "":
		d.Class, d.Word, d.Icon, d.Line = "warn", "CANNOT COMPARE", "unknown", "Not compared: "+v.Cannot+"."
	case v.DigestNote != "":
		d.Class, d.Word, d.Icon, d.Line = "warn", "DIGEST DIFFERS", "tilde", "The digest differs from the run's: "+v.DigestNote+"."
	case len(v.Changes) > 0:
		if !v.Typed {
			// Gates are blanked in this mode: "no approval gate was weakened" would claim what was not compared.
			d.Line = "Gates, kinds and types are not compared in this mode."
		}
	case v.Typed && v.OldDigest != "" && v.OldDigest == v.NewDigest:
		d.Class, d.Word, d.Icon = "good", "SAME DIGEST", "ok"
		d.Line = "The file has the digest the run journaled; this does not say the run followed the file."
	case v.Typed:
		d.Class, d.Word, d.Icon, d.Line = "warn", "NO DIGEST TO COMPARE", "unknown", "The run's topology has no digest that can be computed, so the digests are not compared."
	default:
		d.Class, d.Word, d.Icon = "warn", "NO DIFFERENCE SEEN", "unknown"
		d.Line = "Kinds, types and gates are not compared in this mode, so a change in them would not show."
	}
	return d
}
