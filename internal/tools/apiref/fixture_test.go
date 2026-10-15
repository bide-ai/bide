package apiref

import (
	"strings"
	"testing"
)

const fixture = "testdata/repo"

var fixtureConfig = Config{
	Labels: map[string]string{"a": Stable, "a/sub": Experimental, "nested/b": Stable},
	Rules:  []SymbolRule{{Package: "a", Files: []string{"b.go"}, Names: []string{"Certish"}, Label: BuildingBlock}},
}

func fixtureModules(t *testing.T) []*Module {
	t.Helper()
	mods, err := Modules(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return mods
}

// TestModulesSelection: only published, non-internal, non-program packages are listed, from the
// module that owns them.
func TestModulesSelection(t *testing.T) {
	mods := fixtureModules(t)
	var got []string
	for _, m := range mods {
		for _, p := range m.Packages {
			got = append(got, m.Path+" "+p.ImportPath)
		}
	}
	want := "example.com/m example.com/m/a,example.com/m example.com/m/a/sub,example.com/m/nested example.com/m/nested/b"
	if strings.Join(got, ",") != want {
		t.Fatalf("packages = %v, want %s (internal, main, repo-only and other-OS files are left out; a nested module is listed under its own path)", got, want)
	}
}

// TestUndocumented: an exported symbol with no doc comment is reported.
func TestUndocumented(t *testing.T) {
	got := Undocumented(fixtureModules(t))
	if len(got) != 1 || got[0] != "example.com/m/a/sub Bare" {
		t.Fatalf("Undocumented = %q, want [example.com/m/a/sub Bare]", got)
	}
}

// TestRenderMissingLabel: a package with no label fails the generation, naming the package.
func TestRenderMissingLabel(t *testing.T) {
	_, err := Render(fixtureModules(t), Config{Labels: map[string]string{"a": Stable}})
	if err == nil || !strings.Contains(err.Error(), "example.com/m/a/sub has no stability label") {
		t.Fatalf("Render error = %v, want the missing label named", err)
	}
}

// TestRender: the page shows each kind of symbol as its declaration and first sentence, escapes
// what Markdown and Vue would read, and is the same on every run.
func TestRender(t *testing.T) {
	mods := fixtureModules(t)
	pages, err := Render(mods, fixtureConfig)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Render(fixtureModules(t), fixtureConfig)
	for n, p := range pages {
		if again[n] != p {
			t.Errorf("page %s differs between two runs", n)
		}
	}
	page := pages["a.md"]
	if page == "" {
		t.Fatalf("no page a.md; pages: %v", keys(pages))
	}
	for _, want := range []string{
		"# Package `a`",
		"[pkg.go.dev](https://pkg.go.dev/example.com/m/a) · Stability: **stable**",
		"Package a is the fixture: it shows each kind of symbol.\n",                // the first sentence only
		"- `const Fast Mode`, `const Slow Mode`: The modes.",                       // iota is not shown; Slow repeats the type of Fast
		"`const Limit = 10`: Limit is the most a thing may hold, e.g. 10 of them.", // e.g. is not a sentence end
		"`const LongText`: LongText has a value too long to show",                  // a long value is not shown
		"- `var ErrBad = errors.New(\"bad\")`: ErrBad is a failure with &lt;angle&gt; brackets, a \\*star and a\\_snake\\_name; see `Mode` and &#123;&#123;braces}}.",
		"## type Thing",
		"Thing is a thing, with an em dash.",
		"type Thing struct {\n\tName string\n\t// contains filtered or unexported fields\n}",
		"- `func NewThing(name string) *Thing`: NewThing builds a Thing.",
		"- `func (t *Thing) Run(m Mode) error`: Run runs the thing.",
		"type Pair[K comparable, V any] struct {",
		"- `func Map[T, U any](xs []T, f func(T) U) []U`: Map applies f.",
		"type Doer interface {\n\tDo() error\n}",
		"- `func Certish()`: Certish is a symbol declared in the file the rules label. *(building block, not yet used by approval gates)*",
		"- `func Labelled()`: Rules labels this file's symbols. *(building block, not yet used by approval gates)*",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page a.md lacks %q\n%s", want, page)
		}
	}
	for _, bad := range []string{"OnlyWindows", "hidden", "—", "R is exported", "B is exported"} {
		if strings.Contains(page, bad) {
			t.Errorf("page a.md contains %q", bad)
		}
	}
	idx := pages["index.md"]
	for _, want := range []string{"| `example.com/m` | 2 | one page per package, listed below |", "| [`a/sub`](a-sub.md) | experimental |"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index lacks %q\n%s", want, idx)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSentence: the first sentence, with abbreviations kept inside it.
func TestSentence(t *testing.T) {
	for in, want := range map[string]string{
		"Foo does x. Then y.":                   "Foo does x.",
		"Foo does x (e.g. y) and z. Then y.":    "Foo does x (e.g. y) and z.",
		"Foo does x, i.e. y, etc. and z. More.": "Foo does x, i.e. y, etc. and z.",
		"Foo has no period":                     "Foo has no period",
		"Foo does x.\n\nSecond paragraph.":      "Foo does x.",
		"Foo is 1.5 times. Next.":               "Foo is 1.5 times.",
		"Foo\nwraps over\nlines. Next.":         "Foo wraps over lines.",
		"":                                      "",
	} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}
