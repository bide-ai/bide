package agent

import (
	"bytes"
	"encoding/json"
	"testing"
)

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
