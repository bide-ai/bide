//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runview

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// withDeadline runs f and fails the test if it has not returned in 5 s, which is how a read of a
// named pipe shows: it waits for a writer that never comes.
func withDeadline(t *testing.T, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked on a named pipe", what)
		return nil
	}
}

// The files a bide view flag names (-sth, -evidence, -approver-keys, -stop-policy through
// readStrict; -pubkey through readKey) are never waited on when they are named pipes.
func TestReaders_RefuseANamedPipe(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "p")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	var v map[string]any
	if err := withDeadline(t, "readStrict", func() error { return readStrict("-sth", fifo, &v) }); err == nil {
		t.Error("readStrict read a named pipe")
	}
	if err := withDeadline(t, "readKey", func() error { _, err := readKey(fifo); return err }); err == nil {
		t.Error("readKey read a named pipe")
	}
}
