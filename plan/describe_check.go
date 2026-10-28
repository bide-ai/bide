package plan

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/printable"
	"github.com/bide-ai/bide/internal/toolhook"
)

// Bounds on a RegistryDescription, which may come from a file: Validate refuses more. Each
// distinct type name becomes a type in the process (see describedType), so the count is bounded.
const (
	maxDescribedEntries = 20000
	maxDescribedName    = 512
	maxDescribedType    = 1024
	maxDescribedTypes   = 5000    // distinct type names in one description: a quarter of the process budget
	maxDescribedTypeLen = 1 << 20 // bytes of those names, likewise
	maxGateApprovers    = 256
	maxApproverID       = 256
	maxDescribedError   = 4096
	maxDescribedSources = 64 // untrusted sources of one block
)

// Validate reports whether d is a description this release reads: its version, no registration
// errors (a program whose Registry refused something does not load), every name and
// type name present, printable and within bounds, every kind and safety one of the known words,
// every gate a policy agent.ApprovalPolicy.Validate accepts, no name twice in a namespace, and a
// merge of arity two or three (the arities Registry registers). A Describe result is valid when its
// Errors is empty (Describe reports what Validate would refuse); a description read from a file need
// not be.
func (d RegistryDescription) Validate() error {
	if d.Version != DescriptionVersion && d.Version != descriptionVersionNoTaint {
		return fmt.Errorf("description version %d is not one this release reads (it reads %d and %d)", d.Version, descriptionVersionNoTaint, DescriptionVersion)
	}
	if n := len(d.Blocks) + len(d.Predicates) + len(d.Merges); n > maxDescribedEntries {
		return fmt.Errorf("description has %d entries, more than the %d this release reads", n, maxDescribedEntries)
	}
	if len(d.Errors) > 0 {
		// The program's own Registry refused something, so Load will not load it: there is nothing
		// sound to check a config against. The first error is shown as the reason.
		e := d.Errors[0]
		if !utf8.ValidString(e) || len(e) > maxDescribedError {
			e = "(not shown)"
		}
		return fmt.Errorf("the program's Registry reported %d error(s), so Load will not load it; the first: %s", len(d.Errors), e)
	}
	if err := d.checkTypeBudget(); err != nil {
		return err
	}
	if err := d.checkTaint(); err != nil {
		return err
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

// checkTaint holds the taint members of d to the version: a description of version 1 has none and says
// nothing about taint, one of version 2 carries the marker and every declaration is a valid one.
func (d RegistryDescription) checkTaint() error {
	if d.Version == descriptionVersionNoTaint {
		if d.TaintDeclared || d.TaintPolicy != nil {
			return fmt.Errorf("description version %d has no taint members, but this one carries taint_declared or taint_policy: they are version %d", d.Version, DescriptionVersion)
		}
		for _, b := range d.Blocks {
			if err := checkNoTaint("block", b.Name, b.Untrusted, b.Trusted, b.TaintExempt); err != nil {
				return err
			}
		}
		for _, m := range d.Merges {
			if err := checkNoTaint("merge", m.Name, m.Untrusted, m.Trusted, m.TaintExempt); err != nil {
				return err
			}
		}
		return nil
	}
	if !d.TaintDeclared {
		return fmt.Errorf("description version %d must set taint_declared: it carries the taint declarations, and without the marker an absent one would read as none", d.Version)
	}
	for _, b := range d.Blocks {
		if err := checkDescribedTaint("block", b.Name, b.Untrusted, b.Trusted, b.TaintExempt); err != nil {
			return err
		}
	}
	for _, m := range d.Merges {
		if err := checkDescribedTaint("merge", m.Name, m.Untrusted, m.Trusted, m.TaintExempt); err != nil {
			return err
		}
	}
	if p := d.TaintPolicy; p != nil {
		g, err := describedGate(p.Gate)
		if err != nil {
			return fmt.Errorf("taint policy: %w", err)
		}
		if err := toolhook.CheckTaintPolicy(agent.TaintPolicy{Gate: g, Refuse: p.Refuse, RequireProvenance: p.RequireProvenance, SeparateApprovers: p.SeparateApprovers}); err != nil {
			return fmt.Errorf("taint policy: %w", err)
		}
	}
	return nil
}

func checkNoTaint(what, name string, untrusted []string, trusted, exempt string) error {
	if len(untrusted) > 0 || trusted != "" || exempt != "" {
		return fmt.Errorf("%s %q declares taint, but a description of version %d has no taint members", what, name, descriptionVersionNoTaint)
	}
	return nil
}

// checkDescribedTaint holds a block's or merge's declarations to the rules of plan.Untrusted, plan.Trusted and
// plan.TaintExempt: valid names, sources sorted and each once, and not both untrusted and trusted.
func checkDescribedTaint(what, name string, untrusted []string, trusted, exempt string) error {
	if len(untrusted) > maxDescribedSources {
		return fmt.Errorf("%s %q declares %d untrusted sources, more than the %d this release reads", what, name, len(untrusted), maxDescribedSources)
	}
	for i, src := range untrusted {
		if err := toolhook.CheckProvenanceName("untrusted", src); err != nil {
			return fmt.Errorf("%s %q: %w", what, name, err)
		}
		if i > 0 && untrusted[i-1] >= src {
			return fmt.Errorf("%s %q: the untrusted sources are not sorted and distinct (%q, %q)", what, name, untrusted[i-1], src)
		}
	}
	if trusted != "" {
		if err := toolhook.CheckProvenanceName("trusted", trusted); err != nil {
			return fmt.Errorf("%s %q: %w", what, name, err)
		}
		if len(untrusted) > 0 {
			return fmt.Errorf("%s %q declares both untrusted sources and a trusted reason: a block declares one", what, name)
		}
	}
	if exempt != "" {
		if err := toolhook.CheckProvenanceName("taint_exempt", exempt); err != nil {
			return fmt.Errorf("%s %q: %w", what, name, err)
		}
	}
	return nil
}

// printableText reports whether s is valid UTF-8 without a character that does not print (the rule
// a config's names are held to, internal/printable).
// checkTypeBudget refuses a description with more distinct type names than a quarter of the process
// budget (see reserveStandIns), so a description Validate accepts can be checked in a fresh process
// and no one description can use the whole budget.
func (d RegistryDescription) checkTypeBudget() error {
	seen := map[string]bool{}
	bytes := 0
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			bytes += len(n)
		}
	}
	for _, b := range d.Blocks {
		add(b.In)
		add(b.Out)
	}
	for _, p := range d.Predicates {
		add(p.Switched)
	}
	for _, m := range d.Merges {
		for _, in := range m.Inputs {
			add(in)
		}
		add(m.Out)
	}
	if len(seen) > maxDescribedTypes || bytes > maxDescribedTypeLen {
		return fmt.Errorf("description has %d distinct type names (%d bytes), more than the %d names and %d bytes one description may hold", len(seen), bytes, maxDescribedTypes, maxDescribedTypeLen)
	}
	return nil
}

