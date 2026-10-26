//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos) && !windows

package relay

import "os"

// haveFileLock says whether this platform has an exclusive file lock (lockFile); the tests that need a
// state file skip where it is false.
const haveFileLock = false

// lockFile has no OS lock on this platform (Flock is defined on Linux, macOS, the BSDs, DragonFly and
// illumos, and Windows has its own lock; Solaris, AIX and the rest have neither): a state file cannot
// be made exclusive, so it is refused rather than used unprotected (unsupportedLock).
func lockFile(f *os.File) (bool, error) { return unsupportedLock(f) }
