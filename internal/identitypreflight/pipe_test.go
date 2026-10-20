package identitypreflight

import (
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// The files the identity preflight names are read without waiting on a named pipe, and a directory
// in their place is refused.
func TestReaders_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	d := t.TempDir()
	safefiletest.Deadline(t, "readers", func() {
		if _, _, err := readChain(p, "-chain"); err == nil {
			t.Error("readChain read a named pipe")
		}
		if _, err := readRecord(p); err == nil {
			t.Error("readRecord read a named pipe")
		}
	})
	if _, _, err := readChain(d, "-chain"); err == nil {
		t.Error("readChain read a directory")
	}
	if _, err := readRecord(d); err == nil {
		t.Error("readRecord read a directory")
	}
}
