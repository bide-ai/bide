package agent

import (
	"bytes"
	"encoding/json"
	"testing"
)

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
