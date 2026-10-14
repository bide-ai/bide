package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"
)

// canonicalVectorsPath holds the golden vectors for the canonical arguments and the decision bytes:
// what Go computes, for a verifier or a signer written in another language to reproduce exactly (the
// approval page's script is tested against the same file). Regenerate with
// go test ./agent -run TestCanonicalVectors -update after a deliberate change to either encoding.
const canonicalVectorsPath = "testdata/canonical_args_vectors.json"

var updateVectors = flag.Bool("update", false, "rewrite "+canonicalVectorsPath)

type argsVector struct {
	Name      string `json:"name"`
	Input     string `json:"input_b64"`     // the arguments' bytes, base64
	Canonical string `json:"canonical_b64"` // canonicalArgs of them, base64
	SHA256    string `json:"sha256"`        // of the canonical bytes, lowercase hex
}

type decisionVector struct {
	Name      string `json:"name"`
	RunID     string `json:"run"`
	ToolUseID string `json:"call"`
	ToolName  string `json:"tool"`
	Args      string `json:"args_b64"`
	Approver  string `json:"approver"`
	Approved  bool   `json:"approved"`
	DecidedAt string `json:"decided_at"`
	Bytes     string `json:"bytes_hex"` // ApprovalDecisionBytes, lowercase hex
}

type vectorFile struct {
	Format    string           `json:"format"`
	Args      []argsVector     `json:"args"`
	Decisions []decisionVector `json:"decisions"`
}

// canonicalCases are the inputs the vectors cover by hand: every place a second implementation can
// differ from Go's (key order by UTF-8 bytes against UTF-16 code units, duplicate keys, number
// spellings, escapes, surrogates, invalid UTF-8, what is not one JSON value).
func canonicalCases() map[string][]byte {
	c := map[string][]byte{
		"empty":                 {},
		"spaces only":           []byte("  \n\t "),
		"empty object":          []byte(`{}`),
		"empty array":           []byte(`[]`),
		"sorted keys":           []byte(`{"b":1,"a":2,"c":{"z":1,"y":[3,2,1]}}`),
		"insignificant space":   []byte("{ \"a\" :\t1 ,\n\"b\" : [ 1 , 2 ] }"),
		"number spellings":      []byte(`[1, 1.0, 1e3, 1E+3, -0, 0.1000, 12345678901234567890123456789, 1.5e-7, 0, -1]`),
		"duplicate keys":        []byte(`{"a":1,"a":2,"b":3,"a":4}`),
		"nested duplicate":      []byte(`{"x":{"k":1,"k":[1]},"x":{"k":2}}`),
		"empty key":             []byte(`{"":1,"a":2}`),
		"supplementary vs bmp":  []byte(`{"\ud800\udc00":1,"\ue000":2,"\uffff":3,"a":4}`),
		"raw supplementary":     []byte("{\"\U00010000\":1,\"\ue000\":2}"),
		"escaped keys":          []byte(`{"a\nb":1,"a\u0000b":2,"\u00e9":3,"e\u0301":4}`),
		"unicode escapes":       []byte(`"\u00e9\u00e9\u4e2d\ud83d\ude00"`),
		"lone high surrogate":   []byte(`{"k":"\ud800","\ud800":1}`),
		"lone low surrogate":    []byte(`"\ude00x"`),
		"surrogate then ascii":  []byte(`"\ud800A"`),
		"reversed surrogates":   []byte(`"\ude00\ud800"`),
		"control escapes":       []byte(`"\b\f\n\r\t\u0001\u001f\u007f\u0080"`),
		"line separators":       []byte(`"\u2028\u2029 \u00a0"`),
		"html characters":       []byte(`"<script>&amp;</script>'"`),
		"solidus escape":        []byte(`"a\/b"`),
		"invalid utf8 byte":     []byte("\"a\xffb\""),
		"truncated sequence":    []byte("\"\xe2\x82\""),
		"overlong sequence":     []byte("\"\xc0\xaf\""),
		"encoded surrogate":     []byte("\"\xed\xa0\x80\""),
		"above max rune":        []byte("\"\xf4\x90\x80\x80\""),
		"invalid utf8 key":      []byte("{\"\xff\":1,\"\xfe\":2}"),
		"scalar string":         []byte(`"x"`),
		"scalar null":           []byte(`null`),
		"scalar true":           []byte(`true`),
		"scalar number":         []byte(`  42  `),
		"not json":              []byte(`not json`),
		"not json spaced":       []byte("  not json \n"),
		"truncated":             []byte(`{"a":1`),
		"two values":            []byte(`{"a":1} {"b":2}`),
		"trailing garbage":      []byte(`{"a":1} x`),
		"trailing bracket":      []byte(`{"a":1}]`),
		"trailing brace":        []byte(`[1,2]}`),
		"leading zero":          []byte(`01`),
		"bad number":            []byte(`[1.]`),
		"bad literal":           []byte(`[tru]`),
		"trailing comma":        []byte(`[1,]`),
		"single quotes":         []byte(`{'a':1}`),
		"raw control in string": []byte("\"a\x01b\""),
		"raw newline in string": []byte("\"a\nb\""),
		"unicode space prefix":  []byte("\u00a0{\"a\":1}"),
		"unicode space tail":    []byte("not json\u2003\u3000"),
		"nbsp wrapped":          []byte("\u00a0 text \u00a0"),
		"bom prefix":            []byte("\ufeff{\"a\":1}"),
		"deep but fine":         []byte(strings.Repeat("[", 500) + strings.Repeat("]", 500)),
		"too deep":              []byte(strings.Repeat("[", 10001) + strings.Repeat("]", 10001)),
		"exactly max depth":     []byte(strings.Repeat("[", 10000) + strings.Repeat("]", 10000)),
		"long string":           []byte(`"` + strings.Repeat("ab\u00e9", 2000) + `"`),
		"many keys":             manyKeys(),
		"mixed case keys":       []byte(`{"B":1,"a":2,"A":3,"b":4,"_":5,"~":6,"0":7}`),
		"numeric string keys":   []byte(`{"10":1,"9":2,"2":3,"1":4}`),
		"escaped slash key":     []byte(`{"a/b":1,"a\/b":2}`),
		"quote and backslash":   []byte(`"\"\\"`),
	}
	return c
}

