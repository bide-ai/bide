package plan

import "reflect"

// typeStringOrEmpty renders a captured reflect.Type as its String() name for the
// data view, mapping a nil (unpinned) type to "" rather than the "?" placeholder
// typeName emits for a Mermaid label. Keeping reflect.Type projection in this one
// helper is what keeps reflect.Type out of the public Topology API.
func typeStringOrEmpty(t reflect.Type) string {
	if t == nil {
		return ""
	}
	return t.String()
}

// Topology is the stable, renderer-agnostic view of a frozen flow's DECLARED
// shape: its nodes, the edges between them, its Switch branches, its bounded
// loops, and its fan-in joins, all as pure data (plain strings, no reflect.Type,
// no closures). It is the single structured model that every renderer and every
// external tool builds on: Flow.RenderMermaid derives a Mermaid string from the
// same internal source Topology snapshots, and a visual builder or an audit tool
// consumes Topology directly rather than parsing a Mermaid string or reading the
// opaque Digest. Every field carries a json tag, so a Topology round-trips
// through encoding/json without loss (see Flow.Topology).
//
// Topology reports the same topology the Digest commits to (flow name, entry,
// nodes with their kinds and I/O types, edges, Switch arms, and loop structure),
// so a change that shifts the Digest is visible here too. It reports the DECLARED
// diagram, not a run: it says nothing about which arms a particular run took (see
// agent.RenderMermaid for the actual-ran shape).
type Topology struct {
	// Flow is the flow's name.
	Flow string `json:"flow"`
	// Entry is the name of the entry node (the step that consumes the flow input).
	Entry string `json:"entry"`
	// In and Out are the flow's pinned boundary type names, as reflect.Type.String
	// renders them (for example "int" or "main.Report"). Empty if a side is unpinned.
	In  string `json:"in"`
	Out string `json:"out"`
	// Nodes are the declared nodes in insertion order.
	Nodes []TopologyNode `json:"nodes"`
	// Edges are the declared producer -> consumer connections in insertion order,
	// including the one edge per join input.
	Edges []TopologyEdge `json:"edges"`
	// Branches are the declared Switch branches in insertion order.
	Branches []TopologyBranch `json:"branches,omitempty"`
	// Joins are the declared fan-in joins in insertion order. Each also appears in
	// Nodes with kind "join"; Joins additionally records the ORDERED input ports.
	Joins []TopologyJoin `json:"joins,omitempty"`
	// Loops are the bounded loops Build derived from the LoopBack arms, in insertion
	// order. Empty for an acyclic flow.
	Loops []TopologyLoop `json:"loops,omitempty"`
}

// TopologyNode is one declared node: its journal name, its kind as a string
// ("step", "tool", "model", "switch", or "join"), and its input/output type
// names as reflect.Type.String renders them. A join node has no single input
// type (it fans in several producers), so In is empty for a join; its per-port
// input types live in the matching TopologyJoin. An unpinned side is empty.
type TopologyNode struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	In   string `json:"in"`
	Out  string `json:"out"`
}

// TopologyEdge is one declared connection producer.Out -> consumer.In, by node
// name. An edge feeding a join carries the join input port's type name in Type;
// every other edge leaves Type empty.
type TopologyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type,omitempty"`
}

// TopologyBranch is one declared Switch: the switched-over node and its ordered
// arms. Else names the target of the Else arm if the Switch has one, else empty
// (the Else arm is also present in Arms with Else set true).
type TopologyBranch struct {
	Over string              `json:"over"`
	Arms []TopologyBranchArm `json:"arms"`
	Else string              `json:"else,omitempty"`
}

// TopologyBranchArm is one arm of a Switch: its target node and its role. Else is
// true for the fallback arm (its predicate is nil). LoopBack is true for a bounded
// back-edge to an earlier loop head; LoopMax is that arm's iteration bound (> 0
// for a loop-back arm, 0 otherwise). A forward When arm has Else and LoopBack both
// false and LoopMax 0. The arm predicate itself is a Go closure and is not part of
// this data view (it is not serializable and not part of the topology's identity).
type TopologyBranchArm struct {
	Target   string `json:"target"`
	Else     bool   `json:"else,omitempty"`
	LoopBack bool   `json:"loopBack,omitempty"`
	LoopMax  int    `json:"loopMax,omitempty"`
}

// TopologyJoin is one declared fan-in: the join node's name, its ORDERED input
// source node names (one per merge-function parameter, in declared order), the
// positionally-aligned input port type names, and the join's output type name. A
// join also appears in Topology.Nodes with kind "join"; this records the fan-in
// detail Nodes cannot (the ordered inputs and their per-port types).
type TopologyJoin struct {
	Name    string   `json:"name"`
	Inputs  []string `json:"inputs"`
	InTypes []string `json:"inTypes"`
	Out     string   `json:"out"`
}

