//go:build unix

package runview

import (
	"path/filepath"
	"syscall"
	"testing"
)

func makeFIFO(t *testing.T, dir string) (string, bool) {
	t.Helper()
	p := filepath.Join(dir, "fifo.json")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	return p, true
}
