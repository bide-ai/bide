package govern_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestGovernImportsNoCoreInternal locks the module boundary between govern and the core. govern is
// its own module, versioned apart from the core, so it may use only the core's exported API: a
// package under the core's internal/ has no compatibility promise, and importing one would tie a
// govern release to one exact core commit. Go's internal rule does not stop it (govern's import
// path is under the core's), so this test does. It checks the direct imports of every package in
// the module, test imports included.
func TestGovernImportsNoCoreInternal(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	out, err := exec.Command("go", "list", "-f",
		`{{.ImportPath}}: {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`, "./...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	pkgs := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(pkgs) < 2 {
		t.Fatalf("go list found %d packages, want govern and eventlogtest at least:\n%s", len(pkgs), out)
	}
	for _, line := range pkgs {
		pkg, imports, _ := strings.Cut(line, ": ")
		for imp := range strings.FieldsSeq(imports) {
			if imp == "github.com/bide-ai/bide/internal" || strings.HasPrefix(imp, "github.com/bide-ai/bide/internal/") {
				t.Errorf("%s imports the core's %s; govern may use only the core's exported API", pkg, imp)
			}
		}
	}
}
