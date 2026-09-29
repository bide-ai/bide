package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Provider tokens a later turn must send back (a Gemini thoughtSignature on a call) are
// journaled with the part and survive a decode, so a resumed run sends them as the live one
// would have.
func TestEncodeRecord_KeepsProviderTokens(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{
		Reasoning{Text: "t", Signature: "s1"},
		Reasoning{Redacted: "ENCRYPTED"},
		ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(`{}`), Signature: "sig"},
	}}
	b, err := EncodeRecord(Record{Name: "@llm/0", Kind: StepModel, Message: &msg})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRecord(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got.Message, msg) {
		t.Fatalf("decoded %+v, want %+v", *got.Message, msg)
	}
}
