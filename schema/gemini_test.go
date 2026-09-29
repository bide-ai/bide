package schema

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestGemini_Translates(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// A type list with null is the type plus nullable.
		{`{"type":["integer","null"]}`, `{"nullable":true,"type":"integer"}`},
		// Null in an enum is nullable; a string const is a one-value enum.
		{`{"type":"string","enum":["a",null]}`, `{"enum":["a"],"nullable":true,"type":"string"}`},
		{`{"const":"x"}`, `{"enum":["x"],"type":"string"}`},
		// A null anyOf branch is nullable; the rest are translated.
		{`{"anyOf":[{"type":"string"},{"type":"null"}]}`, `{"anyOf":[{"type":"string"}],"nullable":true}`},
		// A closed object drops additionalProperties:false; required and nested schemas carry over.
		{`{"type":"object","properties":{"a":{"type":"array","items":{"type":"number","format":"double"}}},"required":["a"],"additionalProperties":false}`,
			`{"properties":{"a":{"items":{"format":"double","type":"number"},"type":"array"}},"required":["a"],"type":"object"}`},
		// Kept keywords pass through; dropped ones and unknown formats go.
		{`{"type":"string","description":"d","minLength":1,"pattern":"^a","format":"email","$schema":"x","examples":["a"]}`,
			`{"description":"d","minLength":1,"pattern":"^a","type":"string"}`},
		{`{"type":"string","format":"date-time"}`, `{"format":"date-time","type":"string"}`},
		{`{"type":"integer","minimum":0,"maximum":9,"multipleOf":3}`, `{"maximum":9,"minimum":0,"type":"integer"}`},
	} {
		got, err := Gemini(json.RawMessage(tc.in))
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.in, got, tc.want)
		}
	}
}

func TestGemini_RefusesWhatItCannotExpress(t *testing.T) {
	for _, tc := range []struct{ in, where string }{
		{`{"type":"object","additionalProperties":{"type":"integer"}}`, "the root schema allows additional properties"},
		{`{"type":"object","properties":{"m":{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string"}}}}}`, "properties.m allows additional properties"},
		{`{"type":"object","properties":{"r":{"type":"object"}}}`, "properties.r declares no properties"},
		{`{"type":"object","properties":{"a":{}}}`, "properties.a has no type"},
		{`{"type":["string","integer"]}`, "Gemini takes one type"},
		{`{"type":"null"}`, "admits only null"},
		{`{"anyOf":[{"type":"null"}]}`, "admits only null"},
		{`{"type":"integer","enum":[1,2]}`, "non-string enum"},
		{`{"const":1}`, "non-string const"},
		{`{"type":"integer","enum":["1"]}`, "enum on type integer"},
		{`{"type":"array"}`, "no items"},
		{`{"type":"array","items":{"$ref":"#/$defs/x"}}`, `items uses "$ref"`},
		{`{"oneOf":[{"type":"string"}]}`, `uses "oneOf"`},
		{`{"type":"string","properties":{"a":{"type":"string"}}}`, "has properties but is not an object"},
		{`{"type":"string","items":{"type":"string"}}`, "has items but is not an array"},
		{`true`, "not a schema object"},
		{`{"type":5}`, "has type 5"},
		{`{"type":[5]}`, "has type [5]"},
		{`{"type":"string","enum":"a"}`, "has enum a"},
		{`{"anyOf":{"type":"string"}}`, "has anyOf"},
	} {
		_, err := Gemini(json.RawMessage(tc.in))
		if !errors.Is(err, ErrGeminiUnsupported) || !strings.Contains(err.Error(), tc.where) {
			t.Errorf("%s: err = %v, want ErrGeminiUnsupported mentioning %q", tc.in, err, tc.where)
		}
	}
}

// What For derives from an ordinary struct translates.
func TestGemini_ForStruct(t *testing.T) {
	type In struct {
		Q     string   `json:"q" desc:"query"`
		Limit *int     `json:"limit"`
		Tags  []string `json:"tags,omitempty"`
	}
	s, err := For[In]()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Gemini(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"properties":{"limit":{"type":"integer"},"q":{"description":"query","type":"string"},"tags":{"items":{"type":"string"},"type":"array"}},"required":["q"],"type":"object"}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
