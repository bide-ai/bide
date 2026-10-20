//go:build unix

package safefiletest

import (
	"os"
	"path/filepath"
	"strconv"
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

// Descriptor returns the path /dev/fd/N of a descriptor of this process holding content, as a process
// substitution (<(...)) expands to. It skips the test where there is no /dev/fd.
func Descriptor(t *testing.T, content string) string {
	t.Helper()
	if _, err := os.Stat("/dev/fd"); err != nil {
		t.Skip("no /dev/fd on this system")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { r.Close() })
	return "/dev/fd/" + strconv.Itoa(int(r.Fd()))
}
