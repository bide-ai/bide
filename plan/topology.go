package plan

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

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
// opaque Digest. Every field carries a snake_case json tag, so a Topology round-trips
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
	// InCanonical and OutCanonical are the same types written with every named type qualified
	// by its full package path, as the Digest commits to them ("?" for an unpinned side).
	InCanonical  string `json:"in_canonical"`
	OutCanonical string `json:"out_canonical"`
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

// TopologyNodeKind is the kind of a declared node, as Topology reports it. The set is closed:
// NodeKindStep, NodeKindTool, NodeKindModel, NodeKindSwitch, and NodeKindJoin.
type TopologyNodeKind string

// The node kinds a Topology reports.
const (
	// NodeKindStep is a Go step (Builder.Step, RegisterStep).
	NodeKindStep TopologyNodeKind = "step"
	// NodeKindTool is a node that calls an agent tool (Builder.Tool, RegisterTool).
	NodeKindTool TopologyNodeKind = "tool"
	// NodeKindModel is a node that calls the flow's model (Builder.Model, RegisterModel).
	NodeKindModel TopologyNodeKind = "model"
	// NodeKindSwitch is a journaled branch-choice node.
	NodeKindSwitch TopologyNodeKind = "switch"
	// NodeKindJoin is a fan-in node that merges several producers' outputs.
	NodeKindJoin TopologyNodeKind = "join"
)

// TopologyNode is one declared node: its journal name, its kind, and its input/output type
// names as reflect.Type.String renders them. A join node has no single input
// type (it fans in several producers), so In is empty for a join; its per-port
// input types live in the matching TopologyJoin. An unpinned side is empty.
//
// Block, InCanonical and OutCanonical are what the Digest commits to beyond the names: the
// registered block the node runs, and its input and output types with full package paths ("?"
// for an unpinned side; a join's InCanonical is "?", as it has no single input).
type TopologyNode struct {
	Name         string           `json:"name"`
	Kind         TopologyNodeKind `json:"kind"`
	In           string           `json:"in"`
	Out          string           `json:"out"`
	Block        string           `json:"block"`
	InCanonical  string           `json:"in_canonical"`
	OutCanonical string           `json:"out_canonical"`
	// Gate is the node's approval gate, nil for an ungated node. The Digest commits to it, so a
	// paused run cannot be resumed under a flow whose gate was removed or changed.
	Gate *TopologyGate `json:"gate,omitempty"`
}

