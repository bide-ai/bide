package stopcmd

import (
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// -stop-policy is read without waiting on a named pipe, and a directory is refused.
func TestLoadPolicy_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	safefiletest.Deadline(t, "loadPolicy", func() {
		c := &cmd{f: flags{stopPolicy: p}}
		if _, _, err := c.loadPolicy(); err == nil {
			t.Error("loadPolicy read a named pipe")
		}
	})
	c := &cmd{f: flags{stopPolicy: t.TempDir()}}
	if _, _, err := c.loadPolicy(); err == nil {
		t.Error("loadPolicy read a directory")
	}
}
