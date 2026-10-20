package approvecmd

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// The files bide approve names (-item, -approver-keys, -cert, -log-key) are read without waiting on a
// named pipe, and a directory in their place is refused.
func TestReaders_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	d := t.TempDir()
	safefiletest.Deadline(t, "readers", func() {
		if _, err := approverKeys(p); err == nil {
			t.Error("approverKeys read a named pipe")
		}
		if _, err := readChain(p); err == nil {
			t.Error("readChain read a named pipe")
		}
		if _, err := readLogKey(p); err == nil {
			t.Error("readLogKey read a named pipe")
		}
		f := &flags{item: p}
		if err := f.readItem(strings.NewReader("")); err == nil {
			t.Error("readItem read a named pipe")
		}
	})
	if _, err := approverKeys(d); err == nil {
		t.Error("approverKeys read a directory")
	}
	if _, err := readChain(d); err == nil {
		t.Error("readChain read a directory")
	}
	if _, err := readLogKey(d); err == nil {
		t.Error("readLogKey read a directory")
	}
	f := &flags{item: d}
	if err := f.readItem(strings.NewReader("")); err == nil {
		t.Error("readItem read a directory")
	}
}
