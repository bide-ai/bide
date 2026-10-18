package anchorobj_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/anchorobj"
)

var errNone = errors.New("test: no heads")

func load(dir string, v audit.Verifier) ([]audit.SignedTreeHead, error) {
	return testLayout.LoadDir(dir, v, errNone, errEquiv)
}

// write stores a head's object in dir as an export of the bucket would: <partition>/<name>.
func write(t *testing.T, dir string, sth audit.SignedTreeHead) string {
	t.Helper()
	return writeUnder(t, dir, "p", sth)
}

// writeUnder is write with the exported prefix directory chosen, since an export may be assembled from
// several downloads.
func writeUnder(t *testing.T, dir, parent string, sth audit.SignedTreeHead) string {
	t.Helper()
	partition, name, body := encoded(t, testLayout, sth)
	p := filepath.Join(dir, parent, partition, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDirReturnsVerifiedHeadsByPartitionThenSize(t *testing.T) {
	k := newKeys(t, 1)
	dir := t.TempDir()
	write(t, dir, k.head(t, "r", 5, 1, "a"))
	write(t, dir, k.head(t, "r", 2, 1, "b"))
	write(t, dir, k.head(t, "q", 3, 1, "c"))
	hs, err := load(dir, k.v)
	if err != nil || len(hs) != 3 {
		t.Fatalf("LoadDir = %d heads, %v", len(hs), err)
	}
	for _, run := range []string{"r", "q"} { // within a run, increasing size
		last := -1
		for _, h := range hs {
			if h.RunID == run {
				if h.Size < last {
					t.Errorf("run %s: sizes not increasing", run)
				}
				last = h.Size
			}
		}
	}
}

// Nothing to verify must never read as verified.
func TestLoadDirRefusesAnEmptyDirectory(t *testing.T) {
	k := newKeys(t, 1)
	if hs, err := load(t.TempDir(), k.v); !errors.Is(err, errNone) || hs != nil {
		t.Fatalf("LoadDir of an empty directory = %v, %v; want the not-found error and no heads", hs, err)
	}
	// A directory of empty subdirectories is empty too.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := load(dir, k.v); !errors.Is(err, errNone) {
		t.Fatalf("LoadDir of empty subdirectories = %v", err)
	}
}

func TestLoadDirFlagsEquivocationPerPartition(t *testing.T) {
	k := newKeys(t, 1)
	dir := t.TempDir()
	write(t, dir, k.head(t, "r", 4, 1, "history A"))
	write(t, dir, k.head(t, "r", 4, 2, "history B"))
	if _, err := load(dir, k.v); !errors.Is(err, errEquiv) {
		t.Fatalf("LoadDir with two histories = %v; want the equivocation error", err)
	}
	// The same root at one size (two writers around a lease handoff) and equal sizes in different
	// trees are not equivocation.
	ok := t.TempDir()
	write(t, ok, k.head(t, "r", 4, 1, "same"))
	write(t, ok, k.head(t, "r", 4, 2, "same"))
	write(t, ok, k.head(t, "q", 4, 1, "other"))
	if hs, err := load(ok, k.v); err != nil || len(hs) != 3 {
		t.Fatalf("LoadDir = %d heads, %v; want 3 and no error", len(hs), err)
	}
	// Equivocation in a partition is found even when the other partitions are fine and sort around it.
	mixed := t.TempDir()
	write(t, mixed, k.head(t, "a", 1, 1, "x"))
	write(t, mixed, k.head(t, "m", 4, 1, "history A"))
	write(t, mixed, k.head(t, "m", 4, 2, "history B"))
	write(t, mixed, k.head(t, "z", 9, 1, "y"))
	if _, err := load(mixed, k.v); !errors.Is(err, errEquiv) {
		t.Fatalf("LoadDir = %v; want the equivocation error", err)
	}
}

func TestLoadDirRefusesWhatIsNotAnAnchor(t *testing.T) {
	k := newKeys(t, 1)
	good := k.head(t, "r", 3, 1, "a")
	if _, err := load(t.TempDir(), nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("nil verifier = %v; want agent.ErrConfig", err)
	}
	dir := t.TempDir()
	p := write(t, dir, good)
	if _, err := load(dir, newKeys(t, 2).v); !errors.Is(err, errBadSig) {
		t.Errorf("another key = %v; want the bad-signature error", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := load(dir, k.v); !errors.Is(err, errObject) {
		t.Errorf("stray file = %v; want the object error", err)
	}
	_ = os.Remove(filepath.Join(dir, "stray.txt"))
	body, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append([]byte(" "), body...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := load(dir, k.v); !errors.Is(err, errObject) {
		t.Errorf("modified body = %v", err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("x", anchorobj.MaxObjectBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := load(dir, k.v); !errors.Is(err, errObject) || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized file = %v", err)
	}
	// A head copied under another partition's directory is refused.
	moved := t.TempDir()
	_, name, b := encoded(t, testLayout, good)
	if err := os.MkdirAll(filepath.Join(moved, strings.Repeat("a", 64)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moved, strings.Repeat("a", 64), name), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := load(moved, k.v); !errors.Is(err, errObject) {
		t.Errorf("head under another partition = %v", err)
	}
	if runtime.GOOS != "windows" {
		// A symbolic link with a valid object name pointing at a valid object: refused, since a link
		// can point outside the exported set.
		link := t.TempDir()
		part, name, _ := encoded(t, testLayout, good)
		if err := os.MkdirAll(filepath.Join(link, part), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(link, part, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := load(link, k.v); !errors.Is(err, errObject) || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("symlink = %v; want the object error naming a non-regular file", err)
		}
	}
	if _, err := testLayout.LoadDir(dir, k.v, nil, errEquiv); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("nil not-found error = %v; want agent.ErrConfig", err)
	}
	if _, err := testLayout.LoadDir(dir, k.v, errNone, nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("nil equivocation error = %v; want agent.ErrConfig", err)
	}
}

// The heads of one partition need not sit together on disk: an export assembled from several downloads
// has them under different parents, with other partitions in between.
func TestLoadDirFindsEquivocationAcrossParentDirectories(t *testing.T) {
	k := newKeys(t, 1)
	dir := t.TempDir()
	writeUnder(t, dir, "p1", k.head(t, "r", 4, 1, "history A"))
	writeUnder(t, dir, "p1b", k.head(t, "q", 4, 1, "other"))
	writeUnder(t, dir, "p2", k.head(t, "r", 4, 2, "history B"))
	if _, err := load(dir, k.v); !errors.Is(err, errEquiv) {
		t.Fatalf("LoadDir = %v; want the equivocation error across parents", err)
	}
}
