package plan

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
)

// Bounds on a RegistryDescription, which may come from a file: Validate refuses more. Each
// distinct type name becomes a type in the process (see describedType), so the count is bounded.
const (
	maxDescribedEntries = 20000
	maxDescribedName    = 512
	maxDescribedType    = 1024
	maxDescribedErrors  = 1000
	maxDescribedError   = 4096
)

// Validate reports whether d is a description this release reads: its version, every name and
// type name present, printable and within bounds, every kind and safety one of the known words,
// every gate a policy agent.ApprovalPolicy.Validate accepts, no name twice in a namespace, and a
// merge of arity two or three (the arities Registry registers). A Describe result is always
// valid; a description read from a file need not be.
func (d RegistryDescription) Validate() error {
	if d.Version != DescriptionVersion {
		return fmt.Errorf("description version %d is not one this release reads (it reads %d)", d.Version, DescriptionVersion)
	}
	if n := len(d.Blocks) + len(d.Predicates) + len(d.Merges); n > maxDescribedEntries {
		return fmt.Errorf("description has %d entries, more than the %d this release reads", n, maxDescribedEntries)
	}
	if len(d.Errors) > maxDescribedErrors {
		return fmt.Errorf("description has %d errors, more than the %d this release reads", len(d.Errors), maxDescribedErrors)
	}
	for _, e := range d.Errors {
		if !utf8.ValidString(e) || len(e) > maxDescribedError {
			return errors.New("description has an error text that is not valid UTF-8 or is too long")
		}
	}
	seen := map[string]bool{}
	for _, b := range d.Blocks {
		if err := checkDescribedName("block", b.Name, seen); err != nil {
			return err
		}
		switch b.Kind {
		case "step", "tool", "model":
		default:
			return fmt.Errorf("block %q has kind %q; want \"step\", \"tool\" or \"model\"", b.Name, b.Kind)
		}
		if _, err := describedSafety(b.Safety); err != nil {
			return fmt.Errorf("block %q: %w", b.Name, err)
		}
		if err := checkDescribedType(b.In, "block %q input", b.Name); err != nil {
			return err
		}
		if err := checkDescribedType(b.Out, "block %q output", b.Name); err != nil {
			return err
		}
		if _, err := describedGate(b.Gate); err != nil {
			return fmt.Errorf("block %q: %w", b.Name, err)
		}
	}
	seen = map[string]bool{}
	for _, p := range d.Predicates {
		if err := checkDescribedName("predicate", p.Name, seen); err != nil {
			return err
		}
		if err := checkDescribedType(p.Switched, "predicate %q switched type", p.Name); err != nil {
			return err
		}
	}
	seen = map[string]bool{}
	for _, m := range d.Merges {
		if err := checkDescribedName("merge", m.Name, seen); err != nil {
			return err
		}
		if len(m.Inputs) < 2 || len(m.Inputs) > 3 {
			return fmt.Errorf("merge %q has %d inputs; a merge takes two or three", m.Name, len(m.Inputs))
		}
		for i, in := range m.Inputs {
			if err := checkDescribedType(in, "merge %q input %d", m.Name, i); err != nil {
				return err
			}
		}
		if err := checkDescribedType(m.Out, "merge %q output", m.Name); err != nil {
			return err
		}
		if _, err := describedSafety(m.Safety); err != nil {
			return fmt.Errorf("merge %q: %w", m.Name, err)
		}
		if _, err := describedGate(m.Gate); err != nil {
			return fmt.Errorf("merge %q: %w", m.Name, err)
		}
	}
	return nil
}

func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == unicode.ReplacementChar || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func checkDescribedName(what, name string, seen map[string]bool) error {
	switch {
	case name == "":
		return fmt.Errorf("a %s has no name", what)
	case len(name) > maxDescribedName || !printable(name):
		return fmt.Errorf("a %s name is longer than %d bytes, or has a control or format character", what, maxDescribedName)
	case seen[name]:
		return fmt.Errorf("%s %q is described twice", what, name)
	}
	seen[name] = true
	return nil
}

func checkDescribedType(t, format string, a ...any) error {
	if t == "" || len(t) > maxDescribedType || !printable(t) {
		return fmt.Errorf(format+" is empty, longer than %d bytes, or has a control or format character", append(a, maxDescribedType)...)
	}
	return nil
}

func describedSafety(word string) (agent.Safety, error) {
	switch word {
	case "readonly":
		return agent.Safety{ReadOnly: true}, nil
	case "idempotent":
		return agent.Safety{Idempotent: true}, nil
	case "side_effect":
		return agent.Safety{}, nil
	}
	return agent.Safety{}, fmt.Errorf("safety %q is not \"readonly\", \"idempotent\" or \"side_effect\"", word)
}

