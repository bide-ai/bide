package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"math/rand/v2"
	"os"
	"reflect"
	"runtime"
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
		"deep but fine":         []byte(strings.Repeat("[ ", 500) + strings.Repeat(" ]", 500)),
		"too deep":              []byte(strings.Repeat("[ ", 10001) + strings.Repeat(" ]", 10001)),
		"exactly max depth":     []byte(strings.Repeat("[ ", 10000) + strings.Repeat(" ]", 10000)),
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
		`{"b":1,"a":2}`:                 `{"a":2,"b":1}`,
		`{"a":1,"a":2}`:                 `{"a":2}`,
		`[1.0, 1e3, -0]`:                `[1.0,1e3,-0]`,
		`"\ud800"`:                      "\"\ufffd\"",
		`{"\ud800\udc00":1,"\ue000":2}`: "{\"\ue000\":2,\"\U00010000\":1}",
		`"<&>\u2028"`:                   "\"<&>\\u2028\"",
		`{"a":1}]`:                      `{"a":1}`,
		`{"a":1} x`:                     `{"a":1} x`,
		"":                              `{}`,
	} {
		if got := string(canonicalArgs([]byte(in))); got != want {
			t.Errorf("canonicalArgs(%q) = %q, want %q", in, got, want)
		}
	}
}

// strayPart is a Part this package does not know how to journal.
type strayPart struct{}

func (strayPart) part() {}

// A message holding a part the codec does not know, or a stored message naming a part type it does
// not know, is ErrProtocol: a part is never silently dropped from the transcript.
func TestMessageCodec_UnknownParts(t *testing.T) {
	if _, err := json.Marshal(Message{Role: RoleUser, Parts: []Part{Text{Text: "a"}, strayPart{}}}); !errors.Is(err, ErrProtocol) {
		t.Errorf("Marshal with a stray part = %v; want ErrProtocol", err)
	}
	for name, doc := range map[string]string{
		"unknown type":     `{"role":"user","parts":[{"type":"video","url":"x"}]}`,
		"part not json":    `{"role":"user","parts":[7]}`,
		"part wrong shape": `{"role":"user","parts":[{"type":"text","text":5}]}`,
	} {
		var m Message
		if err := json.Unmarshal([]byte(doc), &m); err == nil {
			t.Errorf("%s: Unmarshal accepted %s as %+v", name, doc, m)
		} else if name == "unknown type" && !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: err = %v; want ErrProtocol", name, err)
		}
	}
	// Every part kind the codec knows round-trips.
	in := Message{Role: RoleAssistant, Parts: []Part{
		Text{Text: "t"}, Reasoning{Text: "r", Signature: "s"}, ToolUse{ID: "c", Name: "n", Args: json.RawMessage(`{"a":1}`)},
		ToolResult{ToolUseID: "c", Result: json.RawMessage(`"ok"`), IsError: true}, Image{URL: "https://x/y.png"},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Parts) != len(in.Parts) {
		t.Fatalf("round trip kept %d of %d parts", len(out.Parts), len(in.Parts))
	}
	for i := range in.Parts {
		if got, want := partTypeName(out.Parts[i]), partTypeName(in.Parts[i]); got != want {
			t.Errorf("part %d is %s after the round trip; want %s", i, got, want)
		}
	}
}

func partTypeName(p Part) string {
	k, _ := partKind(p)
	return k
}

// The journal encoding never writes U+2028 or U+2029 raw (they end a line in some readers), however
// the text reaches it, escapes them the same in every build, and still decodes to the same text.
func TestMarshalJournal_EscapesLineSeparators(t *testing.T) {
	text := "a" + lineSep + "b" + paraSep + "c <&> d"
	b, err := marshalJournal(text)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(lineSep)) || bytes.Contains(b, []byte(paraSep)) {
		t.Fatalf("raw separator in %q", b)
	}
	if !bytes.Contains(b, []byte(jsonEscape+"2028")) || !bytes.Contains(b, []byte(jsonEscape+"2029")) {
		t.Fatalf("separators not escaped in %s", b)
	}
	if !strings.Contains(string(b), "<&>") {
		t.Errorf("HTML characters were escaped in %s; the journal does not", b)
	}
	var back string
	if err := json.Unmarshal(b, &back); err != nil || back != text {
		t.Fatalf("decoded %q, %v; want %q", back, err, text)
	}
	if _, err := marshalJournal(make(chan int)); err == nil {
		t.Error("marshalJournal of a channel succeeded")
	}
}

