package main

import (
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// -pubkey naming a file is read without waiting on a named pipe, and a directory is refused.
func TestReadKey_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	safefiletest.Deadline(t, "readKey", func() {
		if _, err := readKey(p); err == nil {
			t.Error("readKey read a named pipe")
		}
	})
	if _, err := readKey(t.TempDir()); err == nil {
		t.Error("readKey read a directory")
	}
}
