package schema

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// --- For describes the type encoding/json DECODES into, and never recurses without bound ---

type recSlice []recSlice
type recMap map[string]recMap
type recPtr *recPtr

// A recursive named slice or map type is cut off like a recursive struct, and a pointer type
// that points to itself (which encoding/json cannot decode a value into) is rejected. For used
// to recurse without bound on all three: the slice and map overflowed the stack, a fatal error
// no recover can catch, and the pointer looped forever. So each case runs in a child process.
func TestFor_RecursiveNonStructTypesTerminate(t *testing.T) {
	if name := os.Getenv("SCHEMA_TEST_RECURSION_CHILD"); name != "" {
		recursionChild(t, name)
		return
	}
	for _, name := range []string{"slice", "map", "pointer"} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFor_RecursiveNonStructTypesTerminate$", "-test.v")
		cmd.Env = append(os.Environ(), "SCHEMA_TEST_RECURSION_CHILD="+name)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			if len(out) > 2000 {
				out = out[:2000]
			}
			t.Errorf("%s: the child process failed (%v):\n%s", name, err, out)
		}
	}
}

func recursionChild(t *testing.T, name string) {
	switch name {
	case "slice":
		s := schemaOf[recSlice](t)
		if s["type"] != "array" || s["items"].(map[string]any)["type"] != "array" {
			t.Fatalf("recSlice = %v, want an array of arrays", s)
		}
	case "map":
		s := schemaOf[recMap](t)
		if s["type"] != "object" || s["additionalProperties"].(map[string]any)["type"] != "object" {
			t.Fatalf("recMap = %v, want an object of objects", s)
		}
	case "pointer":
		type holder struct {
			P recPtr `json:"p"`
		}
		if _, err := For[holder](); !errors.Is(err, ErrUnsupportedType) {
			t.Fatalf("For(a self-referential pointer) = %v, want ErrUnsupportedType", err)
		}
	}
}

type textOut struct{ A int } // encodes as a string, decodes field by field

func (textOut) MarshalText() ([]byte, error) { return []byte("x"), nil }

type jsonOut struct{ A int } // encodes its own way, decodes field by field

func (jsonOut) MarshalJSON() ([]byte, error) { return []byte(`"x"`), nil }

type textIn struct{ a int } // decodes from a string

func (*textIn) UnmarshalText([]byte) error { return nil }

type jsonIn struct{ A int } // decodes its own way

func (*jsonIn) UnmarshalJSON([]byte) error { return nil }

type levelOut int // a common enum shape: printed as a name, read back only as a number

func (levelOut) MarshalText() ([]byte, error) { return []byte("info"), nil }

// The schema of a type with custom JSON methods follows the DECODING methods, since For
// describes what encoding/json reads: UnmarshalJSON (which encoding/json prefers) leaves the
// value unconstrained, UnmarshalText makes it a string, and a type that only marshals itself is
// decoded field by field. For used to look at the marshaling methods, so *big.Int (which has
// MarshalText but decodes a JSON number) was a string no document could fill.
func TestFor_DecodingMethodsDecideTheShape(t *testing.T) {
	type holder struct {
		Big     *big.Int `json:"big"`
		TextOut textOut  `json:"text_out"`
		JSONOut jsonOut  `json:"json_out"`
		TextIn  textIn   `json:"text_in"`
		JSONIn  jsonIn   `json:"json_in"`
		Level   levelOut `json:"level"`
	}
	props := schemaOf[holder](t)["properties"].(map[string]any)
	if s := props["big"].(map[string]any); len(s) != 0 {
		t.Errorf("*big.Int = %v, want {} (it decodes with its UnmarshalJSON)", s)
	}
	if s := props["text_out"].(map[string]any); s["type"] != "object" || s["properties"] == nil {
		t.Errorf("a type with only MarshalText = %v, want its fields", s)
	}
	if s := props["json_out"].(map[string]any); s["type"] != "object" || s["properties"] == nil {
		t.Errorf("a type with only MarshalJSON = %v, want its fields", s)
	}
	if s := props["text_in"].(map[string]any); s["type"] != "string" {
		t.Errorf("a type with UnmarshalText = %v, want a string", s)
	}
	if s := props["json_in"].(map[string]any); len(s) != 0 {
		t.Errorf("a type with UnmarshalJSON = %v, want {}", s)
	}
	if s := props["level"].(map[string]any); s["type"] != "integer" {
		t.Errorf("an int with only MarshalText = %v, want an integer", s)
	}

	// The shapes above are the ones encoding/json reads.
	var h holder
	doc := `{"big":12,"text_out":{"A":1},"json_out":{"A":2},"text_in":"s","json_in":[1],"level":3}`
	if err := json.Unmarshal([]byte(doc), &h); err != nil {
		t.Fatalf("decode %s: %v", doc, err)
	}
	for _, bad := range []string{`{"big":"12"}`, `{"text_out":"x"}`, `{"json_out":"x"}`, `{"level":"info"}`} {
		if err := json.Unmarshal([]byte(bad), &h); err == nil {
			t.Errorf("decode %s succeeded; the old schema admitted it, and encoding/json should not", bad)
		}
	}
}

