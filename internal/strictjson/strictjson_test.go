package strictjson

import (
	"errors"
	"reflect"
	"testing"
)

// ambiguous returns a new *struct that embeds two structs, A and B, each supplying a tagged x at
// one depth: encoding/json reads no field x, and ExactFields keeps the name for Unmarshal's final
// decode to reject. (It is built with reflect because vet reports the repeated tag in source.)
func ambiguous() any {
	x := reflect.StructOf([]reflect.StructField{{Name: "X", Type: reflect.TypeFor[int](), Tag: `json:"x"`}})
	outer := reflect.StructOf([]reflect.StructField{
		{Name: "A", Type: x, Anonymous: true},
		{Name: "B", Type: reflect.StructOf([]reflect.StructField{{Name: "X", Type: reflect.TypeFor[int](), Tag: `json:"x"`}, {Name: "Pad", Type: reflect.TypeFor[bool](), Tag: `json:"-"`}}), Anonymous: true},
	})
	return reflect.New(outer).Interface()
}

// Unmarshal rejects a name the walk let through that is not a field: with ExactFields (the
// default), an ambiguous name encoding/json ignores.
func TestUnmarshal_RejectsANameEncodingJSONIgnores(t *testing.T) {
	if _, ok := ExactFields(reflect.TypeOf(ambiguous()).Elem())["x"]; !ok {
		t.Fatal("ExactFields dropped the ambiguous name; the test needs it kept")
	}
	if v := ambiguous(); Unmarshal([]byte(`{"x":1}`), v, nil) == nil {
		t.Fatalf("Unmarshal accepted an ambiguous name, decoding %+v", v)
	}
	if err := Unmarshal([]byte(`{}`), ambiguous(), nil); err != nil {
		t.Fatalf("Unmarshal({}) = %v", err)
	}
}

// Unmarshal takes a non-nil pointer, and rejects invalid UTF-8 and trailing data.
func TestUnmarshal_Contract(t *testing.T) {
	var v map[string]int
	if err := Unmarshal([]byte(`{}`), v, nil); err == nil {
		t.Error("Unmarshal into a non-pointer succeeded")
	}
	if err := Unmarshal([]byte(`{}`), nil, nil); err == nil {
		t.Error("Unmarshal into nil succeeded")
	}
	if err := Unmarshal([]byte("{\"a\":\"\xff\"}"), &map[string]string{}, nil); err == nil {
		t.Error("Unmarshal accepted invalid UTF-8")
	}
	if err := Unmarshal([]byte(`{} []`), &v, nil); !errors.Is(err, ErrTrailingData) {
		t.Errorf("Unmarshal with trailing data = %v, want ErrTrailingData", err)
	}
}

// With ExactFields (audit's reading), no field is required and every field takes null, as
// encoding/json reads it: only SchemaFields rejects null for a required field.
func TestUnmarshal_ExactFieldsTakeNull(t *testing.T) {
	var v struct {
		S string `json:"s"`
		L []int  `json:"l"`
	}
	if err := Unmarshal([]byte(`{"s":null,"l":null}`), &v, nil); err != nil {
		t.Fatalf("Unmarshal with ExactFields = %v, want null accepted", err)
	}
	if err := Unmarshal([]byte(`{"s":null}`), &v, &Options{Fields: SchemaFields}); err == nil {
		t.Fatal("Unmarshal with SchemaFields accepted null for a required field")
	}
}

// AllowUnknown tolerates a name the struct does not have, as a document from a later version
// carries, and still refuses everything else a strict read refuses: a case variant of a field
// name, a duplicate name (in the unknown value too), and an escaped lone surrogate.
func TestAllowUnknown(t *testing.T) {
	type rec struct {
		Kind string `json:"kind"`
		N    int    `json:"n"`
	}
	opts := &Options{AllowUnknown: true}
	var v rec
	if err := Unmarshal([]byte(`{"kind":"a","n":1,"added_later":{"x":[1,"😀"]}}`), &v, opts); err != nil || v.Kind != "a" || v.N != 1 {
		t.Fatalf("an unknown field: %+v, %v", v, err)
	}
	for name, doc := range map[string]string{
		"case variant of a field":      `{"kind":"a","Kind":"b"}`,
		"case variant alone":           `{"KIND":"b"}`,
		"duplicate field":              `{"kind":"a","kind":"b"}`,
		"duplicate unknown":            `{"x":1,"x":2}`,
		"duplicate inside an unknown":  `{"x":{"y":1,"y":2}}`,
		"lone surrogate in an unknown": `{"x":"\ud800"}`,
		"lone surrogate in a field":    `{"kind":"\udc00"}`,
		"lone surrogate in a name":     `{"\ud800":1}`,
	} {
		if err := Check([]byte(doc), reflect.TypeFor[rec](), opts); err == nil {
			t.Errorf("%s: %s accepted", name, doc)
		}
	}
	if err := Check([]byte(`{"kind":"a","extra":1}`), reflect.TypeFor[rec](), nil); err == nil {
		t.Error("without AllowUnknown an unknown field was accepted")
	}
}
