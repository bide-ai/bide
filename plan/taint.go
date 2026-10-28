package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/internal/toolhook"
)

// Taint in plan flows (docs/design/flow-taint.md). This file holds what a flow can declare: the
// provenance of a node's results (Untrusted, Trusted), an exemption (TaintExempt), and the flow's
// policy (WithTaintPolicy). With a policy the engine enforces them (docs/design/flow-taint.md); a
// declaration without a policy gates nothing, as an agent's marks do without a policy.

// nodeTaint is what a node declares for taint. The names follow the agent's: a source is the
// declared origin of untrusted content the node's result carries, trusted says why the result is
// the host's own, and exempt says why the node needs no escalation gate.
type nodeTaint struct {
	// untrusted are the sources, sorted, each once. A tool's declaration, a block's option and the
	// config key add to it; nothing removes a source.
	untrusted []string
	// trusted is the reason the node's results are the host's own; never with a source.
	trusted string
	// exempt is the reason the node needs no escalation gate.
	exempt string
}

// any reports whether the node declares anything.
func (t nodeTaint) any() bool { return len(t.untrusted) > 0 || t.trusted != "" || t.exempt != "" }

// clone returns a copy that shares no slice with t.
func (t nodeTaint) clone() nodeTaint {
	t.untrusted = slices.Clone(t.untrusted)
	return t
}

// withSource adds a source and keeps the list sorted and without repeats. It refuses a source on a
// node that declares its results trusted, as the agent refuses a tool that is both.
func (t *nodeTaint) withSource(what, source string) error {
	if err := toolhook.CheckProvenanceName(what, source); err != nil {
		return err
	}
	if t.trusted != "" {
		return fmt.Errorf("%s(%q): the node also declares its results trusted (%q), and a node cannot be both: %w", what, source, t.trusted, agent.ErrConfig)
	}
	if !slices.Contains(t.untrusted, source) {
		t.untrusted = append(t.untrusted, source)
		slices.Sort(t.untrusted)
	}
	return nil
}

// Untrusted declares that the results of the node come from outside, from the named source (for
// example "inbound-email"): text the host did not write and an attacker may have. Use it on a Step,
// Model or Join block, or on a Tool block that does not already declare it (a tool's own Untrusted
// is taken as it is). Sources accumulate: a second Untrusted, a tool's declaration and the config
// key "untrusted" each add one, and none removes one. A node cannot also be Trusted. The name rules
// are those of agent.Untrusted. Experimental. It marks what the run has read for the flow's taint policy
// (WithTaintPolicy); without a policy it gates nothing.
func Untrusted(source string) NodeOption {
	return func(n *node) {
		if err := n.taint.withSource("plan.Untrusted", source); err != nil {
			n.setOptErr(err)
		}
	}
}

// Trusted declares that the results of the node are the host's own, and why (the reason is
// required, and follows the rules of agent.Trusted). It is the positive counterpart of Untrusted and
// records no mark; a flow whose policy requires provenance needs a node to declare one or the
// other. A node cannot be both, and two different reasons are an error. Experimental: see Untrusted.
func Trusted(reason string) NodeOption {
	return func(n *node) {
		if err := toolhook.CheckProvenanceName("plan.Trusted", reason); err != nil {
			n.setOptErr(err)
			return
		}
		switch {
		case len(n.taint.untrusted) > 0:
			n.setOptErr(fmt.Errorf("plan.Trusted(%q): the node also declares untrusted results (%s), and a node cannot be both: %w", reason, strings.Join(n.taint.untrusted, ", "), agent.ErrConfig))
		case n.taint.trusted != "" && n.taint.trusted != reason:
			n.setOptErr(fmt.Errorf("plan.Trusted(%q): the node already declares its results trusted with another reason (%q): %w", reason, n.taint.trusted, agent.ErrConfig))
		default:
			n.taint.trusted = reason
		}
	}
}

// TaintExempt declares that the node needs no escalation gate even when the run has read untrusted
// content, and why (the reason is required, and follows the rules of agent.TaintExempt). It loosens
// a requirement, so it is Go only: the config cannot declare it. A different reason than the one a
// tool already declares is an error. Experimental: see Untrusted.
func TaintExempt(reason string) NodeOption {
	return func(n *node) {
		if err := toolhook.CheckProvenanceName("plan.TaintExempt", reason); err != nil {
			n.setOptErr(err)
			return
		}
		if n.taint.exempt != "" && n.taint.exempt != reason {
			n.setOptErr(fmt.Errorf("plan.TaintExempt(%q): the node already declares another exemption (%q): %w", reason, n.taint.exempt, agent.ErrConfig))
			return
		}
		n.taint.exempt = reason
	}
}

