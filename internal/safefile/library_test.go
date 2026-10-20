package safefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ReadOrStdin owns standard input for the whole process, so only the command-line packages may call
// it: never a library (theme, plan, the agent and audit packages) nor a server handler (flowedit).
func TestReadOrStdinIsCalledFromCommandLinePackagesOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := []string{
		filepath.Join("cmd") + string(filepath.Separator),
		filepath.Join("internal", "runview", "cli.go"),
		filepath.Join("internal", "safefile") + string(filepath.Separator),
	}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), "safefile.ReadOrStdin(") {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for _, ok := range allowed {
			if strings.HasPrefix(rel, ok) {
				return nil
			}
		}
		t.Errorf("%s calls safefile.ReadOrStdin, which only command-line code may", rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
