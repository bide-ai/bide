package notifycmd

import (
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// The secret and CA files bide notify names are read without waiting on a named pipe, and a
// directory in their place is refused.
func TestFiles_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	d := t.TempDir()
	safefiletest.Deadline(t, "notify readers", func() {
		if _, err := readOne(func(string) string { return "" }, readFile, "", p); err == nil {
			t.Error("readOne read a named pipe")
		}
	})
	if _, err := readOne(func(string) string { return "" }, readFile, "", d); err == nil {
		t.Error("readOne read a directory")
	}
}
