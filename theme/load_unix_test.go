//go:build unix

package theme

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLoadRefusesANamedPipe: Load is public, so a path that names a FIFO must be refused at once,
// not wait for a writer.
func TestLoadRefusesANamedPipe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "theme.json")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := Load(p)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("Load of a named pipe = %v, want a refusal as not a regular file", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load of a named pipe blocked")
	}
}