// Distinct values must have distinct canonical forms; equal values one form.
func TestRev103d_CanonicalDistinctness(t *testing.T) {
	distinct := [][2]string{
		{`"\u00e9"`, `"e\u0301"`}, // NFC vs NFD: different characters
		{`1`, `"1"`},
		{`null`, `"null"`},
		{`[]`, `{}`},
		{`[1,2]`, `[2,1]`},
		{`{"a":1}`, `{"a":"1"}`},
		{`{"a\u0000":1}`, `{"a":1}`},
		{`"\u0000"`, `""`},
		{`9007199254740993`, `9007199254740992`},
		{`1e400`, `1e401`},
		{`0.1`, `0.10000000000000001`},
		{`{"a":{"b":1}}`, `{"a":{"b":1},"":null}`},
		{`["a,b"]`, `["a","b"]`},
		{`{"a":"b","c":"d"}`, `{"a":"b\",\"c\":\"d"}`},
		{`true`, `"true"`},
	}
	for _, p := range distinct {
		ca, ea := canonicalJSON(p[0])
		cb, eb := canonicalJSON(p[1])
		if ea != nil || eb != nil {
			t.Errorf("%s / %s: errors %v %v", p[0], p[1], ea, eb)
			continue
		}
		if ca == cb {
			t.Errorf("distinct values %s and %s share canonical form %s", p[0], p[1], ca)
		}
	}
	same := [][2]string{
		{`"\/"`, `"/"`},
		{`"\u002F"`, `"/"`},
		{`"\u003c"`, `"<"`},
		{`"\u0000"`, "\"\\u0000\""},
		{`"\uD83D\uDE00"`, "\"\U0001F600\""},
		{`1E+2`, `100`},
		{`-0`, `0`},
		{`0.000`, `0e99999`},
		{`{"b":1,"a":2}`, `{ "a" : 2 , "b" : 1 }`},
		{`12.3400e-2`, `0.1234`},
	}
	for _, p := range same {
		ca, ea := canonicalJSON(p[0])
		cb, eb := canonicalJSON(p[1])
		if ea != nil || eb != nil || ca != cb {
			t.Errorf("same value %s / %s: %q %v / %q %v", p[0], p[1], ca, ea, cb, eb)
		}
	}
}

// Every malformed text is refused.
func TestRev103d_CanonicalRefusesMalformed(t *testing.T) {
	bad := []string{
		``, ` `, `{`, `[1,]`, `[,1]`, `{"a":1,}`, `{,"a":1}`, `[1 2]`, `{"a" 1}`, `{"a",1}`,
		`{1:2}`, `[1:2]`, `01`, `1.`, `.5`, `+1`, `-`, `1e`, `1e+`, `"\x"`, `"\u12"`, `1 2`, `[1]]`,
		`{"a":1}}`, `nul`, `"a` + "\x00" + `"`, `{"a":1 "b":2}`, `[{]`, `[}`, `{]`, `"\uD800"`,
		`"\uDC00\uD800"`, `"\uD800\u0041"`, `{"a":1,"\u0061":2}`, "\"\xff\"", `NaN`, `Infinity`,
	}
	for _, s := range bad {
		if c, err := canonicalJSON(s); err == nil {
			t.Errorf("canonicalJSON(%q) = %q, nil; want refused", s, c)
		}
	}
}

// Deep nesting does not overflow the stack and is refused cleanly past the decoder's limit.
func TestRev103d_CanonicalDeepNesting(t *testing.T) {
	for _, depth := range []int{9999, 10001, 1_000_000} {
		s := strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth)
		start := time.Now()
		_, err := canonicalJSON(s)
		t.Logf("object depth %d: err=%v in %v", depth, err, time.Since(start))
		s = strings.Repeat(`[`, depth) + "1" + strings.Repeat("]", depth)
		start = time.Now()
		_, err = canonicalJSON(s)
		t.Logf("array depth %d: err=%v in %v", depth, err, time.Since(start))
	}
}

