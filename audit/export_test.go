package audit

// SetFinalityNeedsLC switches the status rule between the user's decision (false) and the design
// note's earlier literal reading (true) for a test, and returns the function that restores it.
func SetFinalityNeedsLC(v bool) func() {
	old := finalityNeedsLC
	finalityNeedsLC = v
	return func() { finalityNeedsLC = old }
}

// SetMaxCRLEntries lowers the entry cap for a test and returns the function that restores it.
func SetMaxCRLEntries(n int) func() {
	old := maxCRLEntries
	maxCRLEntries = n
	return func() { maxCRLEntries = old }
}