func manyKeys() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i := 200; i > 0; i-- {
		if i != 200 {
			b.WriteByte(',')
		}
		b.WriteString(`"k`)
		b.WriteString(strings.Repeat("0", i%5))
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(`":`)
		b.WriteString(string(rune('0' + i%10)))
	}
	b.WriteByte('}')
	return b.Bytes()
}

// randomInputs is a seeded stream of JSON-like inputs, some mangled: the generated half of the
// comparison against another implementation (the committed file fixes the stream, so a change to
// the generator is a visible change to the vectors).
func randomInputs(n int) [][]byte {
	rng := rand.New(rand.NewPCG(20261014, 0xb1de))
	pieces := []string{`{`, `}`, `[`, `]`, `,`, `:`, `"a"`, `"b"`, `"\u00e9"`, `"\ud800"`, `"\ud83d\ude00"`, `"\n"`, `"<&>"`, `"\u2028"`,
		`1`, `1.0`, `-0`, `1e5`, `0.5`, `true`, `null`, ` `, "\n", `"k"`, `"\ue000"`, `"\uffff"`, "\xff", "\u00a0", `"`, `\`, `01`}
	out := make([][]byte, 0, n)
	for len(out) < n {
		var b bytes.Buffer
		for range 1 + rng.IntN(24) {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		out = append(out, b.Bytes())
	}
	// Well-formed random documents, so the canonical path (not the as-is fallback) is exercised.
	for len(out) < 2*n {
		out = append(out, randomDocument(rng, 0))
	}
	return out
}

func randomDocument(rng *rand.Rand, depth int) []byte {
	keys := []string{"a", "b", "A", "\u00e9", "\ue000", "\U00010000", "", "k1", "k10", "k2", "x\ny", "\u2028"}
	str := func() string {
		s := []string{`"plain"`, `"\u00e9"`, `"\ud83d\ude00"`, `"\\"`, `"tab\t"`, `"<>&"`, `"\u0001"`, `"\u007f"`, `"\ud800"`, `""`}
		return s[rng.IntN(len(s))]
	}
	num := func() string {
		s := []string{"0", "1", "-1", "1.0", "1e3", "2.50", "-0", "123456789012345678901", "1E-2"}
		return s[rng.IntN(len(s))]
	}
	if depth > 3 || rng.IntN(3) == 0 {
		switch rng.IntN(4) {
		case 0:
			return []byte(str())
		case 1:
			return []byte(num())
		case 2:
			return []byte("true")
		}
		return []byte("null")
	}
	var b bytes.Buffer
	if rng.IntN(2) == 0 {
		b.WriteByte('{')
		for i := range rng.IntN(5) {
			if i > 0 {
				b.WriteString(", ")
			}
			k, _ := json.Marshal(keys[rng.IntN(len(keys))])
			b.Write(k)
			b.WriteString(" : ")
			b.Write(randomDocument(rng, depth+1))
		}
		b.WriteByte('}')
		return b.Bytes()
	}
	b.WriteByte('[')
	for i := range rng.IntN(5) {
		if i > 0 {
			b.WriteString(",")
		}
		b.Write(randomDocument(rng, depth+1))
	}
	b.WriteByte(']')
	return b.Bytes()
}

func buildVectors() vectorFile {
	v := vectorFile{Format: "bide.canonical-args-vectors.v1"}
	add := func(name string, in []byte) {
		canon := canonicalArgs(in)
		sum := sha256.Sum256(canon)
		v.Args = append(v.Args, argsVector{Name: name, Input: base64.StdEncoding.EncodeToString(in),
			Canonical: base64.StdEncoding.EncodeToString(canon), SHA256: hex.EncodeToString(sum[:])})
	}
	cases := canonicalCases()
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sortStrings(names)
	for _, n := range names {
		add(n, cases[n])
	}
	for i, in := range randomInputs(300) {
		add("random "+itoa(i), in)
	}
	at := time.Date(2026, 10, 14, 9, 30, 15, 123e6, time.UTC)
	for _, d := range []struct {
		name, run, call, tool string
		args                  []byte
		approver              string
		approved              bool
	}{
		{"approve", "run-1", "c1", "charge", []byte(`{"amount":120,"to":"acme"}`), "ops", true},
		{"reject", "run-1", "c1", "charge", []byte(`{"amount":120,"to":"acme"}`), "ops", false},
		{"unicode ids", "r\u00fcn>sub", "node:iter:2:\u4e2d", "t\u00f6ol", []byte(`{"k":"v"}`), "approver-\u00e9", true},
		{"empty args", "r", "c", "t", nil, "a", true},
		{"non-json args", "r", "c", "t", []byte("free text"), "a", false},
		{"long ids", strings.Repeat("r", 300), strings.Repeat("c", 5000), "t", []byte(`[1]`), strings.Repeat("a", 200), true},
	} {
		s := ApprovalSubject{RunID: d.run, ToolUseID: d.call, ToolName: d.tool, Args: d.args}
		v.Decisions = append(v.Decisions, decisionVector{Name: d.name, RunID: d.run, ToolUseID: d.call, ToolName: d.tool,
			Args: base64.StdEncoding.EncodeToString(d.args), Approver: d.approver, Approved: d.approved, DecidedAt: FormatTimestamp(at),
			Bytes: hex.EncodeToString(ApprovalDecisionBytes(s, d.approver, d.approved, at))})
	}
	return v
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// The golden file is what Go computes now: any change to canonicalArgs or the decision bytes shows
// as a failure here, and as a diff of the file when regenerated deliberately.
func TestCanonicalVectors(t *testing.T) {
	want := buildVectors()
	wantJSON, err := json.MarshalIndent(want, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	wantJSON = append(wantJSON, '\n')
	if *updateVectors {
		if err := os.WriteFile(canonicalVectorsPath, wantJSON, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(canonicalVectorsPath)
	if err != nil {
		t.Fatalf("%v: run go test ./agent -run TestCanonicalVectors -update", err)
	}
	if !bytes.Equal(got, wantJSON) {
		t.Fatal(canonicalVectorsPath + " is out of date with canonicalArgs or the decision bytes: if the change is deliberate, regenerate it with go test ./agent -run TestCanonicalVectors -update, and update every other implementation (the approval page's script)")
	}
}

// The cases that matter are pinned by value, so a regenerated file cannot quietly change them.
func TestCanonicalVectors_PinnedCases(t *testing.T) {
	for in, want := range map[string]string{
		`{"b":1,"a":2}`:               `{"a":2,"b":1}`,
		`{"a":1,"a":2}`:               `{"a":2}`,
		`[1.0, 1e3, -0]`:              `[1.0,1e3,-0]`,
		`"\ud800"`:                    "\"\ufffd\"",
		`{"\ud800\udc00":1,"\ue000":2}`: "{\"\ue000\":2,\"\U00010000\":1}",
		`"<&>\u2028"`:                 "\"<&>\\u2028\"",
		`{"a":1}]`:                    `{"a":1}`,
		`{"a":1} x`:                   `{"a":1} x`,
		"":                            `{}`,
	} {
		if got := string(canonicalArgs([]byte(in))); got != want {
			t.Errorf("canonicalArgs(%q) = %q, want %q", in, got, want)
		}
	}
}
