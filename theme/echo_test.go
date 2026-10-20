package theme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Load's error names the file and the problem but does not repeat a very long name from
// the file whole, nor write an escape sequence from it raw.
func TestLoad_ErrorDoesNotEchoALongNameOrEscape(t *testing.T) {
	dir := t.TempDir()
	for name, key := range map[string]string{
		"long name": "HOSTSECRET-" + strings.Repeat("k", 500),
		"escape":      `\u001b[31mred`,
	} {
		p := filepath.Join(dir, "t.json")
		if err := os.WriteFile(p, []byte(`{"`+key+`":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := Load(p)
		if err == nil {
			t.Errorf("%s: loaded", name)
			continue
		}
		if msg := err.Error(); len(msg) > 400 || strings.Contains(msg, strings.Repeat("k", 100)) || strings.Contains(msg, "\x1b") {
			t.Errorf("%s: error echoes the file (%d bytes): %.200q", name, len(msg), msg)
		}
	}
}
