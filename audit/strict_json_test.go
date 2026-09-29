package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type strictInner struct {
	Size int `json:"size"`
}

type strictBase struct {
	Label string `json:"label"`
	Kind  string // untagged: its JSON name is "Kind"
}

type strictDoc struct {
	strictBase                     // promoted: label, Kind
	Name       string              `json:"name"`
	Skip       string              `json:"-"`
	Inner      *strictInner        `json:"inner,omitempty"`
	Items      []strictInner       `json:"items"`
	Raw        json.RawMessage     `json:"raw,omitempty"`
	Bytes      []byte              `json:"bytes,omitempty"`
	Tags       map[string]string   `json:"tags,omitempty"`
	When       time.Time           `json:"when"`
	Nested     map[string][]string `json:"nested,omitempty"`
}

// UnmarshalStrict accepts exactly what encoding/json would decode into the value, spelled exactly,
// and rejects anything that could make a file read differently from what is verified.
func TestUnmarshalStrict(t *testing.T) {
	good := `{"label":"l","Kind":"k","name":"n","inner":{"size":1},"items":[{"size":2},{"size":3}],` +
		`"raw":{"any":1,"Any":2,"nest":[{"x":1}]},"bytes":"AAE=","tags":{"a":"1","A":"2"},` +
		`"when":"2026-09-29T00:00:00Z","nested":{"k":["v"]}}`
	var d strictDoc
	if err := UnmarshalStrict([]byte(good), &d); err != nil {
		t.Fatalf("a well-formed document was rejected: %v", err)
	}
	if d.Label != "l" || d.Kind != "k" || d.Inner.Size != 1 || len(d.Items) != 2 || d.Tags["A"] != "2" || len(d.Bytes) != 2 {
		t.Fatalf("decoded %+v", d)
	}

	bad := map[string]string{
		"a duplicate name":                     `{"name":"a","name":"b"}`,
		"a duplicate name in a nested struct":  `{"inner":{"size":1,"size":2}}`,
		"a duplicate name in a slice element":  `{"items":[{"size":1},{"size":2,"size":3}]}`,
		"a duplicate name inside raw JSON":     `{"raw":{"x":1,"x":2}}`,
		"a duplicate name deep inside raw":     `{"raw":{"a":[{"x":1,"x":2}]}}`,
		"a duplicate map key":                  `{"tags":{"a":"1","a":"2"}}`,
		"a case variant of a field":            `{"Name":"a"}`,
		"a case variant of a promoted field":   `{"LABEL":"a"}`,
		"a case variant of an untagged field":  `{"kind":"a"}`,
		"a case variant in a nested struct":    `{"inner":{"Size":1}}`,
		"an unknown field":                     `{"approved_by":"cfo"}`,
		"the Go name of a tagged field":        `{"Label":"a"}`,
		"a field tagged -":                     `{"Skip":"a"}`,
		"invalid UTF-8 in a value":             "{\"name\":\"\xff\"}",
		"invalid UTF-8 in raw JSON":            "{\"raw\":\"\xff\"}",
		"trailing data":                        `{"name":"a"} {"name":"b"}`,
		"a duplicate name in a time.Time spot": `{"when":"2026-09-29T00:00:00Z","when":"2026-09-30T00:00:00Z"}`,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			var d strictDoc
			if err := UnmarshalStrict([]byte(doc), &d); err == nil {
				t.Fatalf("accepted %s", doc)
			}
		})
	}
}

// Field-name resolution follows encoding/json: a direct field beats a promoted one, and at equal
// depth a tagged field beats an untagged one, even when a deeper level is ambiguous.
func TestUnmarshalStrict_FieldPrecedence(t *testing.T) {
	type a struct{ ID string }
	type b struct{ ID string }
	type tagged struct {
		X string `json:"ID"`
	}
	type doc struct {
		a
		b      // a.ID and b.ID are ambiguous at depth 1 ...
		ID int // ... but the direct field wins
	}
	var d doc
	if err := UnmarshalStrict([]byte(`{"ID":7}`), &d); err != nil || d.ID != 7 {
		t.Fatalf("direct field over ambiguous promoted ones: %+v, %v", d, err)
	}
	type doc2 struct {
		a
		tagged // at depth 1, the tagged ID beats the untagged one
	}
	var d2 doc2
	if err := UnmarshalStrict([]byte(`{"ID":"x"}`), &d2); err != nil || d2.X != "x" {
		t.Fatalf("tagged over untagged at equal depth: %+v, %v", d2, err)
	}
	type doc3 struct {
		a
		b // ambiguous and nothing else claims ID: encoding/json ignores it, so it is unknown
	}
	var d3 doc3
	if err := UnmarshalStrict([]byte(`{"ID":"x"}`), &d3); err == nil || !strings.Contains(err.Error(), "ID") {
		t.Fatalf("an ambiguous name was accepted: %v", err)
	}
}

// The shallowest field wins even when a deeper one of the same name is declared later and has a
// different shape: here the direct Inner is a map (any keys), the promoted Inner a struct.
func TestUnmarshalStrict_ShallowestFieldDecidesShape(t *testing.T) {
	type deep struct {
		Inner strictInner
	}
	type doc struct {
		Inner map[string]int
		deep
	}
	var d doc
	if err := UnmarshalStrict([]byte(`{"Inner":{"any":1}}`), &d); err != nil || d.Inner["any"] != 1 {
		t.Fatalf("the direct map field did not decide the shape: %+v, %v", d, err)
	}
}
