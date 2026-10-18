package apiref

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite docs/reference/api/ from the Go sources")

// repoRoot is the repository root, relative to this directory.
const repoRoot = "../../.."

// TestAPIReference: docs/reference/api/ is what the Go sources document: every exported symbol of
// every published package. It fails when a symbol, a signature or a doc sentence changes and the
// pages were not regenerated, so the reference cannot drift. Regenerate with
//
//	go test ./internal/tools/apiref -run TestAPIReference -update   (from the repository root)
func TestAPIReference(t *testing.T) {
	mods, err := Modules(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if u := Undocumented(mods); len(u) > 0 {
		t.Fatalf("%d exported symbols have no doc comment (doccheck reports them too):\n%s", len(u), strings.Join(u, "\n"))
	}
	want, err := Render(mods, Default)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repoRoot, filepath.FromSlash(PageDir))
	if *update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		old, _ := filepath.Glob(filepath.Join(dir, "*.md"))
		for _, f := range old {
			if _, ok := want[filepath.Base(f)]; !ok {
				if err := os.Remove(f); err != nil {
					t.Fatal(err)
				}
			}
		}
		for name, content := range want {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	have, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	var names []string
	for _, f := range have {
		names = append(names, filepath.Base(f))
	}
	var wantNames []string
	for n := range want {
		wantNames = append(wantNames, n)
	}
	sort.Strings(names)
	sort.Strings(wantNames)
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("%s holds %v, want %v: run go test ./internal/tools/apiref -run TestAPIReference -update and commit the result", PageDir, names, wantNames)
	}
	for _, n := range wantNames {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		// A Windows checkout may hold the pages with CRLF line endings (core.autocrlf).
		if strings.ReplaceAll(string(b), "\r\n", "\n") != want[n] {
			t.Errorf("%s/%s is out of date: run go test ./internal/tools/apiref -run TestAPIReference -update and commit the result", PageDir, n)
		}
	}
}

// TestPackageLabels: every documented package has a label and every label names a documented
// package; every symbol rule names a package, files and symbols that exist.
func TestPackageLabels(t *testing.T) {
	mods, err := Modules(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]*Package{}
	for _, m := range mods {
		for _, p := range m.Packages {
			have[p.Rel] = p
			if _, ok := Labels[p.Rel]; !ok {
				t.Errorf("package %s has no label: add it to Labels in labels.go and to the table in docs/trust/stability.md", p.Rel)
			}
		}
	}
	for rel, l := range Labels {
		if have[rel] == nil {
			t.Errorf("label for %s, which is not a documented package", rel)
		}
		switch l {
		case Core, Experimental, BuildingBlock:
		default:
			t.Errorf("label %q of %s is not one of the three labels", l, rel)
		}
	}
	for _, r := range SymbolRules {
		p := have[r.Package]
		if p == nil {
			t.Errorf("symbol rule for %s, which is not a documented package", r.Package)
			continue
		}
		files, names := map[string]bool{}, map[string]bool{}
		p.each(func(_, name, _, file string) {
			files[file] = true
			names[strings.SplitN(name, ".", 2)[0]] = true
		})
		for _, f := range r.Files {
			if !files[f] {
				t.Errorf("symbol rule for %s: no exported symbol is declared in %s", r.Package, f)
			}
		}
		for _, n := range r.Names {
			if !names[n] {
				t.Errorf("symbol rule for %s: no exported symbol %s", r.Package, n)
			}
		}
	}
}

// TestStabilityPage: the table in docs/trust/stability.md is the label table of labels.go, row for
// row, so the page and the API reference cannot disagree.
func TestStabilityPage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "docs", "trust", "stability.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n## Package stability\n")
	if !ok {
		t.Fatal("docs/trust/stability.md has no \"## Package stability\" section")
	}
	if i := strings.Index(after, "\n## "); i >= 0 {
		after = after[:i]
	}
	got := map[string]string{}
	for _, line := range strings.Split(after, "\n") {
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) != 2 || !strings.HasPrefix(cells[0], "`") {
			continue
		}
		pkg := strings.Trim(cells[0], "`")
		if _, dup := got[pkg]; dup {
			t.Errorf("stability.md lists %s twice", pkg)
		}
		got[pkg] = cells[1]
	}
	for pkg, l := range Labels {
		if got[pkg] != l {
			t.Errorf("stability.md says %q for %s, labels.go says %q", got[pkg], pkg, l)
		}
	}
	for pkg := range got {
		if _, ok := Labels[pkg]; !ok {
			t.Errorf("stability.md lists %s, which labels.go does not", pkg)
		}
	}
}

// TestCoreStaysCore: symbols that sit beside experimental ones or are part of a frozen contract
// keep the label of their package in the generated reference, so a symbol rule cannot sweep them.
func TestCoreStaysCore(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "docs", "reference", "api", "agent.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"RedactURLs", "WithToolErrorRedactor", "LeafHash", "NewTombstone", "TombstoneLeafHash", "TombstoneMillis", "TombstoneName"} {
		found := false
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "- `func "+n+"(") {
				found = true
				if strings.Contains(line, "(experimental)") {
					t.Errorf("%s is labelled experimental; it is the tool-error redactor or the tombstone format and stays core", n)
				}
			}
		}
		if !found {
			t.Errorf("agent.%s is not in the generated reference", n)
		}
	}
}
