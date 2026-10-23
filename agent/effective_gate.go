package agent

// gateInputs is what, besides a tool's own spec, can add a gate to one call: today the gate a cap rule
// holds the call for, and later the escalation gate of untrusted-content taint (see
// docs/design/untrusted-content-and-amount-caps.md, sections 5.3 and 6). Every site that decides a
// call's gates passes the same arguments to the same function.
type gateInputs struct {
	// capGate is the gate the call's cap rules hold it for (a rule with "-> gate" fired), nil for none.
	capGate *ApprovalPolicy
}

// gateSet is the gates one call needs, as effectiveGates decides them.
type gateSet struct {
	own *ApprovalPolicy // the tool's own gate (ToolSpec.Approval), nil for none
}

// effectiveGates is the one function that decides which gates a call of the tool with spec needs: the
// pre-pass of the loop (the gate and a denial's record), preExecute, the snapshot a result record
// carries, and the saga's re-run all ask it, so none can disagree with another or skip a gate the
// function adds. With no input it is the tool's own gate, exactly what those sites read directly
// before; TestNoDirectApprovalReads_InTheEngine keeps it so. A cap gate stands in for a tool with no
// gate of its own; registration refuses a tool that has both (compileCaps), since two gates on one call
// need the second gate instance of the taint work.
func effectiveGates(spec *ToolSpec, in gateInputs) gateSet {
	if spec.Approval == nil && in.capGate != nil {
		return gateSet{own: in.capGate} // registration refuses a tool with both, so the cap gate stands alone
	}
	return gateSet{own: spec.Approval}
}

// Own is the gate the pre-pass and preExecute enforce: the tool's own, nil when ungated.
func (g gateSet) Own() *ApprovalPolicy { return g.own }

// Snapshot is the gate a result record carries (Record.Approval), a copy, so that changing the tool's
// policy later changes no record already written.
func (g gateSet) Snapshot() *ApprovalPolicy { return g.own.Clone() }
