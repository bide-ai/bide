package agent

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/internal/journalhook"
)

// The hooks by which package plan takes part in flow taint (docs/design/flow-taint.md, A2): they join the
// pure parts of flow_taint.go to the journal. Package plan passes plain data (journalhook.FlowTaint) and
// reads back a decision; it never sees the declarations' record or the read set.

func init() {
	journalhook.FlowTaintPrepare = flowTaintPrepare
	journalhook.FlowTaintEnter = flowTaintEnter
	journalhook.FlowTaintNodeHook = flowTaintNode
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
func flowTaintPrepare(ctx context.Context, j any, runID string, start any, d *journalhook.FlowTaint) (any, error) {
	want, ok := start.(RunStart)
	if !ok {
		return nil, fmt.Errorf("agent: FlowTaintPrepare: start is %T, not an agent.RunStart: %w", start, ErrConfig)
	}
	if d == nil {
		return want, nil
	}
	decls, err := declsOf(d)
	if err != nil {
		return nil, err
	}
	if decls.Policy.RequireProvenance {
		stated := want.Settings.UntrustedInput != nil || want.Settings.TrustedInput != nil
		if !stated {
			// A run that has started is judged by its recorded start: a resume is not asked again.
			jr, ok := j.(*Journal)
			if !ok {
				return nil, fmt.Errorf("agent: FlowTaintPrepare: %T is not a journal: %w", j, ErrConfig)
			}
			rec, has, err := RecordedStart(ctx, jr, runID)
			if err != nil {
				return nil, err
			}
			stated = has && (rec.Settings.UntrustedInput != nil || rec.Settings.TrustedInput != nil)
		}
		if !stated {
			return nil, errInputProvenance
		}
	}
	return want.withFlowTaint(decls), nil
}

// flowTaintEnter is journalhook.FlowTaintEnter.
func flowTaintEnter(ctx context.Context, j any, runID string, d *journalhook.FlowTaint) (int, error) {
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
	switch st {
	case flowUnderTaint:
		return journalhook.FlowTaintUnder, nil
	case flowPolicyless:
		return journalhook.FlowTaintPolicyless, nil
	}
	return journalhook.FlowTaintNone, nil
}

// flowTaintNode is journalhook.FlowTaintNodeHook. For a run under taint it computes the node's requirement
// from the declarations the run's start recorded and the journal before the node's own result (the position
// it ran at, or the end of the journal for a node that has not run), records the gate-reason record
// (insert-once, add-only), and returns the escalation gate the node's instance needs. A refused node
// leaves its gate-reason record (refused) and no record of its own, and the error wraps ErrTaintRefused.
func flowTaintNode(ctx context.Context, j any, runID, nodeKey string, safety any, standing int) (journalhook.FlowTaintDecision, error) {
	jr, ok := j.(*Journal)
	if !ok {
		return journalhook.FlowTaintDecision{}, fmt.Errorf("agent: FlowTaintNode: %T is not a journal: %w", j, ErrConfig)
	}
	s, ok := safety.(Safety)
	if !ok {
		return journalhook.FlowTaintDecision{}, fmt.Errorf("agent: FlowTaintNode: safety is %T, not an agent.Safety: %w", safety, ErrConfig)
	}
	switch standing {
	case journalhook.FlowTaintNone:
		return journalhook.FlowTaintDecision{}, nil
	case journalhook.FlowTaintPolicyless:
		if tracked(s) {
			return journalhook.FlowTaintDecision{}, fmt.Errorf("agent: node %q can have a side effect, and its run started under taint declarations this flow has no policy to enforce; drive it with the flow that started it, or cancel it: %w", nodeKey, ErrConfig)
		}
		return journalhook.FlowTaintDecision{}, nil
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
	decls, under, err := start.flowTaintDecls()
	if err != nil {
		return journalhook.FlowTaintDecision{}, err
	}
	if !under {
		return journalhook.FlowTaintDecision{}, nil
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
	marks, err := flowReadSet(recs, decls, before, nil)
	if err != nil {
		return journalhook.FlowTaintDecision{}, err
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
	req := flowRequirementFor(decls, name, tracked(s), marks, recorded)
	if len(req.Reasons) == 0 && !req.Required {
		return journalhook.FlowTaintDecision{}, nil
	}
	if !found {
		rec := gateReasonRecord{Format: gateReasonFormat, ToolUseID: nodeKey, Reasons: req.Reasons, PolicyDigest: gateDigest(decls.Policy.Gate),
			Gate: decls.Policy.Gate.Clone(), Exempt: req.Exempt, Refused: req.Required && req.Refuse, RecordedAt: noteStamp(clockFrom(ctx))}
		winner, _, err := putGateReason(ctx, jr, runID, rec)
		if err != nil {
			return journalhook.FlowTaintDecision{}, err
		}
		// Another driver's record may have won: the requirement is the union of both, add-only.
		req = flowRequirementFor(decls, name, tracked(s), marks, &winner)
	}
	switch {
	case !req.Required:
		return journalhook.FlowTaintDecision{}, nil
	case req.Refuse:
		return journalhook.FlowTaintDecision{}, fmt.Errorf("node %q: %s: %w", nodeKey, reasonText(req.Reasons), journalhook.ErrTaintRefused)
	}
	return journalhook.FlowTaintDecision{Gate: req.Gate}, nil
}

// reasonText names the first taint source of reasons for an error.
func reasonText(rs []gateReason) string {
	for _, r := range rs {
		if r.Kind == gateReasonTaint {
			return fmt.Sprintf("the run has read untrusted content from %s", r.Source)
		}
	}
	return "the run has read untrusted content"
}
