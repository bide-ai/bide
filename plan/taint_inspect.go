package plan

import (
	"slices"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/taintwords"
)

// The static view of flow taint (docs/design/flow-taint.md, section 1): the may-version of the rule the
// engine enforces (agent.flowReadSet), computed once here for InspectConfig, bide check and the editor. It
// over-approximates and never says a node is clean: an undeclared source, a block that runs an agent or a
// nested flow, and a run whose input is declared untrusted are all outside what a config can show.

// Verdict words of NodeTaint.Verdict. They are the words the views print.
const (
	taintVerdictUnknown   = "unknown"
	taintVerdictReads     = "reads untrusted"
	taintVerdictEscalates = "escalates"
	taintVerdictRefused   = "refused"
	taintVerdictExempt    = "exempt"
	taintVerdictNoSource  = "no declared source before it"
)

// Reasons that say what the verdict cannot know.
const (
	taintCaveatNoSource = taintwords.Caveat
	taintReasonNoPolicy = taintwords.NotGiven + ": whether it escalates is not known"
	// taintReasonNone is the reason of a node under a policy that is known to be absent
	// (InspectTaintPolicy with the zero agent.TaintPolicy). Readers tell it from taintReasonNoPolicy by the
	// prefixes in internal/taintwords.
	taintReasonNone   = taintwords.NoPolicy + ": nothing gates its side effect"
	taintReasonBlocks = "blocks not known: check with -blocks or a Registry"
	taintReasonV1     = "the description does not carry taint declarations (version 1)"
)

// NodeTaint is the static taint verdict on one node. It says what a declared source can do to the node,
// never that the node is safe: "no declared source before it" is the strongest word, and it comes with
// the cases it cannot see.
//
// Experimental: may change in any 0.x release.
type NodeTaint struct {
	// Verdict is one of "unknown" (declarations or policy not known), "reads untrusted" (a declared source
	// may run before the node), "escalates" (it also can have a side effect, is not exempt and the policy
	// has a Gate), "refused" (the same under a policy that refuses), "exempt" (it reads untrusted and
	// declares TaintExempt), and "no declared source before it".
	Verdict string `json:"verdict"`
	// Declares are the sources the node itself introduces: its block's Untrusted sources and the config's
	// "untrusted" key, sorted.
	Declares []string `json:"declares,omitempty"`
	// Sources are the declared sources that may run before the node, sorted. A node in a loop sees every
	// source of the loop's body, its own included.
	Sources []string `json:"sources,omitempty"`
	// Path is a shortest path along the flow's edges and arms from a node that declares one of Sources to this
	// node, both ends included. Empty when the source reaches the node only by running earlier in the flow's
	// order.
	Path []string `json:"path,omitempty"`
	// Exempt is the node's TaintExempt reason.
	Exempt string `json:"exempt,omitempty"`
	// Reason says what the verdict cannot know, or why it is unknown.
	Reason string `json:"reason,omitempty"`
}

// InspectTaintPolicy gives InspectConfig the taint policy of the flow, which the config cannot carry (it is
// Go: Builder.WithTaintPolicy). Without it a Registry-checked report does not know the policy, and a node
// that reads untrusted data is reported as such, not as escalating. The zero agent.TaintPolicy states that
// the flow has no policy (it is not one WithTaintPolicy accepts): a node that can have a side effect and
// reads untrusted data is then reported as not gated. InspectConfigDescribed takes the policy
// of the description (RegistryDescription.TaintPolicy); this option overrides it.
//
// Experimental: may change in any 0.x release.
func InspectTaintPolicy(p agent.TaintPolicy) InspectOption {
	return func(o *inspectOptions) { o.taintPolicy = &p }
}

// structuralTaint is the report of a check that knows no blocks: every node is unknown, with the sources
// the config itself states.
func structuralTaint(cfg *config, reason string) map[string]NodeTaint {
	out := map[string]NodeTaint{}
	add := func(name, untrusted string) {
		t := NodeTaint{Verdict: taintVerdictUnknown, Reason: reason}
		if untrusted != "" {
			t.Declares = []string{untrusted}
		}
		out[name] = t
	}
	for _, n := range cfg.Nodes {
		add(n.Name, n.Untrusted)
	}
	for _, w := range cfg.Wiring {
		if w.isJoin() && !w.isEdge() && !w.isSwitch() {
			add(w.Join, w.Untrusted)
		}
	}
	return out
}

