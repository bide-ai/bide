//go:build unix

package runview

import (
	"os"
	"syscall"
)

// openNoWait opens path for reading without waiting for a writer, which a named pipe would make it
// do for ever.
func openNoWait(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
