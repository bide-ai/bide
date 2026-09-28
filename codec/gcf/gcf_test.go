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

// normalize re-encodes through JSON so numeric types (int vs float64) and map
// key ordering compare equal regardless of the decoder that produced them.
func normalize(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
