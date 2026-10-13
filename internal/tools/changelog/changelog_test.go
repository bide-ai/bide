package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `# Changelog

## [Unreleased]

` + Pointer + `

## [0.15.0] - 2026-10-13

### Added

- old entry [#5]

[Unreleased]: https://github.com/o/r/compare/v0.15.0...HEAD
[0.15.0]: https://github.com/o/r/compare/v0.14.1...v0.15.0

[#4]: https://github.com/o/r/pull/4
[#5]: https://github.com/o/r/pull/5

[abc1234]: https://github.com/o/r/commit/abc1234
`

func frag(t *testing.T, name, content string) Fragment {
	t.Helper()
	f, err := ParseFragment(name, content)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseFragment(t *testing.T) {
	f := frag(t, "a.md", "Section: Fixed\n\nA fix, see [#9].\n")
	if f.Section != "Fixed" || f.Text != "A fix, see [#9]." {
		t.Fatalf("got %+v", f)
	}
	for name, bad := range map[string]string{
		"no header":      "A fix\n",
		"unknown":        "Section: Misc\n\ntext\n",
		"lowercase":      "Section: fixed\n\ntext\n",
		"no blank":       "Section: Fixed\ntext\n",
		"empty":          "Section: Fixed\n\n\n",
		"two paragraphs": "Section: Fixed\n\none\n\ntwo\n",
		"bullet":         "Section: Fixed\n\n- one\n",
		"empty file":     "",
	} {
		if _, err := ParseFragment("x.md", bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSectionOrderAndGrouping(t *testing.T) {
	got := Section([]Fragment{
		frag(t, "b.md", "Section: Fixed\n\nfix b\n"),
		frag(t, "a.md", "Section: Security\n\nsec\n"),
		frag(t, "c.md", "Section: Added\n\nadd c\n"),
		frag(t, "d.md", "Section: Added\n\nadd d\nsecond line\n"),
		frag(t, "e.md", "Section: Removed\n\nrm\n"),
		frag(t, "f.md", "Section: Changed\n\nchg\n"),
	})
	want := "### Added\n\n- add c\n- add d\n  second line\n\n### Changed\n\n- chg\n\n### Fixed\n\n- fix b\n\n### Removed\n\n- rm\n\n### Security\n\n- sec\n\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAssemble(t *testing.T) {
	frags := []Fragment{
		frag(t, "a.md", "Section: Added\n\nnew thing [#7] and [#5] and [#6]\n"),
		frag(t, "b.md", "Section: Fixed\n\nfixed\n"),
	}
	got, err := Assemble(sample, frags, "v0.16.0", "2026-10-20")
	if err != nil {
		t.Fatal(err)
	}
	want := `# Changelog

## [Unreleased]

` + Pointer + `

## [0.16.0] - 2026-10-20

### Added

- new thing [#7] and [#5] and [#6]

### Fixed

- fixed

## [0.15.0] - 2026-10-13

### Added

- old entry [#5]

[Unreleased]: https://github.com/o/r/compare/v0.16.0...HEAD
[0.16.0]: https://github.com/o/r/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/o/r/compare/v0.14.1...v0.15.0

[#4]: https://github.com/o/r/pull/4
[#5]: https://github.com/o/r/pull/5
[#6]: https://github.com/o/r/pull/6
[#7]: https://github.com/o/r/pull/7

[abc1234]: https://github.com/o/r/commit/abc1234
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// Assembling again with the same version is refused.
	if _, err := Assemble(got, frags, "0.16.0", "2026-10-20"); err == nil {
		t.Error("a version already in the changelog was accepted")
	}
}

func TestAssembleRefuses(t *testing.T) {
	f := []Fragment{frag(t, "a.md", "Section: Added\n\nx\n")}
	direct := strings.Replace(sample, Pointer, "### Added\n\n- direct entry", 1)
	if _, err := Assemble(direct, f, "0.16.0", "2026-10-20"); err == nil || !strings.Contains(err.Error(), "[Unreleased]") {
		t.Errorf("entries under Unreleased: err = %v", err)
	}
	if _, err := Assemble(sample, nil, "0.16.0", "2026-10-20"); err == nil {
		t.Error("no fragments was accepted")
	}
	if _, err := Assemble(sample, f, "0.16", "2026-10-20"); err == nil {
		t.Error("a bad version was accepted")
	}
	if _, err := Assemble(sample, f, "0.16.0", "20 Oct"); err == nil {
		t.Error("a bad date was accepted")
	}
	if _, err := Assemble(strings.Replace(sample, "[Unreleased]: ", "[Unreleased-x]: ", 1), f, "0.16.0", "2026-10-20"); err == nil {
		t.Error("a changelog without the Unreleased link was accepted")
	}
}

func TestNeedsFragment(t *testing.T) {
	for _, c := range []struct {
		files []string
		want  bool
	}{
		{[]string{"agent/run.go"}, true},
		{[]string{"agent/run.go", "docs/x.md"}, true},
		{[]string{"cmd/bide/main.go"}, true},
		{[]string{"store/sqlite/store.go"}, true},
		{[]string{"agent/run_test.go"}, false},
		{[]string{"agent/testdata/x.json"}, false},
		{[]string{"docs/guides/a.md", "README.md", "docs-site/x.mjs"}, false},
		{[]string{".github/workflows/ci.yml", ".github/pull_request_template.md"}, false},
		{[]string{"examples/plan/main.go", "integration/x.go", "benchmarks/y.go"}, false},
		{[]string{"scripts/release.sh", "internal/tools/doccheck/doccheck.go"}, false},
		{[]string{"changes/unreleased/x.md", "CHANGELOG.md"}, false},
		{[]string{"go.mod", "store/sqlite/go.sum"}, false},
		{[]string{"internal/journal/x.go"}, true},
		{[]string{"spec/tla/claims/Claims.tla"}, false},
	} {
		if got := NeedsFragment(c.files); got != c.want {
			t.Errorf("NeedsFragment(%v) = %v, want %v", c.files, got, c.want)
		}
	}
}

func TestHasFragment(t *testing.T) {
	if !HasFragment([]string{"agent/a.go", "changes/unreleased/x.md"}) {
		t.Error("a fragment was not found")
	}
	for _, f := range []string{"changes/unreleased/.gitkeep", "changes/unreleased/sub/x.md", "changes/README.md", "x/changes/unreleased/y.md"} {
		if HasFragment([]string{f}) {
			t.Errorf("%s counted as a fragment", f)
		}
	}
}

func TestReadFragments(t *testing.T) {
	root := t.TempDir()
	if got, err := ReadFragments(root); err != nil || got != nil {
		t.Fatalf("missing dir: %v, %v", got, err)
	}
	dir := filepath.Join(root, FragmentDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitkeep", "")
	write("b.md", "Section: Fixed\n\nb\n")
	write("a.md", "Section: Added\n\na\n")
	got, err := ReadFragments(root)
	if err != nil || len(got) != 2 || got[0].Name != "a.md" {
		t.Fatalf("got %v, %v", got, err)
	}
	write("bad.md", "nope\n")
	if _, err := ReadFragments(root); err == nil || !strings.Contains(err.Error(), "bad.md") {
		t.Fatalf("a malformed fragment: err = %v", err)
	}
}

func TestRunAssemble(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, FragmentDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte(sample), 0o644)
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("Section: Added\n\nx\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitkeep"), nil, 0o644)
	var out, errb bytes.Buffer
	if code := run([]string{"assemble", "-root", root, "-version", "0.16.0", "-date", "2026-10-20", "-n"}, &out, &errb); code != 0 || out.String() != "### Added\n\n- x\n\n" {
		t.Fatalf("-n: code %d, out %q, err %q", code, out.String(), errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "a.md")); err != nil {
		t.Fatal("-n removed a fragment")
	}
	if code := run([]string{"assemble", "-root", root, "-version", "0.16.0", "-date", "2026-10-20"}, &out, &errb); code != 0 {
		t.Fatalf("code %d, err %q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "a.md")); !os.IsNotExist(err) {
		t.Error("the fragment was not deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitkeep")); err != nil {
		t.Error(".gitkeep was deleted")
	}
	b, _ := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if !strings.Contains(string(b), "## [0.16.0] - 2026-10-20\n\n### Added\n\n- x\n") {
		t.Errorf("CHANGELOG.md:\n%s", b)
	}
}

// TestRunCheck runs the pull request gate against a scratch repository.
func TestRunCheck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	put := func(name, s string) {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	put("agent/a.go", "package agent\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	check := func(labels string) int {
		head := git("rev-parse", "HEAD")
		var out, errb bytes.Buffer
		return run([]string{"check", "-root", root, "-base", base, "-head", head, "-labels", labels}, &out, &errb)
	}

	put("docs/x.md", "x\n")
	put("agent/a_test.go", "package agent\n")
	git("add", ".")
	git("commit", "-q", "-m", "docs and tests")
	if c := check(""); c != 0 {
		t.Errorf("docs and tests only: exit %d", c)
	}
	put("agent/a.go", "package agent\n\nvar X = 1\n")
	git("add", ".")
	git("commit", "-q", "-m", "code")
	if c := check(""); c != 1 {
		t.Errorf("code without a fragment: exit %d, want 1", c)
	}
	if c := check("bug, no-changelog"); c != 0 {
		t.Errorf("code with the no-changelog label: exit %d", c)
	}
	put(FragmentDir+"/x.md", "Section: Fixed\n\nx\n")
	git("add", ".")
	git("commit", "-q", "-m", "fragment")
	if c := check(""); c != 0 {
		t.Errorf("code with a fragment: exit %d", c)
	}
}
