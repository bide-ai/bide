// Package taintwords holds the words of plan.NodeTaint.Reason that another package reads back: plan writes
// them, and the diff in internal/runview tells a policy that is known to be absent from one that is not given
// by them, so both name them here and neither reads the other's message text.
package taintwords

const (
	// NoPolicy begins the reason of a node under a policy that is known to be absent.
	NoPolicy = "no taint policy"
	// NotGiven begins the reason of a node whose policy is not given.
	NotGiven = "taint policy not given"
	// Caveat is the reason of a node with no declared source before it: what that verdict does not cover. A view
	// that prints the node's words leaves it to the panel.
	Caveat = "unless the run declares its input untrusted, or a block runs an agent or a nested flow, whose taint this view cannot see"
	// SelfSource is the note on a node that can have a side effect and declares a source itself.
	SelfSource = "it is itself a source, and its own read is not gated; split the block"
)
