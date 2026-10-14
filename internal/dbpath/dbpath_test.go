package dbpath

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonical_ResolvesEverySpelling(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "real")
	os.Mkdir(real, 0o755)
	file := filepath.Join(real, "j.db")
	want, err := Canonical(file, true)
	if err != nil || want != file {
		t.Fatalf("Canonical(%q) = %q, %v", file, want, err)
	}
	spellings := []string{filepath.Join(real, "..", "real", "j.db")}
	linkDir, linkFile := filepath.Join(dir, "ld"), filepath.Join(dir, "lf.db")
	if os.Symlink(real, linkDir) == nil && os.Symlink(file, linkFile) == nil {
		spellings = append(spellings, filepath.Join(linkDir, "j.db"), linkFile)
	}
	for _, p := range spellings {
		if got, err := Canonical(p, true); err != nil || got != want {
			t.Errorf("Canonical(%q) = %q, %v; want %q", p, got, err, want)
		}
	}
	old, _ := os.Getwd()
	defer os.Chdir(old)
	os.Chdir(real)
	if got, err := Canonical("j.db", true); err != nil || got != want {
		t.Errorf("Canonical(relative) = %q, %v; want %q", got, err, want)
	}
}

func TestCanonical_KeepsPrefixAndQueryAndMemory(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	f := filepath.Join(dir, "q.db")
	got, err := Canonical("file:"+f+"?mode=rwc", true)
	if err != nil || got != "file:"+f+"?mode=rwc" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, m := range []string{":memory:", "file:m?mode=memory&cache=shared"} {
		if got, err := Canonical(m, true); err != nil || got != m {
			t.Errorf("Canonical(%q) = %q, %v", m, got, err)
		}
	}
}

func TestCanonical_CreatesOnlyWhenAsked(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	f := filepath.Join(dir, "n.db")
	got, err := Canonical(f, false)
	if err != nil || got != f {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := os.Stat(f); err == nil {
		t.Fatal("created the file though create was false")
	}
	if _, err := Canonical(f, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatal("did not create the file")
	}
}

// A file with a second hard link is refused: it has no canonical name.
func TestCanonical_RefusesHardLinks(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.db")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(f, filepath.Join(dir, "b.db")); err != nil {
		t.Skip("no hard links:", err)
	}
	for _, p := range []string{f, filepath.Join(dir, "b.db")} {
		_, err := Canonical(p, true)
		if !errors.Is(err, ErrHardLinked) || !strings.Contains(err.Error(), "howtocorrupt") {
			t.Errorf("Canonical(%q) = %v, want ErrHardLinked naming the SQLite page", p, err)
		}
	}
}
