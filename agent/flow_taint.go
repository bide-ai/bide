package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	escsubject "github.com/bide-ai/bide/internal/escalation"
	"github.com/bide-ai/bide/internal/strictjson"
)

// The pure parts of taint in a plan flow (docs/design/flow-taint.md, A2 and A3): the declarations a flow run
// starts under, recorded in run:start and compared exactly on every drive; the read set a node has at a point
// in the run's journal; the requirement that read set makes of a node; and the taint-mark record a node's
// child leaves. Package plan reaches it through the hooks in flow_taint_hooks.go and flow_taint_sink.go.

// flowTaintDeclarationsFormat tags the declarations a flow run records in run:start.
const flowTaintDeclarationsFormat = "bide.flow-taint-declarations.v1"

// extTaintDeclarations is the key of run:start's ext member that holds the declarations in full. The digest
// of them is in extTaintDigest, as for an agent run, and the presence of that digest puts the run under taint.
const extTaintDeclarations = "taint_declarations"

// flowNodeTaint is what a node declares: the sources of untrusted content its results carry (sorted, each
// once), the reason they are the host's own, or the reason the node needs no escalation gate.
type flowNodeTaint struct {
	Untrusted []string `json:"untrusted,omitempty"`
	Trusted   string   `json:"trusted,omitempty"`
	Exempt    string   `json:"exempt,omitempty"`
}

func (n flowNodeTaint) isZero() bool {
	return len(n.Untrusted) == 0 && n.Trusted == "" && n.Exempt == ""
}

// flowTaintPolicy is the flow's taint policy: the escalation Gate or Refuse, and RequireProvenance.
type flowTaintPolicy struct {
	Gate              *ApprovalPolicy `json:"gate,omitempty"`
	Refuse            bool            `json:"refuse,omitempty"`
	RequireProvenance bool            `json:"require_provenance,omitempty"`
	// SeparateApprovers: the escalation gate's approvers exclude those the node's own gate counted.
	SeparateApprovers bool `json:"separate_approvers,omitempty"`
}

func (p flowTaintPolicy) isZero() bool { return p.Gate == nil && !p.Refuse && !p.RequireProvenance }

// flowTaintDecls are the declarations a flow run starts under: the policy and, by node name, what each node
// declares. A node that declares nothing is absent.
type flowTaintDecls struct {
	Format string                   `json:"format"`
	Policy flowTaintPolicy          `json:"policy"`
	Nodes  map[string]flowNodeTaint `json:"nodes,omitempty"`
}

// normalized is d in the form it is hashed and recorded in: the format set, each node's sources sorted and
// without repeats, nodes that declare nothing dropped, and nothing shared with d.
func (d flowTaintDecls) normalized() flowTaintDecls {
	out := flowTaintDecls{Format: flowTaintDeclarationsFormat, Policy: d.Policy}
	out.Policy.Gate = d.Policy.Gate.Clone()
	if g := out.Policy.Gate; g != nil {
		// A gate's approvers are a set: their order and repetition are not part of the declarations, so a
		// reordered list does not change the digest or strand a run in flight.
		g.Approvers = slices.Compact(slices.Sorted(slices.Values(g.Approvers)))
	}
	for name, n := range d.Nodes {
		n.Untrusted = slices.Compact(slices.Sorted(slices.Values(n.Untrusted)))
		if n.isZero() {
			continue
		}
		if out.Nodes == nil {
			out.Nodes = map[string]flowNodeTaint{}
		}
		out.Nodes[name] = n
	}
	return out
}

