//go:build windows

package relay

import (
	"errors"
	"syscall"
)

// retryableRename reports whether a failed rename may succeed a moment later. On Windows, replacing a file
// that another process has open (a reader, an operator's tail, an antivirus scan) fails with a sharing
// violation (ERROR_SHARING_VIOLATION) or access denied (ERROR_ACCESS_DENIED) until that handle closes, which is
// normally milliseconds. Any other error is final.
func retryableRename(err error) bool {
	return errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(5))
}
