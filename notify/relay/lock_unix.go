//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos

package relay

import (
	"errors"
	"os"
	"syscall"
)

// haveFileLock says whether this platform has an exclusive file lock (lockFile); the tests that need a
// state file skip where it is false.
const haveFileLock = true

// lockFile takes an exclusive, non-blocking flock on f. It reports held=true if another holder has it.
func lockFile(f *os.File) (held bool, err error) {
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
}
