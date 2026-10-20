package plan

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	// Assertions are the verdicts on the config's assert lines, one per line, in order.
	Assertions []AssertionResult
	// Gated reports, by node, that every path from the entry to it passes an approval gate (its
	// own included): the run cannot reach it without a human decision (vacuously so for a node the
	// entry cannot reach). Without a Registry only the gates the config declares count.
	Gated map[string]bool
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
// Each problem is attached to the node, edge or arm its message names. Every approval block is
// listed in Approvals and on its node in Topology; one that is not a valid policy is an error. An
// m-of-n gate is checked as Load checks it with approver verifiers bound
// (WithLoadedApproverVerifiers): the keys are not the config's. One warning is not a load error: a Switch with no else, since a value no arm
// matches fails the run.
//
// Safety reports each node's crash behavior, and Assertions the verdict on each line of the
// config's "assert" list (which Load ignores), decided by reachability on the declared graph:
//
//	every path to <node> passes approval >= K [of N]
//	every path to <node> passes <node>
//	every path to a terminal passes <node>
//	no side effect after <node>
//	<node> reachable
//	<node> unreachable
//
// A node is named bare or in double quotes. A gate counts toward "approval >= K of N" when it
// needs at least K decisions from at most N named approvers (a one-decision gate counts only for
// "approval >= 1"); without a Registry only the gates the config declares count. A violated,
// undecided or ill-formed assertion is an error, a violated one with a shortest counterexample
// path.
func InspectConfig(data []byte, reg *Registry, opts ...InspectOption) (r ConfigReport) {
	return inspectConfig(data, reg, opts)
}

// InspectOption changes what InspectConfig and InspectConfigDescribed check.
type InspectOption func(*inspectOptions)

type inspectOptions struct{ allowUnused bool }

// AllowUnusedBlocks makes a registered block the config does not use no problem. Load and Validate
// refuse it (a registration nothing uses is drift), and so does a check without this option; a tool
// that lists the blocks a config could use, as the flow editor's palette does, passes it so that the
// blocks it has not placed yet do not stop the check from reading the ones it has.
func AllowUnusedBlocks() InspectOption { return func(o *inspectOptions) { o.allowUnused = true } }

// inspectConfig is InspectConfig, and what InspectConfigDescribed runs on a Registry built from a
// description: one implementation of the typed checks, whichever side the Registry came from.
func inspectConfig(data []byte, reg *Registry, opts []InspectOption) ConfigReport {
	r := inspectConfigUnbounded(data, reg, opts)
	for i := range r.Problems {
		r.Problems[i].Message = boundMessage(r.Problems[i].Message)
	}
	return r
}

// maxQuotedName is the most of a quoted name (a node, a block, a key from the config) a problem
// message repeats, and maxProblemMessage the most of a message when it still is long.
const (
	maxQuotedName     = 64
	maxProblemMessage = 2048
)

