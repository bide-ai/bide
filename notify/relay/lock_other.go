//go:build !unix && !windows

package relay

import (
	"errors"
	"os"
)

// lockFile has no OS lock on this platform: a state file cannot be made exclusive, so it is refused
// rather than used unprotected.
func lockFile(*os.File) (bool, error) {
	return false, errors.New("exclusive file locks are not supported on this platform")
}
