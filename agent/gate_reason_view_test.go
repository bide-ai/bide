package agent

import (
	"reflect"
	"testing"

	"github.com/bide-ai/bide/internal/gatereason"
)

// GateReason is converted from the reader's struct field by field; the conversion fails to compile when
// the fields differ in name, order or type, and this test holds the JSON names too (the conversion ignores
// tags), so the document the notifier sends is the one the reader reads.
func TestGateReason_MatchesTheReadersStruct(t *testing.T) {
	a, b := reflect.TypeFor[GateReason](), reflect.TypeFor[gatereason.Reason]()
	if a.NumField() != b.NumField() {
		t.Fatalf("GateReason has %d fields, the reader's Reason %d", a.NumField(), b.NumField())
	}
	for i := 0; i < a.NumField(); i++ {
		fa, fb := a.Field(i), b.Field(i)
		if fa.Name != fb.Name || fa.Type != fb.Type || fa.Tag.Get("json") != fb.Tag.Get("json") {
			t.Errorf("field %d: GateReason %s %s `json:%q`, reader %s %s `json:%q`", i, fa.Name, fa.Type, fa.Tag.Get("json"), fb.Name, fb.Type, fb.Tag.Get("json"))
		}
	}
	amt := int64(5)
	in := gatereason.Reason{Kind: "cap", Source: "s", Ref: "r", Pos: 3, Rule: "x", Action: "gate", Why: "over", Currency: "USD", Amount: &amt, Limit: 9, At: "t"}
	out := gateReasons([]gatereason.Reason{in})
	if len(out) != 1 || out[0].Line() != in.Line() || *out[0].Amount != 5 || out[0].Pos != 3 || out[0].At != "t" {
		t.Fatalf("converted = %+v, want every field kept", out)
	}
	if gateReasons(nil) != nil {
		t.Error("no reasons became a non-nil list")
	}
}
