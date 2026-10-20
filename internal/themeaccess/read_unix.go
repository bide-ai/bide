//go:build unix

package themeaccess

import (
	"os"
	"syscall"
)

// openNoWait opens path for reading without blocking on a named pipe until a writer appears.
// O_NONBLOCK does not change how a regular file reads.
func openNoWait(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
