package browsertest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// launchDirs are the directories of the scripts the tests run through Run: the editor's, the run
// viewer's and the approve page's.
var launchDirs = []string{"flowedit/browser", "internal/runview/browser", "approvals/page/test/chrome"}

// TestEveryScriptLaunchesInTheRunsProfile: a script that launches Chrome without the run's profile
// directory starts a Chrome the harness cannot find after a stall, and the leak is silent. Every
// puppeteer.launch call in the scripts names userDataDir: env.BIDE_CHROME_PROFILE.
func TestEveryScriptLaunchesInTheRunsProfile(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	const want = "userDataDir: env." + ProfileEnv
	launches, files := 0, 0
	for _, dir := range launchDirs {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(p) != ".mjs" {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files++
			rest := string(src)
			for {
				i := strings.Index(rest, "puppeteer.launch(")
				if i < 0 {
					return nil
				}
				rest = rest[i+len("puppeteer.launch("):]
				call := rest
				if j := strings.Index(call, "})"); j >= 0 {
					call = call[:j]
				}
				launches++
				if !strings.Contains(call, want) {
					rel, _ := filepath.Rel(root, p)
					t.Errorf("%s launches Chrome without %q: a stalled run would leak that Chrome", filepath.ToSlash(rel), want)
				}
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 20 || launches < 20 {
		t.Errorf("found %d launch calls in %d scripts; the script directories moved, so update launchDirs", launches, files)
	}
}