// setOptErr records the first error an option found; register surfaces it at Build.
func (n *node) setOptErr(err error) {
	if n.optErr == nil {
		n.optErr = fmt.Errorf("plan: step %q: %w", n.name, err)
	}
}

// taintOfSpec is the declarations a tool's spec makes, from either place the agent accepts a source
// (ToolSpec.Untrusted and Safety.Untrusted).
func taintOfSpec(spec agent.ToolSpec) nodeTaint {
	var t nodeTaint
	for _, s := range []string{spec.Untrusted, spec.Safety.Untrusted} {
		if s != "" && !slices.Contains(t.untrusted, s) {
			t.untrusted = append(t.untrusted, s)
		}
	}
	slices.Sort(t.untrusted)
	t.trusted, t.exempt = spec.Trusted, spec.TaintExempt
	return t
}

// WithTaintPolicy gives the flow a taint policy, the same type an agent takes (agent.WithTaintPolicy):
// an escalation Gate, or Refuse, and optionally RequireProvenance. It is Go only, as a policy is: a
// config cannot opt a flow in. The policy is checked as an agent checks one; an invalid policy is an
// error at Build. A node that can have a side effect, run after the run has read untrusted content (a node
// that declares a source, the run's input when it is declared untrusted, or what a run a node started
// brought up), needs the policy's escalation gate on the node's instance, or is refused under Refuse.
// Experimental (docs/design/flow-taint.md).
func (b *Builder[In, Out]) WithTaintPolicy(p agent.TaintPolicy) *Builder[In, Out] {
	b.core.setTaintPolicy(p)
	return b
}

// WithLoadedTaintPolicy gives a flow produced by Load or LoadReader a taint policy: the loader's
// equivalent of Builder.WithTaintPolicy. Experimental.
func WithLoadedTaintPolicy(p agent.TaintPolicy) LoadOption {
	return func(c *builderCore) { c.setTaintPolicy(p) }
}

// setTaintPolicy records p, or the error that makes it invalid, which Build reports.
func (c *builderCore) setTaintPolicy(p agent.TaintPolicy) {
	if err := toolhook.CheckTaintPolicy(p); err != nil {
		c.errs = append(c.errs, fmt.Errorf("plan: flow %q: WithTaintPolicy: %w", c.flowName, err))
		return
	}
	p.Gate = p.Gate.Clone()
	c.taintPolicy = &p
}

// ErrTaintRefused is returned (wrapped, naming the node) by Flow.Run when the flow's taint policy is Refuse,
// the run has read untrusted content, and the node can have a side effect: the node does not run, no record
// is written for it, and every later drive refuses it again; the run stays unfinished until it is cancelled
// (agent.Cancel). It wraps agent.ErrConfig.
var ErrTaintRefused = fmt.Errorf("the taint policy refused the node: %w", agent.ErrConfig)

// trackedNames are the nodes that can have a side effect (not read-only), in order.
func (c *builderCore) trackedNames() []string {
	var out []string
	for _, n := range c.nodes {
		if !n.safety.ReadOnly {
			out = append(out, n.name)
		}
	}
	return out
}

// flowTaint is the flow's taint declarations as plain data for the engine: its policy and, by node name, what
// each node declares. nil for a flow with no policy: node declarations alone gate nothing, as an agent's marks
// do without a policy, and the run does not start under taint.
func (c *builderCore) flowTaint() *journalhook.FlowTaint {
	if c.taintPolicy == nil {
		return nil
	}
	d := &journalhook.FlowTaint{Refuse: c.taintPolicy.Refuse, RequireProvenance: c.taintPolicy.RequireProvenance, SeparateApprovers: c.taintPolicy.SeparateApprovers}
	if c.taintPolicy.Gate != nil {
		d.Gate = c.taintPolicy.Gate.Clone()
	}
	for _, n := range c.nodes {
		if !n.taint.any() {
			continue
		}
		if d.Nodes == nil {
			d.Nodes = map[string]journalhook.FlowTaintNode{}
		}
		d.Nodes[n.name] = journalhook.FlowTaintNode{Untrusted: slices.Clone(n.taint.untrusted), Trusted: n.taint.trusted, Exempt: n.taint.exempt}
	}
	return d
}

