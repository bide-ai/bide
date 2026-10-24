package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
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

// carriedMarks is the marks a context hands to the run it starts. strict says a later drive of that run
// must show the same list (a sub-agent's: the calling turn is fixed in the parent's journal); a session
// turn's list is read when its run starts and not held to afterwards.
type carriedMarks struct {
	list   []inheritedMark
	strict bool
}

// withCallMarks binds to ctx the marks the model turn that made the call was shown: the runs the call
// starts inherit them. Bound only for a run under taint, so a run that is not never passes marks.
func withCallMarks(ctx context.Context, marks []taintMark) context.Context {
	return context.WithValue(ctx, inheritedMarksKey{}, carriedMarks{list: canonicalInherited(marks), strict: true})
}

// withSessionMarks binds the marks a session turn starts with (the session's, from its journal).
func withSessionMarks(ctx context.Context, l []inheritedMark) context.Context {
	return context.WithValue(ctx, inheritedMarksKey{}, carriedMarks{list: l})
}

// callMarksFrom is the marks ctx carries for the runs a call starts, and whether it carries a list at
// all (a list is carried, possibly empty, exactly when the calling run is under taint).
func callMarksFrom(ctx context.Context) (carriedMarks, bool) {
	c, ok := ctx.Value(inheritedMarksKey{}).(carriedMarks)
	return c, ok
}

// startOfRecs is the ext of recs' run:start, as a RunStart that holds only it.
func startOfRecs(recs []Record) RunStart {
	for _, r := range recs {
		if r.Kind != StepValue || r.Name != runStartStep {
			continue
		}
		var s struct {
			Ext map[string]json.RawMessage `json:"ext"`
		}
		if json.Unmarshal(r.Result, &s) != nil {
			return RunStart{}
		}
		return RunStart{Ext: s.Ext}
	}
	return RunStart{}
}

// startInherited is the marks recs' run:start recorded as inherited.
func startInherited(recs []Record) []taintMark {
	return inheritedAsMarks(startOfRecs(recs).inheritedMarks())
}

// A sub-agent's answer carries the taint of the run that gave it: if the child ended with any mark, the
// parent's tool_result for the call carries one, so the parent's next turn is tainted. Without it a
// tainted child's answer would launder the content it read. The mark is dynamic (a tool's declaration is
// static), so the call hands it back through the context.

type resultMarkKey struct{}

// resultMark is the mark a call's result carries beyond its tool's declaration; the tool sets it while
// it runs, and the loop reads it when it records the result.
type resultMark struct {
	mu  sync.Mutex
	src string
}

// withResultMark returns ctx carrying a fresh resultMark, and the mark.
func withResultMark(ctx context.Context) (context.Context, *resultMark) {
	m := &resultMark{}
	return context.WithValue(ctx, resultMarkKey{}, m), m
}

func resultMarkFrom(ctx context.Context) *resultMark {
	m, _ := ctx.Value(resultMarkKey{}).(*resultMark)
	return m
}

func (m *resultMark) set(src string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.src == "" {
		m.src = src
	}
}

// applyTo gives the result's Safety the mark, unless its tool already declares a source.
func (m *resultMark) applyTo(s *Safety) {
	if m == nil || s == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.src != "" && s.Untrusted == "" {
		s.Untrusted = m.src
	}
}

// subAgentMarkSource is the source name of the mark a tainted sub-agent's answer carries: "subagent:" and
// the tool's name, cut on a rune boundary to fit a source name (maxProvenanceName), the same for every
// call of the name.
func subAgentMarkSource(tool string) string {
	s := "subagent:" + tool
	if len(s) <= maxProvenanceName {
		return s
	}
	cut := maxProvenanceName
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// endedTainted reports whether the run runID of this agent's store ended with a mark in its journal.
func (a *Agent) endedTainted(ctx context.Context, runID string) (bool, error) {
	l, _, err := a.endedMarks(ctx, runID)
	return len(l) > 0, err
}

// endedMarks is the marks the run runID of this agent's store ended with: inherited from above, or read by
// the run, one per source, and whether the run was under taint.
func (a *Agent) endedMarks(ctx context.Context, runID string) ([]inheritedMark, bool, error) {
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return nil, false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	declared := func(tool string) string {
		if s := a.specs[tool]; s != nil {
			return s.untrustedSource()
		}
		return ""
	}
	// A model record at the end shows the marks the whole journal holds.
	turns := foldTaint(append(slices.Clone(recs), Record{Kind: StepModel}), startInherited(recs), declared)
	under := startOfRecs(recs).taintDigest() != ""
	if len(turns) == 0 {
		return nil, under, nil
	}
	return canonicalInherited(turns[len(turns)-1].Marks), under, nil
}
