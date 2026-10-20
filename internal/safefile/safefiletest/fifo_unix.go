//go:build unix

package safefiletest

import (
	"path/filepath"
	"syscall"
	"testing"
)

// FIFO returns the path of a new named pipe, with no writer, in a temporary directory. It skips the
// test where the system cannot make one.
func FIFO(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	return p
}
