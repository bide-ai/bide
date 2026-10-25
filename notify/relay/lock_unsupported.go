package relay

import (
	"errors"
	"os"
)

// errNoFileLock is why a state file is refused where there is no exclusive file lock.
var errNoFileLock = errors.New("exclusive file locks are not supported on this platform")

// unsupportedLock is lockFile where the platform has no exclusive file lock (see lock_other.go): it
// always fails, so a state file is refused rather than used unprotected, and never reports the lock as
// held or free. It is built everywhere so its test runs on every OS.
func unsupportedLock(*os.File) (held bool, err error) { return false, errNoFileLock }
