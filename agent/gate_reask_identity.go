package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The tally names of a gate with a certificate-bound approver, and how it asks again
// (docs/design/approver-identity.md). The gate's first count is generation 0, recorded under
// ApprovalIdentTallyStep(call, 0); a plain-name tally (ApprovalTallyStep and its re-tally names) beside
// it is superseded and never the outcome of a bound gate. An exec hold of generation g (ExecHold,
// ApprovalExecHoldStep) supersedes the passed generation-g tally, and the next count is generation g+1,
// recorded under ApprovalIdentTallyStep(call, g+1) and superseding the generation-g tally
// (ApprovalTally.Supersedes). One function, identChain, reads the chain: everything that needs the
// gate's outcome goes through RecordedTally, which calls it.

// ApprovalIdentReaskStep is the journal name of the pause note a certificate-bound gate writes when it
// asks again for generation gen: 0 over a plain-name tally it does not act on (PendingIdentReapproval),
// n >= 1 over the generation n-1 tally that an exec hold superseded (PendingExecHoldReapproval). Like
// ApprovalReaskStep's notes it holds an ApprovalWait with Reask and Supersedes set, is informational,
// and counts only if the journal proves it (ValidReaskNote).
func ApprovalIdentReaskStep(toolUseID string, gen int) string {
	return ApprovalPendingStep(toolUseID) + ":ident:" + strconv.Itoa(gen)
}

// identState is what identChain finds in the identity tally chain of one call.
type identState struct {
	present bool   // some generation's tally is recorded
	decided bool   // the newest recorded tally is the gate's outcome (not held)
	gen     int    // the generation of the outcome when decided, else of the count the gate awaits
	prev    string // the name of the tally the awaited count supersedes (when not decided)
	outcome Record // the outcome tally when decided
}

// identChain reads the identity tally chain of toolUseID among records keyed by name: generation 0's
// tally, and while a generation's tally passed and is held (a valid ExecHold of that generation), the
// next one. A hold counts only over a passed tally: a denial is final, and a hold with no tally under it,
// of another call or generation, or that does not read as a hold is not one.
func identChain(byName map[string]Record, toolUseID string) identState {
	var st identState
	for g := 0; ; g++ {
		r, ok := byName[ApprovalIdentTallyStep(toolUseID, g)]
		if !ok {
			st.gen = g
			return st
		}
		st.present = true
		if !recordPassed(r) || !execHeld(byName, toolUseID, g) {
			st.decided, st.gen, st.outcome = true, g, r
			return st
		}
		st.prev = r.Name
	}
}

// recordPassed reports whether r is a tally that passed. One that does not decode is not passed (a
// reader then errors on it, as for any tally).
func recordPassed(r Record) bool {
	var t ApprovalTally
	return json.Unmarshal(r.Result, &t) == nil && t.Passed()
}

// execHeld reports whether byName holds a valid exec hold of generation g of toolUseID.
func execHeld(byName map[string]Record, toolUseID string, g int) bool {
	r, ok := byName[ApprovalExecHoldStep(toolUseID, g)]
	if !ok || r.Kind != StepValue {
		return false
	}
	var h ExecHold
	return json.Unmarshal(r.Result, &h) == nil && h.Format == ApprovalExecHoldFormat && h.ToolUseID == toolUseID && h.Generation == g
}

// ExecHoldGen parses the generation out of name, which must be ApprovalExecHoldStep of toolUseID; ok is
// false for any other name.
func ExecHoldGen(toolUseID, name string) (int, bool) {
	prefix := "approval-exec-hold:" + encodeID(toolUseID) + ":"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
	if err != nil || n < 0 || ApprovalExecHoldStep(toolUseID, n) != name {
		return 0, false
	}
	return n, true
}

// IdentTallyGen parses the generation out of name, which must be ApprovalIdentTallyStep of toolUseID;
// ok is false for any other name.
func IdentTallyGen(toolUseID, name string) (int, bool) {
	if name == ApprovalIdentTallyStep(toolUseID, 0) {
		return 0, true
	}
	prefix := ApprovalTallyStep(toolUseID) + ":ident-reask:"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
	if err != nil || n < 1 || ApprovalIdentTallyStep(toolUseID, n) != name {
		return 0, false
	}
	return n, true
}

// identGen returns the generation of toolUseID's gate in recs: of its recorded outcome, or of the count
// it awaits after an exec hold.
func identGen(recs []Record, toolUseID string) int {
	return identChain(GateTallyValues(recs, toolUseID), toolUseID).gen
}

// GateTallyValues returns the StepValue records of recs that decide the gate of toolUseID, by name,
// first writer winning: the plain-name tallies (ApprovalTallyStep, ApprovalRetallyStep,
// ApprovalSoDRetallyStep), the identity tally chain and the exec holds.
func GateTallyValues(recs []Record, toolUseID string) map[string]Record {
	plain := map[string]bool{ApprovalTallyStep(toolUseID): true, ApprovalRetallyStep(toolUseID): true, ApprovalSoDRetallyStep(toolUseID): true}
	identPrefix := ApprovalTallyStep(toolUseID) + ":ident"
	holdPrefix := "approval-exec-hold:" + encodeID(toolUseID) + ":"
	m := map[string]Record{}
	for _, r := range recs {
		if r.Kind != StepValue {
			continue
		}
		if plain[r.Name] || r.Name == identPrefix || strings.HasPrefix(r.Name, identPrefix+"-reask:") || strings.HasPrefix(r.Name, holdPrefix) {
			if _, dup := m[r.Name]; !dup {
				m[r.Name] = r
			}
		}
	}
	return m
}