// The claim: the check is linear time, and huge exponents are exact.
func TestRev103d_CanonicalCostLinear(t *testing.T) {
	measure := func(name string, mk func(n int) string) {
		var prev time.Duration
		for _, n := range []int{50_000, 100_000, 200_000, 400_000} {
			s := mk(n)
			start := time.Now()
			if _, err := canonicalJSON(s); err != nil {
				t.Fatalf("%s n=%d: %v", name, n, err)
			}
			d := time.Since(start)
			ratio := 0.0
			if prev > 0 {
				ratio = float64(d) / float64(prev)
			}
			t.Logf("%s: %d bytes in %v (x%.1f for 2x input)", name, len(s), d, ratio)
			prev = d
		}
	}
	measure("exponent digits", func(n int) string { return "1e" + strings.Repeat("7", n) })
	measure("mantissa digits", func(n int) string { return strings.Repeat("7", n) })
	measure("nested objects", func(n int) string {
		depth := n / 6
		if depth > 9000 {
			depth = 9000
		}
		one := strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth)
		copies := n / len(one)
		if copies < 1 {
			copies = 1
		}
		return "[" + strings.Repeat(one+",", copies) + "1]"
	})
}

// Adversarial review of PR #103's F2 (exact decimal comparison of flow inputs).

// F2: canonicalNumber documents "Two number texts denote the same decimal value if and only if
// their canonical forms are equal." A number whose exponent is beyond +-2^62 keeps its text, so
// spellings of one value compare unequal: 0e<huge> is zero but not 0, and 1E<huge> is not 1e<huge>.
func TestRev103c_F2_HugeExponentSpellingsOfOneValue(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{`0e4611686018427387905`, `0`},
		{`0e99999999999999999999`, `0`},
		{`1E4611686018427387905`, `1e4611686018427387905`},
		{`1e+4611686018427387905`, `1e4611686018427387905`},
		{`1.0e4611686018427387905`, `1e4611686018427387905`},
		{`10e4611686018427387905`, `1e4611686018427387906`},
		{`1e-4611686018427387905`, `0.1e-4611686018427387904`},
	} {
		if !sameCanonicalJSON(tc.a, tc.b) {
			ca, _ := canonicalJSON(tc.a)
			cb, _ := canonicalJSON(tc.b)
			t.Errorf("%s and %s are one decimal value, but compare unequal (%s vs %s)", tc.a, tc.b, ca, cb)
		}
	}
}

// F2: no two different values share a canonical form, across the grammar's corners.
func TestRev103c_F2_DistinctValuesStayDistinct(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{`9007199254740993`, `9007199254740992`},
		{`[1,[2,{"k":9007199254740993}]]`, `[1,[2,{"k":9007199254740992}]]`},
		{`1e4611686018427387904`, `1e4611686018427387903`},
		{`-0.5`, `0.5`},
		{`1` + strings.Repeat("0", 400) + `1`, `1` + strings.Repeat("0", 401)},
		{`1e-4611686018427387904`, `0`},
	} {
		if sameCanonicalJSON(tc.a, tc.b) {
			t.Errorf("%.60s and %.60s are different values but compare equal", tc.a, tc.b)
		}
	}
	// Non-JSON spellings the grammar refuses compare as text, never as the number they suggest.
	for _, bad := range []string{`+1`, `.5`, `5.`, `0x10`, `NaN`, `Infinity`, `-Infinity`, `01`} {
		if sameCanonicalJSON(bad, `1`) || sameCanonicalJSON(bad, `0.5`) || sameCanonicalJSON(bad, `5`) || sameCanonicalJSON(bad, `16`) {
			t.Errorf("%s compared equal to a number", bad)
		}
	}
}

