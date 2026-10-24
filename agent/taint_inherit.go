package agent

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// Marks that cross a run boundary (docs/design/untrusted-content-and-amount-caps.md, section 5.5). A
// run under taint passes the marks its requesting turn was shown to the runs that turn starts: a
// sub-agent's run records them in its run:start (ext "taint_inherited") and starts tainted, so its first
// turn is gated as the parent's would have been. The status (the policy digest) is passed on separately
// and unconditionally (taint_policy.go); the marks are passed exactly when the parent run is under taint.

// extTaintInherited is the key of run:start's ext member that holds the marks a run inherited.
const extTaintInherited = "taint_inherited"

// inheritedMark is one inherited mark as run:start records it: the declared source and where the parent
// run read it. It carries no journal position: the position is in the parent's journal, which the child
// need not share.
type inheritedMark struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
}

// parentRefPrefix marks a Ref as read by a run above this one.
const parentRefPrefix = "parent:"

// canonicalInherited is the marks a child records, given those its parent's turn was shown: one per
// source (the first in journal order names where it was read), sorted by source, so two derivations of
// the same marks give the same list. A mark already inherited keeps its Ref, so a chain of runs does not
// grow it.
func canonicalInherited(marks []taintMark) []inheritedMark {
	seen := map[string]bool{}
	var out []inheritedMark
	for _, m := range marks {
		if seen[m.Source] {
			continue
		}
		seen[m.Source] = true
		ref := m.Ref
		if !strings.HasPrefix(ref, parentRefPrefix) {
			ref = parentRefPrefix + ref
		}
		out = append(out, inheritedMark{Source: m.Source, Ref: ref})
	}
	slices.SortFunc(out, func(a, b inheritedMark) int { return strings.Compare(a.Source, b.Source) })
	return out
}

// marks is the list as fold input: marks of no journal position.
func inheritedAsMarks(l []inheritedMark) []taintMark {
	var out []taintMark
	for _, m := range l {
		out = append(out, taintMark{Source: m.Source, Ref: m.Ref})
	}
	return out
}

// withInheritedMarks returns s recording the marks l (a new Ext map: a RunStart copy shares its Ext).
// An empty list records nothing, so a run that inherits no marks is byte for byte as before.
func (s RunStart) withInheritedMarks(l []inheritedMark) RunStart {
	if len(l) == 0 {
		return s
	}
	ext := maps.Clone(s.Ext)
	if ext == nil {
		ext = map[string]json.RawMessage{}
	}
	b, _ := json.Marshal(l)
	ext[extTaintInherited] = b
	s.Ext = ext
	return s
}

// inheritedMarks is the list the start records, nil for none or a member that does not read.
func (s RunStart) inheritedMarks() []inheritedMark {
	raw, ok := s.Ext[extTaintInherited]
	if !ok {
		return nil
	}
	var l []inheritedMark
	if json.Unmarshal(raw, &l) != nil {
		return nil
	}
	return l
}

type inheritedMarksKey struct{}

// withCallMarks binds to ctx the marks the model turn that made the call was shown: the runs the call
// starts inherit them. Bound only for a run under taint, so a run that is not never passes marks.
func withCallMarks(ctx context.Context, marks []taintMark) context.Context {
	return context.WithValue(ctx, inheritedMarksKey{}, canonicalInherited(marks))
}

// callMarksFrom is the marks ctx carries for the runs a call starts, and whether it carries a list at
// all (a list is carried, possibly empty, exactly when the calling run is under taint).
func callMarksFrom(ctx context.Context) ([]inheritedMark, bool) {
	l, ok := ctx.Value(inheritedMarksKey{}).([]inheritedMark)
	return l, ok
}

// startInherited is the marks recs' run:start recorded as inherited.
func startInherited(recs []Record) []taintMark {
	for _, r := range recs {
		if r.Kind != StepValue || r.Name != runStartStep {
			continue
		}
		var s struct {
			Ext map[string]json.RawMessage `json:"ext"`
		}
		if json.Unmarshal(r.Result, &s) != nil {
			return nil
		}
		return inheritedAsMarks(RunStart{Ext: s.Ext}.inheritedMarks())
	}
	return nil
}
