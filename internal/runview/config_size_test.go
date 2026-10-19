package runview

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A flow config is read up to the size plan.LoadReader reads (16 MiB), not the 256 MiB an audit
// input is: one byte more is refused naming the limit, exactly that many is read.
func TestReadConfig_SizeLimit(t *testing.T) {
	dir := t.TempDir()
	at := filepath.Join(dir, "at.json")
	over := filepath.Join(dir, "over.json")
	if err := os.WriteFile(at, []byte(strings.Repeat(" ", 16<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte(strings.Repeat(" ", 16<<20+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := readConfig(at, nil); err != nil || len(c.Data) != 16<<20 {
		t.Fatalf("a config of exactly 16 MiB: %d bytes, %v", len(c.Data), err)
	}
	if _, err := readConfig(over, nil); err == nil || !strings.Contains(err.Error(), "larger than 16777216 bytes") {
		t.Fatalf("a config of 16 MiB + 1 = %v, want a size error", err)
	}
}

// -diff <rev> reads the old config from git show under the same limit.
func TestReadOld_GitRevisionSizeLimit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %q: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "big.json"), []byte(strings.Repeat(" ", 16<<20+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "big")
	t.Chdir(repo)
	if _, err := readOld("HEAD:big.json", "", nil); err == nil || !strings.Contains(err.Error(), "larger than 16777216 bytes") {
		t.Fatalf("readOld of a revision of 16 MiB + 1 = %v, want a size error", err)
	}
}
