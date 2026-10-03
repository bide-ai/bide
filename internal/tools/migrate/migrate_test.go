package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// TestGolden migrates the cases of testdata/oldapi, a stub of bide v0.10's API, with every rule in
// order, as a user runs the tool, and compares each rewritten file, and the list of sites reported
// for a person, with the golden files under testdata/golden. Then it compiles the rewritten cases
// (all but cases/manual, whose sites the tool leaves) against testdata/newapi, a stub of the new
// API, with go vet: every rewrite must produce code that type-checks against the API it targets.
func TestGolden(t *testing.T) {
	t.Setenv("GOWORK", "off")
	old := copyTree(t, "testdata/oldapi")
	out, res, err := MigrateModule(old, []string{"./cases/..."}, Rules())
	if err != nil {
		t.Fatal(err)
	}
	// every case file, rewritten or not, has a golden file
	var files []string
	filepath.WalkDir(filepath.Join(old, "cases"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	for _, f := range files {
		rel, _ := filepath.Rel(old, f)
		got, ok := out[f]
		if !ok {
			got, _ = os.ReadFile(f)
		}
		golden(t, filepath.Join("testdata/golden", rel+".golden"), got)
	}
	var findings bytes.Buffer
	for _, f := range res.Findings {
		rel, _ := filepath.Rel(old, f.Pos.Filename)
		fmt.Fprintf(&findings, "%s:%d: %s: %s\n", filepath.ToSlash(rel), f.Pos.Line, f.Rule, f.Msg)
	}
	golden(t, "testdata/golden/findings.txt", findings.Bytes())

	// the rewritten cases compile against the new API
	nw := copyTree(t, "testdata/newapi")
	for _, f := range files {
		rel, _ := filepath.Rel(old, f)
		if strings.HasPrefix(rel, filepath.Join("cases", "manual")) {
			continue
		}
		src, ok := out[f]
		if !ok {
			src, _ = os.ReadFile(f)
		}
		dst := filepath.Join(nw, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "vet", "./cases/...")
	cmd.Dir = nw
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("the rewritten cases do not compile against the new API: %v\n%s", err, b)
	}
}

// TestIdempotent runs the tool again over its own output: a rebase reruns it over code that is
// already migrated, which it must leave alone.
func TestIdempotent(t *testing.T) {
	t.Setenv("GOWORK", "off")
	nw := copyTree(t, "testdata/newapi")
	// the migrated cases, as the golden files hold them
	filepath.WalkDir("testdata/golden/cases", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(p, string(filepath.Separator)+"manual"+string(filepath.Separator)) {
			return nil
		}
		rel, _ := filepath.Rel("testdata/golden", strings.TrimSuffix(p, ".golden"))
		b, _ := os.ReadFile(p)
		dst := filepath.Join(nw, rel)
		os.MkdirAll(filepath.Dir(dst), 0o755)
		os.WriteFile(dst, b, 0o644)
		return nil
	})
	out, res, err := MigrateModule(nw, []string{"./cases/..."}, Rules())
	if err != nil {
		t.Fatal(err)
	}
	for p := range out {
		t.Errorf("a second run rewrote %s", p)
	}
	for _, f := range res.Findings {
		t.Errorf("a second run reported %s", f)
	}
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// copyTree copies dir into a new temporary directory and returns its path.
func copyTree(t *testing.T, dir string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), filepath.Base(dir))
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestGoldenMarkdown migrates the Go blocks of a markdown document written against the old API,
// and compares the document with its golden file.
func TestGoldenMarkdown(t *testing.T) {
	t.Setenv("GOWORK", "off")
	old := copyTree(t, "testdata/oldapi")
	md := filepath.Join(old, "docs", "doc.md")
	out, res, err := MigrateMarkdown(old, []string{md}, Rules())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out[md]
	if !ok {
		t.Fatal("the document was not rewritten")
	}
	golden(t, "testdata/golden/docs/doc.md.golden", got)
	for _, f := range res.Findings {
		t.Errorf("finding: %s", f)
	}
}
