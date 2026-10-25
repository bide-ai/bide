//go:build solaris || (!unix && !windows)

package relay

import (
	"errors"
	"os"
)

// lockFile has no OS lock on this platform (syscall.Flock is not defined on Solaris, nor on a system that is neither unix nor Windows): a state file cannot be made exclusive, so it is refused
// rather than used unprotected.
func lockFile(*os.File) (bool, error) {
	return false, errors.New("exclusive file locks are not supported on this platform")
}
