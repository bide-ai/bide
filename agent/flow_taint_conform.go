package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	escsubject "github.com/bide-ai/bide/internal/escalation"
	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/internal/strictjson"
)

func init() { journalhook.FlowTaintConform = flowTaintConform }

// flowTaintConform is journalhook.FlowTaintConform: the taint obligations of a flow run's journal, recomputed
// from the declarations its start recorded and the journal alone. A node that ran needed the escalation gate
// if its read set before its record was not empty, it can have a side effect and it is not exempt; the gate
// had to have passed before the node's record, and a Refuse policy means the node could not have run.
func flowTaintConform(recsAny any, cur *journalhook.FlowTaint, sources map[string][]string, safeties map[string]any) []string {
	recs, ok := recsAny.([]Record)
	if !ok {
		return []string{"(the journal passed to the taint conformance is not a record list)"}
	}
	var diffs []string
	var start RunStart
	haveStart := false
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == runStartStep {
			if json.Unmarshal(r.Result, &start) == nil {
				haveStart = true
			}
			break
		}
	}
	var decls flowTaintDecls
	under := false
	if haveStart {
		d, ok, err := start.flowTaintDecls()
		if err != nil {
			return []string{runStartStep + " (the recorded taint declarations do not read or hold together)"}
		}
		decls, under = d, ok
	}
	declared := func(nodeKey string) (string, bool) {
		name, ok := nodeNameOfKey(nodeKey)
		if !ok {
			return "", false
		}
		_, has := safeties[name]
		return name, has
	}
	taintRecord := func(name string) bool {
		return strings.HasPrefix(name, gateReasonPrefix) || strings.HasPrefix(name, taintMarkPrefix) || strings.HasPrefix(name, taintChildPrefix)
	}
	// A run that started with no declarations of its own recorded the sources its flow declared: the flow's now are
	// those (a flow with a policy records them in its declarations, compared below).
	if _, hasDecls := start.Ext[extTaintDeclarations]; haveStart && !hasDecls && (cur == nil || cur.Gate == nil && !cur.Refuse && !cur.RequireProvenance) {
		if was, err := start.declaredSources(); err != nil {
			diffs = append(diffs, runStartStep+" (the declared sources do not read in the canonical form)")
		} else if !sameSources(was, sources) {
			diffs = append(diffs, runStartStep+" (the sources the run started with are not those this flow declares)")
		}
	}
	inheritedList, inhErr := checkInheritedMarks(start)
	if inhErr != nil {
		diffs = append(diffs, runStartStep+" (the inherited taint marks do not read in the canonical form)")
	}
	if start.inheritedOnly() {
		// Started inside a tainted run by a flow with no policy: a flow with a policy now is not the one that
		// started it, and no node that can have a side effect could have run.
		if cur != nil && (cur.Gate != nil || cur.Refuse || cur.RequireProvenance) {
			diffs = append(diffs, runStartStep+" (the run started inside a tainted run with a flow that had no taint policy, and this flow has one)")
		}
		for _, r := range recs {
			if name, ok := declared(r.Name); r.Kind == StepValue && ok {
				if s, _ := safeties[name].(Safety); tracked(s) {
					diffs = append(diffs, r.Name+" (a node that can have a side effect ran in a run that started inside a tainted run with no taint policy)")
				}
			}
		}
	}
	if !under {
		// A run started inside a tainted run with no policy of its own is under taint, and its nodes' children
		// bring marks up into its journal: those records must read and name a declared node, and nothing in it
		// is gated. Any taint record in a run that is not under taint is a divergence.
		inherited := start.inheritedOnly()
		for _, r := range recs {
			switch {
			case !taintRecord(r.Name):
			case !inherited || strings.HasPrefix(r.Name, gateReasonPrefix):
				diffs = append(diffs, r.Name+" (a taint record in a run that did not start under taint with declarations)")
			default:
				diffs = append(diffs, linkRecordProblems(r, declared)...)
			}
		}
		return diffs
	}
	if cur == nil || (cur.Gate == nil && !cur.Refuse && !cur.RequireProvenance) {
		diffs = append(diffs, runStartStep+" (the run started under taint declarations, and this flow has no taint policy)")
	} else if c, err := declsOf(cur); err != nil || c.digest() != decls.digest() {
		diffs = append(diffs, runStartStep+" (the taint declarations the run started under are not this flow's)")
	}
	// The gate-reason and taint-mark records must read, be named for what they hold, and name a declared node.
	reasons := map[string]gateReasonRecord{}
	for _, r := range recs {
		switch {
		case r.Kind == StepValue && strings.HasPrefix(r.Name, gateReasonPrefix):
			var rec gateReasonRecord
			if err := strictjson.Unmarshal(r.Result, &rec, nil); err != nil || rec.Format != gateReasonFormat || r.Name != gateReasonStep(rec.ToolUseID) {
				diffs = append(diffs, r.Name+" (a gate-reason record that does not read)")
				continue
			}
			if _, ok := declared(rec.ToolUseID); !ok {
				diffs = append(diffs, r.Name+" (a gate-reason record of an undeclared node)")
				continue
			}
			reasons[rec.ToolUseID] = rec
			// The record says whether the node's instance is kept apart from its own gate (a display reads it): it
			// agrees with the declarations the run started under, or the record was written by something else. A
			// node whose own gate has decided has a tally, and the policy that separates approvers says so in the
			// record the node was held under.
			_, ownDecided := RecordedTally(GateTallyValues(recs, rec.ToolUseID), rec.ToolUseID)
			switch {
			case rec.SeparateApprovers && !decls.Policy.SeparateApprovers:
				diffs = append(diffs, r.Name+" (the record says the instance is kept apart from the node's own gate, and the run's declarations do not separate approvers)")
			case !rec.SeparateApprovers && decls.Policy.SeparateApprovers && ownDecided:
				diffs = append(diffs, r.Name+" (the declarations separate approvers and the node's own gate decided, but the record does not say the instance is kept apart from it)")
			}
		case strings.HasPrefix(r.Name, taintMarkPrefix), strings.HasPrefix(r.Name, taintChildPrefix):
			diffs = append(diffs, linkRecordProblems(r, declared)...)
		}
	}
	// Each node that ran met its requirement.
	for i, r := range recs {
		if r.Kind != StepValue {
			continue
		}
		name, ok := declared(r.Name)
		if !ok {
			continue
		}
		s, _ := safeties[name].(Safety)
		marks, err := flowReadSet(recs, decls, i, r.Name, inheritedAsMarks(inheritedList))
		if err != nil {
			diffs = append(diffs, r.Name+" (the read set could not be recomputed: "+err.Error()+")")
			continue
		}
		var recorded *gateReasonRecord
		if rec, ok := reasons[r.Name]; ok {
			recorded = &rec
		}
		req := flowRequirementFor(decls, name, tracked(s), marks, recorded)
		if !req.Required {
			continue
		}
		switch {
		case recorded == nil:
			diffs = append(diffs, r.Name+" (ran under taint with no gate-reason record)")
		case req.Refuse:
			diffs = append(diffs, r.Name+" (ran though the taint policy refuses it)")
		default:
			instance := escsubject.NodeInstance(r.Name)
			if !instancePassed(recs[:i], instance, name, req.Gate) {
				diffs = append(diffs, fmt.Sprintf("%s (ran without a passed escalation gate %s)", r.Name, instance))
			}
		}
	}
	return diffs
}

