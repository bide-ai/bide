package agent

// gateInputs is what, besides a tool's own spec, can add a gate to one call. It is empty today and has
// no field a caller can set: untrusted-content taint and amount caps will add theirs (see
// docs/design/untrusted-content-and-amount-caps.md, sections 5.3 and 6). It exists so that every site
// that decides a call's gates passes the same arguments to the same function from the start.
type gateInputs struct{}

// gateSet is the gates one call needs, as effectiveGates decides them.
type gateSet struct {
	own *ApprovalPolicy // the tool's own gate (ToolSpec.Approval), nil for none
}

// effectiveGates is the one function that decides which gates a call of the tool with spec needs: the
// pre-pass of the loop (the gate and a denial's record), preExecute, the snapshot a result record
// carries, and the saga's re-run all ask it, so none can disagree with another or skip a gate the
// function adds. With no input it is the tool's own gate, exactly what those sites read directly
// before; TestNoDirectApprovalReads_InTheEngine keeps it so.
func effectiveGates(spec *ToolSpec, _ gateInputs) gateSet {
	return gateSet{own: spec.Approval}
}

// Own is the gate the pre-pass and preExecute enforce: the tool's own, nil when ungated.
func (g gateSet) Own() *ApprovalPolicy { return g.own }

// Snapshot is the gate a result record carries (Record.Approval), a copy, so that changing the tool's
// policy later changes no record already written.
func (g gateSet) Snapshot() *ApprovalPolicy { return g.own.Clone() }
