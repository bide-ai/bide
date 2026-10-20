//go:build unix

package runview

import (
	"errors"

	"github.com/bide-ai/bide/agent"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A named pipe given as the config is refused, not waited on for ever.
func TestReadConfig_RefusesANonRegularFile(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "cfg.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := readConfig(fifo, nil); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("readConfig(named pipe) = %v, want ErrConfig", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readConfig blocked on a named pipe")
	}
}
