package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// obj unmarshals a schema into a generic map for assertions.
func obj(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal schema: %v (%s)", err, raw)
	}
	return m
}

func schemaOf[T any](t *testing.T) map[string]any {
	t.Helper()
	raw, err := For[T]()
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	return obj(t, raw)
}

func TestFor_Primitives(t *testing.T) {
	cases := []struct {
		got  map[string]any
		want string
	}{
		{schemaOf[string](t), "string"},
		{schemaOf[bool](t), "boolean"},
		{schemaOf[int](t), "integer"},
		{schemaOf[uint64](t), "integer"},
		{schemaOf[float64](t), "number"},
	}
	for _, c := range cases {
		if c.got["type"] != c.want {
			t.Errorf("type = %v, want %v", c.got["type"], c.want)
		}
	}
}

// A named type whose underlying kind is a primitive uses that primitive's schema.
func TestFor_NamedPrimitive(t *testing.T) {
	type Status string
	if s := schemaOf[Status](t); s["type"] != "string" {
		t.Errorf("named string type = %v, want string", s["type"])
	}
}

// []byte marshals as a base64 string; a byte ARRAY marshals as a JSON array of numbers.
func TestFor_BytesSliceVsArray(t *testing.T) {
	if s := schemaOf[[]byte](t); s["type"] != "string" {
		t.Errorf("[]byte = %v, want string (base64)", s["type"])
	}
	s := schemaOf[[4]byte](t)
	if s["type"] != "array" {
		t.Fatalf("[4]byte = %v, want array", s["type"])
	}
	if items := s["items"].(map[string]any); items["type"] != "integer" {
		t.Errorf("[4]byte items = %v, want integer", items["type"])
	}
}

// json.RawMessage is arbitrary JSON, not a string.
func TestFor_RawMessage(t *testing.T) {
	type T struct {
		Payload json.RawMessage `json:"payload"`
	}
	p := schemaOf[T](t)["properties"].(map[string]any)["payload"].(map[string]any)
	if _, hasType := p["type"]; hasType {
		t.Errorf("json.RawMessage should be unconstrained {}, got %v", p)
	}
}

type email struct{ user, host string }

func (e email) MarshalText() ([]byte, error) { return []byte(e.user + "@" + e.host), nil }

func (e *email) UnmarshalText(b []byte) error {
	e.user, e.host, _ = strings.Cut(string(b), "@")
	return nil
}

// A TextUnmarshaler always decodes from a JSON string, regardless of its fields.
func TestFor_TextUnmarshalerIsString(t *testing.T) {
	if s := schemaOf[email](t); s["type"] != "string" {
		t.Errorf("TextUnmarshaler type = %v, want string", s["type"])
	}
}

type customJSON struct{ Ignored int }

func (customJSON) MarshalJSON() ([]byte, error) { return []byte(`{"whatever":1}`), nil }

func (*customJSON) UnmarshalJSON([]byte) error { return nil }

// A json.Unmarshaler with a shape we can't reflect is left unconstrained (not its fields).
func TestFor_JSONUnmarshalerIsUnconstrained(t *testing.T) {
	s := schemaOf[customJSON](t)
	if _, hasType := s["type"]; hasType {
		t.Errorf("custom json.Unmarshaler should be {}, got %v", s)
	}
	if _, hasProps := s["properties"]; hasProps {
		t.Error("custom json.Unmarshaler must not expose reflected fields")
	}
}

func TestFor_TimeIsDateTime(t *testing.T) {
	s := schemaOf[time.Time](t)
	if s["type"] != "string" || s["format"] != "date-time" {
		t.Errorf("time.Time = %v, want string/date-time", s)
	}
}

type base struct {
	ID   string `json:"id"`
	Note string `json:"note,omitempty"`
}