// F2: cost is linear in the text: a 4 MB digit string and a 1e999999999 exponent are cheap.
func TestRev103c_F2_NoBlowup(t *testing.T) {
	long := `1` + strings.Repeat("0", 4<<20)
	start := time.Now()
	if !sameCanonicalJSON(long, `1e`+"4194304") {
		t.Error("1 followed by 4Mi zeros is 1e4194304")
	}
	if !sameCanonicalJSON(`1e999999999`, `10e999999998`) {
		t.Error("1e999999999 is 10e999999998")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

// F2 (beyond numbers): the canonical form decodes strings, so a lone surrogate escape becomes
// U+FFFD, and a duplicate key keeps its last value. Two different JSON texts then compare equal.
// A flow whose input type is json.RawMessage (or that forwards the raw text) sees different bytes.
func TestRev103c_F2_DistinctTextsCollapse(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{`"\ud800"`, `"\udfff"`},
		{`"\ud800"`, `"�"`},
		{`{"a":1,"a":2}`, `{"a":2}`},
	} {
		if sameCanonicalJSON(tc.a, tc.b) {
			t.Errorf("%s and %s are different JSON texts but compare equal", tc.a, tc.b)
		}
	}
}

// canonicalNumber's doc: "The exponent is computed exactly whatever its size, in time linear in the
// text." Parsing and printing the exponent through math/big was quadratic in its digits. A timing
// ratio at these sizes (a few milliseconds) flakes, so the assertion is deterministic: bytes
// allocated per input byte stay constant as the exponent grows 4x (math/big's quadratic
// conversion allocated a growing multiple).
func TestRev103d_HugeExponentIsLinear(t *testing.T) {
	perByte := func(n int) float64 {
		s := "1e" + strings.Repeat("7", n)
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		start := time.Now()
		if _, err := canonicalJSON(s); err != nil {
			t.Fatal(err)
		}
		d := time.Since(start)
		runtime.ReadMemStats(&m1)
		t.Logf("exponent of %d digits: %v", n, d)
		return float64(m1.TotalAlloc-m0.TotalAlloc) / float64(len(s))
	}
	small, large := perByte(250_000), perByte(1_000_000)
	t.Logf("bytes allocated per input byte: 250k digits: %.1f; 1M digits: %.1f", small, large)
	if large > 2*small {
		t.Fatalf("allocation per input byte grows with the exponent's length (%.1f -> %.1f): not linear", small, large)
	}
}

// canonicalJSON's claimed linear cost: each nested object's canonical text is built in its own
// builder and copied into its parent's, so a depth-d nesting copies O(d^2) bytes. Bytes allocated
// per input byte should be a constant; here it grows with depth.
func TestRev103d_NestedObjectsAllocateLinearly(t *testing.T) {
	perByte := func(depth int) float64 {
		s := strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth)
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		if _, err := canonicalJSON(s); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&m1)
		return float64(m1.TotalAlloc-m0.TotalAlloc) / float64(len(s))
	}
	shallow, deep := perByte(1000), perByte(8000)
	t.Logf("bytes allocated per input byte: depth 1000: %.0f; depth 8000: %.0f", shallow, deep)
	if deep > 2*shallow {
		t.Fatalf("allocation per input byte grows with depth (%.0f -> %.0f): quadratic, not linear", shallow, deep)
	}
}

const replacementChar = "\xef\xbf\xbd" // U+FFFD, UTF-8 encoded

// The journal encoding is pinned byte for byte: it is what every store persists and what every
// audit leaf commits to, so it must not depend on the store, the build (GOEXPERIMENT=nojsonv2
// included), or HTML escaping.
func TestEncodeRecord_Golden(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{
		Text{Text: "a <b> & c" + lineSep + "d\x00 bad\xffutf8"},
		ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(`{ "q" : "a<b && c>d" ,  "n": 1.50 }`)},
		Image{Mime: "image/png", Data: []byte{0, 1, 0xff, '<'}},
	}}
	rec := Record{Name: "@llm/0", Kind: StepModel, Message: &msg}
	got, err := EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"@llm/0","kind":"model","message":{"role":"assistant","parts":[` +
		`{"text":"a <b> & c` + jsonEscape + `2028d` + jsonEscape + `0000 bad` + replacementChar + `utf8","type":"text"},` +
		`{"args":{"q":"a<b && c>d","n":1.50},"id":"c1","name":"t","type":"tool_use"},` +
		`{"data":"AAH/PA==","mime":"image/png","type":"image"}]}}`
	if string(got) != want {
		t.Fatalf("EncodeRecord =\n%q\nwant\n%q", got, want)
	}

	res := Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1",
		Result: json.RawMessage("{ \"h\" : \"<b>" + lineSep + paraSep + "\xff\" ,\n \"n\": [1e20, -0.0, 9007199254740993] }")}
	got, err = EncodeRecord(res)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"name":"c1","kind":"tool_result","tool_use_id":"c1","result":{"h":"<b>` + jsonEscape + `2028` + jsonEscape + `2029` + "\xff" + `","n":[1e20,-0.0,9007199254740993]}}`
	if string(got) != want {
		t.Fatalf("EncodeRecord =\n%q\nwant\n%q", got, want)
	}
}

