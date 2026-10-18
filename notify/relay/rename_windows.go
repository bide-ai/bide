//go:build windows

package relay

import (
	"errors"
	"syscall"
)

// renameQuickRetries is how many quick retries a refused rename gets. On Windows, replacing a file that another
// process has open (a reader, an operator's tail, a virus scan) is refused until that handle closes, normally
// within milliseconds: with ERROR_SHARING_VIOLATION (32) when the reader's handle conflicts with the rename's
// access, and with ERROR_ACCESS_DENIED (5) when the reader opened without FILE_SHARE_DELETE, which is how Go and
// most tools open a file. Access denied is also what a real permission problem returns, so it gets a smaller
// budget than a sharing violation: a few retries that cost milliseconds, then the slow, logged backoff. Any other
// error is final.
func renameQuickRetries(err error) int {
	switch {
	case errors.Is(err, syscall.Errno(32)):
		return transientRetries
	case errors.Is(err, syscall.Errno(5)):
		return transientRetries / 2
	}
	return 0
}
