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

// safetyRank orders a node's safety by how much a gate in front of it matters: read-only 0,
// idempotent 1, side effect and unknown 2 (a safety the check cannot read counts as an effect).
func safetyRank(r plan.ConfigReport, node string) int {
	switch r.Safety[node] {
	case plan.SafetyReadOnly:
		return 0
	case plan.SafetyIdempotent:
		return 1
	}
	return 2
}

// safetyName is a node's safety as the report words it.
func safetyName(r plan.ConfigReport, node string) string {
	if s, ok := r.Safety[node]; ok {
		return string(s)
	}
	return string(plan.SafetyUnknown)
}

// safetyLowered reports whether the node's safety moved toward read-only: side effect or unknown to
// idempotent or read-only, or idempotent to read-only. The label is the config's own claim, so a
// downgrade is what removes the effect from every effect-based check below.
func safetyLowered(o, n plan.ConfigReport, from, to string) bool {
	return safetyRank(n, to) < safetyRank(o, from)
}

// reachable are the nodes a path from r's entry reaches, gates ignored.
func reachable(r plan.ConfigReport) map[string]bool {
	adj := map[string][]string{}
	for _, e := range r.Topology.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	for _, b := range r.Topology.Branches {
		for _, a := range b.Arms {
			adj[b.Over] = append(adj[b.Over], a.Target)
		}
	}
	seen := map[string]bool{}
	stack := []string{r.Topology.Entry}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		stack = append(stack, adj[x]...)
	}
	return seen
}

// successorsOf are the nodes o has and n lacks, that no rename pairs, with the block and kind of
// node (a node n adds): the nodes it may be a moved copy of. The check cannot tell a new node from
// an old one rewired and renamed, so it judges the new one against each.
func successorsOf(o, n plan.ConfigReport, renRev map[string]string, node string) []string {
	var added plan.TopologyNode
	for _, x := range n.Topology.Nodes {
		if x.Name == node {
			added = x
		}
	}
	claimed := map[string]bool{}
	for _, from := range renRev {
		claimed[from] = true
	}
	inNew := map[string]bool{}
	for _, x := range n.Topology.Nodes {
		inNew[x.Name] = true
	}
	var out []string
	for _, x := range o.Topology.Nodes {
		if !inNew[x.Name] && !claimed[x.Name] && x.Block == added.Block && x.Kind == added.Kind {
			out = append(out, x.Name)
		}
	}
	return out
}

// ungatedNodes are the nodes of n that a path from the entry reaches without passing an approval
// gate (ConfigReport.Gated is false for them) and that count as a gate bypass, by their name in n:
//   - a node o reaches only through a gate (every path passes one) whatever its safety in either
//     config: a gate that guarded it is gone from some path (broad is true);
//   - a node o does not reach, or n adds, that may have an effect (side effect or unknown safety) in
//     n or in o: there is no earlier gate to lose, so only a node that can act counts;
//   - a node o reached without a gate that could not act (read-only or idempotent) and now may.
//
// A node o already reached without a gate, and as able to act, is not new. renRev maps a renamed node's new name to its
// old one. The old safety counts as well as the new: a relabel to read-only is the one edit that
// would otherwise hide the effect from this check.
func ungatedNodes(o, n plan.ConfigReport, renRev map[string]string) map[string]bool {
	oldHas := map[string]bool{}
	for _, x := range o.Topology.Nodes {
		oldHas[x.Name] = true
	}
	oldReach := reachable(o)
	out := map[string]bool{}
	for _, x := range n.Topology.Nodes {
		if n.Gated[x.Name] {
			continue
		}
		from := x.Name
		if f, ok := renRev[x.Name]; ok {
			from = f
		}
		switch {
		case !oldHas[from]:
			// A new node has no gate to lose, unless it may be a removed node moved: the same block
			// under another name (renamedNodes pairs only an exact match of the edges), relabeled.
			hit := mayHaveEffect(n, x.Name)
			for _, c := range successorsOf(o, n, renRev, x.Name) {
				hit = hit || oldReach[c] && o.Gated[c] || mayHaveEffect(o, c)
			}
			if hit {
				out[x.Name] = true
			}
		case oldReach[from] && o.Gated[from]:
			out[x.Name] = true
		case !oldReach[from] && (mayHaveEffect(n, x.Name) || mayHaveEffect(o, from)):
			out[x.Name] = true
		case mayHaveEffect(n, x.Name) && !mayHaveEffect(o, from):
			// Reached without a gate before, as something that could not act, and now it can.
			out[x.Name] = true
		}
	}
	return out
}

// effectsNow are the nodes of n that may have an effect, in n's safety or in the old one's, and that
// a path from the entry reaches without passing an approval gate (ConfigReport.Gated is false for
// them). A node the entry cannot reach is gated vacuously, so it is not here. The switch-arm rule
// asks which of these another arm reaches.
func effectsNow(o, n plan.ConfigReport, renRev map[string]string) map[string]bool {
	out := map[string]bool{}
	oldHas := map[string]bool{}
	for _, x := range o.Topology.Nodes {
		oldHas[x.Name] = true
	}
	for _, x := range n.Topology.Nodes {
		if n.Gated[x.Name] {
			continue
		}
		from := x.Name
		if f, ok := renRev[x.Name]; ok {
			from = f
		}
		if mayHaveEffect(n, x.Name) || oldHas[from] && mayHaveEffect(o, from) {
			out[x.Name] = true
		}
	}
	return out
}

// ungatedWhy is the report of a node in ungatedNodes(n): "reachable without an approval gate
// (validate -> receipt), not before; its safety is side effect".
func ungatedWhy(n plan.ConfigReport, node string) string {
	why := "; its safety is " + safetyName(n, node)
	if _, ok := n.Safety[node]; !ok || n.Safety[node] == plan.SafetyUnknown {
		why = "; its safety is not known"
	}
	return "reachable without an approval gate (" + strings.Join(n.UngatedPath(node), " -> ") + "), not before" + why
}

// renamedNodes pairs each node only the old topology has with a node only the new one has when they
// are the same node under another name: the same block and kind (a safety that differs is judged
// as the node's change, not a reason to treat the node as removed and a new one added), the same entry role, and the same
// edges and Switch arms once the renamed nodes' names are mapped. The gate may differ: the
// caller reports it as the renamed node's gate change. A rename that also changes any of the rest is
// not paired: it is judged on what changed. It returns old name -> new name.
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
			if ro.Block != an.Block || ro.Kind != an.Kind {
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
	oldArms := map[string]map[string]plan.TopologyBranchArm{}
	for _, b := range o.Topology.Branches {
		if oldArms[b.Over] == nil {
			oldArms[b.Over] = map[string]plan.TopologyBranchArm{}
		}
		for _, a := range b.Arms {
			oldArms[b.Over][key(a)] = a
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
			// A new Switch: no arm is unchanged, so no arm counts as "another arm" below, and its arms
			// are new nodes' edges, judged by the nodes. The skip is an optimisation.
			continue
		}
		newKeys := map[string]bool{}
		var changed []string
		for _, a := range b.Arms {
			newKeys[key(a)] = true
			if _, ok := old[key(a)]; !ok {
				changed = append(changed, armText(a))
			}
		}
		for k, a := range old {
			if !newKeys[k] {
				changed = append(changed, "removed "+armText(a))
			}
		}
		if len(changed) == 0 {
			continue
		}
		slices.Sort(changed)
		var other []string
		for _, a := range b.Arms {
			if _, ok := old[key(a)]; ok {
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