func printableText(s string) bool {
	_, bad := printable.Bad(s)
	return utf8.ValidString(s) && !bad
}

func checkDescribedName(what, name string, seen map[string]bool) error {
	switch {
	case name == "":
		return fmt.Errorf("a %s has no name", what)
	case len(name) > maxDescribedName || !printableText(name):
		return fmt.Errorf("a %s name is longer than %d bytes, or has a control or format character", what, maxDescribedName)
	case seen[name]:
		return fmt.Errorf("%s %q is described twice", what, name)
	}
	seen[name] = true
	return nil
}

func checkDescribedType(t, format string, a ...any) error {
	if t == "" || len(t) > maxDescribedType || !printableText(t) {
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
	if len(g.Approvers) > maxGateApprovers {
		return nil, fmt.Errorf("a gate has %d approvers, more than the %d this release reads", len(g.Approvers), maxGateApprovers)
	}
	for _, a := range g.Approvers {
		if len(a) > maxApproverID {
			return nil, fmt.Errorf("a gate approver id is longer than %d bytes", maxApproverID)
		}
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
// A process keeps one stand-in type for each distinct type name it has been given (reflect keeps
// them), so it admits at most 20000 names and 4 MiB of names in all, about 25 MB; past that the
// result is an error problem and the config is checked structurally, until the process restarts. One
// description counts against that budget with every distinct type name it holds, at most a quarter of
// it (RegistryDescription.Validate). A host that checks descriptions from sources it does not trust
// should do it in a short-lived process.
//
// Experimental: may change in any 0.x release.
func InspectConfigDescribed(data []byte, d RegistryDescription, opts ...InspectOption) ConfigReport {
	reg, err := registryFromDescription(d)
	if err != nil {
		r := inspectConfig(data, nil, nil)
		var first ConfigReport
		first.report(ConfigProblem{Message: "blocks description: " + err.Error()}, Finding{Kind: ProblemOther})
		r.Problems = append(first.Problems, r.Problems...)
		r.Findings = append(first.Findings, r.Findings...)
		return r
	}
	return inspectConfig(data, reg, opts)
}

// reflect.StructOf keeps every type it makes for the life of the process, so the names a process
// turns into stand-ins are bounded as a whole, not per description: a long-lived program that
// checks many descriptions stops, with an error, instead of growing. Measured, a stand-in costs
// about 550 bytes plus about 2.7 times its name, so the bounds hold the worst case to about 25 MB.
// Past them no description with a new type name can be checked until the process restarts.
var (
	maxStandInNames = 20000
	maxStandInBytes = 4 << 20
)

var standIns struct {
	sync.Mutex
	names map[string]bool
	bytes int
}

// reserveStandIns admits the type names of d to the process-wide set, or refuses the description
// when they would not fit. Names already admitted cost nothing.
func reserveStandIns(d RegistryDescription) error {
	var names []string
	add := func(n string) { names = append(names, n) }
	for _, b := range d.Blocks {
		add(b.In)
		add(b.Out)
	}
	for _, p := range d.Predicates {
		add(p.Switched)
	}
	for _, m := range d.Merges {
		for _, in := range m.Inputs {
			add(in)
		}
		add(m.Out)
	}
	standIns.Lock()
	defer standIns.Unlock()
	if standIns.names == nil {
		standIns.names = map[string]bool{}
	}
	n, size := 0, 0
	seen := map[string]bool{}
	for _, name := range names {
		if !standIns.names[name] && !seen[name] {
			seen[name] = true
			n++
			size += len(name)
		}
	}
	if len(standIns.names)+n > maxStandInNames || standIns.bytes+size > maxStandInBytes {
		return errors.New("this process has checked too many distinct type names to check more: restart the process to check new type names (InspectConfigDescribed keeps a type for each name for the life of the process)")
	}
	for name := range seen {
		standIns.names[name] = true
	}
	standIns.bytes += size
	return nil
}

// registryFromDescription builds a Registry that checks like the one d describes and cannot run:
// its blocks have no behavior. Each distinct type name is one type (describedType), so reflect
// identity is name equality.
func registryFromDescription(d RegistryDescription) (*Registry, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if err := reserveStandIns(d); err != nil {
		return nil, err
	}
	reg := NewRegistry()
	reg.inert = true
	reg.taintDeclared = d.TaintDeclared
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
		reg.blocks[b.Name] = &regBlock{kind: kind, inType: describedType(b.In), outType: describedType(b.Out), safety: safety, approval: gate,
			taint: nodeTaint{untrusted: slices.Clone(b.Untrusted), trusted: b.Trusted, exempt: b.TaintExempt}}
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
		reg.merges[m.Name] = &regMerge{approval: gate, safety: safety, arity: len(in), inTypes: in, outType: describedType(m.Out),
			taint: nodeTaint{untrusted: slices.Clone(m.Untrusted), trusted: m.Trusted, exempt: m.TaintExempt}}
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
		Type: reflect.TypeFor[describedMarker](),
		Tag:  reflect.StructTag("name:" + strconv.Quote(name)),
	}})
}

// describedMarker is the type of the stand-in's one field. It is unexported, so no other package can
// write a type that passes for a stand-in.
type describedMarker struct{}

// describedTypeName reports the name of a type made by describedType.
func describedTypeName(t reflect.Type) (string, bool) {
	if t.Kind() != reflect.Struct || t.Name() != "" || t.NumField() != 1 {
		return "", false
	}
	f := t.Field(0)
	if f.Name != describedField || f.PkgPath != "" || f.Type != reflect.TypeFor[describedMarker]() {
		return "", false
	}
	name, ok := f.Tag.Lookup("name")
	return name, ok
}
