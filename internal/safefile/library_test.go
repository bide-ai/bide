package safefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ReadFlag and ReadOrStdin read any descriptor they are handed and own standard input for the whole
// process, so only command-line code may call them: never a library (theme, notify, plan, the agent
// and audit packages) nor a server handler (flowedit, the viewer's description reader).
func TestFlagReadersAreCalledFromCommandLineCodeOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := []string{
		filepath.Join("cmd") + string(filepath.Separator),
		filepath.Join("internal", "runview", "cli.go"),
		filepath.Join("internal", "policyfile") + string(filepath.Separator),
		filepath.Join("internal", "identitypreflight") + string(filepath.Separator),
		filepath.Join("internal", "safefile") + string(filepath.Separator),
	}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		src := string(b)
		if !strings.Contains(src, "safefile.ReadOrStdin(") && !strings.Contains(src, "safefile.ReadFlag(") && !strings.Contains(src, "safefile.LoadX509KeyPairFlags(") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		for _, ok := range allowed {
			if strings.HasPrefix(rel, ok) {
				return nil
			}
		}
		t.Errorf("%s calls a flag reader of safefile, which only command-line code may", rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
