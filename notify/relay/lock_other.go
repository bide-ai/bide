//go:build (solaris && !illumos) || (!unix && !windows)

package relay

import "os"

// lockFile has no OS lock on this platform (syscall.Flock is not defined on Solaris, nor on a system
// that is neither Unix nor Windows): a state file cannot be made exclusive, so it is refused rather than
// used unprotected (unsupportedLock).
func lockFile(f *os.File) (bool, error) { return unsupportedLock(f) }
