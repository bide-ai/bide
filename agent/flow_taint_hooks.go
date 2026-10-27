package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/internal/strictjson"
)

// The hooks by which package plan takes part in flow taint (docs/design/flow-taint.md, A2): they join the
// pure parts of flow_taint.go to the journal. Package plan passes plain data (journalhook.FlowTaint) and
// reads back a decision; it never sees the declarations' record or the read set.

func init() {
	journalhook.FlowTaintPrepare = flowTaintPrepare
	journalhook.FlowTaintEnter = flowTaintEnter
	journalhook.FlowTaintNodeHook = flowTaintNode
	journalhook.FlowTaintSink = flowTaintSink
	journalhook.FlowTaintDriven = flowTaintDriven
	journalhook.FlowTaintBodyDone = flowTaintBodyDone
}

// declsOf converts the plain declarations of package plan.
func declsOf(d *journalhook.FlowTaint) (flowTaintDecls, error) {
	out := flowTaintDecls{Policy: flowTaintPolicy{Refuse: d.Refuse, RequireProvenance: d.RequireProvenance}}
	if d.Gate != nil {
		g, ok := d.Gate.(*ApprovalPolicy)
		if !ok {
			return flowTaintDecls{}, fmt.Errorf("agent: a flow's taint gate is %T, not an *agent.ApprovalPolicy: %w", d.Gate, ErrConfig)
		}
		out.Policy.Gate = g
	}
	if len(d.Nodes) > 0 {
		out.Nodes = make(map[string]flowNodeTaint, len(d.Nodes))
		for name, n := range d.Nodes {
			out.Nodes[name] = flowNodeTaint{Untrusted: n.Untrusted, Trusted: n.Trusted, Exempt: n.Exempt}
		}
	}
	return out.normalized(), nil
}

// flowTaintPrepare is journalhook.FlowTaintPrepare.
func flowTaintPrepare(ctx context.Context, j any, runID string, start any, d *journalhook.FlowTaint, tracked []string, sources map[string][]string) (any, error) {
	want, ok := start.(RunStart)
	if !ok {
		return nil, fmt.Errorf("agent: FlowTaintPrepare: start is %T, not an agent.RunStart: %w", start, ErrConfig)
	}
	jr, ok := j.(*Journal)
	if !ok {
		return nil, fmt.Errorf("agent: FlowTaintPrepare: %T is not a journal: %w", j, ErrConfig)
	}
	// Inside a tainted run (a tool of a tainted agent, the body of a tainted node) the run inherits the status
	// and the marks its parent was shown, as an agent child does.
	parent := underTaintFrom(ctx)
	carried, hasCarried := callMarksFrom(ctx)
	if parent != "" {
		if err := linkChild(ctx, jr, runID, childKindFlow); err != nil {
			return nil, err
		}
	}
	// Only a new run is judged here: a run that has started keeps its recorded start, which holdToStart returns,
	// and a resume is checked by FlowTaintEnter. The start is read only when a judgement needs it.
	startedRun := func() (bool, error) {
		_, has, err := RecordedStart(ctx, jr, runID)
		return has, err
	}
	if d == nil {
		if parent == "" {
			return want.withDeclaredSources(sources), nil
		}
		started := false
		if len(tracked) > 0 {
			var err error
			if started, err = startedRun(); err != nil {
				return nil, err
			}
		}
		if !started && len(tracked) > 0 {
			return nil, fmt.Errorf("agent: this flow has no taint policy, and run %s starts inside a run under taint (%.12s); its steps %s can have a side effect, and a flow cannot run one under taint it has no policy to enforce; give it a taint policy, or start it outside the tainted run: %w", runID, parent, strings.Join(tracked, ", "), ErrConfig)
		}
		want = want.withTaintDigest(parent).withDeclaredSources(sources)
		if hasCarried {
			want = want.withInheritedMarks(carried.list)
		}
		return want, nil
	}
	decls, err := declsOf(d)
	if err != nil {
		return nil, err
	}
	// The run of a flow started inside a tainted run is not asked where its input came from: its input is
	// written by its parent's code, and its marks are inherited.
	if decls.Policy.RequireProvenance && parent == "" && want.Settings.UntrustedInput == nil && want.Settings.TrustedInput == nil {
		if started, err := startedRun(); err != nil {
			return nil, err
		} else if !started {
			return nil, errInputProvenance
		}
	}
	if hasCarried {
		want = want.withInheritedMarks(carried.list)
	}
	return want.withFlowTaint(decls), nil
}

