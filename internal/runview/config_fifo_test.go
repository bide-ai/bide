//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runview

import (
	"errors"
	"os"

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

// A path swapped for a named pipe between the check and the open does not block the open.
func TestReadConfig_RefusesAPathSwappedForAPipe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	betweenStatAndOpen = func() {
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Error(err)
		}
	}
	defer func() { betweenStatAndOpen = nil }()
	done := make(chan error, 1)
	go func() { _, err := readConfig(path, nil); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("readConfig of a path swapped for a pipe = %v, want ErrConfig", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readConfig blocked on a pipe swapped in before the open")
	}
}
