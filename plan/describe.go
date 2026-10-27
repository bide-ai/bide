package plan

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/toolhook"
)

// DescriptionVersion is the Version of a RegistryDescription this release writes. It reads this version and
// descriptionVersionNoTaint.
//
// Version 2 adds the taint declarations (docs/design/flow-taint.md, section 7): the Untrusted, Trusted and
// TaintExempt members of a block, the TaintPolicy member, and TaintDeclared, the marker that says the
// description carries them.
const DescriptionVersion = 2

// descriptionVersionNoTaint is the version before the taint members. A description of it is read, and says
// nothing about taint: it carries no marker, so an absent declaration is not read as "none".
const descriptionVersionNoTaint = 1

// RegistryDescription is what a Registry has registered, as data: names, kinds, type names, the
// safety and the approval gate each block declares in Go. It holds no functions and no reflect
// values, so it can be written to a file, shown by an editor, or checked against a config without
// the program that registered the blocks. Prompts and output schemas are left out.
//
// Experimental: the description and its JSON form may change in any 0.x release.
type RegistryDescription struct {
	// Version is DescriptionVersion (a description of version 1 is read too, and carries no taint
	// declarations).
	Version int `json:"version"`
	// TaintDeclared says the description carries the taint declarations of its blocks and merges
	// (Untrusted, Trusted, TaintExempt), so a block without them declares none. Describe sets it. A
	// description without it (version 1, or written by hand) says nothing about taint: a reader shows
	// "taint not known", never "no taint declared".
	TaintDeclared bool `json:"taint_declared,omitempty"`
	// TaintPolicy is the flow's taint policy, nil when none is given. A Registry does not hold the policy
	// (it is the Builder's: Builder.WithTaintPolicy), so Describe leaves it nil and a host that has one
	// sets it, with DescribeTaintPolicy. With TaintDeclared, nil means no policy was given to the description,
	// which is not a statement that the flow runs without one.
	TaintPolicy *TaintPolicyDescription `json:"taint_policy,omitempty"`
	// Blocks are the Step, Tool and Model blocks, by name.
	Blocks []BlockDescription `json:"blocks"`
	// Predicates are the Switch predicates, by name.
	Predicates []PredicateDescription `json:"predicates"`
	// Merges are the fan-in merge blocks, by name. They are a namespace of their own: a name may
	// be both a block and a merge.
	Merges []MergeDescription `json:"merges"`
	// Errors lists the registrations the Registry refused or Build would refuse, so a description
	// with any is of a program Load will not load: a duplicate name or a tool the agent refuses (in
	// registration order; a duplicate never replaces the first registration, so the lists above hold
	// the first), then each block or merge whose approval gate Validate refuses, by name.
	Errors []string `json:"errors,omitempty"`
}

// BlockDescription is one registered Step, Tool or Model block.
type BlockDescription struct {
	Name string `json:"name"`
	// Kind is "step", "tool" or "model".
	Kind string `json:"kind"`
	// In and Out are the block's input and output types, spelled as the topology digest spells
	// them (a named type is its package path, '.', and its name).
	In  string `json:"in"`
	Out string `json:"out"`
	// Safety is the retry-on-resume class the block declares: "readonly", "idempotent" or
	// "side_effect", the words a config's "safety" uses. A config may keep or lower it.
	Safety string `json:"safety"`
	// Gate is the approval gate the block declares in Go (a wrapped tool's own, or the Approval
	// option), nil when it declares none.
	Gate *GateDescription `json:"gate,omitempty"`
	// Untrusted are the sources the block declares for its results (plan.Untrusted, a tool's Untrusted),
	// sorted, each once. Present only in a description with TaintDeclared.
	Untrusted []string `json:"untrusted,omitempty"`
	// Trusted is the reason the block declares its results the host's own (plan.Trusted).
	Trusted string `json:"trusted,omitempty"`
	// TaintExempt is the reason the block needs no escalation gate under taint (plan.TaintExempt).
	TaintExempt string `json:"taint_exempt,omitempty"`
}

// TaintPolicyDescription is a flow's taint policy as data: the escalation gate a tainted node's side effects
// need, or Refuse, and whether provenance is required. It is the agent.TaintPolicy of Builder.WithTaintPolicy
// (see DescribeTaintPolicy).
//
// Experimental: may change in any 0.x release.
type TaintPolicyDescription struct {
	// Gate is the escalation gate; nil when the policy refuses instead.
	Gate *GateDescription `json:"gate,omitempty"`
	// Refuse: a tainted node's side effect is refused, not gated.
	Refuse bool `json:"refuse,omitempty"`
	// RequireProvenance: every node declares a provenance.
	RequireProvenance bool `json:"require_provenance,omitempty"`
}

// DescribeTaintPolicy is p as a TaintPolicyDescription, for RegistryDescription.TaintPolicy, or an error
// if p is not a policy Builder.WithTaintPolicy accepts.
//
// Experimental: may change in any 0.x release.
func DescribeTaintPolicy(p agent.TaintPolicy) (*TaintPolicyDescription, error) {
	if err := toolhook.CheckTaintPolicy(p); err != nil {
		return nil, err
	}
	return &TaintPolicyDescription{Gate: describeGate(p.Gate), Refuse: p.Refuse, RequireProvenance: p.RequireProvenance}, nil
}

// GateDescription is an approval gate declared in Go.
type GateDescription struct {
	// Single is the one-decision gate agent.SingleApproval returns; Need is then 0 and Approvers empty, as
	// the topology carries it.
	Single bool `json:"single,omitempty"`
	// Need, Approvers and ExcludePrincipal are the agent.ApprovalPolicy fields of the same names.
	Need             int      `json:"need"`
	Approvers        []string `json:"approvers,omitempty"`
	ExcludePrincipal bool     `json:"exclude_principal,omitempty"`
	// Timeout is the gate's deadline as a Go duration ("48h0m0s"), empty for none.
	Timeout string `json:"timeout,omitempty"`
}

