package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/internal/tools/docsnip/snip"
)

// The golden markdown is under testdata. A code line that must yield a finding ends with
// `// want "regexp"`; a finding elsewhere (at a directive) is listed in the test.

var wantComment = regexp.MustCompile(`// want "((?:[^"\\]|\\.)*)"\s*$`)

type want struct {
	pos  string
	re   *regexp.Regexp
	used bool
}

func readWants(t *testing.T, root string, files []string) []*want {
	t.Helper()
	var wants []*want
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			if m := wantComment.FindStringSubmatch(line); m != nil {
				re := strings.ReplaceAll(m[1], `\"`, `"`)
				wants = append(wants, &want{pos: f + ":" + strconv.Itoa(i+1), re: regexp.MustCompile(re)})
			}
		}
	}
	return wants
}

var (
	checkerOnce sync.Once
	checker     *Checker
	checkerErr  error
)

// newChecker type-checks against this repository's own packages, so the api and auto-import
// tests exercise the real agent package. Only the root module's packages are used, so it works
// with and without the workspace.
func newChecker(t *testing.T) *Checker {
	t.Helper()
	checkerOnce.Do(func() { checker, checkerErr = NewChecker("../../..") })
	if checkerErr != nil {
		t.Fatal(checkerErr)
	}
	return checker
}

func checkDir(t *testing.T, dir string) (*Report, []string) {
	t.Helper()
	files, err := markdownFiles(dir, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	var blocks []snip.Block
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		bs, err := snip.Extract(f, data)
		if err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, bs...)
	}
	r, err := newChecker(t).Check(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return r, files
}

func TestGolden(t *testing.T) {
	r, files := checkDir(t, "testdata")
	wants := readWants(t, "testdata", files)
	// Findings at a directive or with no code line to carry a want comment.
	extra := []*want{
		{pos: "broken.md:33", re: regexp.MustCompile(`^setup: undefined: agent\.Nope$`)},
		{pos: "broken.md:53", re: regexp.MustCompile(`^setup has a returns item, but the block is declarations, not statements$`)},
		{pos: "skip.md:13", re: regexp.MustCompile(`^stale skip: the block compiles; delete the skip directive$`)},
		{pos: "dup/a.md:7", re: regexp.MustCompile(`^undefined: missing \(the same block is also at dup/b\.md:7\)$`)},
	}
	wants = append(wants, extra...)
	for _, f := range r.Findings {
		pos := f.File + ":" + strconv.Itoa(f.Line)
		matched := false
		for _, w := range wants {
			if w.pos == pos && w.re.MatchString(f.Msg) {
				w.used, matched = true, true
			}
		}
		if !matched {
			t.Errorf("unexpected finding: %s", f)
		}
	}
	for _, w := range wants {
		if !w.used {
			t.Errorf("%s: no finding matches %q", w.pos, w.re)
		}
	}

	status := map[string]string{
		"ok/ok.md:6":   "program",
		"ok/ok.md:16":  "declarations",
		"ok/ok.md:26":  "statements",
		"ok/ok.md:41":  "statements",
		"ok/ok.md:50":  "statements",
		"ok/ok.md:62":  "declarations and statements",
		"ok/ok.md:79":  "statements",
		"ok/ok.md:89":  "statements",
		"ok/ok.md:99":  "declarations",
		"api.md:6":     "api",
		"skip.md:6":    "skipped: pseudo-code: the loop, not the API",
		"broken.md:12": "does not parse",
		"broken.md:4":  "statements, fails",
	}
	for pos, want := range status {
		if got := r.Status[pos]; got != want {
			t.Errorf("%s: status %q, want %q", pos, got, want)
		}
	}
	// ok.md's last block and the first block of each dup file are identical, as are the dup
	// files' second blocks: 3 of the blocks are not compiled again.
	if r.Unique != r.Blocks-3 {
		t.Errorf("%d blocks, %d distinct; want %d distinct", r.Blocks, r.Unique, r.Blocks-3)
	}
	if r.Skipped != 2 {
		t.Errorf("skipped %d, want 2", r.Skipped)
	}
}

func TestOKDirectoryPasses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-root", "../../..", "internal/tools/docsnip/testdata/ok"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "9 go blocks in 1 files, 9 distinct: 9 compiled") {
		t.Errorf("summary: %s", stderr.String())
	}
}

func TestExitCodes(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"-root", "../../..", "internal/tools/docsnip/testdata/broken.md"}, &out, &out); code != 1 {
		t.Errorf("broken docs: exit %d, want 1\n%s", code, out.String())
	}
	out.Reset()
	if code := run([]string{"-root", "../../..", "internal/tools/docsnip/testdata/nope.md"}, &out, &out); code != 2 {
		t.Errorf("missing file: exit %d, want 2\n%s", code, out.String())
	}
}

