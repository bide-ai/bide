package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/repo is a small tree: a .go file whose every line names a removed API, one that names
// only current API and English uses of the same words, a README with one removed name, and the
// excluded paths, which name removed API freely. want.txt lists the findings, "file:line: name",
// outside the files it describes, so a name written in an expectation is never what matched. The
// Go files are stored as .go.in, so gofmt and go vet skip them; copyTree restores their names.

func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "want.txt" {
			return nil
		}
		out := filepath.Join(dst, strings.TrimSuffix(rel, ".in"))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestCheckTree(t *testing.T) {
	findings, err := CheckTree(copyTree(t, "testdata/repo"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{} // "file:line" -> the names found there
	for _, f := range findings {
		parts := strings.SplitN(f, ": ", 3)
		got[parts[0]] = append(got[parts[0]], parts[1])
	}
	data, err := os.ReadFile("testdata/repo/want.txt")
	if err != nil {
		t.Fatal(err)
	}
	wants := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(wants) < 40 {
		t.Errorf("only %d expectations: the fixture lost its cases", len(wants))
	}
	for _, w := range wants {
		pos, name, _ := strings.Cut(w, ": ")
		if g := got[pos]; len(g) != 1 || !strings.HasPrefix(g[0], name) {
			t.Errorf("%s: found %q, want one finding naming %q", pos, g, name)
		}
		delete(got, pos)
	}
	for pos, g := range got {
		t.Errorf("%s: unexpected findings %q", pos, g)
	}
}

// Every pattern is exercised by a want line, so none can stop matching unnoticed.
func TestEveryPatternHasACase(t *testing.T) {
	bad, err := os.ReadFile("testdata/repo/pkg/bad.go.in")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(patterns, transitional) {
		if !p.re.Match(bad) {
			t.Errorf("no case in bad.go.in for %v", p.re)
		}
	}
}

func TestRun_ExitStatus(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-root", copyTree(t, "testdata/repo")}, &out, &errOut); code != 1 {
		t.Fatalf("run over the fixture = %d, want 1 (findings); stderr %s", code, errOut.String())
	}
	clean := t.TempDir()
	if err := os.WriteFile(filepath.Join(clean, "x.go"), []byte("package x\n\n// Run drives a run.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"-root", clean}, &out, &errOut); code != 0 {
		t.Fatalf("run over a clean tree = %d, want 0; output %s", code, out.String())
	}
}
