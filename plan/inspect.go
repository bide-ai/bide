package plan

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// ConfigReport is what InspectConfig finds in a flow config: the graph it declares, the digest a
// run of it would journal, its approval gates, and its problems, each attached to the node, edge or
// arm it belongs to.
type ConfigReport struct {
	// Topology is the declared graph. With a Registry and no errors it is the Topology Load would
	// build (node kinds, types, derived loops); otherwise it is read from the config alone, with no
	// types and no node kinds but a join's.
	Topology Topology
	// Digest is the digest a run of the flow would journal (TopologyDigest of Topology), set only
	// with a Registry and no errors: it commits to types, which only the Registry knows.
	Digest string
	// Approvals are the approval gates the config declares, by node.
	Approvals map[string]agent.ApprovalPolicy
	// Problems are every error and warning found, in config order.
	Problems []ConfigProblem
	// Typed reports a Registry was given, so blocks, predicates, merges and types were checked as
	// Load checks them. Without one, the check is structural.
	Typed bool
	// Safety is each node's crash behavior: exact with a Registry (the Safety the block registers,
	// lowered by the config's safety), and without one known only where the config states it.
	Safety map[string]NodeSafety
	// Assertions are the verdicts on the config's assert lines, in order.
	Assertions []AssertionResult
}

