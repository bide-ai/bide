package plan

import (
	"fmt"
	"reflect"
	"strings"
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
		label := n.name + " : " + typeName(n.inType) + " -> " + typeName(n.outType)
		if n.kind == kindSwitch {
			b.WriteString(fmt.Sprintf("  %s{%q}\n", id, label))
		} else {
			b.WriteString(fmt.Sprintf("  %s[%q]\n", id, label))
		}
	}

	// Connect the flow entry from the start node so the authored graph has a root.
	if entryID, ok := ids[c.entry]; ok {
		b.WriteString(fmt.Sprintf("  start --> %s\n", entryID))
	}

	// Straight edges (producer.Out -> consumer.In), in insertion order.
	for _, e := range c.edges {
		from, okFrom := ids[e.from]
		to, okTo := ids[e.to]
		if okFrom && okTo {
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

// typeName renders a captured reflect.Type for a node label, tolerating a nil
// type (an unpinned side) by rendering "?". It uses reflect.Type.String so the
// label matches the Go type name a reader sees in source.
func typeName(t reflect.Type) string {
	if t == nil {
		return "?"
	}
	return t.String()
}
