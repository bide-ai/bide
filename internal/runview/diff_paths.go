package runview

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bide-ai/bide/plan"
)

// mayHaveEffect reports whether a node may have a side effect: its safety is side effect, or is not
// known (a config checked without its plan.Registry states it only where the config does).
func mayHaveEffect(r plan.ConfigReport, node string) bool {
	s, ok := r.Safety[node]
	return !ok || s == plan.SafetySideEffect || s == plan.SafetyUnknown
}

// ungatedEffects are the nodes of r that may have an effect and that a path from the entry reaches
// without passing an approval gate (ConfigReport.Gated is false for them). A node the entry cannot
// reach is gated vacuously, so it is not here.
func ungatedEffects(r plan.ConfigReport) map[string]bool {
	out := map[string]bool{}
	for _, n := range r.Topology.Nodes {
		if !r.Gated[n.Name] && mayHaveEffect(r, n.Name) {
			out[n.Name] = true
		}
	}
	return out
}

// ungatedWhy is the report of a node in ungatedEffects(n): "reachable without an approval gate
// (validate -> receipt), not before", with "; its safety is not known" when that is why it counts.
func ungatedWhy(n plan.ConfigReport, node string) string {
	why := ""
	if s, ok := n.Safety[node]; !ok || s == plan.SafetyUnknown {
		why = "; its safety is not known"
	}
	return "reachable without an approval gate (" + strings.Join(n.UngatedPath(node), " -> ") + "), not before" + why
}

