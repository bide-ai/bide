//go:build windows

package relay

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var procLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

const (
	lockfileExclusiveLock   = 0x2
	lockfileFailImmediately = 0x1
	errLockViolation        = syscall.Errno(33)
)

// lockFile takes an exclusive, non-blocking LockFileEx on f. It reports held=true if another holder has it.
func lockFile(f *os.File) (held bool, err error) {
	var ol syscall.Overlapped
	r, _, e := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return false, nil
	}
	if errors.Is(e, errLockViolation) || errors.Is(e, syscall.Errno(32)) {
		return true, nil
	}
	return false, e
}
