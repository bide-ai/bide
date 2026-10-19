package theme

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestIdleIsNotText: the design holds idle to 3:1 (C2) because it is drawn as a stroke, a border or
// a glyph and never as text. A text colour needs 4.5:1, so a page that sets color to var(--idle)
// on text must move that use to a token that has it. The one color use today is an icon.
func TestIdleIsNotText(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{".empty .ic": true} // a 40px icon, drawn with currentColor
	colorIdle := regexp.MustCompile(`(?:^|[;\s])color:\s*var\(--idle\)`)
	found := 0
	for _, dir := range []string{"internal/runview/assets", "flowedit/page", "approvals/page"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".css") {
				return err
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, rule := range strings.Split(string(src), "}") {
				sel, body, ok := strings.Cut(rule, "{")
				if !ok || !colorIdle.MatchString(body) {
					continue
				}
				found++
				sel = strings.TrimSpace(sel)
				if i := strings.LastIndex(sel, "*/"); i >= 0 {
					sel = strings.TrimSpace(sel[i+2:])
				}
				if !allowed[sel] {
					t.Errorf("%s: %q sets color to var(--idle); idle is held to 3:1 and is not for text", filepath.ToSlash(p[len(root)+1:]), sel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if found == 0 {
		t.Error("the scan found no use of color: var(--idle); the allow-listed icon moved, so update the test")
	}
}
