package snip

import "testing"

// Every byte of the elided code outside a replacement is the original's byte at the offset the
// spans map it to, and the spans cover exactly the replaced text.
func TestElideSpans(t *testing.T) {
	for _, code := range []string{
		"func f() { ... }\nx := T{...}\n...\ny := 1\n",
		"a := S{ ... }\nfunc g() {...}\n\t...\nfunc h() { /* ... */ }\n",
		"no elisions here\n",
	} {
		el, spans := ElideSpans(code)
		if el != Elide(code) {
			t.Fatalf("ElideSpans(%q) = %q, Elide = %q", code, el, Elide(code))
		}
		shift, si := 0, 0
		for e := 0; e < len(el); e++ {
			if si < len(spans) && e == spans[si].EStart {
				s := spans[si]
				e = s.EEnd - 1
				shift = s.OEnd - s.EEnd
				si++
				continue
			}
			if o := e + shift; o >= len(code) || code[o] != el[e] {
				t.Fatalf("%q: elided byte %d (%q) does not map to the original", code, e, el[e])
			}
		}
		for _, s := range spans {
			if el[s.EStart:s.EEnd] == code[s.OStart:s.OEnd] {
				t.Errorf("%q: span %+v replaced nothing", code, s)
			}
		}
	}
}
