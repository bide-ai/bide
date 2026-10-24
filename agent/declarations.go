package agent

import (
	"maps"
	"slices"

	"github.com/bide-ai/bide/internal/declarations"
)

// declarationsDoc is the agent's taint and cap declarations as the report reads them
// (internal/declarations): the policy, and every registered tool with what it declares. Its Digest is the
// taint_policy_digest a run this agent starts records, "" for an agent with no policy. It is the document
// the declarations report and diff are built on; nothing exports it yet.
func (a *Agent) declarationsDoc() declarations.Document {
	d := declarations.Document{Format: declarations.Format, Digest: a.taintDigest(), Tools: []declarations.Tool{}}
	if a.taint != nil {
		d.Policy = &declarations.Policy{Gate: gateDoc(a.taint.Gate), Refuse: a.taint.Refuse}
	}
	for _, name := range slices.Sorted(maps.Keys(a.specs)) {
		s := a.specs[name]
		d.Tools = append(d.Tools, declarations.Tool{
			Name: name, Effect: tracked(s.Safety), OwnGate: gateDoc(s.Approval),
			Untrusted: s.untrustedSource(), Exempt: s.TaintExempt, Caps: slices.Clone(s.Caps),
		})
	}
	return d
}

// gateDoc is an approval policy as the report holds it.
func gateDoc(p *ApprovalPolicy) *declarations.Gate {
	if p == nil {
		return nil
	}
	g := &declarations.Gate{TimeoutMS: p.Timeout.Milliseconds()}
	if p.single() {
		g.Single = true
		return g
	}
	g.Need, g.Approvers, g.ExcludePrincipal = p.Need, slices.Clone(p.Approvers), p.ExcludePrincipal
	return g
}
