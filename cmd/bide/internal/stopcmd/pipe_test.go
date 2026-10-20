package stopcmd

import (
	"strings"
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

// "-" takes the policy from standard input: the error is about its content, not about the path.
func TestLoadPolicy_Stdin(t *testing.T) {
	safefiletest.Stdin(t, `{"no_such_member":1}`)
	c := &cmd{f: flags{stopPolicy: "-"}}
	_, _, err := c.loadPolicy()
	if err == nil || strings.Contains(err.Error(), "regular file") || strings.Contains(err.Error(), "no such file") {
		t.Fatalf("loadPolicy(-) = %v, want a complaint about the policy read from standard input", err)
	}
}
