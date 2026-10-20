package runview

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/safefile"
)

// A directory named as one of those files is refused, on every platform, with the reason.
func TestReaders_RefuseADirectory(t *testing.T) {
	dir := t.TempDir()
	var v map[string]any
	var refused *safefile.RefusedError
	if err := readStrict(dir, &v); !errors.As(err, &refused) || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("readStrict(dir) = %v, want a refusal", err)
	}
	if _, err := readKey(dir); !errors.As(err, &refused) || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("readKey(dir) = %v, want a refusal", err)
	}
}

// The size limits stay: readStrict keeps maxInput's wording, and a public key file past 1 MiB is
// refused.
func TestReaders_SizeLimits(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(p, []byte(strings.Repeat(" ", maxKeyFile+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(p); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("readKey of an oversize file = %v", err)
	}
}
