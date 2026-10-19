package plan

import (
	"sort"
	"time"

	"github.com/bide-ai/bide/agent"
)

// DescriptionVersion is the Version of a RegistryDescription this release writes and reads.
const DescriptionVersion = 1

// RegistryDescription is what a Registry has registered, as data: names, kinds, type names, the
// safety and the approval gate each block declares in Go. It holds no functions and no reflect
// values, so it can be written to a file, shown by an editor, or checked against a config without
// the program that registered the blocks. Prompts and output schemas are left out.
//
// Experimental: the description and its JSON form may change in any 0.x release.
type RegistryDescription struct {
	// Version is DescriptionVersion.
	Version int `json:"version"`
	// Blocks are the Step, Tool and Model blocks, by name.
	Blocks []BlockDescription `json:"blocks"`
	// Predicates are the Switch predicates, by name.
	Predicates []PredicateDescription `json:"predicates"`
	// Merges are the fan-in merge blocks, by name. They are a namespace of their own: a name may
	// be both a block and a merge.
	Merges []MergeDescription `json:"merges"`
	// Errors are the registration errors Load would report (a duplicate name, a tool the agent
	// refuses), in registration order. A duplicate never replaces the first registration, so the
	// lists above hold the first.
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
}

// GateDescription is an approval gate declared in Go.
type GateDescription struct {
	// Single is the one-decision gate agent.SingleApproval returns; Need and Approvers are then
	// 1 and empty.
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
}

// Describe returns what r has registered (see RegistryDescription). It is read-only and sorted by
// name, so the same registrations give the same description whatever order they were made in.
// Like Register, it is not safe to call while another goroutine registers.
//
// Experimental: part of the editor's typed mode, and may change in any 0.x release.
func (r *Registry) Describe() RegistryDescription {
	d := RegistryDescription{
		Version:    DescriptionVersion,
		Blocks:     []BlockDescription{},
		Predicates: []PredicateDescription{},
		Merges:     []MergeDescription{},
	}
	for name, b := range r.blocks {
		d.Blocks = append(d.Blocks, BlockDescription{
			Name:   name,
			Kind:   blockKindName(b.kind),
			In:     canonicalType(b.inType),
			Out:    canonicalType(b.outType),
			Safety: safetyWord(b.safety),
			Gate:   describeGate(b.approval),
		})
	}
	for name, p := range r.preds {
		d.Predicates = append(d.Predicates, PredicateDescription{Name: name, Switched: canonicalType(p.mType)})
	}
	for name, m := range r.merges {
		in := make([]string, len(m.inTypes))
		for i, t := range m.inTypes {
			in[i] = canonicalType(t)
		}
		d.Merges = append(d.Merges, MergeDescription{
			Name:   name,
			Inputs: in,
			Out:    canonicalType(m.outType),
			Safety: safetyWord(m.safety),
			Gate:   describeGate(m.approval),
		})
	}
	sort.Slice(d.Blocks, func(i, j int) bool { return d.Blocks[i].Name < d.Blocks[j].Name })
	sort.Slice(d.Predicates, func(i, j int) bool { return d.Predicates[i].Name < d.Predicates[j].Name })
	sort.Slice(d.Merges, func(i, j int) bool { return d.Merges[i].Name < d.Merges[j].Name })
	for _, err := range r.errs {
		d.Errors = append(d.Errors, err.Error())
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

func describeGate(p *agent.ApprovalPolicy) *GateDescription {
	if p == nil {
		return nil
	}
	g := &GateDescription{
		Single:           len(p.Approvers) == 0 && p.Need == 1, // an m-of-n gate with no approvers is refused (ErrConfig)
		Need:             p.Need,
		Approvers:        append([]string(nil), p.Approvers...),
		ExcludePrincipal: p.ExcludePrincipal,
	}
	if p.Timeout > 0 {
		g.Timeout = p.Timeout.Round(time.Millisecond).String()
	}
	return g
}