// checkSeparation holds the nodes to a policy that separates approvers (TaintPolicy.SeparateApprovers): a node
// that can be escalated (it can have a side effect and is not exempt) and has an own gate needs that gate to
// be m-of-n, and its approvers to share no ID with the policy's Gate. It runs at Build, on the final node set,
// after a config has been lowered into it; a read-only or exempt node is skipped.
func (c *builderCore) checkSeparation() error {
	if !c.taintPolicy.SeparateApprovers {
		return nil
	}
	esc := c.taintPolicy.Gate
	for _, n := range c.nodes {
		if n.approval == nil || n.safety.ReadOnly || n.taint.exempt != "" {
			continue
		}
		if topologyGate(n.approval).Single {
			return fmt.Errorf("plan: build %q: node %q has an own gate that is one decision (Approval(SingleApproval)) and the taint policy separates approvers: a single gate has no approver IDs to keep apart; make it m-of-n, or declare the node TaintExempt: %w", c.flowName, n.name, agent.ErrConfig)
		}
		var shared []string
		for _, id := range n.approval.Approvers {
			if slices.ContainsFunc(esc.Approvers, func(e string) bool { return agent.SameApprover(id, e) }) && !slices.Contains(shared, id) {
				shared = append(shared, id)
			}
		}
		if len(shared) > 0 {
			slices.Sort(shared)
			return fmt.Errorf("plan: build %q: node %q has an own gate and the taint policy's Gate that share the approvers %s, and the policy separates approvers: the second look could be one person; give the gates disjoint approvers: %w", c.flowName, n.name, strings.Join(shared, ", "), agent.ErrConfig)
		}
	}
	return nil
}

// declaredSources are the sources each node declares, by node name, sorted: nil when no node declares one. A flow
// with no policy records them in its runs' start, so what the run learns can be counted for the node that started
// it (agent.RunStart, "taint_sources").
func (c *builderCore) declaredSources() map[string][]string {
	var out map[string][]string
	for _, n := range c.nodes {
		if len(n.taint.untrusted) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[n.name] = slices.Clone(n.taint.untrusted)
	}
	return out
}

// checkTaintPolicy holds a flow with a taint policy to what its enforcement needs: an m-of-n escalation Gate
// needs approver verifiers (no decision could pass it without), and a policy that requires provenance needs a
// declaration on every node: Untrusted, or Trusted with a reason. (A Run that states where its input came from
// is checked when the run starts.)
func (c *builderCore) checkTaintPolicy() error {
	if c.taintPolicy == nil {
		return nil
	}
	if g := c.taintPolicy.Gate; g != nil && !topologyGate(g).Single && c.approverVerifiers == nil {
		return fmt.Errorf("plan: build %q: the taint policy's escalation gate (%s) is m-of-n but the flow has no approver verifiers; bind them with WithApproverVerifiers (or WithLoadedApproverVerifiers): %w", c.flowName, gateText(g), agent.ErrConfig)
	}
	if err := c.checkSeparation(); err != nil {
		return err
	}
	if !c.taintPolicy.RequireProvenance {
		return nil
	}
	for _, n := range c.nodes {
		if len(n.taint.untrusted) == 0 && n.taint.trusted == "" {
			return fmt.Errorf("plan: build %q: node %q declares no provenance, and the taint policy requires one: declare Untrusted(source) if its results come from outside, or Trusted(reason) if they are the host's own: %w", c.flowName, n.name, agent.ErrConfig)
		}
	}
	return nil
}

// configTaint is the declarations of a node whose block declares base and whose config says
// untrusted ("" for no key): the block's, plus the config's source. The problem it returns, "" for
// none, is a source that is not a valid name or that contradicts the block's trusted output. A
// config can add a source and nothing else, so it cannot loosen what Go declared.
func configTaint(base nodeTaint, untrusted string) (nodeTaint, string) {
	t := base.clone()
	if untrusted == "" {
		return t, ""
	}
	if err := t.withSource("untrusted", untrusted); err != nil {
		return base.clone(), err.Error()
	}
	return t, ""
}