// digest is the SHA-256 in hex of the normalized declarations (maps are encoded with sorted keys), the
// digest run:start records and a resume compares.
func (d flowTaintDecls) digest() string {
	b, err := json.Marshal(d.normalized())
	if err != nil { // strings and a policy always marshal
		panic(fmt.Sprintf("agent: encode the flow taint declarations: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// withFlowTaint returns s recording the declarations d and their digest (a new Ext map: a RunStart copy
// shares its Ext). A run is under taint by its policy: declarations with no policy record nothing, and s is
// returned as it is.
func (s RunStart) withFlowTaint(d flowTaintDecls) RunStart {
	if d.Policy.isZero() {
		return s
	}
	n := d.normalized()
	ext := maps.Clone(s.Ext)
	if ext == nil {
		ext = map[string]json.RawMessage{}
	}
	dig, _ := json.Marshal(d.digest())
	body, err := json.Marshal(n)
	if err != nil {
		panic(fmt.Sprintf("agent: encode the flow taint declarations: %v", err))
	}
	ext[extTaintDigest], ext[extTaintDeclarations] = dig, body
	s.Ext = ext
	return s
}

// inheritedOnly reports whether s records the taint digest of a run started inside a tainted run with no
// declarations of its own: a flow with no policy that was started by a tool of a tainted agent or by the body
// of a tainted node. The digest is the parent's status. (A digest that is not a string is not that shape.)
func (s RunStart) inheritedOnly() bool {
	raw, hasDigest := s.Ext[extTaintDigest]
	if _, hasDecls := s.Ext[extTaintDeclarations]; !hasDigest || hasDecls {
		return false
	}
	var d string
	return json.Unmarshal(raw, &d) == nil && d != ""
}

// flowTaintDecls reads the declarations s records. ok is false when it records none (or only an inherited
// digest, see inheritedOnly). A start that records a digest that is not a string, declarations and no digest, declarations that do not read strictly or are of
// another format, or declarations that do not hash to the recorded digest, is an ErrStorage refusal: the
// engine cannot recompute a requirement from it, and no drive runs a node on it.
func (s RunStart) flowTaintDecls() (d flowTaintDecls, ok bool, err error) {
	rawDigest, hasDigest := s.Ext[extTaintDigest]
	rawDecls, hasDecls := s.Ext[extTaintDeclarations]
	if !hasDigest && !hasDecls {
		return flowTaintDecls{}, false, nil
	}
	switch {
	case hasDigest && !hasDecls:
		// The digest of a run started inside a tainted run, with no declarations of its own to compare (see
		// inheritedOnly).
		return flowTaintDecls{}, false, nil
	case hasDecls && !hasDigest:
		return flowTaintDecls{}, false, fmt.Errorf("agent: run:start records %s without %s: %w", extTaintDeclarations, extTaintDigest, ErrStorage)
	}
	var recorded string
	if err := json.Unmarshal(rawDigest, &recorded); err != nil {
		return flowTaintDecls{}, false, fmt.Errorf("agent: run:start's %s is not a string: %w", extTaintDigest, ErrStorage)
	}
	if err := strictjson.Unmarshal(rawDecls, &d, nil); err != nil || d.Format != flowTaintDeclarationsFormat {
		return flowTaintDecls{}, false, fmt.Errorf("agent: run:start's %s does not read as a %s record: %w", extTaintDeclarations, flowTaintDeclarationsFormat, ErrStorage)
	}
	if d.Policy.isZero() {
		return flowTaintDecls{}, false, fmt.Errorf("agent: run:start's %s records no policy, and a run is under taint by its policy: %w", extTaintDeclarations, ErrStorage)
	}
	if got := d.digest(); got != recorded {
		return flowTaintDecls{}, false, fmt.Errorf("agent: run:start's %s hashes to %.12s, not the recorded %.12s: %w", extTaintDeclarations, got, recorded, ErrStorage)
	}
	return d.normalized(), true, nil
}

// flowTaintStanding says where a drive of a flow stands against the declarations its run started under.
type flowTaintStanding int

const (
	// flowNotUnderTaint: the run started with no taint declarations, so it is never gated or refused for
	// taint, whatever the flow declares now (a run is not retro-gated).
	flowNotUnderTaint flowTaintStanding = iota + 1
	// flowUnderTaint: the run started under declarations, and this flow declares exactly those.
	flowUnderTaint
	// flowPolicyless: the run started under declarations, or under taint it inherited, and this flow has no
	// taint policy to enforce it: it must not run a node that can have a side effect (ErrConfig); a flow of
	// read-only nodes resumes.
	flowPolicyless
)

// flowTaintCheck compares a drive of a flow with the declarations the run's start records. current is the
// flow's declarations now, nil for a flow with none. The comparison is exact: any difference in the policy
// or in what a node declares is ErrConfig, naming both digests, before a node runs. An operator cancels the
// run or restores the declarations it started under; a stricter policy is not allowed yet either.
func flowTaintCheck(start RunStart, current *flowTaintDecls) (flowTaintStanding, error) {
	if start.inheritedOnly() {
		// Started inside a tainted run by a flow with no policy: there are no declarations to compare. A flow that
		// has a policy now is not the one that started it (and a policy added after the start is not trusted to
		// be the one the parent expected): ErrConfig; one without is policyless.
		if current != nil && !current.Policy.isZero() {
			return 0, fmt.Errorf("agent: the run started inside a tainted run with a flow that had no taint policy (run:start records taint %s without declarations), and this flow has one; drive it with the flow that started it, or start a new run with a new run ID: %w", digestPrefix(start.Ext[extTaintDigest]), ErrConfig)
		}
		return flowPolicyless, nil
	}
	rec, ok, err := start.flowTaintDecls()
	if err != nil {
		return 0, err
	}
	if !ok {
		return flowNotUnderTaint, nil
	}
	if current == nil || current.Policy.isZero() {
		return flowPolicyless, nil
	}
	if now, was := current.digest(), rec.digest(); now != was {
		return 0, fmt.Errorf("agent: the flow's taint declarations (digest %.12s) are not those the run started under (digest %.12s); restore them, or cancel the run: %w", now, was, ErrConfig)
	}
	return flowUnderTaint, nil
}

// nodeNameOfKey is the name of the plan flow node a node key belongs to, and false for any name that is
// not a node key.
func nodeNameOfKey(key string) (string, bool) { return escsubject.NodeName(key) }

// taintMarkPrefix is the name prefix of a taint-mark record, reserved to the engine.
const taintMarkPrefix = "taint-mark:"

// taintMarkFormat tags a taint-mark record.
const taintMarkFormat = "bide.taint-mark.v1"

// taintMarkStep is the journal name of the record that node nodeKey's child brought the mark source up.
func taintMarkStep(nodeKey, source string) string {
	return taintMarkPrefix + encodeID(nodeKey) + ":" + encodeID(source)
}

// taintMarkRecord is what a taint-mark record holds: a node whose child learned a mark, and its source. It
// is how the child's taint comes up into the flow's own journal, where the read-set fold can see it.
type taintMarkRecord struct {
	Format  string `json:"format"`
	NodeKey string `json:"node_key"`
	Source  string `json:"source"`
}

// putTaintMark records, insert-once, that the child of node nodeKey brought the mark source up, and reports
// whether this call wrote the record. The record is read back strictly.
func putTaintMark(ctx context.Context, j *Journal, runID, nodeKey, source string) (bool, error) {
	if !planNodeKey(nodeKey) {
		return false, fmt.Errorf("agent: a taint mark names %q, which is not a plan node key: %w", nodeKey, ErrConfig)
	}
	if err := checkProvenanceName("taint mark", source); err != nil {
		return false, err
	}
	b, err := marshalJournal(taintMarkRecord{Format: taintMarkFormat, NodeKey: nodeKey, Source: source})
	if err != nil {
		return false, fmt.Errorf("encode %s (run %s): %w (%w)", taintMarkStep(nodeKey, source), runID, err, ErrConfig)
	}
	got, inserted, err := j.putNew(ctx, runID, taintMarkStep(nodeKey, source), Record{Kind: StepValue, Result: b})
	if err != nil {
		return false, fmt.Errorf("record %s (run %s): %w", taintMarkStep(nodeKey, source), runID, err)
	}
	if _, err := decodeTaintMark(got); err != nil {
		return false, err
	}
	return inserted, nil
}

// decodeTaintMark reads a taint-mark record: strictly, of the right format, under the name its body
// determines. A record that does not read is ErrStorage: the fold never treats it as no mark.
func decodeTaintMark(r Record) (taintMarkRecord, error) {
	var rec taintMarkRecord
	if err := strictjson.Unmarshal(r.Result, &rec, nil); err != nil || rec.Format != taintMarkFormat || r.Name != taintMarkStep(rec.NodeKey, rec.Source) || !planNodeKey(rec.NodeKey) || rec.Source == "" {
		return taintMarkRecord{}, fmt.Errorf("agent: the record %q does not read as a %s record: %w", r.Name, taintMarkFormat, ErrStorage)
	}
	return rec, nil
}

// protocol:flows begin PGate

// flowReadSet is the marks the node with key self has read at position before of the run's journal recs, the
// unfiltered History: positions are indexes into it, so a copy with records left out (a header, a not-started
// marker) gives other positions and so other reasons. It is the inherited marks, then in journal order the run's input when its start declares it untrusted, the declared sources of
// every node whose result is recorded before, and the taint-mark records before. It is the run-level rule
// (docs/design/flow-taint.md, A2): no edge, join or switch is read, and a node's own result is after it. A
// source is a mark once, at its first position, whatever the iteration; the marks the node's own child wrote
// (taint-mark records for self) are not read. A taint-mark record that does not
// read is an ErrStorage error.
func flowReadSet(recs []Record, d flowTaintDecls, before int, self string, inherited []taintMark) ([]taintMark, error) {
	marks := slices.Clone(inherited)
	seen := map[[2]string]bool{}
	for _, m := range marks {
		seen[[2]string{m.Source, m.Ref}] = true
	}
	add := func(source, ref string, pos int) {
		if k := [2]string{source, ref}; !seen[k] {
			seen[k] = true
			marks = append(marks, taintMark{Source: source, Ref: ref, Pos: pos})
		}
	}
	nodes := d.normalized().Nodes
	for i, r := range recs[:min(max(before, 0), len(recs))] {
		if r.Kind != StepValue {
			continue
		}
		switch {
		case r.Name == runStartStep:
			if src := startInputSource(r); src != "" {
				add(src, "input", i)
			}
		case strings.HasPrefix(r.Name, taintMarkPrefix):
			rec, err := decodeTaintMark(r)
			if err != nil {
				return nil, err
			}
			// A mark the node's own child wrote is written before the node's result, and the node could not
			// have read it when it was gated: the requirement must be the same at the gate, at the record and
			// on a re-drive. The same node in another iteration is another node (another key).
			if rec.NodeKey != self {
				add(rec.Source, "child:"+rec.NodeKey, i)
			}
		default:
			if name, ok := nodeNameOfKey(r.Name); ok {
				for _, s := range nodes[name].Untrusted {
					add(s, "node:"+name, i)
				}
			}
		}
	}
	return marks, nil
}

// protocol:flows end

// flowRequirement is what taint requires of a node about to run.
type flowRequirement struct {
	// Reasons are the taint reasons: those recomputed from the read set and those a record holds, each once.
	Reasons []gateReason
	// Exempt is the node's declared exemption, from the declarations alone.
	Exempt string
	// Required says the node needs the escalation gate (Gate) or is refused (Refuse).
	Required bool
	Refuse   bool
	Gate     *ApprovalPolicy
}

// flowRequirementFor is the requirement taint makes of node, given the declarations the run started under,
// whether the node can have a side effect (tracked), its read set, and the gate-reason record of the node,
// when there is one. It mirrors an agent's escalate: the reasons are the union of those recomputed and those
// recorded, so a record can add a requirement and never remove one; an exemption comes from the
// declarations, never from a record's text; a recorded refusal refuses the node again. A node with no taint
// reason, or that cannot have a side effect, requires nothing. A policy with neither a Gate nor Refuse is
// read as Refuse, so a node is never run ungated for want of a gate.
func flowRequirementFor(d flowTaintDecls, node string, tracked bool, marks []taintMark, rec *gateReasonRecord) flowRequirement {
	if !tracked {
		return flowRequirement{}
	}
	var recomputed, recorded []gateReason
	for _, m := range marks {
		recomputed = append(recomputed, gateReason{Kind: gateReasonTaint, Source: m.Source, Ref: m.Ref, Pos: m.Pos})
	}
	refused := false
	if rec != nil {
		recorded, refused = rec.Reasons, rec.Refused
	}
	reasons := unionReasons(recomputed, recorded)
	taint := slices.ContainsFunc(reasons, func(r gateReason) bool { return r.Kind == gateReasonTaint })
	if !taint && !refused {
		return flowRequirement{}
	}
	out := flowRequirement{Reasons: reasons, Exempt: d.Nodes[node].Exempt}
	if out.Exempt != "" {
		return out
	}
	out.Required = true
	if d.Policy.Refuse || d.Policy.Gate == nil || refused {
		out.Refuse = true
		return out
	}
	out.Gate = d.Policy.Gate.Clone()
	return out
}

// digestPrefix is the first 12 characters of a recorded digest, for an error.
func digestPrefix(raw json.RawMessage) string {
	var d string
	if json.Unmarshal(raw, &d) != nil {
		return "(unreadable)"
	}
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

// extTaintSources is the key of run:start's ext member that holds the sources a flow with no taint policy declares:
// node name to its sorted sources. A flow with a policy records them in its declarations; one with none records
// them here, and only when a node declares one, so a flow that declares nothing starts as it always did. They
// gate nothing. They are what the run's own marks are counted from, for the node that started it.
const extTaintSources = "taint_sources"

// withDeclaredSources returns s recording the sources src (a new Ext map). Nothing is recorded for none.
func (s RunStart) withDeclaredSources(src map[string][]string) RunStart {
	if len(src) == 0 {
		return s
	}
	ext := maps.Clone(s.Ext)
	if ext == nil {
		ext = map[string]json.RawMessage{}
	}
	b, err := json.Marshal(src)
	if err != nil {
		panic(fmt.Sprintf("agent: encode the flow's declared sources: %v", err))
	}
	ext[extTaintSources] = b
	s.Ext = ext
	return s
}

// declaredSources reads the sources a policyless flow's start records, strictly: every node has at least one,
// sorted, once each, valid names. A member that is not so is ErrStorage.
func (s RunStart) declaredSources() (map[string][]string, error) {
	raw, ok := s.Ext[extTaintSources]
	if !ok {
		return nil, nil
	}
	var m map[string][]string
	if err := strictjson.Unmarshal(raw, &m, nil); err != nil || len(m) == 0 {
		return nil, fmt.Errorf("agent: run:start's %s does not read: %w", extTaintSources, ErrStorage)
	}
	for node, l := range m {
		if node == "" || len(l) == 0 {
			return nil, fmt.Errorf("agent: run:start's %s names a node with no source: %w", extTaintSources, ErrStorage)
		}
		for i, src := range l {
			if checkProvenanceName("declared source", src) != nil || i > 0 && l[i-1] >= src {
				return nil, fmt.Errorf("agent: run:start's %s is not in the canonical form (sorted, once each, valid names): %w", extTaintSources, ErrStorage)
			}
		}
	}
	return m, nil
}

// nodeSources are the sources each node of the flow that started s declares: from the declarations a policy
// flow records, or from the sources a policyless one does.
func (s RunStart) nodeSources() (map[string][]string, error) {
	d, ok, err := s.flowTaintDecls()
	if err != nil {
		return nil, err
	}
	if !ok {
		return s.declaredSources()
	}
	out := map[string][]string{}
	for name, n := range d.normalized().Nodes {
		if len(n.Untrusted) > 0 {
			out[name] = n.Untrusted
		}
	}
	return out, nil
}

// sameSources reports whether two source maps are equal.
func sameSources(a, b map[string][]string) bool {
	return maps.EqualFunc(a, b, slices.Equal[[]string])
}