// checkInheritedMarks reads the inherited marks a start records strictly: in the canonical form a run records
// them in (one per source, sorted by source, a valid name, a "parent:" ref). A list that is not is ErrStorage:
// a read set built on it would not be the one the parent passed.
func checkInheritedMarks(s RunStart) ([]inheritedMark, error) {
	raw, ok := s.Ext[extTaintInherited]
	if !ok {
		return nil, nil
	}
	var l []inheritedMark
	if err := strictjson.Unmarshal(raw, &l, nil); err != nil {
		return nil, fmt.Errorf("agent: run:start's %s does not read: %w", extTaintInherited, ErrStorage)
	}
	for i, m := range l {
		if checkProvenanceName("inherited mark", m.Source) != nil || !strings.HasPrefix(m.Ref, parentRefPrefix) || i > 0 && l[i-1].Source >= m.Source {
			return nil, fmt.Errorf("agent: run:start's %s is not in the canonical form (one mark per source, sorted, a valid name, a %q ref): %w", extTaintInherited, parentRefPrefix, ErrStorage)
		}
	}
	return l, nil
}

// flowTaintEnter is journalhook.FlowTaintEnter.
func flowTaintEnter(ctx context.Context, j any, runID string, d *journalhook.FlowTaint, tracked []string, sources map[string][]string) (int, error) {
	jr, ok := j.(*Journal)
	if !ok {
		return 0, fmt.Errorf("agent: FlowTaintEnter: %T is not a journal: %w", j, ErrConfig)
	}
	rec, has, err := recordedStartOf(ctx, jr, runID)
	if err != nil {
		return 0, err
	}
	if !has {
		return 0, fmt.Errorf("agent: run %s has no recorded start to check its taint declarations against: %w", runID, ErrStorage)
	}
	recorded, err := checkInheritedMarks(rec)
	if err != nil {
		return 0, err
	}
	// A run is held to the marks it started with: a drive that reaches it with other ones (the parent's node
	// shows another set now) is ErrConfig. A drive that carries none (a recovery pass, a direct call) runs under
	// the recorded ones.
	if c, ok := callMarksFrom(ctx); ok && len(c.list) > 0 && !slices.Equal(c.list, recorded) {
		return 0, fmt.Errorf("run %s was started with other inherited taint marks than the calling node shows now; drive it with the declarations it started under: %w", runID, ErrConfig)
	}
	// A run that started with no declarations of its own (no policy) is held to the sources it recorded: a flow
	// that declares others now is not the one that started it. (A flow with a policy compares its declarations.)
	if _, hasDecls := rec.Ext[extTaintDeclarations]; d == nil && !hasDecls {
		was, err := rec.declaredSources()
		if err != nil {
			return 0, err
		}
		if !sameSources(was, sources) {
			return 0, fmt.Errorf("agent: the flow declares other untrusted sources than the run %s started with; drive it with the flow that started it, or start a new run with a new run ID: %w", runID, ErrConfig)
		}
	}
	var cur *flowTaintDecls
	if d != nil {
		c, err := declsOf(d)
		if err != nil {
			return 0, err
		}
		cur = &c
	}
	st, err := flowTaintCheck(rec, cur)
	if err != nil {
		return 0, err
	}
	// A run under taint driven by a flow with no policy to enforce it runs no node that can have a side effect, on
	// any drive, whether it started under a policy of its own that the flow has lost or inside a tainted run under
	// a flow with none: the flow is refused when it has one, before its read-only nodes run (a redeployed flow whose
	// node became a side effect is not the one that started it).
	if st == flowPolicyless && len(tracked) > 0 {
		return 0, fmt.Errorf("agent: run %s is under taint (%.12s), and this flow has no taint policy to enforce it but its steps %s can have a side effect; drive it with the flow that started it, restore its taint policy, or cancel the run: %w", runID, digestPrefix(rec.Ext[extTaintDigest]), strings.Join(tracked, ", "), ErrConfig)
	}
	switch st {
	case flowUnderTaint:
		return journalhook.FlowTaintUnder, nil
	case flowPolicyless:
		return journalhook.FlowTaintPolicyless, nil
	}
	return journalhook.FlowTaintNone, nil
}

// protocol:flows begin TRefuse