// describedGate is the policy a GateDescription stands for, or an error if it is not one.
func describedGate(g *GateDescription) (*agent.ApprovalPolicy, error) {
	if g == nil {
		return nil, nil
	}
	var timeout time.Duration
	if g.Timeout != "" {
		d, err := time.ParseDuration(g.Timeout)
		if err != nil || d <= 0 || d%time.Millisecond != 0 {
			return nil, fmt.Errorf("gate timeout %q is not a positive whole number of milliseconds", g.Timeout)
		}
		timeout = d
	}
	if g.Single {
		if len(g.Approvers) != 0 || g.ExcludePrincipal || g.Need > 1 {
			return nil, errors.New("a single-decision gate has no approvers, no separation of duties and at most one decision")
		}
		p := agent.SingleApproval()
		p.Timeout = timeout
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("gate: %w", err)
		}
		return p, nil
	}
	p := &agent.ApprovalPolicy{Need: g.Need, Approvers: append([]string(nil), g.Approvers...), ExcludePrincipal: g.ExcludePrincipal, Timeout: timeout}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("gate: %w", err)
	}
	return p, nil
}

// InspectConfigDescribed is InspectConfig for a program's registrations known only as a
// description: it runs the same checks (the same code) on a Registry built from d, so blocks,
// predicates, merges, types, safety, gates and the digest are checked without the program.
//
// What differs from InspectConfig with the real Registry:
//   - Types are told apart by name. Two distinct types spelled the same (a function-local type of
//     the same name in two functions) are one type here, as the digest already cannot tell them
//     apart; Load, comparing reflect types, would refuse an edge between them.
//   - A type is written with its package path wherever InspectConfig writes reflect's short form
//     (in messages, and in Topology's In and Out), because that is what d holds.
//   - d is data from a file and may lie; the answer is only as good as the description. A d that
//     is not valid (RegistryDescription.Validate) is reported as an error problem and the config
//     is checked structurally.
//
// Experimental: may change in any 0.x release.
func InspectConfigDescribed(data []byte, d RegistryDescription) ConfigReport {
	reg, err := registryFromDescription(d)
	if err != nil {
		r := inspectConfig(data, nil)
		r.Problems = append([]ConfigProblem{{Message: "blocks description: " + err.Error(), Severity: "error"}}, r.Problems...)
		return r
	}
	return inspectConfig(data, reg)
}

// registryFromDescription builds a Registry that checks like the one d describes and cannot run:
// its blocks have no behavior. Each distinct type name is one type (describedType), so reflect
// identity is name equality.
func registryFromDescription(d RegistryDescription) (*Registry, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	reg := NewRegistry()
	for _, b := range d.Blocks {
		kind := kindStep
		switch b.Kind {
		case "tool":
			kind = kindTool
		case "model":
			kind = kindModel
		}
		safety, _ := describedSafety(b.Safety)
		gate, _ := describedGate(b.Gate)
		reg.blocks[b.Name] = &regBlock{kind: kind, inType: describedType(b.In), outType: describedType(b.Out), safety: safety, approval: gate}
	}
	for _, p := range d.Predicates {
		reg.preds[p.Name] = &regPred{mType: describedType(p.Switched)}
	}
	for _, m := range d.Merges {
		in := make([]reflect.Type, len(m.Inputs))
		for i, t := range m.Inputs {
			in[i] = describedType(t)
		}
		safety, _ := describedSafety(m.Safety)
		gate, _ := describedGate(m.Gate)
		reg.merges[m.Name] = &regMerge{approval: gate, safety: safety, arity: len(in), inTypes: in, outType: describedType(m.Out)}
	}
	for _, e := range d.Errors {
		reg.errs = append(reg.errs, errors.New(e))
	}
	return reg, nil
}

// describedField is the one field of the struct type that stands for a named type.
const describedField = "BideDescribedType"

// describedType is the type that stands for the type spelled name in a description: an unnamed
// struct with one field whose tag carries name. reflect.StructOf returns the same type for the
// same fields, so two uses of a name are one type and two names are two. canonicalType and
// typeName write such a type as its name. The type is never instantiated.
func describedType(name string) reflect.Type {
	return reflect.StructOf([]reflect.StructField{{
		Name: describedField,
		Type: reflect.TypeFor[struct{}](),
		Tag:  reflect.StructTag("name:" + strconv.Quote(name)),
	}})
}

// describedTypeName reports the name of a type made by describedType.
func describedTypeName(t reflect.Type) (string, bool) {
	if t.Kind() != reflect.Struct || t.Name() != "" || t.NumField() != 1 {
		return "", false
	}
	f := t.Field(0)
	if f.Name != describedField || f.PkgPath != "" {
		return "", false
	}
	name, ok := f.Tag.Lookup("name")
	return name, ok
}
