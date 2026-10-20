//go:build unix

package runview

import (
	"os"
	"syscall"
)

// openConfig opens path for reading without blocking, so that a path swapped for a named pipe
// after it was checked is opened at once (and then refused as not a regular file) instead of
// blocking until a writer appears. O_NONBLOCK does not change how a regular file reads.
func openConfig(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