// ConfigProblem is one problem in a flow config. Node names the node, switched node or join it
// belongs to; From and To name the edge or arm it belongs to (Node is then empty); all three are
// empty for a problem of the config as a whole.
type ConfigProblem struct {
	Node     string `json:"node,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Message  string `json:"message"`
	Severity string `json:"severity"` // "error" (Load refuses the config) or "warning"
}

// Errors reports whether any problem is an error.
func (r ConfigReport) Errors() bool {
	for _, p := range r.Problems {
		if p.Severity == "error" {
			return true
		}
	}
	return false
}

// InspectConfig checks a flow config without running or building a Flow, for a viewer or a CI
// check. It never panics on bad input: every failure is a ConfigProblem.
//
// Without a Registry (reg nil) it runs the structural checks, which need no Go code: the config
// parses as Load parses it (strictly, version 1); node names are unique; each wiring element is
// exactly one of an edge, a switch or a join; every edge, arm, else, join input and the entry name a
// declared node or join; every safety string is known; and every approval block is a valid policy.
// Blocks, predicates, merges and types are not checked.
//
// With a Registry it also runs Load's own checks that do not need the flow's In and Out (the
// checks Validate runs) and Build's whole-graph checks, with In taken as the entry node's input
// type and Out as the terminal nodes' output type, which are the only In and Out Load accepts.
// The digest it reports is then the digest Load's flow would journal.
//
// Each problem is attached to the node, edge or arm its message names. A valid approval block is
// an error, since Load refuses it (plan flows do not enforce approval gates yet), and is still
// listed in Approvals. One warning is not a load error: a Switch with no else, since a value no arm
// matches fails the run.
func InspectConfig(data []byte, reg *Registry) (r ConfigReport) {
	r = ConfigReport{Typed: reg != nil, Approvals: map[string]agent.ApprovalPolicy{}}
	var cfg config
	if err := parseConfig(data, &cfg); err != nil {
		r.Problems = append(r.Problems, ConfigProblem{Message: strings.TrimSuffix(err.Error(), ": "+agent.ErrConfig.Error()), Severity: "error"})
		return r
	}
	r.Topology = configTopology(&cfg)
	r.Safety = configSafety(&cfg)
	structural(&cfg, &r)
	defer func() { r.assertions(cfg.Assert) }()
	if reg == nil || r.Errors() {
		return r
	}
	core, err := assemble(&cfg, reg)
	if err != nil {
		r.attribute(&cfg, err)
		return r
	}
	for _, n := range core.nodes {
		r.Safety[n.name] = safetyClass(n.safety.ReadOnly, n.safety.Idempotent)
	}
	// Load's boundary types: the entry's input, and the output every terminal must share.
	core.inType = core.byName[core.entry].inType
	hasOut := map[string]bool{}
	for _, e := range core.edges {
		hasOut[e.from] = true
	}
	for _, br := range core.branches {
		hasOut[br.over] = true
	}
	for _, n := range core.nodes {
		if !hasOut[n.name] && core.outType == nil {
			core.outType = n.outType
		}
	}
	f, err := (&Builder[struct{}, struct{}]{core: core}).Build()
	if err != nil {
		r.attribute(&cfg, err)
		return r
	}
	r.Topology = f.Topology()
	r.Digest = f.Digest()
	return r
}

// configSafety is each node's safety as the config alone states it. A config "safety" sets the
// node's level (it may keep or lower what Go registers, and a raise is a load error), so a stated
// safety is exact; a node that states none is unknown without the Registry.
func configSafety(cfg *config) map[string]NodeSafety {
	s := map[string]NodeSafety{}
	for _, n := range cfg.Nodes {
		switch n.Safety {
		case "readonly":
			s[n.Name] = SafetyReadOnly
		case "idempotent":
			s[n.Name] = SafetyIdempotent
		case "side_effect":
			s[n.Name] = SafetySideEffect
		default:
			s[n.Name] = SafetyUnknown
		}
	}
	for _, w := range cfg.Wiring {
		if w.Join != "" {
			switch w.Safety {
			case "readonly":
				s[w.Join] = SafetyReadOnly
			case "idempotent":
				s[w.Join] = SafetyIdempotent
			case "side_effect":
				s[w.Join] = SafetySideEffect
			default:
				s[w.Join] = SafetyUnknown
			}
		}
	}
	return s
}

func safetyClass(readOnly, idempotent bool) NodeSafety {
	switch {
	case readOnly:
		return SafetyReadOnly
	case idempotent:
		return SafetyIdempotent
	}
	return SafetySideEffect
}

// configTopology reads the declared graph from the config alone: nodes (a join's kind is join;
// the rest have no kind without the Registry), edges (a join's inputs included), and switches.
func configTopology(cfg *config) Topology {
	t := Topology{Flow: cfg.Flow, Entry: cfg.Entry, In: cfg.In, Out: cfg.Out}
	if t.Entry == "" && len(cfg.Nodes) > 0 {
		t.Entry = cfg.Nodes[0].Name
	}
	for _, n := range cfg.Nodes {
		t.Nodes = append(t.Nodes, TopologyNode{Name: n.Name, Block: n.Block})
	}
	for _, w := range cfg.Wiring {
		switch {
		case w.isEdge() && len(w.Edge) == 2:
			t.Edges = append(t.Edges, TopologyEdge{From: w.Edge[0], To: w.Edge[1]})
		case w.isJoin() && !w.isEdge() && !w.isSwitch():
			t.Nodes = append(t.Nodes, TopologyNode{Name: w.Join, Kind: NodeKindJoin, Block: w.Merge})
			for _, in := range w.Inputs {
				t.Edges = append(t.Edges, TopologyEdge{From: in, To: w.Join})
			}
			t.Joins = append(t.Joins, TopologyJoin{Name: w.Join, Inputs: w.Inputs})
		case w.isSwitch() && !w.isEdge() && !w.isJoin():
			b := TopologyBranch{Over: w.Switch, Else: w.Else}
			for _, a := range w.When {
				b.Arms = append(b.Arms, TopologyBranchArm{Target: a.To, Pred: a.Pred, LoopBack: a.LoopMax > 0, LoopMax: a.LoopMax})
			}
			if w.Else != "" {
				b.Arms = append(b.Arms, TopologyBranchArm{Target: w.Else, Else: true})
			}
			t.Branches = append(t.Branches, b)
		}
	}
	return t
}

// structural runs the checks that need no Registry, attaching each problem where it belongs.
func structural(cfg *config, r *ConfigReport) {
	errAt := func(p ConfigProblem) { p.Severity = "error"; r.Problems = append(r.Problems, p) }
	declared := map[string]bool{}
	if len(cfg.Nodes) == 0 {
		errAt(ConfigProblem{Message: "flow has no nodes"})
	}
	for _, n := range cfg.Nodes {
		switch {
		case n.Name == "":
			errAt(ConfigProblem{Message: fmt.Sprintf("a node (block %q) has no name", n.Block)})
		case declared[n.Name]:
			errAt(ConfigProblem{Node: n.Name, Message: fmt.Sprintf("node %q is declared more than once", n.Name)})
		}
		declared[n.Name] = true
		if n.Block == "" {
			errAt(ConfigProblem{Node: n.Name, Message: fmt.Sprintf("node %q names no block", n.Name)})
		}
		switch n.Safety {
		case "", "readonly", "idempotent", "side_effect":
		default:
			errAt(ConfigProblem{Node: n.Name, Message: fmt.Sprintf("node %q has unknown safety %q (want readonly, idempotent or side_effect)", n.Name, n.Safety)})
		}
		if n.Approval != nil {
			p := agent.ApprovalPolicy{Need: n.Approval.Need, Approvers: n.Approval.Approvers}
			r.Approvals[n.Name] = p
			if err := p.Validate(); err != nil {
				errAt(ConfigProblem{Node: n.Name, Message: fmt.Sprintf("node %q approval: %s", n.Name, strings.TrimSuffix(err.Error(), ": "+agent.ErrConfig.Error()))})
			} else {
				errAt(ConfigProblem{Node: n.Name,
					Message: fmt.Sprintf("node %q declares an approval gate, which plan flows do not enforce yet, so Load refuses it; gate the agent tool the node calls instead (docs/guides/hitl-approval.md)", n.Name)})
			}
		}
	}
	for _, w := range cfg.Wiring {
		if w.isJoin() && w.Join != "" && !w.isEdge() && !w.isSwitch() {
			if declared[w.Join] {
				errAt(ConfigProblem{Node: w.Join, Message: fmt.Sprintf("join %q is already a declared node; a join creates its own node", w.Join)})
			}
			declared[w.Join] = true
		}
	}
	if cfg.Entry != "" && !declared[cfg.Entry] {
		errAt(ConfigProblem{Message: fmt.Sprintf("entry %q names no declared node", cfg.Entry)})
	}
	for i, w := range cfg.Wiring {
		n := 0
		for _, set := range []bool{w.isEdge(), w.isSwitch(), w.isJoin()} {
			if set {
				n++
			}
		}
		if n != 1 {
			errAt(ConfigProblem{Message: fmt.Sprintf("wiring[%d] sets %d of edge/switch/join; set exactly one", i, n)})
			continue
		}
		switch {
		case w.isEdge():
			if len(w.Edge) != 2 {
				errAt(ConfigProblem{Message: fmt.Sprintf("wiring[%d] edge must be [from, to], got %d element(s)", i, len(w.Edge))})
				continue
			}
			for _, end := range w.Edge {
				if !declared[end] {
					errAt(ConfigProblem{From: w.Edge[0], To: w.Edge[1], Message: fmt.Sprintf("wiring[%d] edge %q -> %q: %q is not a declared node", i, w.Edge[0], w.Edge[1], end)})
				}
			}
		case w.isSwitch():
			if !declared[w.Switch] {
				errAt(ConfigProblem{Node: w.Switch, Message: fmt.Sprintf("wiring[%d] switch over unknown node %q", i, w.Switch)})
			}
			for j, a := range w.When {
				if a.Pred == "" {
					errAt(ConfigProblem{From: w.Switch, To: a.To, Message: fmt.Sprintf("wiring[%d].when[%d] names no predicate", i, j)})
				}
				if !declared[a.To] {
					errAt(ConfigProblem{From: w.Switch, To: a.To, Message: fmt.Sprintf("wiring[%d].when[%d] routes to unknown node %q", i, j, a.To)})
				}
			}
			if w.Else != "" && !declared[w.Else] {
				errAt(ConfigProblem{From: w.Switch, To: w.Else, Message: fmt.Sprintf("wiring[%d] else routes to unknown node %q", i, w.Else)})
			}
			if w.Else == "" {
				r.Problems = append(r.Problems, ConfigProblem{Node: w.Switch, Severity: "warning",
					Message: fmt.Sprintf("switch over %q has no else: a value no when arm matches fails the run (\"matched no arm and has no Else\")", w.Switch)})
			}
		case w.isJoin():
			if w.Join == "" {
				errAt(ConfigProblem{Message: fmt.Sprintf("wiring[%d] join has no name", i)})
			}
			if w.Merge == "" {
				errAt(ConfigProblem{Node: w.Join, Message: fmt.Sprintf("wiring[%d] join %q names no merge block", i, w.Join)})
			}
			for _, in := range w.Inputs {
				if !declared[in] {
					errAt(ConfigProblem{From: in, To: w.Join, Message: fmt.Sprintf("wiring[%d] join %q reads unknown input %q", i, w.Join, in)})
				}
			}
		}
	}
}

var (
	wiringRef = regexp.MustCompile(`wiring\[(\d+)\](?:\.when\[(\d+)\])?`)
	edgeRef   = regexp.MustCompile(`edge "((?:[^"\\]|\\.)*)" -> "((?:[^"\\]|\\.)*)"`)
	quoted    = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
)

// attribute adds the problems of an error from assemble or Build, each attached to the wiring
// element, edge or node its message names. assemble joins the problems it collects with "; ".
func (r *ConfigReport) attribute(cfg *config, err error) {
	msg := strings.TrimSuffix(err.Error(), ": "+agent.ErrConfig.Error())
	if errors.Is(err, agent.ErrConfig) {
		msg = strings.ReplaceAll(msg, ": "+agent.ErrConfig.Error(), "")
	}
	for _, prefix := range []string{"plan: load " + strconv.Quote(cfg.Flow) + ": ", "plan: build " + strconv.Quote(cfg.Flow) + ": "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	declared := map[string]bool{}
	for _, n := range r.Topology.Nodes {
		declared[n.Name] = true
	}
	for _, part := range strings.Split(msg, "; ") {
		p := ConfigProblem{Message: part, Severity: "error"}
		if m := edgeRef.FindStringSubmatch(part); m != nil {
			p.From, p.To = m[1], m[2]
		} else if m := wiringRef.FindStringSubmatch(part); m != nil {
			i, _ := strconv.Atoi(m[1])
			if i < len(cfg.Wiring) {
				w := cfg.Wiring[i]
				switch {
				case m[2] != "" && w.isSwitch():
					j, _ := strconv.Atoi(m[2])
					if j < len(w.When) {
						p.From, p.To = w.Switch, w.When[j].To
					}
				case w.isSwitch():
					p.Node = w.Switch
				case w.isJoin():
					p.Node = w.Join
				case len(w.Edge) == 2:
					p.From, p.To = w.Edge[0], w.Edge[1]
				}
			}
		} else {
			for _, q := range quoted.FindAllStringSubmatch(part, -1) {
				if name, err := strconv.Unquote(`"` + q[1] + `"`); err == nil && declared[name] {
					p.Node = name
					break
				}
			}
		}
		r.Problems = append(r.Problems, p)
	}
}
