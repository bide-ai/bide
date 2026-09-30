package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const goldenRoot = "testdata/repo"

// want is one expectation from a "// want `re` ..." comment: a finding on that line whose
// "key: message" text matches re.
type want struct {
	pos  string // file:line, relative to goldenRoot
	re   *regexp.Regexp
	used bool
}

var (
	wantComment = regexp.MustCompile("// want ((?:(?:`[^`]*`|\"(?:[^\"\\\\]|\\\\.)*\")\\s*)+)$")
	wantArg     = regexp.MustCompile("`[^`]*`|\"(?:[^\"\\\\]|\\\\.)*\"")
)

// readWants collects the want comments of every .go file under root, including the directories
// the check skips: a want there would go unmet, so the skipped files carry none and any finding
// from them fails the test.
func readWants(t *testing.T, root string) []*want {
	t.Helper()
	var wants []*want
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(data), "\n") {
			m := wantComment.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			for _, arg := range wantArg.FindAllString(m[1], -1) {
				s, err := strconv.Unquote(arg)
				if err != nil {
					return fmt.Errorf("%s:%d: bad want %s: %v", rel, i+1, arg, err)
				}
				wants = append(wants, &want{
					pos: fmt.Sprintf("%s:%d", filepath.ToSlash(rel), i+1),
					re:  regexp.MustCompile(s),
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return wants
}

// TestGolden checks the golden repository: every finding is expected by a want comment on its
// line, and every want comment is met by exactly one finding. The golden tree covers each kind
// of declaration, grouped and single declarations, generic receivers, the standard methods
// (Error, Unwrap, MarshalJSON, UnmarshalJSON, String), package comments, package main, a nested
// module, and the files and directories the walk skips (tests, generated files, testdata,
// vendor, and names starting with "." or "_").
func TestGolden(t *testing.T) {
	findings, err := Check(goldenRoot)
	if err != nil {
		t.Fatal(err)
	}
	wants := readWants(t, goldenRoot)
	if len(wants) < 35 {
		t.Fatalf("read only %d want comments; the harness is not reading the golden files", len(wants))
	}
	for _, f := range findings {
		pos := fmt.Sprintf("%s:%d", f.Pos.Filename, f.Pos.Line)
		text := f.Key + ": " + f.Msg
		matched := false
		for _, w := range wants {
			if !w.used && w.pos == pos && w.re.MatchString(text) {
				w.used, matched = true, true
				break
			}
		}
		if !matched {
			t.Errorf("unexpected finding %s", f)
		}
	}
	for _, w := range wants {
		if !w.used {
			t.Errorf("%s: no finding matched want %q", w.pos, w.re)
		}
	}
}

// TestGoodPackageClean pins that the fully documented golden package yields nothing, so a rule
// that starts flagging a documented form fails here even if a want comment were added for it.
func TestGoodPackageClean(t *testing.T) {
	findings, err := Check(filepath.Join(goldenRoot, "good"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("finding in the documented package: %s", f)
	}
}

func TestStartsWithName(t *testing.T) {
	for _, tc := range []struct {
		text, name string
		want       bool
	}{
		{"Foo does x.", "Foo", true},
		{"  Foo does x.", "Foo", true},
		{"Foo", "Foo", true},
		{"Foo's job.", "Foo", true},
		{"Foo, Bar are.", "Foo", true},
		{"Foobar does.", "Foo", false},
		{"Foo_bar does.", "Foo", false},
		{"Foo2 does.", "Foo", false},
		{"Fooé does.", "Foo", false},
		{"foo does.", "Foo", false},
		{"The Foo.", "Foo", false},
	} {
		if got := startsWithName(tc.text, tc.name); got != tc.want {
			t.Errorf("startsWithName(%q, %q) = %v, want %v", tc.text, tc.name, got, tc.want)
		}
	}
}

func TestParseAllowlist(t *testing.T) {
	good := "# comment\n\n. Foo\nagent Bar\nagent Bar.Error\nagent package\n"
	a, err := ParseAllowlist([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(a.keys, "|"), ". Foo|agent Bar|agent Bar.Error|agent package"; got != want {
		t.Errorf("keys = %q, want %q", got, want)
	}
	for name, in := range map[string]string{
		"unsorted":       "agent B\nagent A\n",
		"duplicate":      "agent A\nagent A\n",
		"one field":      "agent\n",
		"three fields":   "agent A B\n",
		"double space":   "agent  A\n",
		"leading space":  " agent A\n",
		"trailing space": "agent A \n",
		"tab":            "agent\tA\n",
	} {
		if _, err := ParseAllowlist([]byte(in)); err == nil {
			t.Errorf("%s: ParseAllowlist(%q) succeeded, want an error", name, in)
		}
	}
}

func TestAllowlistApply(t *testing.T) {
	a, err := ParseAllowlist([]byte("agent A\nagent Gone\n"))
	if err != nil {
		t.Fatal(err)
	}
	unlisted, stale := a.Apply([]Finding{{Key: "agent A"}, {Key: "agent B"}})
	if len(unlisted) != 1 || unlisted[0].Key != "agent B" {
		t.Errorf("unlisted = %v, want [agent B]", unlisted)
	}
	if len(stale) != 1 || stale[0] != "agent Gone" {
		t.Errorf("stale = %v, want [agent Gone]", stale)
	}
}

// goldenKeys is the allowlist that exactly covers the golden repository.
func goldenKeys(t *testing.T) []string {
	t.Helper()
	findings, err := Check(goldenRoot)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, f := range findings {
		keys = append(keys, f.Key)
	}
	slices.Sort(keys) // byte order, as ParseAllowlist requires
	return slices.Compact(keys)
}

func runWith(t *testing.T, allow string) (int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "allow")
	if err := os.WriteFile(path, []byte(allow), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-root", goldenRoot, "-allow", path}, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// TestRunAllowlist covers the command's contract: an allowlist that exactly covers the findings
// passes; a finding it does not list fails; an entry whose identifier is now documented (or
// gone) fails, so the list can only shrink; a malformed list cannot run.
func TestRunAllowlist(t *testing.T) {
	keys := goldenKeys(t)
	if len(keys) < 30 {
		t.Fatalf("only %d golden keys", len(keys))
	}
	exact := strings.Join(keys, "\n") + "\n"

	if code, out := runWith(t, exact); code != 0 {
		t.Fatalf("exact allowlist: exit %d, want 0\n%s", code, out)
	}

	missing := strings.Join(keys[1:], "\n") + "\n"
	if code, out := runWith(t, missing); code != 1 || !strings.Contains(out, keys[0]+": ") {
		t.Errorf("allowlist missing %q: exit %d, want 1 naming it\n%s", keys[0], code, out)
	}

	// "good New" is documented in the golden tree; listing it is stale.
	withDocumented := strings.Join(append(append([]string{}, keys...), "good New"), "\n") + "\n"
	withDocumented = sortLines(withDocumented)
	if code, out := runWith(t, withDocumented); code != 1 || !strings.Contains(out, `stale allowlist entry "good New"`) {
		t.Errorf("allowlist with a documented identifier: exit %d, want 1 naming it stale\n%s", code, out)
	}

	withGone := sortLines(exact + "good NoSuchIdentifier\n")
	if code, out := runWith(t, withGone); code != 1 || !strings.Contains(out, `stale allowlist entry "good NoSuchIdentifier"`) {
		t.Errorf("allowlist with a missing identifier: exit %d, want 1 naming it stale\n%s", code, out)
	}

	if code, _ := runWith(t, "b X\na X\n"); code != 2 {
		t.Errorf("unsorted allowlist: exit %d, want 2", code)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-root", filepath.Join(goldenRoot, "good"), "-allow", ""}, &stdout, &stderr); code != 0 {
		t.Errorf("documented package without an allowlist: exit %d, want 0\n%s%s", code, &stdout, &stderr)
	}
	if code := run([]string{"-root", goldenRoot, "-allow", ""}, &stdout, &stderr); code != 1 {
		t.Errorf("golden repository without an allowlist: exit %d, want 1", code)
	}
	if code := run([]string{"-root", goldenRoot, "-allow", filepath.Join(t.TempDir(), "absent")}, &stdout, &stderr); code != 2 {
		t.Errorf("absent allowlist: exit %d, want 2", code)
	}
	if code := run([]string{"extra"}, &stdout, &stderr); code != 2 {
		t.Errorf("extra argument: exit %d, want 2", code)
	}
}

func sortLines(s string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}
