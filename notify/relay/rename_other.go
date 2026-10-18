//go:build !windows

package relay

// retryableRename is false here: rename replaces a file that is open elsewhere, so a failure is final.
func retryableRename(error) bool { return false }
