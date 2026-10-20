package theme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadRefusals: Load is public, so a path that is not a theme file is refused with a reason,
// whatever it names.
func TestLoadRefusals(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, append(custom("dark", nil), []byte(strings.Repeat(" ", MaxSize))...), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ path, want string }{
		"a directory": {dir, "not a regular file"},
		"too large":   {big, "larger than"},
		"missing":     {filepath.Join(dir, "nope.json"), ""},
	} {
		_, _, err := Load(tc.path)
		if err == nil || !strings.Contains(err.Error(), tc.want) { // want "" accepts any error
			t.Errorf("%s: Load = %v, want an error with %q", name, err, tc.want)
		}
	}
	ok := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(ok, custom("dark", surface("accent", "#79b8ff")), 0o600); err != nil {
		t.Fatal(err)
	}
	if th, _, err := Load(ok); err != nil || th.Name() != "acme-test" {
		t.Errorf("Load of a good file = %v, %v", th, err)
	}
}
