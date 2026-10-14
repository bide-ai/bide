package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

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
