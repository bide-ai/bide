package flowedit

import (
	"fmt"
	"strings"
)

// diffContext is the number of unchanged lines a hunk of UnifiedDiff keeps around a change.
const diffContext = 3

// maxDiffCells bounds the work of the line diff: the middle of the two texts (what is left after
// their common start and end are set aside) is compared by dynamic programming only when its two
// sides multiply to at most this many cells; a bigger middle is shown as one block replaced, which
// is always a correct diff and, for a config edit, a rare one.
const maxDiffCells = 4 << 20

// UnifiedDiff is the plain unified diff that turns old into new, the bytes the editor would write
// over the file shown as `diff -u` shows them ("--- a/name", "+++ b/name", hunks), or "" when the
// two are the same. A line is what ends in a newline; a text that does not end in one is marked as
// git marks it. The pre-save diff is this, so formatting churn and a removed comment are seen
// before they are written.
func UnifiedDiff(name string, old, new []byte) string {
	if string(old) == string(new) {
		return ""
	}
	a, b := splitLines(string(old)), splitLines(string(new))
	edits := lineEdits(a, b)
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)
	// Hunks: runs of changes with diffContext unchanged lines either side, merged when the unchanged
	// lines between two changes are no more than twice that.
	var changed []int
	for i, e := range edits {
		if e.op != ' ' {
			changed = append(changed, i)
		}
	}
	for g := 0; g < len(changed); {
		first, last := changed[g], changed[g]
		h := g + 1
		for h < len(changed) && changed[h]-last-1 <= 2*diffContext {
			last = changed[h]
			h++
		}
		g = h
		start, stop := max(0, first-diffContext), min(len(edits), last+1+diffContext)
		aStart, bStart := 1, 1
		for _, e := range edits[:start] {
			if e.op != '+' {
				aStart++
			}
			if e.op != '-' {
				bStart++
			}
		}
		aLen, bLen := 0, 0
		for _, e := range edits[start:stop] {
			if e.op != '+' {
				aLen++
			}
			if e.op != '-' {
				bLen++
			}
		}
		// An empty side is numbered by the line before it, as diff does.
		if aLen == 0 {
			aStart--
		}
		if bLen == 0 {
			bStart--
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", rng(aStart, aLen), rng(bStart, bLen))
		for _, e := range edits[start:stop] {
			out.WriteByte(e.op)
			out.WriteString(strings.TrimSuffix(e.text, noEOL))
			out.WriteByte('\n')
			if strings.HasSuffix(e.text, noEOL) {
				out.WriteString("\\ No newline at end of file\n")
			}
		}
	}
	return out.String()
}

func rng(start, n int) string {
	if n == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}

// noEOL ends the last line of a text that does not end in a newline, in the line list, so that a
// file that gains or loses its final newline differs on that line, as it does in git's diff. It is
// removed again when a line is written.
const noEOL = "\x00"

// splitLines splits s into lines without their line ends; the last line of a text that does not end
// in a newline carries noEOL.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if !strings.HasSuffix(s, "\n") {
		lines[len(lines)-1] += noEOL
	}
	return lines
}

type lineEdit struct {
	op   byte // ' ' kept, '-' removed, '+' added
	text string
}

// lineEdits is the shortest edit script from a to b (by longest common subsequence of lines),
// after setting aside the lines the two share at the start and the end.
func lineEdits(a, b []string) []lineEdit {
	lo := 0
	for lo < len(a) && lo < len(b) && a[lo] == b[lo] {
		lo++
	}
	ae, be := len(a), len(b)
	for ae > lo && be > lo && a[ae-1] == b[be-1] {
		ae--
		be--
	}
	var mid []lineEdit
	am, bm := a[lo:ae], b[lo:be]
	switch {
	case len(am) == 0:
		for _, s := range bm {
			mid = append(mid, lineEdit{op: '+', text: s})
		}
	case len(bm) == 0:
		for _, s := range am {
			mid = append(mid, lineEdit{op: '-', text: s})
		}
	case len(am)*len(bm) > maxDiffCells:
		for _, s := range am {
			mid = append(mid, lineEdit{op: '-', text: s})
		}
		for _, s := range bm {
			mid = append(mid, lineEdit{op: '+', text: s})
		}
	default:
		mid = lcsEdits(am, bm)
	}
	out := make([]lineEdit, 0, len(a)+len(b))
	for _, s := range a[:lo] {
		out = append(out, lineEdit{op: ' ', text: s})
	}
	out = append(out, mid...)
	for _, s := range a[ae:] {
		out = append(out, lineEdit{op: ' ', text: s})
	}
	return out
}

// lcsEdits is the edit script by the textbook table of common-subsequence lengths.
func lcsEdits(a, b []string) []lineEdit {
	n, m := len(a), len(b)
	t := make([][]int32, n+1)
	for i := range t {
		t[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	var out []lineEdit
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, lineEdit{op: ' ', text: a[i]})
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			out = append(out, lineEdit{op: '-', text: a[i]})
			i++
		default:
			out = append(out, lineEdit{op: '+', text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, lineEdit{op: '-', text: a[i]})
	}
	for ; j < m; j++ {
		out = append(out, lineEdit{op: '+', text: b[j]})
	}
	return out
}

// RemovedComments lists the lines the diff from old to new removes that are comments: a line whose
// first non-blank character is "#" (YAML), and the trailing comment of a removed or changed line.
// The pre-save dialog names them, because an edit that drops a comment is easy to miss in a diff
// of a long file. A JSON config has none.
func RemovedComments(old, new []byte) []string {
	var out []string
	for _, e := range lineEdits(splitLines(string(old)), splitLines(string(new))) {
		if e.op != '-' {
			continue
		}
		t := strings.TrimSpace(strings.TrimSuffix(e.text, noEOL))
		if strings.HasPrefix(t, "#") {
			out = append(out, t)
		} else if _, c := splitTrailing(strings.TrimSuffix(e.text, noEOL)); c != "" {
			out = append(out, strings.TrimSpace(c))
		}
	}
	return out
}
