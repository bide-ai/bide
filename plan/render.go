package plan

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/bide-ai/bide/internal/mermaid"
)

// renderMermaid returns a Mermaid flowchart of the DECLARED topology carried by
// the reified builderCore: each node labelled by its journal key and its I,O
// types. This is the authored diagram; agent.RenderMermaid derives the
// actual-ran diagram from the journal, and conformance compares them.
//
// It is a pure function of the spec: no context, no store, no I/O. Output is
// deterministic because it iterates the insertion-ordered nodes, edges, and
// branches slices rather than any map.
//
// The exported entry point is Flow[In,Out].RenderMermaid, which Agent D wires in
// plan/flow.go as a one-line forwarder to c.renderMermaid(). It lives here as a
// method on *builderCore so it can be built and tested in isolation without the
// Flow type, and so it reaches the frozen spec directly.
func (c *builderCore) renderMermaid() string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	b.WriteString("  start([user])\n")

	// Map each node name to a stable Mermaid id, assigned in insertion order.
	ids := make(map[string]string, len(c.nodes))
	for i, n := range c.nodes {
		ids[n.name] = fmt.Sprintf("n%d", i)
	}

	// One Mermaid node per declared node, labelled "name : InType -> OutType".
	// A kindSwitch renders as a diamond (branch); every other kind as a box.
	for _, n := range c.nodes {
		id := ids[n.name]
		label := n.name + " : " + inLabel(n) + " -> " + typeName(n.outType)
		if n.kind == kindSwitch {
			b.WriteString(fmt.Sprintf("  %s{%s}\n", id, mermaid.Label(label)))
		} else {
			b.WriteString(fmt.Sprintf("  %s[%s]\n", id, mermaid.Label(label)))
		}
	}

	// Connect the flow entry from the start node so the authored graph has a root.
	if entryID, ok := ids[c.entry]; ok {
		b.WriteString(fmt.Sprintf("  start --> %s\n", entryID))
	}

	// Straight edges (producer.Out -> consumer.In), in insertion order. An edge into
	// a Join is labelled with the input port's type (from the join's joinInTypes,
	// positionally aligned with joinInputs), so a fan-in shows each contributed type
	// rather than a bare arrow.
	for _, e := range c.edges {
		from, okFrom := ids[e.from]
		to, okTo := ids[e.to]
		if !okFrom || !okTo {
			continue
		}
		if lbl := joinEdgeLabel(c, e); lbl != "" {
			b.WriteString(fmt.Sprintf("  %s -->|%s| %s\n", from, lbl, to))
		} else {
			b.WriteString(fmt.Sprintf("  %s --> %s\n", from, to))
		}
	}

	// Branch edges: from the switched node to each arm target, labelled with the
	// arm (a When predicate reads "When", an Else arm reads "Else"). Because
	// handles express branches, the output is a real DAG shape, not a chain.
	for _, br := range c.branches {
		from, okFrom := ids[br.over]
		if !okFrom {
			continue
		}
		for _, a := range br.arms {
			to, okTo := ids[a.target]
			if !okTo {
				continue
			}
			armLabel := "When"
			switch {
			case a.loopBack:
				// A bounded back-edge: label it as a loop with its iteration bound so the
				// authored diagram shows the cycle and its max, distinct from a forward arm.
				armLabel = fmt.Sprintf("loop &le;%d", a.loopMax)
			case a.isElse:
				armLabel = "Else"
			}
			b.WriteString(fmt.Sprintf("  %s -->|%s| %s\n", from, armLabel, to))
		}
	}

	return b.String()
}

// joinEdgeLabel returns the type label for an edge feeding a Join, or "" if the
// edge's target is not a Join (or its type cannot be resolved). The join records
// its inputs in declared order (joinInputs) with positionally-aligned types
// (joinInTypes); the label is the type of the port whose source is e.from. A
// source wired to the same join twice takes its first port, which is enough for
// the diagram.
func joinEdgeLabel(c *builderCore, e edge) string {
	to := c.byName[e.to]
	if to == nil || to.kind != kindJoin {
		return ""
	}
	for i, src := range to.joinInputs {
		if src == e.from && i < len(to.joinInTypes) {
			return typeName(to.joinInTypes[i])
		}
	}
	return ""
}

// inLabel renders a node's input side for its Mermaid label. A kindJoin node has
// no single inType (it fans in several producers); its real input types live in
// joinInTypes, so render them as a parenthesized tuple ("(int, string)") rather
// than the "?" typeName would emit for the nil inType. Every other kind pins a
// single inType and renders through typeName.
func inLabel(n *node) string {
	if n.kind == kindJoin && len(n.joinInTypes) > 0 {
		parts := make([]string, len(n.joinInTypes))
		for i, t := range n.joinInTypes {
			parts[i] = typeName(t)
		}
		return "(" + strings.Join(parts, ", ") + ")"
	}
	return typeName(n.inType)
}

// typeName renders a captured reflect.Type for a node label, tolerating a nil
// type (an unpinned side) by rendering "?". It uses reflect.Type.String so the
// label matches the Go type name a reader sees in source.
func typeName(t reflect.Type) string {
	if t == nil {
		return "?"
	}
	return t.String()
}
