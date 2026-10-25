package flowedit

import "github.com/bide-ai/bide/plan"

// renameTopology replays renames, in order, on a topology, with the rules renameBaseline applies to a
// config and the editor's own rename_node operation: the node must be there once and not be a join,
// the new name must pass checkName and be neither a node's nor a join's. A rename that is refused is
// skipped whole. Every place that names the node is rewritten: the entry, the nodes, both ends of an
// edge, a Switch's node, its arms and `else`, a join's inputs, and a loop's head, switch and body.
// Assert lines are not in a topology. t is not changed; the result shares nothing with it. It returns
// the topology as renamed and the renames it made.
func renameTopology(t plan.Topology, renames []Rename) (plan.Topology, []Rename) {
	out := plan.InspectTopology(t).Topology // a deep copy
	if len(renames) == 0 || len(renames) > maxRenames {
		return out, nil
	}
	names := map[string]int{}
	for _, n := range out.Nodes {
		names[n.Name]++
	}
	joins := map[string]bool{}
	for _, n := range out.Nodes {
		if n.Kind == plan.NodeKindJoin {
			joins[n.Name] = true
		}
	}
	for _, j := range out.Joins {
		joins[j.Name] = true
	}
	var made []Rename
	for _, r := range renames {
		if names[r.From] != 1 || joins[r.From] || checkName(r.To) != nil || names[r.To] > 0 || joins[r.To] {
			continue
		}
		sub := func(s *string) {
			if *s == r.From {
				*s = r.To
			}
		}
		sub(&out.Entry)
		for i := range out.Nodes {
			sub(&out.Nodes[i].Name)
		}
		for i := range out.Edges {
			sub(&out.Edges[i].From)
			sub(&out.Edges[i].To)
		}
		for i := range out.Branches {
			b := &out.Branches[i]
			sub(&b.Over)
			sub(&b.Else)
			for j := range b.Arms {
				sub(&b.Arms[j].Target)
			}
		}
		for i := range out.Joins {
			for j := range out.Joins[i].Inputs {
				sub(&out.Joins[i].Inputs[j])
			}
		}
		for i := range out.Loops {
			l := &out.Loops[i]
			sub(&l.Head)
			sub(&l.Over)
			for j := range l.Body {
				sub(&l.Body[j])
			}
		}
		names[r.From]--
		names[r.To]++
		made = append(made, r)
	}
	return out, made
}
