package agent

import (
	"bytes"
	"encoding/json"
	"testing"
)

// slowPart is what marshalPart wrote before spliceType: the part's struct decoded into a map and
// the map encoded. spliceType must write exactly these bytes, since they are journaled and an
// audit leaf commits to them.
func slowPart(t *testing.T, p Part) []byte {
	t.Helper()
	kind, err := partKind(p)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := marshalJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tagPart(inner, kind)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func checkSplice(t *testing.T, p Part) {
	t.Helper()
	kind, err := partKind(p)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := marshalJournal(p)
	if err != nil {
		return // marshalPart fails the same way whichever path it would take
	}
	got, ok := spliceType(inner, kind)
	if !ok {
		t.Fatalf("spliceType(%q) declined the encoding of a %T", inner, p)
	}
	if want := slowPart(t, p); !bytes.Equal(got, want) {
		t.Fatalf("spliceType(%q) =\n%q\nthe map encoding writes\n%q", inner, got, want)
	}
	viaPart, err := marshalPart(p)
	if err != nil || !bytes.Equal(viaPart, got) {
		t.Fatalf("marshalPart = %q, %v; want %q", viaPart, err, got)
	}
}

// Every kind of part, with the strings and raw JSON the journal must keep as they are, is
// spliced to the bytes the map encoding writes.
func TestSpliceType_MatchesMapEncoding(t *testing.T) {
	odd := "a <b> & c\"d\\e}f,g]h{i[j:k" + lineSep + paraSep + "\x00\x1f bad\xffutf8 é 😀"
	raws := []string{
		`{}`, `[]`, `null`, `true`, `false`, `0`, `-1.50e+20`, `""`, `"x"`,
		`{ "q" : "a<b && c>d" ,  "n": 1.50 }`,
		`{"a":{"b":[1,{"c":"}]"}],"d":"\"}"},"e":[[],{}]}`,
		"\"" + lineSep + "\xff\\u0041\\/\"",
		`[1e20, -0.0, 9007199254740993]`,
		`{"type":"not the part's"}`,
	}
	parts := []Part{
		Text{}, Text{Text: odd},
		Reasoning{}, Reasoning{Text: odd, Signature: odd, Redacted: odd},
		ToolUse{}, ToolUse{ID: odd, Name: odd, Signature: odd},
		ToolResult{}, ToolResult{ToolUseID: odd, IsError: true},
		Image{}, Image{Mime: odd, Data: []byte{0, 1, 0xff, '<', '"'}, URL: odd},
	}
	for _, raw := range raws {
		parts = append(parts,
			ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(raw)},
			ToolResult{ToolUseID: "c1", Result: json.RawMessage(raw), IsError: true})
	}
	for _, p := range parts {
		checkSplice(t, p)
	}
}

// spliceType declines what it cannot splice verbatim, and marshalPart then takes the map
// encoding: names that need unescaping, are not ASCII, repeat, or are "type" already, and
// anything that is not one compact object.
func TestSpliceType_Declines(t *testing.T) {
	for _, in := range []string{
		``, `{`, `[]`, `"x"`, `{"a":1,}`, `{"a":,"b":1}`, `{"a":1,"b"}`, `{"a"}`, `{"a":}`, `{,}`, `{"a":1"b":2}`,
		`{"a\"b":1}`, `{"a` + jsonEscape + `0062":1}`, `{"é":1}`, `{"a":1,"a":2}`, `{"type":"x"}`, `{"a":"unterminated}`,
		`{"a":[1}`, `{"a":[1}]}`, `{"a":{"b":1]}`, `{"a":[1],"b":}`, `{"a":1]`, `{ "a":1}`,
	} {
		if b, ok := spliceType([]byte(in), "text"); ok {
			t.Errorf("spliceType(%q) = %q, true; want it declined", in, b)
		}
	}
	if b, ok := spliceType([]byte(`{}`), "text"); !ok || string(b) != `{"type":"text"}` {
		t.Errorf(`spliceType("{}") = %q, %v`, b, ok)
	}
}

func FuzzSpliceType(f *testing.F) {
	f.Add("a <b> & c", []byte(`{ "h" : "<b>" }`), []byte{0xff})
	f.Add("x\xffy"+lineSep+"\x00", []byte("\"a\xffb"+paraSep+"\""), []byte(nil))
	f.Add("}],\"", []byte(`[1.50, 1e20, -0.0, {"a":"}"}]`), []byte("<>"))
	f.Fuzz(func(t *testing.T, s string, raw, data []byte) {
		parts := []Part{Text{Text: s}, Reasoning{Text: s, Signature: s, Redacted: s},
			ToolUse{ID: s, Name: s, Signature: s}, ToolResult{ToolUseID: s},
			Image{Mime: s, Data: data, URL: s}}
		if json.Valid(raw) {
			parts = append(parts, ToolUse{ID: s, Name: s, Args: raw}, ToolResult{ToolUseID: s, Result: raw, IsError: len(data)%2 == 0})
		}
		for _, p := range parts {
			checkSplice(t, p)
		}
	})
}
