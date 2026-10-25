//go:build (solaris && !illumos) || (!unix && !windows)

package relay

import "os"

// haveFileLock says whether this platform has an exclusive file lock (lockFile); the tests that need a
// state file skip where it is false.
const haveFileLock = false

// lockFile has no OS lock on this platform (syscall.Flock is not defined on Solaris, nor on a system
// that is neither Unix nor Windows): a state file cannot be made exclusive, so it is refused rather than
// used unprotected (unsupportedLock).
func lockFile(f *os.File) (bool, error) { return unsupportedLock(f) }
