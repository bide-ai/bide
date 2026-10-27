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
func flowTaintConform(recsAny any, cur *journalhook.FlowTaint, safeties map[string]any) []string {
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
		return strings.HasPrefix(name, gateReasonPrefix) || strings.HasPrefix(name, taintMarkPrefix)
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
		for _, r := range recs {
			if taintRecord(r.Name) {
				diffs = append(diffs, r.Name+" (a taint record in a run that did not start under taint)")
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
		case strings.HasPrefix(r.Name, taintMarkPrefix):
			rec, err := decodeTaintMark(r)
			if err != nil {
				diffs = append(diffs, r.Name+" (a taint-mark record that does not read)")
			} else if _, ok := declared(rec.NodeKey); !ok {
				diffs = append(diffs, r.Name+" (a taint-mark record of an undeclared node)")
			}
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
