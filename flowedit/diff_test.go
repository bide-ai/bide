package flowedit

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestUnifiedDiffShape(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"
	for _, c := range []struct {
		name, new, want string
	}{
		{"same", old, ""},
		{"one line changed", "a\nb\nc\nd\nE\nf\ng\nh\ni\nj\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -2,7 +2,7 @@\n b\n c\n d\n-e\n+E\n f\n g\n h\n"},
		{"first line removed", "b\nc\nd\ne\nf\ng\nh\ni\nj\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,4 +1,3 @@\n-a\n b\n c\n d\n"},
		{"line added at the end", old + "k\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -8,3 +8,4 @@\n h\n i\n j\n+k\n"},
		{"two distant changes are two hunks", "A\nb\nc\nd\ne\nf\ng\nh\ni\nJ\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,4 +1,4 @@\n-a\n+A\n b\n c\n d\n@@ -7,4 +7,4 @@\n g\n h\n i\n-j\n+J\n"},
		{"two near changes are one hunk", "A\nb\nc\nd\ne\nF\ng\nh\ni\nj\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,9 +1,9 @@\n-a\n+A\n b\n c\n d\n e\n-f\n+F\n g\n h\n i\n"},
		{"the final newline is dropped", "a\nb\nc\nd\ne\nf\ng\nh\ni\nj",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -7,4 +7,4 @@\n g\n h\n i\n-j\n+j\n\\ No newline at end of file\n"},
	} {
		if got := UnifiedDiff("f.yaml", []byte(old), []byte(c.new)); got != c.want {
			t.Errorf("%s:\n%q\nwant\n%q", c.name, got, c.want)
		}
	}
	if got := UnifiedDiff("f.yaml", nil, []byte("a\n")); got != "--- a/f.yaml\n+++ b/f.yaml\n@@ -0,0 +1 @@\n+a\n" {
		t.Errorf("from empty: %q", got)
	}
	if got := UnifiedDiff("f.yaml", []byte("a\n"), nil); got != "--- a/f.yaml\n+++ b/f.yaml\n@@ -1 +0,0 @@\n-a\n" {
		t.Errorf("to empty: %q", got)
	}
}

// apply is a patch for the diffs UnifiedDiff writes: it takes old and the diff and returns what the
// diff says new is, so a test can check a diff against the text it is meant to turn old into.
func applyDiff(t *testing.T, old, diff string) string {
	t.Helper()
	if diff == "" {
		return old
	}
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	var oldLines []string
	if old != "" {
		oldLines = strings.Split(strings.TrimSuffix(old, "\n"), "\n")
		if !strings.HasSuffix(old, "\n") {
			oldLines[len(oldLines)-1] += noEOL
		}
	}
	var out []string
	pos := 0 // next line of old
	for i := 2; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "@@"):
			f := strings.Fields(l)
			start := strings.TrimPrefix(f[1], "-")
			n, count := start, "1"
			if a, b, ok := strings.Cut(start, ","); ok {
				n, count = a, b
			}
			s, _ := strconv.Atoi(n)
			c, _ := strconv.Atoi(count)
			if c == 0 {
				s++ // an empty side is numbered by the line before it
			}
			for pos < s-1 {
				out = append(out, oldLines[pos])
				pos++
			}
		case strings.HasPrefix(l, "\\"):
			// "No newline" marks the line before it; on a removed or a kept line it is old's own,
			// already in oldLines.
			if lines[i-1][0] == '+' {
				out[len(out)-1] += noEOL
			}
		case l[0] == ' ':
			out = append(out, oldLines[pos])
			pos++
		case l[0] == '-':
			if oldLines[pos] != l[1:] && oldLines[pos] != l[1:]+noEOL {
				t.Fatalf("the diff removes %q where old has %q", l[1:], oldLines[pos])
			}
			pos++
		case l[0] == '+':
			out = append(out, l[1:])
		}
	}
	out = append(out, oldLines[pos:]...)
	res := strings.Join(out, "\n")
	if strings.HasSuffix(res, noEOL) {
		return strings.TrimSuffix(res, noEOL)
	}
	if res == "" {
		return ""
	}
	return res + "\n"
}

// Over random edits of random texts (a few lines drawn from a small alphabet, so repeats and
// reorderings are common), the diff turns old into new, and git applies it too.
func TestUnifiedDiffAppliesBack(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	words := []string{"a", "b", "c", "# note", "  x: 1", "", "- y", "a"}
	text := func() string {
		n := r.IntN(30)
		var sb strings.Builder
		for i := 0; i < n; i++ {
			sb.WriteString(words[r.IntN(len(words))])
			if i < n-1 || r.IntN(4) != 0 {
				sb.WriteByte('\n')
			}
		}
		return sb.String()
	}
	_, gitErr := exec.LookPath("git")
	dir := t.TempDir()
	for i := 0; i < 400; i++ {
		old, nw := text(), text()
		if i%3 == 0 { // a small edit of old, the case that matters
			ls := strings.Split(old, "\n")
			if len(ls) > 2 {
				ls[r.IntN(len(ls))] = "changed " + fmt.Sprint(i)
				nw = strings.Join(ls, "\n")
			}
		}
		d := UnifiedDiff("f.yaml", []byte(old), []byte(nw))
		if got := applyDiff(t, old, d); got != nw {
			t.Fatalf("case %d: the diff does not turn old into new\nold %q\nnew %q\ngot %q\n%s", i, old, nw, got, d)
		}
		if gitErr == nil && d != "" && i%4 == 0 {
			p := filepath.Join(dir, "f.yaml")
			if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "d.patch"), []byte(d), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("git", "-c", "core.autocrlf=false", "-c", "core.eol=lf", "apply", "--unsafe-paths", "d.patch")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("case %d: git does not take the diff: %v\n%s\n%s", i, err, out, d)
			}
			if b, _ := os.ReadFile(p); string(b) != nw {
				t.Fatalf("case %d: git applied it to %q, want %q", i, b, nw)
			}
		}
	}
}

// A very different pair of long texts is still a correct diff (one block replaced) and takes no
// quadratic time.
func TestUnifiedDiffOfHugeMiddle(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&a, "old %d\n", i)
		fmt.Fprintf(&b, "new %d\n", i)
	}
	d := UnifiedDiff("f.yaml", []byte(a.String()), []byte(b.String()))
	if got := applyDiff(t, a.String(), d); got != b.String() {
		t.Error("the diff of a huge middle does not apply back")
	}
}

func TestRemovedComments(t *testing.T) {
	old := "# header\na: 1   # why a\n  # inner\nb: 2\nc: 3 # keep\n"
	got := RemovedComments([]byte(old), []byte("a: 1\nb: 2\nc: 3 # keep\n"))
	if strings.Join(got, "|") != "# header|# why a|# inner" {
		t.Errorf("%q", got)
	}
	if got := RemovedComments([]byte(old), []byte(old)); len(got) != 0 {
		t.Errorf("no change: %q", got)
	}
	if got := RemovedComments([]byte(old), []byte(old+"# new\n")); len(got) != 0 {
		t.Errorf("an added comment is not a removed one: %q", got)
	}
}
