package runview

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestThemeJS runs the theme script against a fake page under Node (testdata/theme_js_test.mjs). It
// is skipped where node is not installed, and fails in CI, where the Node setup is the job's to
// provide.
func TestThemeJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed: the theme script test needs it")
		}
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/theme_js_test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("theme_js_test.mjs: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "theme.js ok") {
		t.Fatalf("theme_js_test.mjs did not finish: %s", out)
	}
}
