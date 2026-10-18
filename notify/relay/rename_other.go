//go:build !windows

package relay

// renameQuickRetries is 0 here: rename replaces a file that is open elsewhere, so a failure is final.
func renameQuickRetries(error) int { return 0 }