// TopologyGate is a node's approval gate: Single for agent.SingleApproval (one decision, recorded
// with agent.Approve), otherwise Need signed decisions of the Approvers, in policy order (an
// m-of-n gate), never counting the run's requester when ExcludePrincipal is set
// (agent.ApprovalPolicy.ExcludePrincipal).
type TopologyGate struct {
	Single           bool     `json:"single,omitempty"`
	Need             int      `json:"need,omitempty"`
	Approvers        []string `json:"approvers,omitempty"`
	ExcludePrincipal bool     `json:"exclude_principal,omitempty"`
	// TimeoutMS is the gate's deadline in milliseconds (ApprovalPolicy.Timeout), zero for none.
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// TimeoutText is the gate's deadline as a short duration ("24h", "1h30m", "90s"), or "" for a gate
// with none (or a nil gate). It is the text the viewer, the diff and RenderMermaid show.
func (g *TopologyGate) TimeoutText() string {
	if g == nil || g.TimeoutMS <= 0 {
		return ""
	}
	s := (time.Duration(g.TimeoutMS) * time.Millisecond).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// topologyGate is p as a TopologyGate, nil for no gate.
func topologyGate(p *agent.ApprovalPolicy) *TopologyGate {
	if p == nil {
		return nil
	}
	var g TopologyGate
	b, err := json.Marshal(p)
	if err != nil || json.Unmarshal(b, &g) != nil {
		return &TopologyGate{Need: p.Need, Approvers: append([]string(nil), p.Approvers...), ExcludePrincipal: p.ExcludePrincipal, TimeoutMS: p.Timeout.Milliseconds()}
	}
	return &g
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
//
// Pred is the name of the arm's predicate, which the Digest commits to (Arm.Named, or the
// registered predicate a config arm references); empty for an Else arm and an unnamed predicate.
type TopologyBranchArm struct {
	Target   string `json:"target"`
	Pred     string `json:"pred,omitempty"`
	Else     bool   `json:"else,omitempty"`
	LoopBack bool   `json:"loop_back,omitempty"`
	LoopMax  int    `json:"loop_max,omitempty"`
}

// TopologyJoin is one declared fan-in: the join node's name, its ORDERED input
// source node names (one per merge-function parameter, in declared order), the
// positionally-aligned input port type names, and the join's output type name. A
// join also appears in Topology.Nodes with kind "join"; this records the fan-in
// detail Nodes cannot (the ordered inputs and their per-port types).
//
// InCanonical is InTypes with full package paths, as the Digest commits to them.
type TopologyJoin struct {
	Name        string   `json:"name"`
	Inputs      []string `json:"inputs"`
	InTypes     []string `json:"in_types"`
	InCanonical []string `json:"in_canonical"`
	Out         string   `json:"out"`
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

// nodeKindName renders a node kind as the TopologyNodeKind the Topology exposes. It
// is the public spelling of the internal nodeKind enum, kept in one place so the
// wire form does not drift if the enum's numeric values change. Every nodeKind has a
// name; the empty kind is returned only for a value outside the enum, which Build never
// produces.
func nodeKindName(k nodeKind) TopologyNodeKind {
	switch k {
	case kindStep:
		return NodeKindStep
	case kindTool:
		return NodeKindTool
	case kindModel:
		return NodeKindModel
	case kindSwitch:
		return NodeKindSwitch
	case kindJoin:
		return NodeKindJoin
	default:
		return ""
	}
}

// topology builds the pure-data Topology from the frozen builderCore. It walks the
// same insertion-ordered slices the digest and renderMermaid walk, so the three
// views cannot disagree about the flow's shape. reflect.Type is projected to its
// String() name here (via typeName, which renders a nil type as "?" for a label
// but "" here, see below) so no reflect.Type escapes into the public API.
func (c *builderCore) topology() Topology {
	t := Topology{
		Flow:         c.flowName,
		Entry:        c.entry,
		In:           typeStringOrEmpty(c.inType),
		Out:          typeStringOrEmpty(c.outType),
		InCanonical:  canonicalType(c.inType),
		OutCanonical: canonicalType(c.outType),
	}

	// Nodes in insertion order. A join has no single inType, so In stays empty for
	// it; its per-port input types are recorded on the matching TopologyJoin below.
	t.Nodes = make([]TopologyNode, 0, len(c.nodes))
	for _, n := range c.nodes {
		tn := TopologyNode{
			Name:         n.name,
			Kind:         nodeKindName(n.kind),
			Out:          typeStringOrEmpty(n.outType),
			Block:        n.block,
			InCanonical:  canonicalType(n.inType),
			OutCanonical: canonicalType(n.outType),
		}
		if n.kind != kindJoin {
			tn.In = typeStringOrEmpty(n.inType)
		}
		tn.Gate = topologyGate(n.approval)
		t.Nodes = append(t.Nodes, tn)

		// A join contributes its ordered inputs and their port types. The type of each
		// port is joinInTypes positionally aligned with joinInputs.
		if n.kind == kindJoin {
			tj := TopologyJoin{
				Name:        n.name,
				Inputs:      append([]string(nil), n.joinInputs...),
				InTypes:     make([]string, len(n.joinInputs)),
				InCanonical: make([]string, len(n.joinInputs)),
				Out:         typeStringOrEmpty(n.outType),
			}
			for i := range n.joinInputs {
				var portType reflect.Type
				if i < len(n.joinInTypes) {
					portType = n.joinInTypes[i]
				}
				tj.InTypes[i] = typeStringOrEmpty(portType)
				tj.InCanonical[i] = canonicalType(portType)
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
				Pred:     a.predName,
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