// taintReport is the static verdict on each node of an assembled flow: policy is the taint policy when known
// (nil: not given). It uses the engine's own order (topoOrder) and loops (deriveLoops), never its own.
func (c *builderCore) taintReport(policy *agent.TaintPolicy) (map[string]NodeTaint, error) {
	order, err := c.topoOrder()
	if err != nil {
		return nil, err
	}
	idx := make(map[string]int, len(order))
	for i, name := range order {
		idx[name] = i
	}
	// bodies[name] are the nodes that share a loop with name (itself included), through any loop that holds it.
	shared := map[string]map[string]bool{}
	for _, lp := range c.loops {
		for _, a := range lp.body {
			if shared[a] == nil {
				shared[a] = map[string]bool{}
			}
			for _, b := range lp.body {
				shared[a][b] = true
			}
		}
	}
	adj := map[string][]string{}
	for _, e := range c.edges {
		adj[e.from] = append(adj[e.from], e.to)
	}
	for _, br := range c.branches {
		for _, a := range br.arms {
			adj[br.over] = append(adj[br.over], a.target)
		}
	}
	out := make(map[string]NodeTaint, len(c.nodes))
	for _, n := range c.nodes {
		t := NodeTaint{Declares: slices.Clone(n.taint.untrusted), Exempt: n.taint.exempt}
		// A source may run before n when it is earlier in the engine's order, or when it shares a loop with n
		// (the iterations after the first see the whole body, a node's own result included).
		var from []string
		for _, m := range c.nodes {
			if len(m.taint.untrusted) == 0 {
				continue
			}
			if idx[m.name] < idx[n.name] || shared[n.name][m.name] {
				from = append(from, m.name)
				for _, s := range m.taint.untrusted {
					if !slices.Contains(t.Sources, s) {
						t.Sources = append(t.Sources, s)
					}
				}
			}
		}
		slices.Sort(t.Sources)
		tracked := !n.safety.ReadOnly
		switch {
		case len(t.Sources) == 0:
			t.Verdict, t.Reason = taintVerdictNoSource, taintCaveatNoSource
			if len(t.Declares) > 0 && tracked {
				// The node is a source that can have a side effect: it runs ungated for its own read, as an agent's
				// tool does in the turn that reads.
				t.Reason = taintwords.SelfSource + "; " + taintCaveatNoSource
			}
		case !tracked:
			t.Verdict = taintVerdictReads
		case n.taint.exempt != "":
			t.Verdict = taintVerdictExempt
		case policy == nil:
			t.Verdict, t.Reason = taintVerdictReads, taintReasonNoPolicy
		case !policy.Refuse && policy.Gate == nil:
			// The zero policy: known to be none, so nothing gates the node.
			t.Verdict, t.Reason = taintVerdictReads, taintReasonNone
		case policy.Refuse:
			t.Verdict = taintVerdictRefused
		default:
			t.Verdict = taintVerdictEscalates
		}
		if len(from) > 0 {
			t.Path = shortestPath(adj, from, func(x string) bool { return x == n.name }, func(string) bool { return false })
			if len(t.Path) < 2 {
				t.Path = nil // the node is its own source (a loop), or no edge leads there
			}
		}
		out[n.name] = t
	}
	return out, nil
}

// describedPolicy is the taint policy a description gives, nil when it gives none.
func describedPolicy(p *TaintPolicyDescription) *agent.TaintPolicy {
	if p == nil {
		return nil
	}
	g, _ := describedGate(p.Gate) // Validate has checked it
	return &agent.TaintPolicy{Gate: g, Refuse: p.Refuse, RequireProvenance: p.RequireProvenance}
}

// taintUnknownReason is why the taint of a Registry-checked report is not known, "" when it is.
func (r *Registry) taintUnknownReason() string {
	if r.inert && !r.taintDeclared {
		return taintReasonV1
	}
	return ""
}