// Decoding the journal encoding and encoding the result again gives the same bytes, so an audit
// leaf computed from a record read back from a store is the bytes the store persisted.
func FuzzEncodeRecord_FixedPoint(f *testing.F) {
	f.Add("a <b> & c", []byte(`{ "h" : "<b>" }`))
	f.Add("x\xffy"+lineSep+"\x00", []byte("\"a\xffb"+paraSep+"\""))
	f.Add("", []byte(`[1.50, 1e20, -0.0]`))
	f.Add(paraSep, []byte(`"`+jsonEscape+`003c"`))
	f.Fuzz(func(t *testing.T, text string, raw []byte) {
		if !json.Valid(raw) {
			return
		}
		msg := Message{Role: RoleAssistant, Parts: []Part{Text{Text: text}, Reasoning{Text: text, Signature: text},
			ToolUse{ID: text, Name: text, Args: raw}, ToolResult{ToolUseID: text, Result: raw}}}
		rec := Record{Name: text, Kind: StepModel, Message: &msg, Result: raw, Evidence: raw, ApproverSignature: &ApproverSignature{Approver: text}}
		data, err := EncodeRecord(rec)
		if err != nil {
			return // not every valid JSON text survives encoding/json's Marshaler checks
		}
		back, err := DecodeRecord(data)
		if err != nil {
			t.Fatalf("DecodeRecord(%q): %v", data, err)
		}
		again, err := EncodeRecord(back)
		if err != nil {
			t.Fatalf("EncodeRecord(decoded): %v", err)
		}
		if !bytes.Equal(again, data) {
			t.Fatalf("not a fixed point:\nfirst:  %q\nsecond: %q", data, again)
		}
		// A single plain pass over the decoded record is already stable too, so the fixed point
		// does not rest on EncodeRecord's own second pass.
		single, err := marshalJournal(back)
		if err != nil {
			t.Fatalf("marshalJournal(decoded): %v", err)
		}
		if !bytes.Equal(single, data) {
			t.Fatalf("a single pass over the decoded record differs:\nstored: %q\nsingle: %q", data, single)
		}
		back2, err := DecodeRecord(again)
		if err != nil || !reflect.DeepEqual(back, back2) {
			t.Fatalf("decoding the fixed point again gives a different record (%v)", err)
		}
	})
}