// linkRecordProblems are the divergences of a taint-mark or taint-child record: one that does not read, or that
// names a node the flow does not declare.
func linkRecordProblems(r Record, declared func(string) (string, bool)) []string {
	var nodeKey, what string
	if strings.HasPrefix(r.Name, taintChildPrefix) {
		what = "taint-child"
		rec, err := decodeTaintChild(r)
		if err != nil {
			return []string{r.Name + " (a " + what + " record that does not read)"}
		}
		nodeKey = rec.NodeKey
	} else {
		what = "taint-mark"
		rec, err := decodeTaintMark(r)
		if err != nil {
			return []string{r.Name + " (a " + what + " record that does not read)"}
		}
		nodeKey = rec.NodeKey
	}
	if _, ok := declared(nodeKey); !ok {
		return []string{r.Name + " (a " + what + " record of an undeclared node)"}
	}
	return nil
}

// instancePassed reports whether the escalation gate pol on instance had passed in recs, the journal before the
// node's record: a one-decision gate by an approval recorded after its request, an m-of-n gate by a recorded
// terminal tally that passed. It recounts no signature (a conformance check has no verifiers): the tally and
// the decision are what the engine recorded, and VerifyApprovals is the offline recount.
func instancePassed(recs []Record, instance, name string, pol *ApprovalPolicy) bool {
	if pol.single() {
		st := EvaluateGateAfter(recs, ApprovalSubject{ToolUseID: instance, ToolName: name}, pol, nil, ApprovalRequestStep(instance))
		return st.Status == GateSingle && st.SingleApproved
	}
	rt, ok := RecordedTally(GateTallyValues(recs, instance), instance)
	if !ok {
		return false
	}
	t, err := decodeTally("", rt)
	return err == nil && t.Passed() && !t.Expired
}
