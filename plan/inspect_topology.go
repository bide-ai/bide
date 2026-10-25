package plan

import (
	"slices"
	"time"

	"github.com/bide-ai/bide/agent"
)

// InspectTopology is the report of a declared Topology alone, for a tool that holds a topology and
// no config: the topology a run journaled (JournaledTopology), compared with the one a config
// declares. It is what InspectConfig derives from a topology, and nothing else, by the same code.
//
// Topology is a copy of t (the report does not alias t). Digest is TopologyDigest(t), empty when it
// cannot be computed. Approvals holds each gate in t as a policy (a one-decision gate as
// agent.SingleApproval's). That is every gate the topology carries, including a gate a block declares
// in Go; InspectConfig's Approvals lists only the gates the config declares. Gated is computed as InspectConfig computes it: a node is gated when the
// entry cannot reach it with the gated nodes removed.
//
// Safety and Assertions are nil: a topology does not carry them (a node's crash behavior and the
// config's assert lines are not in the digest, and a journal holds neither). Typed is false and
// Problems and Findings are empty: InspectTopology does not validate, so a dangling edge, a
// duplicate node name or an entry that names no node is tolerated, and what the entry does not reach
// is simply not gated. It never panics, whatever t holds, and costs O(nodes + edges).
func InspectTopology(t Topology) ConfigReport {
	t = cloneTopology(t)
	r := ConfigReport{Topology: t, Approvals: map[string]agent.ApprovalPolicy{}}
	if d, err := TopologyDigest(t); err == nil {
		r.Digest = d
	}
	for _, n := range t.Nodes {
		if g := n.Gate; g != nil {
			r.Approvals[n.Name] = policyOf(g)
		}
	}
	r.gated()
	return r
}

// policyOf is g as the policy it came from.
func policyOf(g *TopologyGate) agent.ApprovalPolicy {
	if g.Single {
		return *agent.SingleApproval()
	}
	return agent.ApprovalPolicy{Need: g.Need, Approvers: slices.Clone(g.Approvers),
		ExcludePrincipal: g.ExcludePrincipal, Timeout: time.Duration(g.TimeoutMS) * time.Millisecond}
}

// cloneTopology returns a deep copy of t: nothing in it shares memory with t.
func cloneTopology(t Topology) Topology {
	c := t
	c.Nodes = slices.Clone(t.Nodes)
	for i, n := range c.Nodes {
		if n.Gate != nil {
			g := *n.Gate
			g.Approvers = slices.Clone(n.Gate.Approvers)
			c.Nodes[i].Gate = &g
		}
	}
	c.Edges = slices.Clone(t.Edges)
	c.Branches = slices.Clone(t.Branches)
	for i, b := range c.Branches {
		c.Branches[i].Arms = slices.Clone(b.Arms)
	}
	c.Joins = slices.Clone(t.Joins)
	for i, j := range c.Joins {
		c.Joins[i].Inputs = slices.Clone(j.Inputs)
		c.Joins[i].InTypes = slices.Clone(j.InTypes)
		c.Joins[i].InCanonical = slices.Clone(j.InCanonical)
	}
	c.Loops = slices.Clone(t.Loops)
	for i, l := range c.Loops {
		c.Loops[i].Body = slices.Clone(l.Body)
	}
	return c
}
