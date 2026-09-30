package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"testing"
)

// strictTargets are the proof artifacts a verifier reads from a file, selected by the fuzz input's
// first byte.
var strictTargets = []func() any{
	func() any { return new(ProofBundle) },
	func() any { return new(AbsenceBundle) },
	func() any { return new(EvidencePackage) },
	func() any { return new(SignedTreeHead) },
	func() any { return new(RunCertificate) },
}

// strictSeeds returns a marshalled, valid instance of each target (index-aligned with strictTargets).
func strictSeeds(tb testing.TB) [][]byte {
	store, recs, sth := fuzzRun(tb)
	ctx := context.Background()
	pb, err := ProveRecord(ctx, store, "run", 1, sth) // leaf 0 is the journal header
	if err != nil {
		tb.Fatal(err)
	}
	head, err := NewAbsenceTreeHead(recs, ToolUseKeys, sth.TreeHead, 1000)
	if err != nil {
		tb.Fatal(err)
	}
	ash := SignTreeHead(head, fuzzPriv)
	ab, err := ProveAbsentBundle(recs, ToolUseKeys, "tooluse:zz", ash)
	if err != nil {
		tb.Fatal(err)
	}
	ev, err := Evidence(ctx, store, "run", fuzzPriv, 1000, WithAllToolCalls(), WithLabel("l"))
	if err != nil {
		tb.Fatal(err)
	}
	rc := RunCertificate{Format: RunCertificateFormat, RunID: "run", Properties: []string{"a"}, UsedPolicies: []string{"p"}, STH: sth}
	var out [][]byte
	for _, v := range []any{pb, ab, ev, sth, rc} {
		b, err := json.Marshal(v)
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// FuzzUnmarshalStrict checks, for arbitrary input into each proof artifact type:
//   - no panic;
//   - differential: whenever UnmarshalStrict accepts, encoding/json decodes the same value;
//   - determinism: re-marshalling and strict-decoding again is a fixed point;
//   - reads-as: every name/value a reader sees in the accepted file is what the decoded value holds
//     (a name absent from the re-encoding may only carry a zero value), so no case-variant, unknown,
//     or shadowing name is accepted anywhere, including inside embedded records.
func FuzzUnmarshalStrict(f *testing.F) {
	for i, s := range strictSeeds(f) {
		f.Add(byte(i), s)
	}
	f.Add(byte(0), []byte(`{"run_id":"r","RUN_ID":"x"}`))
	f.Add(byte(3), []byte(`{"kind":"journal","size":1,"size":2}`))
	// The earlier findings, as edits of the genuine proof bundle (0) and tree head (3): case-variant
	// and unknown names inside a message part, a lone surrogate escape, and non-canonical base64.
	seeds := strictSeeds(f)
	for _, e := range [][3]string{
		{"0", `"text":"refund $10",`, `"text":"refund $1","Text":"refund $10",`},
		{"0", `"text":"refund $10",`, `"text":"refund $10","approved_by":"cfo",`},
		{"0", `"text":"refund $10",`, `"text":"refund $10\ud800",`},
		{"3", `"run_id":"run"`, `"run_id":"run\ud800"`},
		{"3", `"signature":"`, `"signature":"\n`},
	} {
		i := int(e[0][0] - '0')
		forged := bytes.Replace(seeds[i], []byte(e[1]), []byte(e[2]), 1)
		if bytes.Equal(forged, seeds[i]) {
			f.Fatalf("seed edit %q does not apply to %s", e[1], seeds[i])
		}
		f.Add(byte(i), forged)
	}
	// A root of one byte, 0xd3, is "0w==" in standard base64; "0x==" decodes to the same byte.
	th := SignTreeHead(TreeHead{Kind: TreeJournal, RunID: "run", Root: []byte{0xd3}, Timestamp: 1}, fuzzPriv)
	thb, _ := json.Marshal(th)
	f.Add(byte(3), bytes.Replace(thb, []byte(`"root":"0w=="`), []byte(`"root":"0x=="`), 1))
	f.Fuzz(func(t *testing.T, sel byte, data []byte) {
		mk := strictTargets[int(sel)%len(strictTargets)]
		v := mk()
		if err := UnmarshalStrict(data, v); err != nil {
			return
		}
		// Differential against encoding/json.
		w := mk()
		if err := json.Unmarshal(data, w); err != nil {
			t.Fatalf("UnmarshalStrict accepted what encoding/json rejects (%v): %q", err, data)
		}
		enc1, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal accepted value: %v", err)
		}
		encW, _ := json.Marshal(w)
		if !bytes.Equal(enc1, encW) {
			t.Fatalf("strict and encoding/json decode differently:\nstrict: %s\njson:   %s\ninput:  %q", enc1, encW, data)
		}
		// Determinism.
		v2 := mk()
		if err := UnmarshalStrict(enc1, v2); err != nil {
			t.Fatalf("re-marshalled value rejected by UnmarshalStrict: %v\n%s", err, enc1)
		}
		enc2, _ := json.Marshal(v2)
		if !bytes.Equal(enc1, enc2) {
			t.Fatalf("not a fixed point:\n%s\n%s", enc1, enc2)
		}
		// Reads-as.
		var in, out any
		dIn := json.NewDecoder(bytes.NewReader(data))
		dIn.UseNumber()
		if err := dIn.Decode(&in); err != nil {
			t.Fatalf("generic decode of accepted input: %v", err)
		}
		dOut := json.NewDecoder(bytes.NewReader(enc1))
		dOut.UseNumber()
		_ = dOut.Decode(&out)
		if p, why := readsAs(in, out, "$"); why != "" {
			t.Fatalf("accepted file does not read as it decodes at %s: %s\ninput:   %q\ndecoded: %s", p, why, data, enc1)
		}
		// A generic decode turns a lone surrogate escape into U+FFFD too, so readsAs cannot see
		// one. Raw JSON keeps its escapes verbatim, so any escape the input has and the decoded
		// value lacks was read as U+FFFD.
		if a, b := loneSurrogates(data), loneSurrogates(enc1); a != b {
			t.Fatalf("accepted %d lone surrogate escapes, of which the decoded value keeps %d\ninput:   %q\ndecoded: %s", a, b, data, enc1)
		}
	})
}

// readsAs reports the first path where the generic reading of the input (in) disagrees with the
// re-encoding of the decoded value (out).
func readsAs(in, out any, path string) (string, string) {
	switch a := in.(type) {
	case map[string]any:
		b, ok := out.(map[string]any)
		if !ok {
			if isZeroJSON(a) && out == nil {
				return "", ""
			}
			return path, fmt.Sprintf("object read as %T", out)
		}
		for k, av := range a {
			bv, ok := b[k]
			if !ok {
				if isZeroJSON(av) {
					continue
				}
				return path + "." + k, "name is not in the decoded value (case variant, unknown, or shadowed)"
			}
			if p, why := readsAs(av, bv, path+"."+k); why != "" {
				return p, why
			}
		}
		return "", ""
	case []any:
		b, ok := out.([]any)
		if !ok {
			if len(a) == 0 && out == nil {
				return "", ""
			}
			return path, fmt.Sprintf("array read as %T", out)
		}
		if len(a) != len(b) {
			return path, fmt.Sprintf("array length %d read as %d", len(a), len(b))
		}
		for i := range a {
			if p, why := readsAs(a[i], b[i], fmt.Sprintf("%s[%d]", path, i)); why != "" {
				return p, why
			}
		}
		return "", ""
	case json.Number:
		b, ok := out.(json.Number)
		if !ok {
			return path, fmt.Sprintf("number %s read as %v", a, out)
		}
		x, _, err1 := big.ParseFloat(string(a), 10, 256, big.ToNearestEven)
		y, _, err2 := big.ParseFloat(string(b), 10, 256, big.ToNearestEven)
		if err1 != nil || err2 != nil || x.Cmp(y) != 0 {
			return path, fmt.Sprintf("number %s read as %s", a, b)
		}
		return "", ""
	case nil:
		if out == nil || isZeroJSON(out) {
			return "", ""
		}
		return path, fmt.Sprintf("null read as %v", out)
	default:
		if !reflect.DeepEqual(in, out) {
			if isZeroJSON(in) && out == nil {
				return "", ""
			}
			return path, fmt.Sprintf("%#v read as %#v", in, out)
		}
		return "", ""
	}
}

// loneSurrogates counts the \uXXXX escapes in b that encode a UTF-16 surrogate outside a
// high-then-low pair.
func loneSurrogates(b []byte) int {
	unit := func(i int) int {
		if i+6 > len(b) || b[i] != '\\' || b[i+1] != 'u' {
			return -1
		}
		u, err := strconv.ParseUint(string(b[i+2:i+6]), 16, 16)
		if err != nil {
			return -1
		}
		return int(u)
	}
	n := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		u := unit(i)
		switch {
		case u >= 0xd800 && u < 0xdc00:
			if low := unit(i + 6); low >= 0xdc00 && low <= 0xdfff {
				i += 11
				continue
			}
			n++
		case u >= 0xdc00 && u <= 0xdfff:
			n++
		}
		i++ // skip the escaped character, so an escaped backslash does not start an escape
	}
	return n
}

func isZeroJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case json.Number:
		f, _, err := big.ParseFloat(string(x), 10, 64, big.ToNearestEven)
		return err == nil && f.Sign() == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, e := range x {
			if !isZeroJSON(e) {
				return false
			}
		}
		return true
	}
	return false
}
