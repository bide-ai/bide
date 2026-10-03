package main

import (
	"slices"
	"strings"
	"testing"
)

func TestLineMap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want []int // per line of a, 1-based
	}{
		{"x\ny\nz", "x\ny\nz", []int{1, 2, 3}},
		{"x\ny\nz", "x\nnew1\nnew2\ny\nz", []int{1, 4, 5}}, // lines inserted
		{"x\nold\ny\nz", "x\ny\nz", []int{1, 2, 2, 3}},     // a line deleted: the next kept line
		{"x\nold\ny", "x\nnew\nnewer\ny", []int{1, 2, 4}},  // a line changed into two
		{"a\nb\nc\nd", "c\nd\na\nb", nil},                  // a reordering: checked for range only
	} {
		got := lineMap(strings.Split(c.a, "\n"), strings.Split(c.b, "\n"))[1:]
		if len(c.want) == 0 {
			// a reordering: every line still maps inside b
			for _, l := range got {
				if l < 1 || l > 4 {
					t.Errorf("%q -> %q: line %d out of range", c.a, c.b, l)
				}
			}
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%q -> %q: %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
