package audit

import "time"

// SetFinalityNeedsLC switches the status rule between the user's decision (false) and the design
// note's earlier literal reading (true) for a test, and returns the function that restores it.
func SetFinalityNeedsLC(v bool) func() {
	old := finalityNeedsLC
	finalityNeedsLC = v
	return func() { finalityNeedsLC = old }
}

// SetAttemptTimeout shortens the per-attempt deadline of every feed, so a test of a slow server does not
// wait the real two minutes.
func SetAttemptTimeout(r *CRLRefresher, d time.Duration) {
	for _, f := range r.feeds() {
		f.attemptTimeout = d
	}
}
