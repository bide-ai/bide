package agent

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

const replacementChar = "\xef\xbf\xbd" // U+FFFD, UTF-8 encoded

// The journal encoding is pinned byte for byte: it is what every store persists and what every
// audit leaf commits to, so it must not depend on the store, the build (GOEXPERIMENT=nojsonv2
// included), or HTML escaping.
func TestEncodeRecord_Golden(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{
		Text{Text: "a <b> & c" + lineSep + "d\x00 bad\xffutf8"},
		ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(`{ "q" : "a<b && c>d" ,  "n": 1.50 }`)},
		Image{Mime: "image/png", Data: []byte{0, 1, 0xff, '<'}},
	}}
	rec := Record{Name: "@llm/0", Kind: StepModel, Message: &msg}
	got, err := EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"@llm/0","kind":"model","message":{"role":"assistant","parts":[` +
		`{"text":"a <b> & c` + jsonEscape + `2028d` + jsonEscape + `0000 bad` + replacementChar + `utf8","type":"text"},` +
		`{"args":{"q":"a<b && c>d","n":1.50},"id":"c1","name":"t","type":"tool_use"},` +
		`{"data":"AAH/PA==","mime":"image/png","type":"image"}]}}`
	if string(got) != want {
		t.Fatalf("EncodeRecord =\n%q\nwant\n%q", got, want)
	}

	res := Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1",
		Result: json.RawMessage("{ \"h\" : \"<b>" + lineSep + paraSep + "\xff\" ,\n \"n\": [1e20, -0.0, 9007199254740993] }")}
	got, err = EncodeRecord(res)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"name":"c1","kind":"tool_result","tool_use_id":"c1","result":{"h":"<b>` + jsonEscape + `2028` + jsonEscape + `2029` + "\xff" + `","n":[1e20,-0.0,9007199254740993]}}`
	if string(got) != want {
		t.Fatalf("EncodeRecord =\n%q\nwant\n%q", got, want)
	}
}

// Decoding the journal encoding and encoding the result again gives the same bytes, so an audit
// leaf computed from a record read back from a store is the bytes the store persisted.
func FuzzEncodeRecord_FixedPoint(f *testing.F) {
	f.Add("a <b> & c", []byte(`{ "h" : "<b>" }`))
	f.Add("x\xffy"+lineSep+"\x00", []byte("\"a\xffb"+paraSep+"\""))
	f.Add("", []byte(`[1.50, 1e20, -0.0]`))
	f.Add(paraSep, []byte(`"`+jsonEscape+`003c"`))
	f.Fuzz(func(t *testing.T, text string, raw []byte) {
		if !json.Valid(raw) {
			return
		}
		msg := Message{Role: RoleAssistant, Parts: []Part{Text{Text: text}, Reasoning{Text: text, Signature: text},
			ToolUse{ID: text, Name: text, Args: raw}, ToolResult{ToolUseID: text, Result: raw}}}
		rec := Record{Name: text, Kind: StepModel, Message: &msg, Result: raw, Evidence: raw, Approver: text}
		data, err := EncodeRecord(rec)
		if err != nil {
			return // not every valid JSON text survives encoding/json's Marshaler checks
		}
		back, err := DecodeRecord(data)
		if err != nil {
			t.Fatalf("DecodeRecord(%q): %v", data, err)
		}
		again, err := EncodeRecord(back)
		if err != nil {
			t.Fatalf("EncodeRecord(decoded): %v", err)
		}
		if !bytes.Equal(again, data) {
			t.Fatalf("not a fixed point:\nfirst:  %q\nsecond: %q", data, again)
		}
		// A single plain pass over the decoded record is already stable too, so the fixed point
		// does not rest on EncodeRecord's own second pass.
		single, err := marshalJournal(back)
		if err != nil {
			t.Fatalf("marshalJournal(decoded): %v", err)
		}
		if !bytes.Equal(single, data) {
			t.Fatalf("a single pass over the decoded record differs:\nstored: %q\nsingle: %q", data, single)
		}
		back2, err := DecodeRecord(again)
		if err != nil || !reflect.DeepEqual(back, back2) {
			t.Fatalf("decoding the fixed point again gives a different record (%v)", err)
		}
	})
}
