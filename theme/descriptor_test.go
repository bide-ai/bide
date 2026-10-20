//go:build unix

package theme

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// Load is a library entry point and reads a regular file only: a path a host got from its own caller
// that names an open descriptor (what a process substitution expands to), a named pipe or standard
// input is refused, and nothing is read from it, so its content cannot reach an error message.
func TestLoad_RefusesDescriptorsAndPipes(t *testing.T) {
	for name, p := range map[string]string{
		"descriptor": safefiletest.Descriptor(t, `{"HOSTSECRET-api-key-123":1}`),
		"stdin":      "/dev/stdin",
		"fifo":       safefiletest.FIFO(t),
	} {
		_, _, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") || strings.Contains(err.Error(), "HOSTSECRET") {
			t.Errorf("Load(%s %s) = %v, want a refusal that does not read it", name, p, err)
		}
	}
}
