package agent

import (
	"bytes"
	"fmt"
	"maps"
	"slices"

	"github.com/bide-ai/bide/internal/declarations"
)

// declarationsDoc is the agent's taint and cap declarations as the report reads them
// (internal/declarations): the policy, and every registered tool with what it declares. Its Digest is the
// taint_policy_digest a run this agent starts records, "" for an agent with no policy. It is the document
// the declarations report and diff are built on; nothing exports it yet.
func (a *Agent) declarationsDoc() declarations.Document {
	d := declarations.Document{Format: declarations.Format, TracksIdempotent: true, Digest: a.taintDigest(), Tools: []declarations.Tool{}}
	if a.taint != nil {
		d.Policy = &declarations.Policy{Gate: gateDoc(a.taint.Gate), Refuse: a.taint.Refuse, RequireProvenance: a.taint.RequireProvenance, SeparateApprovers: a.taint.SeparateApprovers}
	}
	for _, name := range slices.Sorted(maps.Keys(a.specs)) {
		s := a.specs[name]
		d.Tools = append(d.Tools, declarations.Tool{
			Name: name, Effect: tracked(s.Safety), OwnGate: gateDoc(effectiveGates(s, gateInputs{}).Own()),
			Untrusted: s.untrustedSource(), Trusted: s.Trusted, Exempt: s.TaintExempt, Caps: slices.Clone(s.Caps),
			Idempotent: s.Safety.Idempotent && !s.Safety.ReadOnly, RaisesSafety: s.RaisesSafety,
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

// Declarations returns the agent's taint and cap declarations as a document (format
// bide.declarations.v1, indented JSON): the taint policy, and every registered tool with whether it can have
// a side effect, its own gate, its Untrusted source, its TaintExempt reason, its RaisesSafety reason and its cap rules. Its digest
// member is the taint_policy_digest that the runs of this agent record in run:start ("" for an agent with no
// policy). Commit the document next to the code and read it with bide doctor -declarations, or compare two
// with bide doctor -diff: a raised limit, a removed or renamed cap rule, a removed Untrusted, a new
// TaintExempt or a new RaisesSafety is reported as weakened. It is experimental.
func (a *Agent) Declarations() ([]byte, error) {
	var buf bytes.Buffer
	if err := declarations.Encode(&buf, a.declarationsDoc()); err != nil {
		return nil, fmt.Errorf("agent: encode the declarations: %w", err)
	}
	return buf.Bytes(), nil
}