// TopologyLoop is one bounded loop Build derived from a LoopBack arm: the loop
// head (the back-edge target, an earlier node re-entered each iteration), the loop
// switch (the node the Switch is over, which carries the back-edge arm), the
// ordered body region (head..switch inclusive, in forward topological order), and
// the iteration bound Max (> 0). It mirrors the resolved loopSpec, minus the
// forward-order indices, which are an internal execution detail.
type TopologyLoop struct {
	Head string   `json:"head"`
	Over string   `json:"over"`
	Body []string `json:"body"`
	Max  int      `json:"max"`
}

// Topology returns the stable, renderer-agnostic snapshot of this frozen flow's
// DECLARED topology as pure, JSON-serializable data. It is the structured model
// that Flow.RenderMermaid renders to Mermaid and that external tooling (a visual
// builder, an audit viewer) consumes directly, so callers are not coupled to the
// Mermaid string or to the opaque Digest. The returned value shares nothing with
// the frozen spec (it holds only copied strings and ints), so a caller may hold or
// mutate it freely, and it marshals through encoding/json without loss.
//
// It reports the same topology the Digest commits to: the flow name and boundary
// types, the entry node, every node's name/kind/I-O types, every edge, every
// Switch and its ordered arms, every fan-in join with its ordered inputs, and
// every derived loop. It is a pure function of the sealed spec: no context, no
// store, no I/O, and deterministic because it walks the insertion-ordered nodes,
// edges, branches, and loops slices rather than any map.
func (f *Flow[In, Out]) Topology() Topology {
	return f.core.topology()
}

// nodeKindName renders a node kind as the stable string the Topology exposes. It
// is the public spelling of the internal nodeKind enum, kept in one place so the
// wire form does not drift if the enum's numeric values change.
func nodeKindName(k nodeKind) string {
	switch k {
	case kindStep:
		return "step"
	case kindTool:
		return "tool"
	case kindModel:
		return "model"
	case kindSwitch:
		return "switch"
	case kindJoin:
		return "join"
	default:
		return "unknown"
	}
}

// topology builds the pure-data Topology from the frozen builderCore. It walks the
// same insertion-ordered slices the digest and renderMermaid walk, so the three
// views cannot disagree about the flow's shape. reflect.Type is projected to its
// String() name here (via typeName, which renders a nil type as "?" for a label
// but "" here, see below) so no reflect.Type escapes into the public API.
func (c *builderCore) topology() Topology {
	t := Topology{
		Flow:  c.flowName,
		Entry: c.entry,
		In:    typeStringOrEmpty(c.inType),
		Out:   typeStringOrEmpty(c.outType),
	}

	// Nodes in insertion order. A join has no single inType, so In stays empty for
	// it; its per-port input types are recorded on the matching TopologyJoin below.
	t.Nodes = make([]TopologyNode, 0, len(c.nodes))
	for _, n := range c.nodes {
		tn := TopologyNode{
			Name: n.name,
			Kind: nodeKindName(n.kind),
			Out:  typeStringOrEmpty(n.outType),
		}
		if n.kind != kindJoin {
			tn.In = typeStringOrEmpty(n.inType)
		}
		t.Nodes = append(t.Nodes, tn)

		// A join contributes its ordered inputs and their port types. The type of each
		// port is joinInTypes positionally aligned with joinInputs.
		if n.kind == kindJoin {
			tj := TopologyJoin{
				Name:    n.name,
				Inputs:  append([]string(nil), n.joinInputs...),
				InTypes: make([]string, len(n.joinInputs)),
				Out:     typeStringOrEmpty(n.outType),
			}
			for i := range n.joinInputs {
				if i < len(n.joinInTypes) {
					tj.InTypes[i] = typeStringOrEmpty(n.joinInTypes[i])
				}
			}
			t.Joins = append(t.Joins, tj)
		}
	}

	// Edges in insertion order. An edge feeding a join carries its port type label,
	// derived from the same joinEdgeLabel renderMermaid uses so the two agree.
	t.Edges = make([]TopologyEdge, 0, len(c.edges))
	for _, e := range c.edges {
		t.Edges = append(t.Edges, TopologyEdge{
			From: e.from,
			To:   e.to,
			Type: joinEdgeLabel(c, e),
		})
	}

	// Branches in insertion order, with their ordered arms. Record the Else target
	// separately as a convenience for tooling; it is also present in Arms.
	for _, br := range c.branches {
		tb := TopologyBranch{Over: br.over}
		for _, a := range br.arms {
			tb.Arms = append(tb.Arms, TopologyBranchArm{
				Target:   a.target,
				Else:     a.isElse,
				LoopBack: a.loopBack,
				LoopMax:  a.loopMax,
			})
			if a.isElse {
				tb.Else = a.target
			}
		}
		t.Branches = append(t.Branches, tb)
	}

	// Loops in insertion order, mirroring the derived loopSpec (minus the internal
	// forward-order indices).
	for _, lp := range c.loops {
		t.Loops = append(t.Loops, TopologyLoop{
			Head: lp.head,
			Over: lp.over,
			Body: append([]string(nil), lp.body...),
			Max:  lp.max,
		})
	}

	return t
}
