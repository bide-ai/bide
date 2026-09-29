package gcf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"

	gcfgo "github.com/blackwell-systems/gcf-go"
)

// exactJSON decodes b with every number kept as its literal.
func exactJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, fmt.Errorf("trailing data")
	}
	return v, nil
}

// sameValue compares two exact-decoded JSON values; numbers compare as exact rationals.
func sameValue(a, b any, path string) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return fmt.Sprintf("%s: object %v vs %v", path, a, b)
		}
		for k := range x {
			if d := sameValue(x[k], y[k], path+"."+k); d != "" {
				return d
			}
		}
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return fmt.Sprintf("%s: array %v vs %v", path, a, b)
		}
		for i := range x {
			if d := sameValue(x[i], y[i], fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return fmt.Sprintf("%s: number %s vs %v (%T)", path, x, b, b)
		}
		ra, ok1 := exactNumber(string(x))
		rb, ok2 := exactNumber(string(y))
		if !ok1 || !ok2 || ra.Cmp(rb) != 0 {
			return fmt.Sprintf("%s: number %s vs %s", path, x, y)
		}
	default:
		if a != b {
			return fmt.Sprintf("%s: %#v vs %#v", path, a, b)
		}
	}
	return ""
}

// exactNumber parses a JSON number literal exactly. big.Rat refuses exponents it cannot hold; a
// literal whose digits are all zero is zero whatever its exponent.
func exactNumber(s string) (*big.Rat, bool) {
	if r, ok := new(big.Rat).SetString(s); ok {
		return r, true
	}
	mant, _, _ := strings.Cut(strings.ToLower(s), "e")
	if strings.Trim(mant, "-0.") == "" {
		return new(big.Rat), true
	}
	return nil, false
}

// FuzzEncodeToolResult: arbitrary bytes never panic the codec; whenever it returns GCF instead of
// an error (the JSON fallback), the GCF decodes back to exactly the tool's JSON value, every number
// at its exact value, so the model is never shown a lossy rendering.
func FuzzEncodeToolResult(f *testing.F) {
	for _, s := range []string{
		`{"city":"Paris","temp_c":21,"conditions":["sunny","breezy"]}`,
		`[1.50, 1e20, -0.0, 9007199254740993, 0.1]`,
		`{"a":{"b":{"c":[{"x":1,"y":"2"},{"x":3,"y":"4"}]}}}`,
		`{"a":1,"a":2}`,
		`"\ud800"`, `null`, `[]`, `{}`, `[[],{},""]`,
		`{"k":"line\nbreak | pipe = eq : colon # hash"}`,
		// Trailing data after the first value (it was dropped from what the model read).
		`{"a":1} {"b":2}`, `[1][2]`, `""0`, `1 2`,
		// Lone surrogate escapes and invalid UTF-8, which decoders rewrite to U+FFFD.
		`{"k":"\udc00x"}`, "\"\xff\"", `["\ud83d"]`,
		// Numbers with no exact float64 form, and integers outside int64.
		`0.30000000000000000001`, `9223372036854775808`, `1e400`, `-0e-400`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		out, err := New().EncodeToolResult(raw)
		if err != nil || len(raw) == 0 {
			return
		}
		if !json.Valid(raw) {
			t.Fatalf("codec encoded input that is not exactly one valid JSON value: %q -> %q", raw, out)
		}
		want, err := exactJSON(raw)
		if err != nil {
			t.Fatalf("codec encoded input that is not one JSON value (%v): %q -> %q", err, raw, out)
		}
		got, err := gcfgo.DecodeGeneric(out)
		if err != nil {
			t.Fatalf("codec emitted GCF that does not decode (%v):\ninput %q\ngcf   %q", err, raw, out)
		}
		gb, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("decoded GCF does not marshal: %v", err)
		}
		gv, _ := exactJSON(gb)
		if d := sameValue(want, gv, "$"); d != "" {
			t.Fatalf("GCF reads differently from the JSON: %s\ninput %q\ngcf   %q", d, raw, out)
		}
	})
}