// Embedded structs are flattened (promoted), matching encoding/json.
func TestFor_EmbeddedStructPromoted(t *testing.T) {
	type Doc struct {
		base
		Title string `json:"title"`
	}
	s := schemaOf[Doc](t)
	props := s["properties"].(map[string]any)
	for _, k := range []string{"id", "note", "title"} {
		if _, ok := props[k]; !ok {
			t.Errorf("promoted field %q missing (props=%v)", k, props)
		}
	}
	if _, nested := props["base"]; nested {
		t.Error("embedded struct must be flattened, not nested under its type name")
	}
	req := toStringSet(s["required"])
	if !req["id"] || !req["title"] {
		t.Errorf("id and title should be required, got %v", s["required"])
	}
	if req["note"] {
		t.Error("note is omitempty → not required")
	}
}

// Embedded via pointer is also promoted. (The embedded type is exported: through a pointer to an
// unexported struct encoding/json cannot decode, see TestFor_EmbeddedPointerToUnexportedStructIsRejected.)
func TestFor_EmbeddedPointerPromoted(t *testing.T) {
	type Doc struct {
		*Address
		Title string `json:"title"`
	}
	props := schemaOf[Doc](t)["properties"].(map[string]any)
	if _, ok := props["city"]; !ok {
		t.Errorf("pointer-embedded field not promoted: %v", props)
	}
}

// An exported embedded field WITH a json tag is nested (a normal named field), not
// promoted. (Address is the exported struct from schema_test.go.)
func TestFor_EmbeddedWithTagNests(t *testing.T) {
	type Doc struct {
		Address `json:"addr"`
		Title   string `json:"title"`
	}
	props := schemaOf[Doc](t)["properties"].(map[string]any)
	nested, ok := props["addr"].(map[string]any)
	if !ok || nested["type"] != "object" {
		t.Fatalf("tagged embedded field should nest as object: %v", props["addr"])
	}
	if _, promoted := props["city"]; promoted {
		t.Error("tagged embedded field must not be promoted")
	}
}

// An outer field shadows a same-named promoted field (shallower wins).
func TestFor_OuterFieldShadowsEmbedded(t *testing.T) {
	type Doc struct {
		base
		ID int `json:"id"` // int shadows base.ID (string)
	}
	props := schemaOf[Doc](t)["properties"].(map[string]any)
	if got := props["id"].(map[string]any)["type"]; got != "integer" {
		t.Errorf("outer id.type = %v, want integer (shadows embedded string)", got)
	}
}

func TestFor_MapAndSliceOfStruct(t *testing.T) {
	type Item struct {
		N int `json:"n"`
	}
	type T struct {
		ByName map[string]int `json:"by_name"`
		Items  []Item         `json:"items"`
	}
	props := schemaOf[T](t)["properties"].(map[string]any)

	m := props["by_name"].(map[string]any)
	if m["type"] != "object" || m["additionalProperties"].(map[string]any)["type"] != "integer" {
		t.Errorf("map schema wrong: %v", m)
	}
	arr := props["items"].(map[string]any)
	if arr["type"] != "array" {
		t.Fatalf("items not array: %v", arr)
	}
	if items := arr["items"].(map[string]any); items["properties"].(map[string]any)["n"] == nil {
		t.Errorf("slice element struct not inlined: %v", items)
	}
}

// A self-referential type must terminate (recursion guard) rather than loop forever.
func TestFor_RecursiveTypeTerminates(t *testing.T) {
	type Node struct {
		Val  int   `json:"val"`
		Next *Node `json:"next,omitempty"`
	}
	props := schemaOf[Node](t)["properties"].(map[string]any)
	next := props["next"].(map[string]any)
	if next["type"] != "object" {
		t.Errorf("recursive field should degrade to a permissive object, got %v", next)
	}
}

// An interface{} field is unconstrained.
func TestFor_InterfaceIsUnconstrained(t *testing.T) {
	type T struct {
		Data any `json:"data"`
	}
	d := schemaOf[T](t)["properties"].(map[string]any)["data"].(map[string]any)
	if len(d) != 0 {
		t.Errorf("any field should be {}, got %v", d)
	}
}
