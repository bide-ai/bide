package capledger

// The reasons a cap-release record gives (package agent writes the record, in the run that made the call,
// before it releases the call's slot; bide audit caps reads it).
const (
	// ReleaseWhyRefused: a cumulative cap refused the call after another rule's slot was reserved.
	ReleaseWhyRefused = "cap refusal"
	// ReleaseWhyNoCall: the chain returned without reaching the tool's Call after a reservation (a later
	// reservation the store failed).
	ReleaseWhyNoCall = "chain without a tool call"
)