// flowTaintNode is journalhook.FlowTaintNodeHook. For a run under taint it computes the node's read set from
// the declarations the run's start recorded, the marks it inherited and the journal before the node's own
// result (the position it ran at, or the end of the journal for a node that has not run), and from it the
// node's requirement: it records the gate-reason record (insert-once, add-only) and returns the escalation
// gate the node's instance needs. A refused node leaves its gate-reason record (refused) and no record of its
// own, and the error wraps ErrTaintRefused. The decision's context binds the run's status and the read set for
// the runs the node's body starts, for a node that can have a side effect or not.
func flowTaintNode(ctx context.Context, j any, runID, nodeKey string, safety any, standing int) (journalhook.FlowTaintDecision, error) {
	jr, ok := j.(*Journal)
	if !ok {
		return journalhook.FlowTaintDecision{}, fmt.Errorf("agent: FlowTaintNode: %T is not a journal: %w", j, ErrConfig)
	}
	s, ok := safety.(Safety)
	if !ok {
		return journalhook.FlowTaintDecision{}, fmt.Errorf("agent: FlowTaintNode: safety is %T, not an agent.Safety: %w", safety, ErrConfig)
	}
	if standing == journalhook.FlowTaintNone {
		return journalhook.FlowTaintDecision{}, nil
	}
	if standing == journalhook.FlowTaintPolicyless && tracked(s) {
		return journalhook.FlowTaintDecision{}, fmt.Errorf("agent: node %q can have a side effect, and its run is under taint this flow has no policy to enforce; drive it with the flow that started it, or cancel it: %w", nodeKey, ErrConfig)
	}
	name, ok := nodeNameOfKey(nodeKey)
	if !ok {
		return journalhook.FlowTaintDecision{}, fmt.Errorf("agent: FlowTaintNode: %q is not a plan node key: %w", nodeKey, ErrConfig)
	}
	start, has, err := recordedStartOf(ctx, jr, runID)
	if err != nil {
		return journalhook.FlowTaintDecision{}, err
	}
	if !has {
		return journalhook.FlowTaintDecision{}, fmt.Errorf("agent: run %s has no recorded start: %w", runID, ErrStorage)
	}
	decls, _, err := start.flowTaintDecls()
	if err != nil {
		return journalhook.FlowTaintDecision{}, err
	}
	digest := start.taintDigest()
	if digest == "" {
		return journalhook.FlowTaintDecision{}, nil
	}
	inherited, err := checkInheritedMarks(start)
	if err != nil {
		return journalhook.FlowTaintDecision{}, err
	}
	recs, err := jr.History(ctx, runID)
	if err != nil {
		return journalhook.FlowTaintDecision{}, err
	}
	before := len(recs)
	for i, r := range recs {
		if r.Kind == StepValue && r.Name == nodeKey {
			before = i
			break
		}
	}
	// The read set is folded for a node that cannot have a side effect too, and so is the journal read above:
	// the node's body is bound to it, as the marks the runs the body starts inherit. A read-only node was once
	// left out (it needs no gate, so it read nothing); that no longer holds, because a read-only node's body is
	// where a nested flow or an agent starts.
	marks, err := flowReadSet(recs, decls, before, nodeKey, inheritedAsMarks(inherited))
	if err != nil {
		return journalhook.FlowTaintDecision{}, err
	}
	// The body's runs inherit the status and the read set, whether or not the node is gated: a read-only node
	// that runs an agent hands it the taint too.
	// The body also gets a result mark: an agent it runs registers there, and hands its marks up when the body is
	// done (FlowTaintBodyDone), before the node's record.
	bound, _ := withResultMark(withMarkSink(withCallMarks(withUnderTaint(ctx, digest), marks), jr, runID, nodeKey))
	if standing == journalhook.FlowTaintPolicyless || !tracked(s) {
		return journalhook.FlowTaintDecision{Ctx: bound}, nil
	}
	have, found, err := getGateReason(ctx, jr, runID, nodeKey)
	if err != nil {
		return journalhook.FlowTaintDecision{}, err
	}
	var recorded *gateReasonRecord
	if found {
		recorded = &have
		if want := gateDigest(decls.Policy.Gate); have.PolicyDigest != want {
			return journalhook.FlowTaintDecision{}, fmt.Errorf("run %s: the node %q was held under an escalation gate with digest %.12s, and the taint policy now has %.12s; restore the policy it was held under, or cancel the run: %w", runID, nodeKey, have.PolicyDigest, want, ErrConfig)
		}
	}
	req := flowRequirementFor(decls, name, true, marks, recorded)
	if len(req.Reasons) == 0 && !req.Required {
		return journalhook.FlowTaintDecision{Ctx: bound}, nil
	}
	if !found {
		rec := gateReasonRecord{Format: gateReasonFormat, ToolUseID: nodeKey, Reasons: req.Reasons, PolicyDigest: gateDigest(decls.Policy.Gate),
			Gate: decls.Policy.Gate.Clone(), Exempt: req.Exempt, Refused: req.Required && req.Refuse, RecordedAt: noteStamp(clockFrom(ctx))}
		winner, _, err := putGateReason(ctx, jr, runID, rec)
		if err != nil {
			return journalhook.FlowTaintDecision{}, err
		}
		// Another driver's record may have won: the requirement is the union of both, add-only.
		req = flowRequirementFor(decls, name, true, marks, &winner)
	}
	switch {
	case !req.Required:
		return journalhook.FlowTaintDecision{Ctx: bound}, nil
	case req.Refuse:
		return journalhook.FlowTaintDecision{}, fmt.Errorf("node %q: %s: %w", nodeKey, reasonText(req.Reasons), journalhook.ErrTaintRefused)
	}
	return journalhook.FlowTaintDecision{Gate: req.Gate, Ctx: bound}, nil
}

// protocol:flows end

// reasonText names the first taint source of reasons for an error.
func reasonText(rs []gateReason) string {
	for _, r := range rs {
		if r.Kind == gateReasonTaint {
			return fmt.Sprintf("the run has read untrusted content from %s", r.Source)
		}
	}
	return "the run has read untrusted content"
}
