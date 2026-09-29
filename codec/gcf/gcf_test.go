package gcf

import (
	"encoding/json"
	"reflect"
	"testing"

	gcfgo "github.com/blackwell-systems/gcf-go"
)

func TestEncodeToolResult_RoundTrip(t *testing.T) {
	raw := json.RawMessage(`{"city":"Paris","temp_c":21,"conditions":["sunny","breezy"]}`)

	out, err := New().EncodeToolResult(raw)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty GCF output")
	}

	// The GCF form must decode back to the same structured value.
	got, err := gcfgo.DecodeGeneric(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if !reflect.DeepEqual(normalize(got), normalize(want)) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestEncodeToolResult_Empty(t *testing.T) {
	out, err := New().EncodeToolResult(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty output for empty input, got %q", out)
	}
}

func TestEncodeToolResult_InvalidJSON(t *testing.T) {
	if _, err := New().EncodeToolResult(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("expected an error decoding invalid JSON")
	}
}

// A tool result is one JSON value. Input with a second value after the first is an error, so the
// caller falls back to JSON; encoding only the first would drop the rest from what the model reads.
func TestEncodeToolResult_TrailingDataIsAnError(t *testing.T) {
	for _, raw := range []string{`{"a":1} {"b":2}`, `[1][2]`, `""0`, `1 2`, `null null`, `{"a":1}]`} {
		if out, err := New().EncodeToolResult(json.RawMessage(raw)); err == nil {
			t.Errorf("EncodeToolResult(%q) = %q, want an error (trailing data)", raw, out)
		}
	}
	// Surrounding whitespace is not data.
	if _, err := New().EncodeToolResult(json.RawMessage(" {\"a\":1}\n\t ")); err != nil {
		t.Errorf("whitespace around one value: %v", err)
	}
}

// normalize re-encodes through JSON so numeric types (int vs float64) and map
// key ordering compare equal regardless of the decoder that produced them.
func normalize(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
