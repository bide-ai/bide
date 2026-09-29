package schema

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// --- the neutral schema describes what encoding/json actually decodes ---

type fInner struct {
	A int `json:"a"`
}
type fInnerB struct {
	B int `json:"b"`
}
type fEmbTagged struct {
	fInner `json:",omitempty"` // no name: still promoted, like an untagged embed
}
type fStringOpt struct {
	N int     `json:"n,string"`
	F float64 `json:"f,string"`
	B bool    `json:"b,string"`
	P *int    `json:"p,string"`
}
type fEmbPtrUnexported struct {
	*fInnerB
}
type fArr struct {
	X [2]int `json:"x"`
}
type fE1 struct{ X int }
type fE2 struct{ X int }
type fConflict struct {
	fE1
	fE2
	Y int
}
type fDash struct {
	D int `json:"-,"`
	H int `json:"-"`
}
type fOmitZero struct {
	Z int `json:"z,omitzero"`
}

// propertyNames returns the sorted property names of an object schema.
func propertyNames(t *testing.T, s map[string]any) []string {
	t.Helper()
	props, _ := s["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

// marshaledKeys returns the sorted top-level keys encoding/json writes for v.
func marshaledKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// An embedded struct whose tag names nothing (json:",omitempty") is promoted by encoding/json,
// so its fields belong at the top level; nesting them under the type name made the model send
// {"fInner":{"a":7}}, which decodes to a silent zero.
func TestFor_EmbeddedWithUnnamedTagIsPromoted(t *testing.T) {
	s := schemaOf[fEmbTagged](t)
	if got, want := propertyNames(t, s), marshaledKeys(t, fEmbTagged{fInner{A: 1}}); !slices.Equal(got, want) {
		t.Fatalf("schema properties %v, encoding/json writes %v", got, want)
	}
	var v fEmbTagged
	if err := json.Unmarshal([]byte(`{"a":7}`), &v); err != nil || v.A != 7 {
		t.Fatalf("a document valid under the schema decoded to %+v (%v)", v, err)
	}
}

// Two embedded structs that both supply X at the same depth cancel out in encoding/json: X is
// neither written nor read, so the schema must not offer it.
func TestFor_ConflictingEmbeddedFieldsAreDropped(t *testing.T) {
	s := schemaOf[fConflict](t)
	if got, want := propertyNames(t, s), marshaledKeys(t, fConflict{fE1{1}, fE2{2}, 3}); !slices.Equal(got, want) {
		t.Fatalf("schema properties %v, encoding/json writes %v", got, want)
	}
}

type DX struct{ X int }
type DTaggedX struct {
	Z int `json:"X"`
}
type DTaggedX2 struct {
	W int `json:"X"`
}
type DWrap1 struct{ DX }
type DWrap2 struct{ DX }
type DTagWins struct {
	DX
	DTaggedX
}

// taggedTie embeds DTaggedX and DTaggedX2, whose fields share the json name "X" at one depth. It
// is built at run time because go vet reports the repeated tag in a declared struct.
var taggedTie = reflect.StructOf([]reflect.StructField{
	{Name: "DTaggedX", Type: reflect.TypeFor[DTaggedX](), Anonymous: true},
	{Name: "DTaggedX2", Type: reflect.TypeFor[DTaggedX2](), Anonymous: true},
})

type DShallowWins struct {
	X string
	DX
}
type DSameTypeTwice struct {
	DWrap1
	DWrap2
}
type DDeepVsTagged struct {
	DWrap1
	DTaggedX
}

func schemaOfType(t *testing.T, typ reflect.Type) map[string]any {
	t.Helper()
	s, err := reflectSchema(typ, map[reflect.Type]bool{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func filledTie() any {
	v := reflect.New(taggedTie).Elem()
	v.Field(0).Field(0).SetInt(1)
	v.Field(1).Field(0).SetInt(2)
	return v.Interface()
}

// Go's dominance rules decide which of several same-named fields encoding/json uses: the
// shallowest, then a tagged one over an untagged one at that depth; any other tie drops the name.
// The schema's properties are exactly the keys encoding/json writes.
func TestFor_FieldDominanceMatchesEncodingJSON(t *testing.T) {
	for name, c := range map[string]struct {
		schema map[string]any
		value  any
	}{
		"tag wins":        {schemaOf[DTagWins](t), DTagWins{DX{1}, DTaggedX{2}}},
		"tagged tie":      {schemaOfType(t, taggedTie), filledTie()},
		"shallow wins":    {schemaOf[DShallowWins](t), DShallowWins{"s", DX{1}}},
		"same type twice": {schemaOf[DSameTypeTwice](t), DSameTypeTwice{DWrap1{DX{1}}, DWrap2{DX{2}}}},
		"deep vs tagged":  {schemaOf[DDeepVsTagged](t), DDeepVsTagged{DWrap1{DX{1}}, DTaggedX{2}}},
	} {
		if got, want := propertyNames(t, c.schema), marshaledKeys(t, c.value); !slices.Equal(got, want) {
			t.Errorf("%s: schema properties %v, encoding/json writes %v", name, got, want)
		}
	}
	// The surviving field has the winning field's type.
	x := schemaOf[DShallowWins](t)["properties"].(map[string]any)["X"].(map[string]any)
	if x["type"] != "string" {
		t.Errorf("shallow wins: X is %v, want the outer string", x)
	}
}

type DTaggedS struct {
	S string `json:"X"`
}
type DTagWinsS struct {
	DX
	DTaggedS
}

// At one depth a tagged field beats an untagged one of the same name, whichever comes first.
func TestFor_TaggedFieldWinsATie(t *testing.T) {
	x := schemaOf[DTagWinsS](t)["properties"].(map[string]any)["X"].(map[string]any)
	if x["type"] != "string" {
		t.Fatalf("X is %v, want the tagged string field", x)
	}
	b, _ := json.Marshal(DTagWinsS{DX{1}, DTaggedS{"s"}})
	if string(b) != `{"X":"s"}` {
		t.Fatalf("encoding/json writes %s; the premise of this test is wrong", b)
	}
}

type myInt int
type fUnexportedEmbed struct {
	myInt // an embedded unexported non-struct type: encoding/json ignores it
	Y     int
}

// An embedded unexported non-struct type contributes nothing.
func TestFor_UnexportedNonStructEmbedIsIgnored(t *testing.T) {
	s := schemaOf[fUnexportedEmbed](t)
	if got, want := propertyNames(t, s), marshaledKeys(t, fUnexportedEmbed{1, 2}); !slices.Equal(got, want) {
		t.Fatalf("schema properties %v, encoding/json writes %v", got, want)
	}
}

type fBadTagName struct {
	Bad int `json:"na\\me"`
}
type fBadTagNameEmbedded struct {
	fBadTagName
}

// A json tag name with a character encoding/json reserves has no one meaning: encoding/json on
// its v2 implementation (the Go 1.27 default) cuts the name short, and built with
// GOEXPERIMENT=nojsonv2 it falls back to the Go field name. For refuses it rather than describe
// one of the two.
func TestFor_MalformedTagNameIsRejected(t *testing.T) {
	if _, err := For[fBadTagName](); !errors.Is(err, ErrUnsupportedType) || !strings.Contains(err.Error(), "Bad") {
		t.Fatalf("For = %v, want ErrUnsupportedType naming field Bad", err)
	}
	if _, err := For[fBadTagNameEmbedded](); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("For (promoted) = %v, want ErrUnsupportedType", err)
	}
}

type Cyc struct {
	A int
	*Cyc
}

// A struct that embeds a pointer to itself terminates, with the fields encoding/json uses.
func TestFor_SelfEmbeddingTerminates(t *testing.T) {
	s := schemaOf[Cyc](t)
	if got, want := propertyNames(t, s), marshaledKeys(t, Cyc{A: 1}); !slices.Equal(got, want) {
		t.Fatalf("schema properties %v, encoding/json writes %v", got, want)
	}
}

// json:"-," names a field "-"; json:"-" omits it.
func TestFor_DashTag(t *testing.T) {
	s := schemaOf[fDash](t)
	if got, want := propertyNames(t, s), marshaledKeys(t, fDash{D: 1, H: 2}); !slices.Equal(got, want) {
		t.Fatalf("schema properties %v, encoding/json writes %v", got, want)
	}
}

// omitzero, like omitempty, makes a field optional.
func TestFor_OmitZeroIsOptional(t *testing.T) {
	s := schemaOf[fOmitZero](t)
	if req := toStringSet(s["required"]); req["z"] {
		t.Fatalf("an omitzero field is required: %v", s)
	}
}

// A ,string field travels as a JSON string holding the value; encoding/json rejects the bare
// value, so the schema must ask for the string form.
func TestFor_StringOption(t *testing.T) {
	s := schemaOf[fStringOpt](t)
	props := s["properties"].(map[string]any)
	for _, name := range []string{"n", "f", "b", "p"} {
		if got := props[name].(map[string]any)["type"]; got != "string" {
			t.Errorf("%s: type %v, want string", name, got)
		}
	}
	var v fStringOpt
	if err := json.Unmarshal([]byte(`{"n":"5","f":"1.5","b":"true","p":"7"}`), &v); err != nil || v.N != 5 || v.F != 1.5 || !v.B || v.P == nil || *v.P != 7 {
		t.Fatalf("the string form decoded to %+v (%v)", v, err)
	}
	if err := json.Unmarshal([]byte(`{"n":5}`), &v); err == nil {
		t.Fatal("encoding/json accepted a bare number for a ,string field; the premise of this test is wrong")
	}
}

// A field reached through an embedded pointer to an unexported struct cannot be decoded
// (encoding/json cannot allocate the pointer), so a schema offering it would describe documents
// that fail to decode. For rejects the type instead.
func TestFor_EmbeddedPointerToUnexportedStructIsRejected(t *testing.T) {
	if _, err := For[fEmbPtrUnexported](); err == nil || !strings.Contains(err.Error(), "unexported") {
		t.Fatalf("For = %v, want an error naming the embedded pointer to an unexported struct", err)
	}
	var v fEmbPtrUnexported
	if err := json.Unmarshal([]byte(`{"b":3}`), &v); err == nil {
		t.Fatal("encoding/json decoded through a nil embedded pointer to an unexported struct; the premise of this test is wrong")
	}
}

// A Go array has a fixed length: encoding/json drops extra items and zero-fills missing ones, so
// the schema pins the length.
func TestFor_ArrayLengthIsFixed(t *testing.T) {
	x := schemaOf[fArr](t)["properties"].(map[string]any)["x"].(map[string]any)
	if x["minItems"] != float64(2) || x["maxItems"] != float64(2) {
		t.Fatalf("[2]int schema = %v, want minItems 2 and maxItems 2", x)
	}
}

// --- OpenAIStrict expresses what strict mode can and refuses the rest ---

type sMap struct {
	M map[string]int `json:"m"`
}
type sNode struct {
	V    int    `json:"v"`
	Next *sNode `json:"next"`
}
type sOpt struct {
	P    *int     `json:"p"`
	S    string   `json:"s,omitempty"`
	L    []string `json:"l,omitempty"`
	Keep int      `json:"keep"`
}
type sEmpty struct {
	E struct{} `json:"e"`
}

// A map's keys are data, so strict mode (every object closed, every property listed) cannot
// describe one: closing it made the only valid instance {}, and the model's map arrived empty.
func TestOpenAIStrict_MapIsAnError(t *testing.T) {
	neutral, _ := For[sMap]()
	if s, err := OpenAIStrict(neutral); !errors.Is(err, ErrStrictUnsupported) || !strings.Contains(err.Error(), "properties.m") {
		t.Fatalf("OpenAIStrict = %s, %v; want ErrStrictUnsupported naming properties.m", s, err)
	}
}

// A recursive type is cut off as an open object in the neutral schema; strict mode would close it
// into an always-empty object, so it is an error too.
func TestOpenAIStrict_RecursiveTypeIsAnError(t *testing.T) {
	neutral, _ := For[sNode]()
	if s, err := OpenAIStrict(neutral); !errors.Is(err, ErrStrictUnsupported) {
		t.Fatalf("OpenAIStrict = %s, %v; want ErrStrictUnsupported", s, err)
	}
}

// Strict mode requires every property, so an optional one (pointer, omitempty) is required but
// nullable: the model sends null instead of inventing a value.
func TestOpenAIStrict_OptionalPropertiesAreNullable(t *testing.T) {
	neutral, _ := For[sOpt]()
	strict, err := OpenAIStrict(neutral)
	if err != nil {
		t.Fatal(err)
	}
	s := obj(t, strict)
	if req := toStringSet(s["required"]); !req["p"] || !req["s"] || !req["l"] || !req["keep"] {
		t.Fatalf("required = %v, want every property", s["required"])
	}
	props := s["properties"].(map[string]any)
	for name, want := range map[string][]any{"p": {"integer", "null"}, "s": {"string", "null"}, "l": {"array", "null"}} {
		if got, _ := props[name].(map[string]any)["type"].([]any); !slices.Equal(got, want) {
			t.Errorf("%s: type %v, want %v", name, props[name].(map[string]any)["type"], want)
		}
	}
	if got := props["keep"].(map[string]any)["type"]; got != "integer" {
		t.Errorf("keep: type %v, want integer (a required field stays non-null)", got)
	}
	var v sOpt
	if err := json.Unmarshal([]byte(`{"p":null,"s":null,"l":null,"keep":3}`), &v); err != nil || v.P != nil || v.S != "" || v.L != nil || v.Keep != 3 {
		t.Fatalf("the all-null answer decoded to %+v (%v)", v, err)
	}
}

// An enum that is optional admits null as one of its values.
func TestOpenAIStrict_OptionalEnumAdmitsNull(t *testing.T) {
	strict, err := OpenAIStrict(json.RawMessage(`{"type":"object","properties":{"c":{"type":"string","enum":["a","b"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c := obj(t, strict)["properties"].(map[string]any)["c"].(map[string]any)
	if got, _ := c["enum"].([]any); !slices.Equal(got, []any{"a", "b", nil}) {
		t.Fatalf("enum = %v, want [a b null]", c["enum"])
	}
}

// Optional properties of other shapes admit null too: a type list gains "null", a typeless enum
// gains null, and an anyOf gains a null branch. A required property is left alone.
func TestOpenAIStrict_NullableShapes(t *testing.T) {
	strict, err := OpenAIStrict(json.RawMessage(`{"type":"object","required":["r"],"properties":{
		"list":{"type":["string","integer"]},
		"en":{"enum":["a",1]},
		"any":{"anyOf":[{"type":"string"},{"type":"integer"}]},
		"r":{"anyOf":[{"type":"string"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	props := obj(t, strict)["properties"].(map[string]any)
	if got, _ := props["list"].(map[string]any)["type"].([]any); !slices.Equal(got, []any{"string", "integer", "null"}) {
		t.Errorf("list: type %v", props["list"])
	}
	if got, _ := props["en"].(map[string]any)["enum"].([]any); !slices.Equal(got, []any{"a", float64(1), nil}) {
		t.Errorf("en: enum %v", props["en"])
	}
	if got, _ := props["any"].(map[string]any)["anyOf"].([]any); len(got) != 3 {
		t.Errorf("any: anyOf %v, want a null branch added", props["any"])
	}
	if got, _ := props["r"].(map[string]any)["anyOf"].([]any); len(got) != 1 {
		t.Errorf("r: anyOf %v, want the required property unchanged", props["r"])
	}
}

// Objects nested in arrays and in anyOf branches are checked and closed too.
func TestOpenAIStrict_ReachesItemsAndBranches(t *testing.T) {
	for _, in := range []string{
		`{"type":"array","items":{"type":"object","additionalProperties":{"type":"integer"}}}`,
		`{"anyOf":[{"type":"string"},{"type":"object"}]}`,
		`{"type":"object","properties":{"p":{"type":"object","patternProperties":{"^x":{}}, "properties":{}}}}`,
		`{"type":"object","properties":{"a":{"type":"integer"}},"additionalProperties":{"type":"string"}}`,
	} {
		if s, err := OpenAIStrict(json.RawMessage(in)); !errors.Is(err, ErrStrictUnsupported) {
			t.Errorf("%s: OpenAIStrict = %s, %v; want ErrStrictUnsupported", in, s, err)
		}
	}
	strict, err := OpenAIStrict(json.RawMessage(`{"type":"array","items":{"type":"object","properties":{"a":{"type":"integer"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if items := obj(t, strict)["items"].(map[string]any); items["additionalProperties"] != false {
		t.Fatalf("items object not closed: %v", items)
	}
}

type sUntyped struct {
	A   any             `json:"a"`
	Raw json.RawMessage `json:"raw,omitempty"`
	N   int             `json:"n"`
}

// An untyped schema ({}, from any or json.RawMessage) admits every value, which strict mode cannot
// express: every schema there must say what it admits. It is an error naming the location, wherever
// it sits, rather than a schema the API refuses or reads its own way.
func TestOpenAIStrict_UntypedSchemaIsAnError(t *testing.T) {
	neutral, _ := For[sUntyped]()
	if s, err := OpenAIStrict(neutral); !errors.Is(err, ErrStrictUnsupported) || !strings.Contains(err.Error(), "properties.a") {
		t.Fatalf("OpenAIStrict = %s, %v; want ErrStrictUnsupported naming properties.a", s, err)
	}
	for _, in := range []string{
		`{}`,
		`{"description":"anything"}`,
		`{"type":"object","properties":{"raw":{}}}`,
		`{"type":"object","properties":{"raw":{"description":"any JSON"}}}`,
		`{"type":"array","items":{}}`,
		`{"type":"array","items":true}`,
		`{"type":"array"}`, // no items: any items, as For cuts off a recursive slice
		`{"type":"object","properties":{"a":{"type":"array"}},"required":["a"]}`,
		`{"anyOf":[{"type":"string"},{}]}`,
	} {
		if s, err := OpenAIStrict(json.RawMessage(in)); !errors.Is(err, ErrStrictUnsupported) {
			t.Errorf("%s: OpenAIStrict = %s, %v; want ErrStrictUnsupported", in, s, err)
		}
	}
	// A schema without a type that still says what it admits is fine.
	for _, in := range []string{
		`{"type":"object","properties":{"e":{"enum":["a","b"]},"c":{"const":1},"u":{"anyOf":[{"type":"string"},{"type":"null"}]},"o":{"oneOf":[{"type":"string"}]},"l":{"allOf":[{"type":"string"}]},"r":{"$ref":"#/$defs/t"}},"required":["e","c","u","o","l","r"],"$defs":{"t":{"type":"string"}}}`,
		`{"type":"array","items":{"type":"integer"}}`,
	} {
		if _, err := OpenAIStrict(json.RawMessage(in)); err != nil {
			t.Errorf("%s: OpenAIStrict = %v", in, err)
		}
	}
}

// An empty struct is a closed object with no properties, which strict mode expresses exactly.
func TestOpenAIStrict_EmptyStructIsFine(t *testing.T) {
	neutral, _ := For[sEmpty]()
	if _, err := OpenAIStrict(neutral); err != nil {
		t.Fatalf("OpenAIStrict(struct{}) = %v", err)
	}
	// An object already closed with no properties is the same empty object.
	if _, err := OpenAIStrict(json.RawMessage(`{"type":"object","additionalProperties":false}`)); err != nil {
		t.Fatalf("OpenAIStrict(closed empty object) = %v", err)
	}
}
