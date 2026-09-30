package gcf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strings"
	"testing"

	gcfgo "github.com/blackwell-systems/gcf-go"
)

// decoded collects every number in a value GCF decoded, as exact rationals.
func decoded(t *testing.T, v any, out *[]*big.Rat) {
	t.Helper()
	add := func(s string) {
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("not a number: %q", s)
		}
		*out = append(*out, r)
	}
	switch x := v.(type) {
	case *gcfgo.OrderedMap:
		for _, k := range x.Keys() {
			val, _ := x.Get(k)
			decoded(t, val, out)
		}
	case map[string]any:
		for _, val := range x {
			decoded(t, val, out)
		}
	case []any:
		for _, e := range x {
			decoded(t, e, out)
		}
	case int64, int:
		add(fmt.Sprint(x))
	case float64:
		// The GCF text is the shortest decimal that reads back as x, which is what the model
		// reads; fmt prints the same shortest decimal.
		add(fmt.Sprint(x))
	}
}

// literals returns every number literal in a JSON document, as exact rationals.
func literals(t *testing.T, in string) []*big.Rat {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(in)))
	dec.UseNumber()
	var out []*big.Rat
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if n, ok := tok.(json.Number); ok {
			r, ok := new(big.Rat).SetString(n.String())
			if !ok {
				t.Fatalf("not a number: %s", n)
			}
			out = append(out, r)
		}
	}
}

func sortRats(rs []*big.Rat) {
	slices.SortFunc(rs, func(a, b *big.Rat) int { return a.Cmp(b) })
}

// Every number reaches the model as exactly the value the tool returned. Decoding into any turned
// numbers into float64, so an integer above 2^53 (a snowflake ID, say) was rounded to a different
// integer, and the model read the wrong ID.
func TestEncodeToolResult_NumbersExact(t *testing.T) {
	for _, in := range []string{
		`{"id":9007199254740993}`,
		`{"snowflake":1234567890123456789,"max":9223372036854775807,"min":-9223372036854775808}`,
		`{"neg":-9007199254740993,"n":-42}`,
		`{"exp":1e20,"small":1.5e-7,"neg_exp":-2.5E+30}`,
		`{"dec":0.1,"money":19.99,"trail":1.50,"zero":-0.0}`,
		`[{"a":1,"b":2.5},{"a":9007199254740993,"b":-0.125}]`,
	} {
		out, err := Codec{}.EncodeToolResult(json.RawMessage(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		back, err := gcfgo.DecodeGeneric(out)
		if err != nil {
			t.Fatalf("%s: decode GCF %q: %v", in, out, err)
		}
		var got []*big.Rat
		decoded(t, back, &got)
		want := literals(t, in)
		sortRats(got)
		sortRats(want)
		if len(got) != len(want) {
			t.Fatalf("%s: GCF %q carries %d numbers, want %d", in, out, len(got), len(want))
		}
		for i := range want {
			if got[i].Cmp(want[i]) != 0 {
				t.Errorf("%s: GCF %q carries %s where the tool returned %s", in, out, got[i].RatString(), want[i].RatString())
			}
		}
	}
}

// A number GCF cannot carry exactly (an integer outside int64, a decimal float64 cannot hold)
// is an error, so provider.EncodeToolResultOr falls back to the JSON, which carries it as written.
func TestEncodeToolResult_InexactNumberFallsBack(t *testing.T) {
	for _, in := range []string{
		`{"big":123456789012345678901234567890}`,
		`{"pi":3.14159265358979323846264338327950288}`,
		`{"huge":1e400}`,
		`{"tiny":1e-400}`,
		`{"e":1e99999999999999999999}`,
		`{"e":1e-99999999999999999999}`,
		`{"x":[1, 0.30000000000000000001]}`,
	} {
		if out, err := (Codec{}).EncodeToolResult(json.RawMessage(in)); err == nil {
			t.Errorf("%s: encoded as %q, want an error so the caller falls back to JSON", in, out)
		}
	}
}

// Object keys keep the tool's order: a model reading a record sees its fields as the tool laid
// them out.
func TestEncodeToolResult_KeepsKeyOrder(t *testing.T) {
	out, err := Codec{}.EncodeToolResult(json.RawMessage(`{"zeta":1,"alpha":2,"mid":3}`))
	if err != nil {
		t.Fatal(err)
	}
	z, a, m := strings.Index(out, "zeta"), strings.Index(out, "alpha"), strings.Index(out, "mid")
	if z < 0 || a < 0 || m < 0 || !(z < a && a < m) {
		t.Fatalf("GCF %q does not keep the key order zeta, alpha, mid", out)
	}
}
