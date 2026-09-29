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
