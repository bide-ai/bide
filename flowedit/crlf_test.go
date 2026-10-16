package flowedit

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func toCRLF(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// A file with CRLF line ends, with or without a byte-order mark, is edited exactly as the same file
// with LF ends: the same operations apply or are refused, the text after is the LF result with the
// line ends and the mark put back, and the byte range it reports is the one in the original.
func TestCRLFAndBOMEditLikeLF(t *testing.T) {
	stride := 4 * opStride
	checked, applied := 0, 0
	for _, f := range collect(t) {
		lf := strings.TrimPrefix(strings.ReplaceAll(f.text, "\r\n", "\n"), bom)
		for i, op := range ops(lf) {
			if i%stride != 0 {
				continue
			}
			want, werr := Apply([]byte(lf), op)
			for name, variant := range map[string]string{
				"CRLF":     toCRLF(lf),
				"BOM":      bom + lf,
				"BOM CRLF": bom + toCRLF(lf),
			} {
				got, gerr := Apply([]byte(variant), op)
				checked++
				if (werr == nil) != (gerr == nil) {
					t.Fatalf("%s %s %+v: LF result %v, %s result %v", f.name, name, op, werr, name, gerr)
				}
				if werr != nil {
					var a, b *Refusal
					if !errors.As(werr, &a) || !errors.As(gerr, &b) {
						t.Fatalf("%s: %v / %v", f.name, werr, gerr)
					}
					continue
				}
				applied++
				wantText := string(want.Text)
				if strings.HasPrefix(name, "BOM") {
					wantText = bom + wantText
				}
				if strings.HasSuffix(name, "CRLF") {
					wantText = bom[:0] + toCRLFAfterMark(wantText)
				}
				if string(got.Text) != wantText {
					t.Fatalf("%s %s %+v: the text differs from the LF result with its line ends restored", f.name, name, op)
				}
				// The reported range is in the original bytes: outside it the text is the original.
				if !bytes.Equal(got.Text[:got.Lo], []byte(variant)[:got.Lo]) || !bytes.Equal(got.Text[got.Lo+len(got.Frag):], []byte(variant)[got.Hi:]) {
					t.Fatalf("%s %s %+v: bytes outside [%d,%d) changed", f.name, name, op, got.Lo, got.Hi)
				}
				if !bytes.Equal(got.Text[got.Lo:got.Lo+len(got.Frag)], got.Frag) {
					t.Fatalf("%s %s: Frag is not what was written", f.name, name)
				}
			}
		}
	}
	if applied < 1000 {
		t.Errorf("only %d CRLF and BOM edits applied of %d: the corpus is not being read", applied, checked)
	}
}

func toCRLFAfterMark(s string) string {
	if strings.HasPrefix(s, bom) {
		return bom + toCRLF(s[len(bom):])
	}
	return toCRLF(s)
}

// Line ends that cannot be reproduced are refused: a file that mixes LF and CRLF, a carriage return
// that is not part of a CRLF, a CRLF file where only some lines have it.
func TestMixedLineEndsAreRefused(t *testing.T) {
	for name, src := range map[string]string{
		"LF then CRLF":     "version: 1\nflow: a\r\nnodes: []\r\n",
		"CRLF then LF":     "version: 1\r\nflow: a\nnodes: []\n",
		"a lone CR":        "version: 1\rflow: a\n",
		"CRLF and lone CR": "version: 1\r\nflow: a\rnodes: []\r\n",
		"BOM, then mixed":  bom + "version: 1\r\nflow: a\nnodes: []\n",
		"a CR at the end":  "version: 1\nflow: a\r",
	} {
		_, err := Apply([]byte(src), Op{Kind: Set, Path: []any{"flow"}, Value: "x"})
		var rf *Refusal
		if !errors.As(err, &rf) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A CRLF file with a new entry, a removed entry and a changed value keeps CRLF on every line, new
// ones included, and a BOM stays at the start.
func TestCRLFSpliceShape(t *testing.T) {
	src := bom + toCRLF("# c\nversion: 1\nnodes:\n  - {name: a, block: a}\n  - {name: b, block: b}\nwiring: []\n")
	r, err := Apply([]byte(src), Op{Kind: Insert, Path: []any{"nodes"}, Index: 2, Value: Map{{"name", "c"}, {"block", "c"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := bom + toCRLF("# c\nversion: 1\nnodes:\n  - {name: a, block: a}\n  - {name: b, block: b}\n  - {name: c, block: c}\nwiring: []\n")
	if string(r.Text) != want {
		t.Errorf("%q", r.Text)
	}
	r, err = Apply(r.Text, Op{Kind: Delete, Path: []any{"nodes", 0}})
	if err != nil || strings.Count(string(r.Text), "\r\n") != strings.Count(string(r.Text), "\n") || !strings.HasPrefix(string(r.Text), bom) || strings.Contains(string(r.Text), "name: a") {
		t.Errorf("%v %q", err, r.Text)
	}
}
