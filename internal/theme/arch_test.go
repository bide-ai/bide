package theme

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImporters pins who may import internal/theme: the packages that draw the visual pages and
// the commands that serve them. The nested modules (flowedit, cmd/bide) reach an internal package
// of the root module only because their import paths sit under the root's; this test is what keeps
// the set from growing by accident until the package becomes public.
func TestImporters(t *testing.T) {
	allowed := []string{"internal/theme", "internal/runview", "internal/tools", "flowedit", "cmd/bide", "approvals"}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	const path = `"github.com/bide-ai/bide/internal/theme"`
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), path) {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		rel = filepath.ToSlash(rel)
		for _, a := range allowed {
			if rel == a || strings.HasPrefix(rel, a+"/") {
				return nil
			}
		}
		t.Errorf("%s imports internal/theme; allowed importers: %s", rel, strings.Join(allowed, ", "))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
