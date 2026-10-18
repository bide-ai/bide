package awss3_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/audit"
)

func writeObject(t *testing.T, dir string, k keys, h audit.SignedTreeHead) {
	t.Helper()
	key, body := keyAndBody(t, k, h)
	p := filepath.Join(dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Nothing to verify must never read as verified.
func TestLoadDirRefusesAnEmptyDirectory(t *testing.T) {
	k := newKeys(t, 1)
	if hs, err := awss3.LoadDir(t.TempDir(), k.v); !errors.Is(err, awss3.ErrNotFound) || hs != nil {
		t.Fatalf("LoadDir of an empty directory = %v, %v; want ErrNotFound and no heads", hs, err)
	}
}

func TestLoadDirFlagsEquivocation(t *testing.T) {
	k := newKeys(t, 1)
	dir := t.TempDir()
	writeObject(t, dir, k, k.headRoot(t, "r", 4, 1, "history A"))
	writeObject(t, dir, k, k.headRoot(t, "r", 4, 2, "history B"))
	if _, err := awss3.LoadDir(dir, k.v); !errors.Is(err, awss3.ErrEquivocation) {
		t.Fatalf("LoadDir with two histories = %v; want ErrEquivocation", err)
	}
	// The same root at one size (two writers around a lease handoff) and equal sizes in different trees
	// are not equivocation.
	ok := t.TempDir()
	writeObject(t, ok, k, k.headRoot(t, "r", 4, 1, "same"))
	writeObject(t, ok, k, k.headRoot(t, "r", 4, 2, "same"))
	writeObject(t, ok, k, k.headRoot(t, "q", 4, 1, "other"))
	if hs, err := awss3.LoadDir(ok, k.v); err != nil || len(hs) != 3 {
		t.Fatalf("LoadDir = %d heads, %v; want 3 and no error", len(hs), err)
	}
}
