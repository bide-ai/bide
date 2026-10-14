package initcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteFailures: a project that cannot be written is exit 1 with the reason, whether the
// directory cannot be made or cannot be read; a directory that exists and is empty is fine.
func TestWriteFailures(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"below a file": filepath.Join(f, "proj"), "a file itself": f} {
		code, out := initCmd("v0.14.0", "-module", "example.com/p", dir)
		if code != 1 || !strings.Contains(out, "bide init:") {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
	}
	empty := t.TempDir()
	if code, out := initCmd("v0.14.0", "-module", "example.com/p", empty); code != 0 {
		t.Errorf("an empty directory: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(empty, "go.mod")); err != nil {
		t.Errorf("go.mod not written into the empty directory: %v", err)
	}
}

// TestDevNotACheckout: -dev must name a bide checkout; anything else is a usage error that says so
// and writes nothing.
func TestDevNotACheckout(t *testing.T) {
	proj := filepath.Join(t.TempDir(), "p")
	code, out := initCmd("", "-module", "example.com/p", "-dev", t.TempDir(), proj)
	if code != 2 || !strings.Contains(out, "is not a bide checkout") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(proj); err == nil {
		t.Error("a project was written")
	}
}
