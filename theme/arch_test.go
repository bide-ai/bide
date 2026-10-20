package theme

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoThirdPartyImports pins that the public theme package imports only the standard library and
// the module's own internal helpers: a host that embeds it takes on no third-party dependency.
func TestNoThirdPartyImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") && !strings.HasPrefix(p, "github.com/bide-ai/bide/internal/") {
				t.Errorf("%s imports %s: the theme package may import only the standard library and bide's internal packages", f, p)
			}
		}
	}
}