// boundMessage shortens the double-quoted names in s to maxQuotedName characters each, marking the
// cut inside the quotes, so that a name as long as the file does not make a message as long as the
// file and the words around the names (which callers match, such as "(types must match exactly)")
// are kept. A message that is still longer than maxProblemMessage keeps its head and its tail.
func boundMessage(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '"' {
			b.WriteRune(rs[i])
			continue
		}
		j := i + 1
		for j < len(rs) && rs[j] != '"' {
			if rs[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(rs) { // an unclosed quote: leave the rest as it is
			b.WriteString(string(rs[i:]))
			break
		}
		name := rs[i+1 : j]
		b.WriteRune('"')
		if len(name) > maxQuotedName {
			b.WriteString(string(name[:maxQuotedName]))
			b.WriteString("...")
		} else {
			b.WriteString(string(name))
		}
		b.WriteRune('"')
		i = j
	}
	out := []rune(b.String())
	if len(out) > maxProblemMessage {
		return string(out[:maxProblemMessage/2]) + " ... " + string(out[len(out)-maxProblemMessage/2:])
	}
	return b.String()
}

func inspectConfigUnbounded(data []byte, reg *Registry, opts []InspectOption) (r ConfigReport) {
	var o inspectOptions
	for _, f := range opts {
		f(&o)
	}
	r = ConfigReport{Typed: reg != nil, Approvals: map[string]agent.ApprovalPolicy{}}
	var cfg config
	if err := parseConfig(data, &cfg); err != nil {
		r.Problems = append(r.Problems, ConfigProblem{Message: strings.TrimSuffix(err.Error(), ": "+agent.ErrConfig.Error()), Severity: "error"})
		return r
	}
	r.Topology = configTopology(&cfg)
	r.Safety = configSafety(&cfg)
	structural(&cfg, &r)
	if reg == nil && !r.Errors() {
		// Without a Registry nothing runs Build, so its checks on the wiring alone are made here: a cycle,
		// a loop's shape, reachability, a join's inputs, what feeds a node. With a Registry Build makes
		// them itself, after the type checks, so a type error is not hidden behind a wiring one. They
		// run over a spec with no types.
		if err := graphChecks(&cfg); err != nil {
			// One refusal, as one problem: attribute splits an error at "; " (it joins several), and Build's
			// own wording has one inside it.
			r.attribute(&cfg, errors.New(strings.ReplaceAll(err.Error(), "; ", ", ")))
		}
	}
	defer func() { r.gated(); r.assertions(cfg.Assert) }()
	if reg == nil || r.Errors() {
		return r
	}
	core, err := assemble(&cfg, reg, o.allowUnused)
	// Type problems of the wiring come with the assembled core: the checks that follow run over it
	// as well, so a report lists every type problem, not the first.
	var typeErr *typeProblemsError
	if err != nil && !(errors.As(err, &typeErr) && core != nil) {
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
	// Every type problem, before Build (which stops at the first problem of any kind, so a second
	// misfit, or one behind a wiring error, would not show).
	var typeMsgs []string
	if typeErr != nil {
		typeMsgs = typeErr.msgs
	}
	typeMsgs = append(typeMsgs, core.buildTypeProblems()...)
	if len(typeMsgs) > 0 {
		r.attribute(&cfg, fmt.Errorf("plan: load %q: %s", cfg.Flow, strings.Join(capTypeProblems(typeMsgs), "; ")))
		return r
	}
	// The check is of the policy, not of approvers' keys, which a config does not carry: a
	// resolver that knows no approver lets Build check an m-of-n gate as Load would with keys.
	if core.approverVerifiers == nil {
		core.approverVerifiers = func(string) (agent.ApproverVerifier, bool) { return nil, false }
	}
	// A Model node calls the flow's bound model at run time, which a check never does; bind one
	// that fails if it is ever called, so a Model block is not reported as a missing binding.
	if core.model == nil {
		core.model = unboundModel{}
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

// unboundModel stands in for the model a program binds at Load, so Build accepts a Model node. A
// check never runs a flow, so Stream is never called.
type unboundModel struct{}

func (unboundModel) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	return nil, errors.New("plan: a config check binds no model")
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
	gateOf := func(ca *configApproval) *TopologyGate {
		if ca == nil {
			return nil
		}
		g := &TopologyGate{Need: ca.Need, Approvers: append([]string(nil), ca.Approvers...), ExcludePrincipal: ca.ExcludePrincipal}
		if d, err := time.ParseDuration(ca.Timeout); err == nil && d > 0 {
			g.TimeoutMS = d.Milliseconds()
		}
		return g
	}
	for _, n := range cfg.Nodes {
		t.Nodes = append(t.Nodes, TopologyNode{Name: n.Name, Block: n.Block, Gate: gateOf(n.Approval)})
	}
	for _, w := range cfg.Wiring {
		switch {
		case w.isEdge() && len(w.Edge) == 2:
			t.Edges = append(t.Edges, TopologyEdge{From: w.Edge[0], To: w.Edge[1]})
		case w.isJoin() && !w.isEdge() && !w.isSwitch():
			t.Nodes = append(t.Nodes, TopologyNode{Name: w.Join, Kind: NodeKindJoin, Block: w.Merge, Gate: gateOf(w.Approval)})
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
			p, why := configGate(nil, n.Approval)
			r.Approvals[n.Name] = *p
			for _, w := range why {
				errAt(ConfigProblem{Node: n.Name, Message: fmt.Sprintf("node %q %s", n.Name, w)})
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
	joinOrder(cfg, errAt)
	joinsAndSwitches(cfg, errAt)
	loopShape(cfg, declared, errAt)
}

// joinsAndSwitches reports what Load and Build refuse about how edges and switches meet joins and
// switches, which the wiring alone shows: a join referenced (as the source of an edge or the node a
// switch is over) before its wiring element, an edge, arm or else into a join (a join is entered only
// by its inputs), a node that is switched over and has an edge out, and two switches over one node.
func joinsAndSwitches(cfg *config, errAt func(ConfigProblem)) {
	joinAt := map[string]int{}
	for i, w := range cfg.Wiring {
		if w.isJoin() && w.Join != "" && !w.isEdge() && !w.isSwitch() {
			joinAt[w.Join] = i
		}
	}
	later := func(i int, name string) bool { p, ok := joinAt[name]; return ok && p > i }
	isJoin := func(name string) bool { _, ok := joinAt[name]; return ok }
	switched := map[string]int{}
	for i, w := range cfg.Wiring {
		if w.isSwitch() && !w.isEdge() {
			if first, ok := switched[w.Switch]; ok {
				errAt(ConfigProblem{Node: w.Switch, Message: fmt.Sprintf("wiring[%d] more than one switch is over %q (the first is wiring[%d])", i, w.Switch, first)})
			} else {
				switched[w.Switch] = i
			}
		}
	}
	for i, w := range cfg.Wiring {
		switch {
		case w.isEdge() && len(w.Edge) == 2:
			from, to := w.Edge[0], w.Edge[1]
			if later(i, from) {
				errAt(ConfigProblem{From: from, To: to, Message: fmt.Sprintf("wiring[%d] edge %q -> %q leaves the join %q, which is defined after it in the wiring (a join is registered when its element is reached)", i, from, to, from)})
			}
			if isJoin(to) {
				errAt(ConfigProblem{From: from, To: to, Message: fmt.Sprintf("wiring[%d] edge %q -> %q is an edge into the join %q; a join is entered only by its inputs", i, from, to, to)})
			}
			if _, ok := switched[from]; ok {
				errAt(ConfigProblem{From: from, To: to, Message: fmt.Sprintf("wiring[%d] node %q is switched over and has an edge out (a node has a switch or an edge out, not both)", i, from)})
			}
		case w.isSwitch() && !w.isEdge():
			if later(i, w.Switch) {
				errAt(ConfigProblem{Node: w.Switch, Message: fmt.Sprintf("wiring[%d] the switch is over the join %q, which is defined after it in the wiring (a join is registered when its element is reached)", i, w.Switch)})
			}
			for j, a := range w.When {
				if isJoin(a.To) {
					errAt(ConfigProblem{From: w.Switch, To: a.To, Message: fmt.Sprintf("wiring[%d].when[%d] routes to the join %q; a join is entered only by its inputs", i, j, a.To)})
				}
			}
			if isJoin(w.Else) {
				errAt(ConfigProblem{From: w.Switch, To: w.Else, Message: fmt.Sprintf("wiring[%d] else routes to the join %q; a join is entered only by its inputs", i, w.Else)})
			}
		}
	}
}

// joinOrder reports a join that reads a join defined after it in the wiring. plan.Load registers a join
// when it reaches its wiring element, so an input that is a join must come earlier; this also rules
// out a cycle of joins.
func joinOrder(cfg *config, errAt func(ConfigProblem)) {
	at := map[string]int{}
	for i, w := range cfg.Wiring {
		if w.isJoin() && w.Join != "" && !w.isEdge() && !w.isSwitch() {
			at[w.Join] = i
		}
	}
	for i, w := range cfg.Wiring {
		if !w.isJoin() || w.isEdge() || w.isSwitch() {
			continue
		}
		for _, in := range w.Inputs {
			if p, ok := at[in]; ok && p >= i {
				errAt(ConfigProblem{From: in, To: w.Join, Message: fmt.Sprintf("wiring[%d] join %q reads the join %q, which is defined after it in the wiring; a join's inputs must be defined before it (a join is registered when its wiring element is reached)", i, w.Join, in)})
			}
		}
	}
}

// loopShape reports what Build refuses about a loop-back arm (an arm with loop_max above 0), which the
// structural check can see from the wiring alone: more than one loop-back arm on a switch, no arm or
// else that does not loop back, and a loop head that does not reach the switch in the forward graph
// (the edges, a join's inputs and the arms that do not loop back).
func loopShape(cfg *config, declared map[string]bool, errAt func(ConfigProblem)) {
	fwd := map[string][]string{}
	for _, w := range cfg.Wiring {
		switch {
		case w.isEdge() && len(w.Edge) == 2:
			fwd[w.Edge[0]] = append(fwd[w.Edge[0]], w.Edge[1])
		case w.isSwitch():
			for _, a := range w.When {
				if a.LoopMax <= 0 {
					fwd[w.Switch] = append(fwd[w.Switch], a.To)
				}
			}
			if w.Else != "" {
				fwd[w.Switch] = append(fwd[w.Switch], w.Else)
			}
		case w.isJoin() && w.Join != "":
			for _, in := range w.Inputs {
				fwd[in] = append(fwd[in], w.Join)
			}
		}
	}
	for i, w := range cfg.Wiring {
		if !w.isSwitch() || w.isEdge() {
			continue
		}
		loops, exits := 0, 0
		if w.Else != "" {
			exits++
		}
		for _, a := range w.When {
			if a.LoopMax > 0 {
				loops++
			} else {
				exits++
			}
		}
		if loops == 0 {
			continue
		}
		if loops > 1 {
			errAt(ConfigProblem{Node: w.Switch, Message: fmt.Sprintf("wiring[%d] the switch over %q has more than one loop-back arm; a switch expresses a single loop back-edge", i, w.Switch)})
		}
		if exits == 0 {
			errAt(ConfigProblem{Node: w.Switch, Message: fmt.Sprintf("wiring[%d] the loop over %q has no exit arm; add an arm or an else that does not loop back so the loop can terminate", i, w.Switch)})
		}
		for j, a := range w.When {
			if a.LoopMax > 0 && declared[a.To] && declared[w.Switch] && !forwardReaches(fwd, a.To, w.Switch) {
				errAt(ConfigProblem{From: w.Switch, To: a.To, Message: fmt.Sprintf("wiring[%d].when[%d] loops back to %q, which is not an ancestor of the switch over %q (a loop head must reach its loop switch in the forward graph)", i, j, a.To, w.Switch)})
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

// graphChecks runs the checks Build makes on the wiring alone, in Build's order, over a spec built from
// the config with no types: acyclicity, the loops' shape, reachability from the entry, a join's inputs
// and what feeds each node. It returns the first refusal, worded as Build words it, except that a node
// not reached from the entry says so as "not reached from the entry": a node not wired in yet is a config
// that will not load, and the words read as work in progress. It assumes the structural check found no
// error (every name resolves, every wiring element has one kind).
func graphChecks(cfg *config) error {
	core := &builderCore{flowName: cfg.Flow, byName: map[string]*node{}}
	add := func(n *node) {
		core.nodes = append(core.nodes, n)
		core.byName[n.name] = n
	}
	for _, n := range cfg.Nodes {
		add(&node{name: n.Name, block: n.Block})
	}
	core.entry = cfg.Entry
	if core.entry == "" && len(cfg.Nodes) > 0 {
		core.entry = cfg.Nodes[0].Name
	}
	for _, w := range cfg.Wiring {
		switch {
		case w.isEdge():
			core.edges = append(core.edges, edge{from: w.Edge[0], to: w.Edge[1]})
		case w.isJoin():
			add(&node{name: w.Join, block: w.Merge, kind: kindJoin, joinInputs: append([]string(nil), w.Inputs...)})
			for _, src := range w.Inputs {
				core.edges = append(core.edges, edge{from: src, to: w.Join})
			}
		case w.isSwitch():
			var arms []arm
			for _, a := range w.When {
				if a.LoopMax > 0 {
					arms = append(arms, arm{predName: a.Pred, target: a.To, loopBack: true, loopMax: a.LoopMax})
				} else {
					arms = append(arms, arm{predName: a.Pred, target: a.To})
				}
			}
			if w.Else != "" {
				arms = append(arms, arm{isElse: true, target: w.Else})
			}
			core.branches = append(core.branches, branch{over: w.Switch, arms: arms})
		}
	}
	if core.entry == "" || core.byName[core.entry] == nil {
		return nil // no entry or an unknown one is reported by the structural check
	}
	if _, err := core.topoOrder(); err != nil {
		return fmt.Errorf("plan: build %q: %w", core.flowName, err)
	}
	if err := core.deriveLoops(); err != nil {
		return err
	}
	reached := core.reachableFrom(core.entry)
	for _, n := range core.nodes {
		if !reached[n.name] {
			return fmt.Errorf("plan: build %q: step %q is not reached from the entry %q: wire it in from the entry with an edge or an arm, or remove it", core.flowName, n.name, core.entry)
		}
	}
	if err := core.checkJoins(); err != nil {
		return err
	}
	return core.checkShapes()
}