// An integer that does not fit the Go field fails to decode, so the schema bounds it: every
// unsigned kind has minimum 0, and a kind of 32 bits or fewer has both bounds. (The 64-bit
// bounds are left out: float64, which JSON Schema tools and providers read numbers into, cannot
// hold them exactly.) uintptr, which For left unconstrained, is an unsigned integer.
func TestFor_IntegerBounds(t *testing.T) {
	type holder struct {
		I8  int8    `json:"i8"`
		I16 int16   `json:"i16"`
		I32 int32   `json:"i32"`
		I64 int64   `json:"i64"`
		U8  uint8   `json:"u8"`
		U16 uint16  `json:"u16"`
		U32 uint32  `json:"u32"`
		U64 uint64  `json:"u64"`
		U   uint    `json:"u"`
		UP  uintptr `json:"up"`
	}
	props := schemaOf[holder](t)["properties"].(map[string]any)
	want := map[string][2]any{ // minimum, maximum (nil: absent)
		"i8": {-128.0, 127.0}, "i16": {-32768.0, 32767.0}, "i32": {-2147483648.0, 2147483647.0}, "i64": {nil, nil},
		"u8": {0.0, 255.0}, "u16": {0.0, 65535.0}, "u32": {0.0, 4294967295.0}, "u64": {0.0, nil},
		"up": {0.0, nil},
	}
	if strconv.IntSize == 64 {
		want["u"] = [2]any{0.0, nil}
	}
	for name, w := range want {
		s := props[name].(map[string]any)
		if s["type"] != "integer" || s["minimum"] != w[0] || s["maximum"] != w[1] {
			t.Errorf("%s = %v, want an integer with minimum %v and maximum %v", name, s, w[0], w[1])
		}
	}

	// The bounds are encoding/json's: a value one past either one fails to decode.
	var h holder
	for _, bad := range []string{`{"i8":128}`, `{"i8":-129}`, `{"u8":256}`, `{"u8":-1}`, `{"u64":-1}`, `{"up":-1}`} {
		if err := json.Unmarshal([]byte(bad), &h); err == nil {
			t.Errorf("decode %s succeeded, want an error", bad)
		}
	}
}

// A type encoding/json cannot decode any value but null into (a channel, a function, a complex
// number, an unsafe.Pointer, an interface with methods, or a map whose keys it cannot read) is
// ErrUnsupportedType naming the field, as the doc on For promises, rather than {}, which told the
// model any value would do. The same field tagged "-" is skipped, so it is fine.
func TestFor_UndecodableKindsAreRejected(t *testing.T) {
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrUnsupportedType) || !strings.Contains(err.Error(), "F") {
			t.Errorf("%s: For = %v, want ErrUnsupportedType naming field F", name, err)
		}
	}
	_, err := For[struct{ F chan int }]()
	check("chan", err)
	_, err = For[struct{ F func() }]()
	check("func", err)
	_, err = For[struct{ F complex128 }]()
	check("complex128", err)
	_, err = For[struct{ F unsafe.Pointer }]()
	check("unsafe.Pointer", err)
	_, err = For[struct{ F io.Reader }]()
	check("io.Reader", err)
	_, err = For[struct{ F []map[bool]int }]()
	check("map[bool]int", err)
	_, err = For[struct{ F map[float64]string }]()
	check("map[float64]string", err)

	if _, err := For[struct {
		F chan int `json:"-"`
		G string
	}](); err != nil {
		t.Errorf("a skipped chan field: For = %v, want no error", err)
	}

	// Each of these fails to decode a non-null value.
	for _, c := range []struct {
		v   any
		doc string
	}{
		{new(struct{ F chan int }), `{"F":1}`},
		{new(struct{ F complex128 }), `{"F":1}`},
		{new(struct{ F io.Reader }), `{"F":{}}`},
		{new(struct{ F map[bool]int }), `{"F":{"true":1}}`},
	} {
		if err := json.Unmarshal([]byte(c.doc), c.v); err == nil {
			t.Errorf("decode %s into %T succeeded, want an error", c.doc, c.v)
		}
	}
}

type textKey struct{ s string }

func (k *textKey) UnmarshalText(b []byte) error { k.s = string(b); return nil }

// The keys of a map with integer keys are integers in decimal, which propertyNames says; keys a
// type reads with UnmarshalText, or string keys, are any string.
func TestFor_IntegerMapKeys(t *testing.T) {
	type holder struct {
		Signed   map[int]string     `json:"signed"`
		Unsigned map[uint8]string   `json:"unsigned"`
		Text     map[textKey]string `json:"text"`
		Str      map[string]string  `json:"str"`
	}
	props := schemaOf[holder](t)["properties"].(map[string]any)
	pattern := func(name string) string {
		pn, _ := props[name].(map[string]any)["propertyNames"].(map[string]any)
		p, _ := pn["pattern"].(string)
		return p
	}
	if pattern("text") != "" || pattern("str") != "" {
		t.Errorf("text and string keys = %v, %v; want no propertyNames", props["text"], props["str"])
	}
	signed, unsigned := pattern("signed"), pattern("unsigned")
	if signed == "" || unsigned == "" {
		t.Fatalf("integer keys: signed %v, unsigned %v; want a propertyNames pattern on each", props["signed"], props["unsigned"])
	}

	// Each pattern admits exactly the keys encoding/json reads (leaving range aside).
	for _, c := range []struct {
		pattern string
		v       any
	}{{signed, new(map[int]string)}, {unsigned, new(map[uint8]string)}} {
		re := regexp.MustCompile(c.pattern)
		for _, key := range []string{"0", "7", "007", "+1", "-3", "1.5", "1e2", "x", "", " 1", "0x1"} {
			doc, _ := json.Marshal(map[string]string{key: "v"})
			reflect.ValueOf(c.v).Elem().SetZero()
			decodes := json.Unmarshal(doc, c.v) == nil
			if re.MatchString(key) != decodes {
				t.Errorf("%T key %q: pattern %q matches %v, encoding/json decodes %v", c.v, key, c.pattern, re.MatchString(key), decodes)
			}
		}
	}
}
