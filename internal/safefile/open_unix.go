//go:build unix

package safefile

import (
	"os"
	"syscall"
)

// Open opens path for reading without waiting for a writer, which a named pipe would make it do
// for ever, so that a path swapped for a named pipe after it was checked is opened at once (and
// then refused by the caller as not a regular file).
// O_NONBLOCK does not change how a regular file reads.
func Open(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// OpenIn is Open for a name inside root, which cannot leave it.
func OpenIn(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
