//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package hsmkeys

import (
	"path/filepath"
	"syscall"
	"testing"
)

// makeFIFO creates a named pipe in dir and returns its path.
func makeFIFO(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "pin.fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
