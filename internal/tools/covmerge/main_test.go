package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func run(t *testing.T, profiles ...string) (string, error) {
	t.Helper()
	var rs []io.Reader
	for _, p := range profiles {
		rs = append(rs, strings.NewReader(p))
	}
	var b bytes.Buffer
	err := merge(&b, rs)
	return b.String(), err
}

func TestMergeTakesMaxAndDedups(t *testing.T) {
	a := "mode: atomic\nx/a.go:1.1,2.2 3 0\nx/a.go:3.1,4.2 2 5\n"
	b := "mode: atomic\nx/a.go:1.1,2.2 3 7\nx/a.go:3.1,4.2 2 1\nx/b.go:1.1,2.2 1 0\n"
	got, err := run(t, a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := "mode: atomic\nx/a.go:1.1,2.2 3 7\nx/a.go:3.1,4.2 2 5\nx/b.go:1.1,2.2 1 0\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestMergeDistinguishesStatementCount(t *testing.T) {
	got, err := run(t, "mode: set\nf.go:1.1,2.2 1 1\nf.go:1.1,2.2 2 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "f.go") != 2 {
		t.Fatalf("blocks with different statement counts must stay apart:\n%s", got)
	}
}

func TestMergeErrors(t *testing.T) {
	for name, in := range map[string][]string{
		"mode mismatch": {"mode: set\n", "mode: atomic\n"},
		"no mode line":  {"f.go:1.1,2.2 1 1\n"},
		"bad count":     {"mode: set\nf.go:1.1,2.2 1 x\n"},
		"malformed":     {"mode: set\nnospace\n"},
		"empty":         {""},
	} {
		if _, err := run(t, in...); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