// renamedNodes pairs each node only the old topology has with a node only the new one has when they
// are the same node under another name: the same block, kind, safety and gate, the same entry role,
// and the same edges and Switch arms once the renamed nodes' names are mapped. A rename that also
// changes any of those is not paired: it is judged on what changed. It returns old name -> new name.
func renamedNodes(o, n plan.ConfigReport) map[string]string {
	ot, nt := o.Topology, n.Topology
	oldNode := map[string]plan.TopologyNode{}
	for _, x := range ot.Nodes {
		oldNode[x.Name] = x
	}
	newNode := map[string]plan.TopologyNode{}
	for _, x := range nt.Nodes {
		newNode[x.Name] = x
	}
	var removed, added []string
	for _, x := range ot.Nodes {
		if _, ok := newNode[x.Name]; !ok {
			removed = append(removed, x.Name)
		}
	}
	for _, x := range nt.Nodes {
		if _, ok := oldNode[x.Name]; !ok {
			added = append(added, x.Name)
		}
	}
	// Pair the nodes that look alike with every other unpaired removed or added node's name blurred
	// (so neighbours renamed together can pair up), then keep only the pairs whose edges match
	// exactly under the names as paired.
	removedOnly, addedOnly := map[string]bool{}, map[string]bool{}
	for _, r := range removed {
		removedOnly[r] = true
	}
	for _, a := range added {
		addedOnly[a] = true
	}
	blur := func(set map[string]bool) map[string]string {
		m := map[string]string{}
		for k := range set {
			m[k] = "~"
		}
		return m
	}
	m := map[string]string{}
	taken := map[string]bool{}
	for _, r := range removed {
		for _, a := range added {
			if taken[a] {
				continue
			}
			ro, an := oldNode[r], newNode[a]
			if ro.Block != an.Block || ro.Kind != an.Kind || o.Safety[r] != n.Safety[a] || !gateEqual(ro.Gate, an.Gate) {
				continue
			}
			if slices.Equal(incident(ot, r, blur(removedOnly)), incident(nt, a, blur(addedOnly))) {
				m[r], taken[a] = a, true
				break
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for r, a := range m {
			if !slices.Equal(incident(ot, r, m), incident(nt, a, nil)) {
				delete(m, r)
				changed = true
			}
		}
	}
	return m
}

// incident is the sorted description of every edge, Switch arm and entry role that touches node in
// t, with the names in rename (old -> new) replaced.
func incident(t plan.Topology, node string, rename map[string]string) []string {
	nm := func(s string) string {
		if r, ok := rename[s]; ok {
			return r
		}
		return s
	}
	var out []string
	for _, e := range t.Edges {
		if e.From == node || e.To == node {
			out = append(out, fmt.Sprintf("edge %s>%s:%s", nm(e.From), nm(e.To), e.Type))
		}
	}
	for _, b := range t.Branches {
		for _, a := range b.Arms {
			if b.Over == node || a.Target == node {
				out = append(out, fmt.Sprintf("arm %s>%s:%s:%t:%t:%d", nm(b.Over), nm(a.Target), a.Pred, a.Else, a.LoopBack, a.LoopMax))
			}
		}
	}
	if t.Entry == node {
		out = append(out, "entry")
	}
	slices.Sort(out)
	return out
}

// switchChanges flags a Switch whose arm conditions changed (an arm added, removed, or with another
// predicate) while some other arm of it leads to an effect reachable without an approval gate. The
// check cannot read the predicates, so it cannot tell which inputs now take which arm: a changed
// condition may send what went to a gated arm to the ungated one. It is conservative, and asks for a
// review.
func switchChanges(o, n plan.ConfigReport, ungated map[string]bool) []Change {
	key := func(a plan.TopologyBranchArm) string {
		return fmt.Sprintf("%s>%s:%t:%t:%d", a.Pred, a.Target, a.Else, a.LoopBack, a.LoopMax)
	}
	oldArms := map[string]map[string]bool{}
	for _, b := range o.Topology.Branches {
		if oldArms[b.Over] == nil {
			oldArms[b.Over] = map[string]bool{}
		}
		for _, a := range b.Arms {
			oldArms[b.Over][key(a)] = true
		}
	}
	gate := map[string]bool{}
	adj := map[string][]string{}
	for _, x := range n.Topology.Nodes {
		gate[x.Name] = x.Gate != nil
	}
	for _, e := range n.Topology.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	for _, b := range n.Topology.Branches {
		for _, a := range b.Arms {
			adj[b.Over] = append(adj[b.Over], a.Target)
		}
	}
	// reaches names the ungated effects a path from start reaches without passing a gate.
	reaches := func(start string) []string {
		var out []string
		seen := map[string]bool{}
		var walk func(string)
		walk = func(x string) {
			if seen[x] || gate[x] {
				return
			}
			seen[x] = true
			if ungated[x] {
				out = append(out, x)
			}
			for _, y := range adj[x] {
				walk(y)
			}
		}
		walk(start)
		return out
	}
	var cs []Change
	for _, b := range n.Topology.Branches {
		old, had := oldArms[b.Over]
		if !had {
			continue // a new Switch: its arms are new nodes' edges, judged by the nodes
		}
		newKeys := map[string]bool{}
		var changed []string
		for _, a := range b.Arms {
			newKeys[key(a)] = true
			if !old[key(a)] {
				changed = append(changed, armText(a))
			}
		}
		for k := range old {
			if !newKeys[k] {
				changed = append(changed, "removed "+k)
			}
		}
		if len(changed) == 0 {
			continue
		}
		slices.Sort(changed)
		var other []string
		for _, a := range b.Arms {
			if old[key(a)] {
				other = append(other, reaches(a.Target)...)
			}
		}
		if len(other) == 0 {
			continue
		}
		slices.Sort(other)
		other = slices.Compact(other)
		cs = append(cs, Change{Change: "changed", What: "switch", Node: b.Over, Gate: "weakened", Weakening: "condition", Details: []string{
			"arm conditions changed (" + strings.Join(changed, "; ") + ") while another arm reaches an ungated effect (" + strings.Join(other, ", ") + "): the predicates are not readable, so review which inputs take which arm (weakened)"}})
	}
	return cs
}

func armText(a plan.TopologyBranchArm) string {
	switch {
	case a.Else:
		return "else -> " + a.Target
	case a.LoopBack:
		return "loop " + a.Pred + " -> " + a.Target
	}
	return "when " + a.Pred + " -> " + a.Target
}
