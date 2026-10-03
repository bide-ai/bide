package main

import (
	"os"
	"strings"
)

// lineMap maps each line of a to the line of b it became, by a shortest edit script (Myers):
// a line kept maps to its place in b, and a line changed or deleted to the first line of b after
// the kept lines before it. Lines are 1-based; the result has len(a)+1 entries (index 0 unused).
func lineMap(a, b []string) []int {
	n, m := len(a), len(b)
	max := n + m
	off := max + 1
	v := make([]int, 2*max+3)
	var trace [][]int
	for d := 0; d <= max; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || k != d && v[off+k-1] < v[off+k+1] {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, a, b, off, d, n, m)
			}
		}
	}
	return backtrack(trace, a, b, off, max, n, m)
}

// backtrack walks the edit script back from (n, m), recording the kept lines' places.
func backtrack(trace [][]int, a, b []string, off, d, n, m int) []int {
	out := make([]int, n+1)
	x, y := n, m
	for ; d > 0; d-- {
		v := trace[d]
		k := x - y
		var pk int
		if k == -d || k != d && v[off+k-1] < v[off+k+1] {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := v[off+pk]
		py := px - pk
		for x > px && y > py {
			out[x] = y
			x, y = x-1, y-1
		}
		if x > px { // a deleted line: it maps to where the next kept line is
			out[x] = y + 1
		}
		x, y = px, py
	}
	for x > 0 && y > 0 {
		out[x] = y
		x, y = x-1, y-1
	}
	for i := 1; i <= n; i++ {
		if out[i] == 0 {
			out[i] = 1
		}
		if out[i] > m && m > 0 {
			out[i] = m
		}
	}
	return out
}

// remapFindings moves the findings made on the contents before (by path) to the line their site
// holds in after (the final contents; a path missing from after is read from disk), so every
// finding names a line of the file as the run leaves it.
func remapFindings(fs []Finding, before, after map[string][]byte) {
	maps := map[string][]int{}
	for i, f := range fs {
		src, ok := before[f.Pos.Filename]
		if !ok || f.Pos.Line == 0 {
			continue
		}
		lm, ok := maps[f.Pos.Filename]
		if !ok {
			final, ok := after[f.Pos.Filename]
			if !ok {
				final, _ = os.ReadFile(f.Pos.Filename)
			}
			lm = lineMap(strings.Split(string(src), "\n"), strings.Split(string(final), "\n"))
			maps[f.Pos.Filename] = lm
		}
		if f.Pos.Line < len(lm) {
			fs[i].Pos.Line = lm[f.Pos.Line]
		}
	}
}
