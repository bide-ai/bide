package agent

import (
	"encoding/base64"
	"testing"
)

// skipValue returns where a JSON value ends: at the separator or closer after it, len(b) when the
// value ends the input, and -1 for a value that does not close.
func TestSkipValue(t *testing.T) {
	for _, c := range []struct {
		in   string
		from int
		want int
	}{
		{`1,2`, 0, 1},
		{`1}`, 0, 1},
		{`12`, 0, 2},
		{`"a,b",1`, 0, 5},
		{`"a\"b",1`, 0, 6},
		{`"a\\",1`, 0, 5},
		{`{"a":[1,2]},3`, 0, 11},
		{`[[1],2]]`, 0, 7},
		{`"unterminated`, 0, -1},
		{`"ends with escape\`, 0, -1},
		{`{"a":1`, 0, -1},
		{`[1,[2]`, 0, -1},
	} {
		if got := skipValue([]byte(c.in), c.from); got != c.want {
			t.Errorf("skipValue(%q, %d) = %d; want %d", c.in, c.from, got, c.want)
		}
	}
}

func TestSkipSpace(t *testing.T) {
	b := []byte(" \t\r\n x ")
	if got := skipSpace(b, 0); got != 5 {
		t.Errorf("skipSpace = %d; want 5", got)
	}
	if got := skipSpace(b, 6); got != 7 {
		t.Errorf("skipSpace at the trailing space = %d; want 7 (end)", got)
	}
	if got := skipSpace(b, 7); got != 7 {
		t.Errorf("skipSpace at the end = %d; want 7", got)
	}
}

// startMembersOnly admits an object of exactly the four members, once each, spelled exactly, and
// nothing else: the caller falls back to the full decoding for the rest.
func TestStartMembersOnly(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{`{"name":"a","kind":"b","result":{"x":[1,2]},"salt":"s"}`, true},
		{` { "salt" : "s" , "result" : 1 , "kind" : "b" , "name" : "a" } `, true},
		{`{"name":"a","kind":"b","result":1,"salt":"s","extra":1}`, false},
		{`{"name":"a","name":"a","kind":"b","result":1,"salt":"s"}`, false},
		{`{"Name":"a","kind":"b","result":1,"salt":"s"}`, false},
		{`{"na\u006de":"a","kind":"b","result":1,"salt":"s"}`, false},
		{`[1]`, false},
		{``, false},
		{`{`, false},
		{`{"name"`, false},
		{`{"name":"a",}`, false},
		{`{"name":"unterminated`, false},
	} {
		if got := startMembersOnly([]byte(c.in)); got != c.want {
			t.Errorf("startMembersOnly(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

// decodePlainStart reads only the exact record a plain start is written as: a truncated string,
// an escape, a bad salt or any other framing is not read.
func TestDecodePlainStart_RefusesWhatIsNotExact(t *testing.T) {
	salt := base64.StdEncoding.EncodeToString(make([]byte, SaltSize))
	if len(salt) != saltLen {
		t.Fatalf("test salt has %d bytes; the journal writes %d", len(salt), saltLen)
	}
	good := plainStartPrefix + `hi` + plainStartKind + salt + `"}`
	if st, ok := decodePlainStart([]byte(good)); !ok || st.Input.Text() != "hi" || st.Kind != RunKindAgent {
		t.Fatalf("decodePlainStart(good) = %+v, %v", st, ok)
	}
	withAt := plainStartPrefix + `hi` + plainStartAt + `2026-10-13T00:00:00.000Z` + plainStartLegacy + salt + `"}`
	if st, ok := decodePlainStart([]byte(withAt)); !ok || st.StartedAt != "2026-10-13T00:00:00.000Z" {
		t.Fatalf("decodePlainStart(with start time) = %+v, %v", st, ok)
	}
	legacy := plainStartPrefix + `hi` + plainStartLegacy + salt + `"}`
	if st, ok := decodePlainStart([]byte(legacy)); !ok || st.Kind != "" {
		t.Fatalf("decodePlainStart(legacy) = %+v, %v", st, ok)
	}
	for name, b := range map[string]string{
		"no input end":       plainStartPrefix + `hi`,
		"escaped input":      plainStartPrefix + `h\ni` + plainStartKind + salt + `"}`,
		"control in input":   plainStartPrefix + "h\ni" + plainStartKind + salt + `"}`,
		"unterminated time":  plainStartPrefix + `hi` + plainStartAt + `2026`,
		"escaped time":       plainStartPrefix + `hi` + plainStartAt + `20\n26` + plainStartLegacy + salt + `"}`,
		"unknown framing":    plainStartPrefix + `hi","extra":1},"salt":"` + salt + `"}`,
		"short salt":         plainStartPrefix + `hi` + plainStartKind + `AAAA"}`,
		"salt not base64":    plainStartPrefix + `hi` + plainStartKind + "!" + salt[1:] + `"}`,
		"trailing garbage":   good + ` `,
		"not the run:start":  `{"name":"other"}`,
		"time then no salt":  plainStartPrefix + `hi` + plainStartAt + `t` + `"}`,
		"legacy bad closing": plainStartPrefix + `hi` + plainStartLegacy + salt,
	} {
		if st, ok := decodePlainStart([]byte(b)); ok {
			t.Errorf("%s: decodePlainStart accepted %q as %+v", name, b, st)
		}
	}
}