func TestExtractErrors(t *testing.T) {
	for _, tc := range []struct{ name, md, err string }{
		{"orphan directive", "<!-- docsnip: skip why -->\n\ntext\n```go\n```\n", "x.md:1: docsnip directive is not followed by a go block"},
		{"directive at the end", "<!-- docsnip: skip why -->\n", "x.md:1: docsnip directive is not followed by a go block"},
		{"directive before another fence", "<!-- docsnip: skip why -->\n```json\n{}\n```\n", `x.md:1: docsnip directive is followed by a "json" block, not a go block`},
		{"two directives", "<!-- docsnip: skip a -->\n<!-- docsnip: skip b -->\n```go\n```\n", "x.md:1: docsnip directive is not followed by a go block"},
		{"unknown directive", "<!-- docsnip: ignore -->\n```go\n```\n", `x.md:1: unknown docsnip directive "ignore" (want skip, setup or api)`},
		{"skip without a reason", "<!-- docsnip: skip -->\n```go\n```\n", "x.md:1: docsnip: skip needs a reason"},
		{"empty setup", "<!-- docsnip: setup   -->\n```go\n```\n", "x.md:1: docsnip: setup needs at least one item"},
		{"api without a package", "<!-- docsnip: api -->\n```go\n```\n", "x.md:1: docsnip: api needs one package name or import path"},
		{"unterminated comment", "<!-- docsnip: setup x int\n```go\n```\n", "x.md:1: unterminated docsnip comment"},
		{"text after the comment", "<!-- docsnip: skip why --> more\n```go\n```\n", "x.md:1: text after a docsnip comment on the same line"},
		{"unclosed fence", "```go\nx := 1\n", "x.md:1: unclosed code fence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := snip.Extract("x.md", []byte(tc.md))
			if err == nil || !strings.HasPrefix(err.Error(), tc.err) {
				t.Fatalf("err = %v, want prefix %q", err, tc.err)
			}
		})
	}
}

func TestExtract(t *testing.T) {
	md := "intro\r\n\r\n<!-- docsnip: setup\r\n  x int\r\n-->\r\n\r\n```go\r\ny := x\r\n```\r\n" +
		"- item\n  ````go title\n  a := 1\n    b := 2\n  ````\n" +
		"~~~go\nc := 3\n~~~\n" +
		"```text\n<!-- docsnip: skip not a directive inside a fence -->\n```\n" +
		"inline ```go code``` is not a fence\n"
	blocks, err := snip.Extract("x.md", []byte(md))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %+v", len(blocks), blocks)
	}
	if b := blocks[0]; b.Line != 8 || b.Code != "y := x\n" || b.Dir == nil || b.Dir.Kind != "setup" || b.Dir.Arg != "x int" || b.Dir.Line != 3 {
		t.Errorf("block 0 = %+v, dir %+v", b, b.Dir)
	}
	if b := blocks[1]; b.Line != 12 || b.Code != "a := 1\n  b := 2\n" || b.Dir != nil {
		t.Errorf("block 1 = %+v", b)
	}
	if b := blocks[2]; b.Line != 16 || b.Code != "c := 3\n" {
		t.Errorf("block 2 = %+v", b)
	}
}

func TestParseSetup(t *testing.T) {
	s, err := snip.ParseSetup(`ctx context.Context; a, b string
type T struct{ X int; Y string }
import "fmt"; import f2 "fmt"
func g() (int, error); var v = "x;y"; const c = ';'
returns (string, error)`)
	if err != nil {
		t.Fatal(err)
	}
	wantDecls := []string{"var ctx context.Context", "var a, b string", "type T struct{ X int; Y string }", "func g() (int, error)", `var v = "x;y"`, "const c = ';'"}
	if strings.Join(s.Decls, "|") != strings.Join(wantDecls, "|") {
		t.Errorf("decls = %q, want %q", s.Decls, wantDecls)
	}
	if strings.Join(s.Imports, "|") != `"fmt"|f2 "fmt"` {
		t.Errorf("imports = %q", s.Imports)
	}
	if s.Returns != "(string, error)" {
		t.Errorf("returns = %q", s.Returns)
	}
	if _, err := snip.ParseSetup("returns error; returns int"); err == nil {
		t.Error("two returns items: no error")
	}
}

func TestElide(t *testing.T) {
	for in, want := range map[string]string{
		"func f() error { ... }":       `func f() error { panic("docsnip: elided") }`,
		"func f() error {...}":         `func f() error { panic("docsnip: elided") }`,
		"func f() error { /* ... */ }": `func f() error { panic("docsnip: elided") }`,
		"x := T{...}":                  "x := T{}",
		"x := []T{ ... }":              "x := []T{}",
		"\t...\n":                      "\t// ...\n",
		"f(args...)":                   "f(args...)",
		"// ... a comment stays":       "// ... a comment stays",
	} {
		if got := snip.Elide(in); got != want {
			t.Errorf("elide(%q) = %q, want %q", in, got, want)
		}
	}
}