// IdentReaskGen parses the generation out of name, which must be ApprovalIdentReaskStep of toolUseID;
// ok is false for any other name.
func IdentReaskGen(toolUseID, name string) (int, bool) {
	prefix := ApprovalPendingStep(toolUseID) + ":ident:"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
	if err != nil || n < 0 || ApprovalIdentReaskStep(toolUseID, n) != name {
		return 0, false
	}
	return n, true
}

// validIdentReaskNote is ValidReaskNote for a note named by ApprovalIdentReaskStep: generation 0's
// supersedes a plain-name tally that passed (the gate would ask again over it), generation n >= 1's the
// passed generation n-1 tally that a valid exec hold of that generation holds.
func validIdentReaskNote(byName map[string]Record, toolUseID, name string, w ApprovalWait) bool {
	n, ok := IdentReaskGen(toolUseID, name)
	if !ok || w.ToolUseID != toolUseID {
		return false
	}
	if n == 0 {
		if w.Reask != PendingIdentReapproval {
			return false
		}
		switch w.Supersedes {
		case ApprovalTallyStep(toolUseID), ApprovalRetallyStep(toolUseID), ApprovalSoDRetallyStep(toolUseID):
		default:
			return false
		}
		r, ok := byName[w.Supersedes]
		return ok && recordPassed(r)
	}
	if w.Reask != PendingExecHoldReapproval || w.Supersedes != ApprovalIdentTallyStep(toolUseID, n-1) {
		return false
	}
	r, ok := byName[w.Supersedes]
	return ok && recordPassed(r) && execHeld(byName, toolUseID, n-1)
}

// gateTallyRecords reads, with get (a point read of one journal name), every tally record of the gate of
// toolUseID that exists: the plain-name tallies, then the identity tally of generation 0, 1, ... until
// one is absent. It is for a reader that holds a store and not the run's records (the notifier's engine,
// the deadline check); the first error is returned.
func gateTallyRecords(get func(name string) (Record, bool, error), toolUseID string) ([]Record, error) {
	var out []Record
	for _, name := range []string{ApprovalTallyStep(toolUseID), ApprovalRetallyStep(toolUseID), ApprovalSoDRetallyStep(toolUseID)} {
		r, ok, err := get(name)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, r)
		}
	}
	for g := 0; ; g++ {
		r, ok, err := get(ApprovalIdentTallyStep(toolUseID, g))
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, r)
	}
}

// ErrGateNeedsNewRun is wrapped (with ErrConfig) by the error of a certificate-bound gate that cannot go
// on in place: its recorded count passed without excluding the requester and the policy now excludes
// them, so no re-ask can be named. The drive refuses; a reader of the gate (EvaluateGate) reports the
// same error as GateMisconfigured, and an operator cancels the run and starts the call again.
var ErrGateNeedsNewRun = errors.New("the gate needs a new run")

// journalBound reports what the journal says of the gate of toolUseID, whatever the policy is now: that
// it is certificate-bound (an identity tally, an exec hold, or a pending or phase note with
// identity_bound), and the identity_kinds digest of the newest note that says so ("" when none does).
// A gate that is bound is never counted with bare keys: a policy or resolver that lost its
// certificate-bound approvers (a downgrade, a replica configured without them) is refused.
func journalBound(recs []Record, toolUseID string) (bound bool, kinds string) {
	pending := ApprovalPendingStep(toolUseID)
	pausePrefix := "approval-pause:" + encodeID(toolUseID) + ":"
	tallyPrefix := ApprovalTallyStep(toolUseID) + ":ident"
	holdPrefix := "approval-exec-hold:" + encodeID(toolUseID) + ":"
	for _, r := range recs {
		if r.Kind != StepValue {
			continue
		}
		switch {
		case r.Name == tallyPrefix || strings.HasPrefix(r.Name, tallyPrefix+"-reask:") || strings.HasPrefix(r.Name, holdPrefix):
			bound = true
		case r.Name == pending || strings.HasPrefix(r.Name, pending+":") || strings.HasPrefix(r.Name, pausePrefix):
			var w ApprovalWait
			if json.Unmarshal(r.Result, &w) == nil && w.ToolUseID == toolUseID && w.IdentityBound {
				bound, kinds = true, w.IdentityKinds
			}
		}
	}
	return bound, kinds
}

// checkBoundPolicy refuses (ErrConfig) a policy that does not match what the journal says of the gate: the
// gate is certificate-bound and pol has no certificate-bound approver, or the approvers' kinds
// (bare or certificate) differ from those the gate's note recorded.
func checkBoundPolicy(recs []Record, runID, toolUseID string, pol *ApprovalPolicy, verifierFor ApproverVerifierFor) error {
	bound, kinds := journalBound(recs, toolUseID)
	if !bound {
		return nil
	}
	ids, _ := identityApprovers(pol, verifierFor)
	switch {
	case len(ids) == 0:
		return fmt.Errorf("run %s: the approval gate on %s is certificate-bound (its journal says so), but the policy now has no certificate-bound approver; a gate is never counted with bare keys after it was bound, so the call does not run (cancel the run and start the call again if the downgrade is intended): %w", runID, toolUseID, ErrConfig)
	case kinds != "" && identityKinds(pol, ids) != kinds:
		return fmt.Errorf("run %s: the approval gate on %s was asked with other certificate-bound approvers than the policy now names (its recorded approver kinds differ); the call does not run (cancel the run and start the call again if the change is intended): %w", runID, toolUseID, ErrConfig)
	}
	return nil
}