// PredicateDescription is one registered Switch predicate.
type PredicateDescription struct {
	Name string `json:"name"`
	// Switched is the type of the value the predicate tests: the output type of the node a Switch
	// over it must follow.
	Switched string `json:"switched"`
}

// MergeDescription is one registered fan-in merge block.
type MergeDescription struct {
	Name string `json:"name"`
	// Inputs are the types of the merge's ordered inputs; its arity is len(Inputs).
	Inputs []string `json:"inputs"`
	// Out is the merged result type.
	Out string `json:"out"`
	// Safety and Gate are as for a BlockDescription.
	Safety string           `json:"safety"`
	Gate   *GateDescription `json:"gate,omitempty"`
	// Untrusted, Trusted and TaintExempt are as for a BlockDescription.
	Untrusted   []string `json:"untrusted,omitempty"`
	Trusted     string   `json:"trusted,omitempty"`
	TaintExempt string   `json:"taint_exempt,omitempty"`
}

// Describe returns what r has registered (see RegistryDescription). It is read-only and sorted by
// name, so registrations of distinct names give the same description whatever order they were made in
// (with a duplicate name the first registration wins, and Errors follows registration order).
// Like Register, it is not safe to call while another goroutine registers.
//
// Experimental: part of the editor's typed mode, and may change in any 0.x release.
func (r *Registry) Describe() RegistryDescription {
	d := RegistryDescription{
		Version:       DescriptionVersion,
		TaintDeclared: true,
		Blocks:        []BlockDescription{},
		Predicates:    []PredicateDescription{},
		Merges:        []MergeDescription{},
	}
	var gateErrs []string
	for name, b := range r.blocks {
		if w := gateProblem(b.approval); w != "" {
			gateErrs = append(gateErrs, fmt.Sprintf("block %q: approval gate: %s", name, w))
		}
		d.Blocks = append(d.Blocks, BlockDescription{
			Name:      name,
			Kind:      blockKindName(b.kind),
			In:        canonicalType(b.inType),
			Out:       canonicalType(b.outType),
			Safety:    safetyWord(b.safety),
			Gate:      describeGate(b.approval),
			Untrusted: slices.Clone(b.taint.untrusted), Trusted: b.taint.trusted, TaintExempt: b.taint.exempt,
		})
	}
	for name, p := range r.preds {
		d.Predicates = append(d.Predicates, PredicateDescription{Name: name, Switched: canonicalType(p.mType)})
	}
	for name, m := range r.merges {
		if w := gateProblem(m.approval); w != "" {
			gateErrs = append(gateErrs, fmt.Sprintf("merge %q: approval gate: %s", name, w))
		}
		in := make([]string, len(m.inTypes))
		for i, t := range m.inTypes {
			in[i] = canonicalType(t)
		}
		d.Merges = append(d.Merges, MergeDescription{
			Name:      name,
			Inputs:    in,
			Out:       canonicalType(m.outType),
			Safety:    safetyWord(m.safety),
			Gate:      describeGate(m.approval),
			Untrusted: slices.Clone(m.taint.untrusted), Trusted: m.taint.trusted, TaintExempt: m.taint.exempt,
		})
	}
	sort.Slice(d.Blocks, func(i, j int) bool { return d.Blocks[i].Name < d.Blocks[j].Name })
	sort.Slice(d.Predicates, func(i, j int) bool { return d.Predicates[i].Name < d.Predicates[j].Name })
	sort.Slice(d.Merges, func(i, j int) bool { return d.Merges[i].Name < d.Merges[j].Name })
	for _, err := range r.errs {
		d.Errors = append(d.Errors, err.Error())
	}
	sort.Strings(gateErrs)
	d.Errors = append(d.Errors, gateErrs...)
	if len(d.Errors) == 0 {
		// A registration the Registry accepts can still be one a reader refuses (a name or a type
		// name over the bounds): say so here, so a description without Errors is always valid.
		if err := d.Validate(); err != nil {
			d.Errors = append(d.Errors, "description: "+err.Error())
		}
	}
	return d
}

func blockKindName(k nodeKind) string {
	switch k {
	case kindTool:
		return "tool"
	case kindModel:
		return "model"
	}
	return "step"
}

// safetyWord is the config word for a block's declared safety (ReadOnly wins over Idempotent, as
// safetyClass has it).
func safetyWord(s agent.Safety) string {
	switch {
	case s.ReadOnly:
		return "readonly"
	case s.Idempotent:
		return "idempotent"
	}
	return "side_effect"
}

// describeGate is p as the topology and the digest carry it (topologyGate), with the deadline as a
// Go duration. It describes the gate as declared: one that agent.ApprovalPolicy.Validate refuses is
// still described, and reported by gateProblem.
func describeGate(p *agent.ApprovalPolicy) *GateDescription {
	tg := topologyGate(p)
	if tg == nil {
		return nil
	}
	g := &GateDescription{Single: tg.Single, Need: tg.Need, Approvers: tg.Approvers, ExcludePrincipal: tg.ExcludePrincipal}
	if tg.TimeoutMS != 0 {
		g.Timeout = (time.Duration(tg.TimeoutMS) * time.Millisecond).String()
	}
	return g
}

// gateProblem is why Build would refuse p, empty when it would not: a gate a registration accepts
// can still be one Validate refuses (a literal {Need: 1} with no approvers, a negative or
// fractional-millisecond deadline).
func gateProblem(p *agent.ApprovalPolicy) string {
	if p == nil {
		return ""
	}
	if err := p.Validate(); err != nil {
		return err.Error()
	}
	return ""
}
