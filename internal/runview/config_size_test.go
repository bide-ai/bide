package runview

import (
	"errors"

	"github.com/bide-ai/bide/agent"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	// A revision far over the limit is refused without being held in memory: git show is stopped
	// at the limit, not buffered whole first.
	if err := os.WriteFile(filepath.Join(repo, "huge.json"), []byte(strings.Repeat(" ", 96<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "huge")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := readOld("HEAD:huge.json", "", nil)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "larger than 16777216 bytes") {
		t.Fatalf("readOld of a 96 MiB revision = %v, want a size error", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 48<<20 {
		t.Fatalf("readOld of a 96 MiB revision allocated %d bytes: the blob was buffered whole", got)
	}
}

// endless yields spaces for ever and counts how many it handed over.
type endless struct{ n int }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	e.n += len(p)
	return len(p), nil
}

// A config input is read for at most one byte past the limit, and a longer one is refused with
// agent.ErrConfig: nothing is read to the end of an input that has none.
func TestReadConfigBytes_StopsAtTheLimit(t *testing.T) {
	r := &endless{}
	_, err := readConfigBytes(r)
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "larger than 16777216 bytes") {
		t.Fatalf("readConfigBytes(endless) = %v, want ErrConfig naming the limit", err)
	}
	// io.LimitReader hands over at most cap+1 bytes; the wrapper may have been asked for a buffer
	// more than that, but never for a second read after the limit.
	if r.n > 16<<20+1+64<<10 {
		t.Fatalf("read %d bytes of an endless input, want about %d", r.n, 16<<20+1)
	}
	if b, err := readConfigBytes(strings.NewReader("{}")); err != nil || string(b) != "{}" {
		t.Fatalf("readConfigBytes of a small input = %q, %v", b, err)
	}
}

// readConfig refuses a directory as it does a pipe, with agent.ErrConfig.
func TestReadConfig_RefusesADirectory(t *testing.T) {
	if _, err := readConfig(t.TempDir(), nil); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("readConfig(dir) = %v, want ErrConfig", err)
	}
}