// twoPassStartJSON is RunStart's journal encoding as it was written before MarshalJSON encoded
// the input in the same pass: the input encoded alone, then embedded raw.
func twoPassStartJSON(t *testing.T, s RunStart) []byte {
	t.Helper()
	var in json.RawMessage
	var err error
	if txt, ok := plainUserText(s.Input); ok {
		in, err = marshalJournal(txt)
	} else {
		in, err = marshalJournal(s.Input)
	}
	if err != nil {
		t.Fatal(err)
	}
	b, err := marshalJournal(runStartWire{Input: in, Saga: s.Saga, Kind: s.Kind, Session: s.Session, Flow: s.Flow,
		Typed: s.Typed, Settings: s.Settings, Principal: s.Principal, Tools: s.Tools, Ext: s.Ext})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The one-pass encoding writes the bytes the two-pass one wrote, for every kind of input and
// setting: journals keep their exact form (their digests and audit leaves depend on it).
func TestRunStartOnePassEncodingIsUnchanged(t *testing.T) {
	turns, temp := 3, 0.5
	sys := "be <brief> & kind \u2028 now"
	n := 1
	for _, s := range []RunStart{
		{Input: UserText("go"), Kind: RunKindAgent},
		{Input: UserText(`<b>"quoted"</b> & more` + "\u2028\u2029\x00\t"), Kind: RunKindAgent, Saga: true},
		{Input: Message{Role: RoleUser, Parts: []Part{Text{Text: "look"}, Image{Mime: "image/png", Data: []byte{1, 2, 3}}, Image{URL: "https://x/a?b=<c>&d"}}}, Kind: RunKindAgent},
		{Input: Message{Role: RoleUser, Parts: []Part{Text{Text: "a"}, Text{Text: "b<>"}}}, Kind: RunKindSessionTurn, Session: &SessionRef{ID: "s", Turn: &n}},
		{Input: UserText("typed"), Kind: RunKindAgent, Typed: &TypedStart{Mode: OutputTool, SchemaDigest: "d", Schema: json.RawMessage(`{"type":"object"}`)},
			Settings:  RunSettings{MaxTurns: &turns, SystemPrompt: &sys, Sampling: &Sampling{Temperature: &temp}, ToolChoice: &ToolChoice{Mode: "tool", Name: "x"}},
			Principal: &Principal{OnBehalfOf: "u<1>", AuthorityRef: "a&b"}, Tools: []string{"x", "y"}, Ext: map[string]json.RawMessage{"k": json.RawMessage("{\"v\":\"<\u2028>\"}")}},
		{Input: UserText(""), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}},
	} {
		got, err := marshalJournal(s)
		if err != nil {
			t.Fatal(err)
		}
		if want := twoPassStartJSON(t, s); !bytes.Equal(got, want) {
			t.Errorf("one-pass encoding differs:\n got %s\nwant %s", got, want)
		}
		var back RunStart
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("decode %s: %v", got, err)
		}
		if !sameMessage(back.Input, s.Input) {
			t.Errorf("input read back as %+v, want %+v", back.Input, s.Input)
		}
	}
}

func twoPassStartJSONErr(s RunStart) ([]byte, error) {
	var in json.RawMessage
	var err error
	if txt, ok := plainUserText(s.Input); ok {
		in, err = marshalJournal(txt)
	} else {
		in, err = marshalJournal(s.Input)
	}
	if err != nil {
		return nil, err
	}
	return marshalJournal(runStartWire{Input: in, Saga: s.Saga, Kind: s.Kind, Session: s.Session, Flow: s.Flow,
		Typed: s.Typed, Settings: s.Settings, Principal: s.Principal, Tools: s.Tools, Ext: s.Ext})
}

func FuzzRev138c_RunStartOnePass(f *testing.F) {
	f.Add("go", "", "user", uint8(0), []byte(`{"v":1}`), "x", "s")
	f.Add("a\xffb <&>", "b\x00", "assistant", uint8(7), []byte(" { \"a\" : [1, 2] } "), "", "")
	f.Add("", "", "", uint8(3), []byte(`not json`), "<t>", "")
	f.Fuzz(func(t *testing.T, text, text2, role string, mode uint8, ext []byte, tool, sess string) {
		var in Message
		switch mode % 5 {
		case 0:
			in = UserText(text)
		case 1:
			in = Message{Role: Role(role), Parts: []Part{Text{Text: text}}}
		case 2:
			in = Message{Role: RoleUser, Parts: []Part{Text{Text: text}, Text{Text: text2}}}
		case 3:
			in = Message{Role: RoleUser, Parts: []Part{Image{Mime: text2, Data: []byte(text)}}}
		case 4:
			in = Message{Role: RoleUser}
		}
		s := RunStart{Input: in, Kind: RunKind(text2), Saga: mode&8 != 0}
		if mode&16 != 0 {
			s.Ext = map[string]json.RawMessage{text2: json.RawMessage(ext)}
		}
		if mode&32 != 0 {
			s.Tools = []string{tool}
		}
		if mode&64 != 0 {
			s.Session = &SessionRef{ID: sess}
		}
		if mode&128 != 0 {
			s.Principal = &Principal{OnBehalfOf: tool}
		}
		got, gerr := marshalJournal(s)
		want, werr := twoPassStartJSONErr(s)
		if (gerr == nil) != (werr == nil) {
			t.Fatalf("errors differ: one-pass %v, two-pass %v", gerr, werr)
		}
		if gerr == nil && !bytes.Equal(got, want) {
			t.Fatalf("bytes differ:\n got %q\nwant %q", got, want)
		}
	})
}
