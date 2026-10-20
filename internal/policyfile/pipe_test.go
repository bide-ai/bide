package policyfile

import (
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// A policy file is read without waiting on a named pipe, and a directory is refused.
func TestRead_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	d := t.TempDir()
	safefiletest.Deadline(t, "Read", func() {
		if _, err := Read(p); err == nil {
			t.Error("Read read a named pipe")
		}
		if _, _, err := ReadSources(p); err == nil {
			t.Error("ReadSources read a named pipe")
		}
	})
	if _, err := Read(d); err == nil {
		t.Error("Read read a directory")
	}
	if _, _, err := ReadSources(d); err == nil {
		t.Error("ReadSources read a directory")
	}
}
