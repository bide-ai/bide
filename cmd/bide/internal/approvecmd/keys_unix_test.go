//go:build unix

package approvecmd

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A key file that is a named pipe is refused at once: opening it would block until a writer came.
func TestLoadSignerRefusesAFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadSigner(p); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadSigner blocked on a FIFO")
	}
}
