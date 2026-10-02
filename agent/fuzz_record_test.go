package agent

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"unicode/utf8"
)

// normRaw is the journal's spelling of a raw JSON value: compacted, with U+2028/U+2029 escaped.
func normRaw(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, r); err != nil {
		return r
	}
	b := bytes.ReplaceAll(buf.Bytes(), []byte(lineSep), []byte(jsonEscape+"2028"))
	return bytes.ReplaceAll(b, []byte(paraSep), []byte(jsonEscape+"2029"))
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// normRecord maps a record to the form the journal keeps: raw JSON in its journal spelling, and
// empty slices as nil (omitempty drops them).
func normRecord(r Record) Record {
	r.raw = nil // the bytes it was decoded from, not its content
	r.Result = normRaw(r.Result)
	r.Evidence = normRaw(r.Evidence)
	if r.ApproverSignature != nil {
		s := *r.ApproverSignature
		s.Signature = nilIfEmpty(s.Signature)
		if s.Approver == "" && s.ApproverAlg == "" && s.Signature == nil {
			r.ApproverSignature = nil // a decoded record carries none when the journal holds none
		} else {
			r.ApproverSignature = &s
		}
	}
	if r.Message != nil {
		m := Message{Role: r.Message.Role}
		for _, p := range r.Message.Parts {
			switch v := p.(type) {
			case ToolUse:
				v.Args = normRaw(v.Args)
				p = v
			case ToolResult:
				v.Result = normRaw(v.Result)
				p = v
			case Image:
				v.Data = nilIfEmpty(v.Data)
				p = v
			}
			m.Parts = append(m.Parts, p)
		}
		r.Message = &m
	}
	return r
}

// FuzzRecordRoundTrip: for an arbitrary record built from valid-UTF-8 strings and valid raw JSON,
// decode(encode(r)) equals r in every field the journal keeps, and encode(decode(b)) == b.
func FuzzRecordRoundTrip(f *testing.F) {
	f.Add("n", "a <b> & c", []byte(`{ "h" : "<b>" }`), []byte("\x00\xff"), uint8(0), int64(5), true)
	f.Add(lineSep, "x"+paraSep, []byte(`[1.50, 1e20, -0.0]`), []byte{}, uint8(63), int64(-1), false)
	f.Add("", "", []byte(`null`), []byte(nil), uint8(255), int64(0), false)
	f.Add("\U0001F600", `"\ud800"`, []byte(`{"k":"\ud800","a":"`+lineSep+`"}`), []byte{0xd3}, uint8(62), int64(1<<40), true)
	kinds := []StepKind{StepModel, StepToolResult, StepValue, StepSignal, StepApproval, StepAttempt, StepSagaFail, ""}
	f.Fuzz(func(t *testing.T, s1, s2 string, raw, bin []byte, sel uint8, n int64, flag bool) {
		if !utf8.ValidString(s1) || !utf8.ValidString(s2) {
			return // encoding rewrites invalid UTF-8 to U+FFFD by design; audit refuses such records
		}
		if len(raw) > 0 && !json.Valid(raw) {
			raw = nil
		}
		var parts []Part
		if sel&1 != 0 {
			parts = append(parts, Text{Text: s1})
		}
		if sel&2 != 0 {
			parts = append(parts, Reasoning{Text: s2, Signature: s1}, Reasoning{Redacted: s2})
		}
		if sel&4 != 0 {
			parts = append(parts, ToolUse{ID: s1, Name: s2, Args: raw, Signature: s2})
		}
		if sel&8 != 0 {
			parts = append(parts, ToolResult{ToolUseID: s2, Result: raw, IsError: flag})
		}
		if sel&16 != 0 {
			parts = append(parts, Image{Mime: s1, Data: bin, URL: s2})
		}
		r := Record{Name: s1, Kind: kinds[int(sel>>5)%len(kinds)], ToolUseID: s2, Result: raw, IsError: flag,
			Approved: !flag, AttemptedAt: n, Reconciled: flag, Evidence: raw, claim: s1, ApproverSignature: &ApproverSignature{Approver: s2, Signature: bin}}
		if sel&32 != 0 {
			r.Message = &Message{Role: Role(s2), Parts: parts}
			r.Usage = &Usage{InputTokens: int(n), OutputTokens: int(sel), CacheReadTokens: -int(n), CacheWriteTokens: int(n) / 3}
		}
		b, err := EncodeRecord(r)
		if err != nil {
			t.Fatalf("EncodeRecord of a well-formed record: %v", err)
		}
		back, err := DecodeRecord(b)
		if err != nil {
			t.Fatalf("DecodeRecord(EncodeRecord(r)): %v\n%s", err, b)
		}
		if want, got := normRecord(r), normRecord(back); !reflect.DeepEqual(want, got) {
			t.Fatalf("round trip lost data:\nwant %#v\ngot  %#v\nenc  %s", want, got, b)
		}
		again, err := EncodeRecord(back)
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("encode(decode(b)) != b (%v):\n%s\n%s", err, b, again)
		}
	})
}

// FuzzDecodeRecord: arbitrary bytes never panic DecodeRecord, and anything it accepts reaches the
// journal fixed point in one EncodeRecord.
func FuzzDecodeRecord(f *testing.F) {
	for _, s := range []string{
		`{"name":"@llm/0","kind":"model","message":{"role":"assistant","parts":[{"text":"a","type":"text"},{"args":{"q":1},"id":"c1","name":"t","type":"tool_use"},{"data":"AAH/PA==","mime":"image/png","type":"image"}]}}`,
		`{"name":"c1","kind":"tool_result","tool_use_id":"c1","result":{"n":[1e20,-0.0]},"evidence":" "}`,
		`{"message":{"parts":[{"type":"reasoning","text":"t","signature":"s"},{"type":"tool_result","result":null}]}}`,
		// Case-variant and unknown names inside a message part.
		`{"message":{"role":"assistant","parts":[{"type":"text","text":"refund $1","Text":"refund $10","approved_by":"cfo"}]}}`,
		// Non-canonical base64 (unused bits set, a line break).
		`{"kind":"approval","signature":"0x==","message":{"parts":[{"type":"image","mime":"image/png","data":"AB\nC="}]}}`,
		// Escaped lone surrogates, a surrogate pair, and U+2028 raw and escaped.
		`{"name":"run\ud800","result":{"k":"\udc00"},"message":{"parts":[{"type":"text","text":"😀\ud800"}]}}`,
		"{\"name\":\"a b\\u2029\",\"claim\":\"\xff\"}",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := DecodeRecord(b)
		if err != nil {
			return
		}
		enc, err := EncodeRecord(r)
		if err != nil {
			return // e.g. a part whose raw JSON encoding/json refuses to re-emit
		}
		r2, err := DecodeRecord(enc)
		if err != nil {
			t.Fatalf("DecodeRecord(EncodeRecord(decoded)): %v\n%s", err, enc)
		}
		if !reflect.DeepEqual(normRecord(r), normRecord(r2)) {
			t.Fatalf("decode(encode(decoded)) differs:\n%#v\n%#v", r, r2)
		}
		enc2, err := EncodeRecord(r2)
		if err != nil || !bytes.Equal(enc, enc2) {
			t.Fatalf("not a fixed point (%v):\n%s\n%s", err, enc, enc2)
		}
	})
}
