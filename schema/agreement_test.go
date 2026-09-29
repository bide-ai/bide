package schema

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/strictjson"
)

// The schema For describes and the strict decoder that reads tool arguments (strictjson with
// SchemaFields) agree on every struct in this package's test corpus, at every depth: the same
// property names, and the same required ones. So an argument the schema calls valid is never
// rejected for a missing or unknown name, and one the decoder accepts never lacks a name the
// schema requires.
func TestFor_AgreesWithTheStrictDecoder(t *testing.T) {
	type local struct {
		base
		Person   `json:"person"`
		Opt      *int              `json:"opt"`
		Omit     string            `json:"omit,omitempty"`
		Zero     time.Time         `json:"zero,omitzero"`
		List     []fInner          `json:"list"`
		ByName   map[string]fInner `json:"by_name"`
		Skip     string            `json:"-"`
		Dash     string            `json:"-,"`
		Untagged int
		hidden   int
	}
	corpus := []reflect.Type{
		reflect.TypeFor[local](), reflect.TypeFor[email](), reflect.TypeFor[customJSON](), reflect.TypeFor[base](),
		reflect.TypeFor[recSlice](), reflect.TypeFor[recMap](), reflect.TypeFor[recNode](), reflect.TypeFor[textOut](),
		reflect.TypeFor[jsonOut](), reflect.TypeFor[textIn](), reflect.TypeFor[jsonIn](), reflect.TypeFor[Address](),
		reflect.TypeFor[Person](), reflect.TypeFor[fInner](), reflect.TypeFor[fInnerB](), reflect.TypeFor[fEmbTagged](),
		reflect.TypeFor[fStringOpt](), reflect.TypeFor[fArr](), reflect.TypeFor[fConflict](), reflect.TypeFor[fDash](),
		reflect.TypeFor[fOmitZero](), reflect.TypeFor[DX](), reflect.TypeFor[DTaggedX](), reflect.TypeFor[DTagWins](),
		reflect.TypeFor[DShallowWins](), reflect.TypeFor[DSameTypeTwice](), reflect.TypeFor[DDeepVsTagged](),
		reflect.TypeFor[DTaggedS](), reflect.TypeFor[DTagWinsS](), reflect.TypeFor[fUnexportedEmbed](),
		reflect.TypeFor[Cyc](), reflect.TypeFor[sMap](), reflect.TypeFor[sNode](), reflect.TypeFor[sOpt](),
		reflect.TypeFor[sEmpty](), reflect.TypeFor[sUntyped](),
	}
	structs := 0
	for _, typ := range corpus {
		s, err := reflectSchema(typ, map[reflect.Type]bool{})
		if err != nil {
			t.Fatalf("%s: For: %v", typ, err)
		}
		// Round-trip through JSON, as a provider or a reader sees the schema.
		b, _ := json.Marshal(s)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		structs += agree(t, typ, m, typ.String(), map[reflect.Type]bool{})
	}
	if structs < len(corpus) {
		t.Fatalf("compared %d structs, want at least %d", structs, len(corpus))
	}
}

// agree compares schema s of type typ with the strict decoder's view of typ, recursively, and
// returns how many structs it compared.
func agree(t *testing.T, typ reflect.Type, s map[string]any, path string, seen map[reflect.Type]bool) int {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if seen[typ] {
		return 0
	}
	seen[typ] = true
	defer delete(seen, typ)
	n := 0
	switch typ.Kind() {
	case reflect.Struct:
		props, ok := s["properties"].(map[string]any)
		if !ok {
			return 0 // a type that decodes itself, or a recursion cut off: no names to agree on
		}
		n++
		fields := strictjson.SchemaFields(typ)
		if got, want := slices.Sorted(maps.Keys(fields)), slices.Sorted(maps.Keys(props)); !slices.Equal(got, want) {
			t.Errorf("%s: the decoder accepts %v, the schema has %v", path, got, want)
		}
		var required []string
		for name, f := range fields {
			if f.Required {
				required = append(required, name)
			}
		}
		slices.Sort(required)
		var want []string
		if rs, ok := s["required"].([]any); ok {
			for _, r := range rs {
				want = append(want, r.(string))
			}
		}
		if !slices.Equal(required, want) {
			t.Errorf("%s: the decoder requires %v, the schema requires %v", path, required, want)
		}
		for name, f := range fields {
			if p, ok := props[name].(map[string]any); ok {
				n += agree(t, f.Type, p, path+"."+name, seen)
			}
		}
	case reflect.Slice, reflect.Array:
		if items, ok := s["items"].(map[string]any); ok {
			n += agree(t, typ.Elem(), items, path+"[]", seen)
		}
	case reflect.Map:
		if values, ok := s["additionalProperties"].(map[string]any); ok {
			n += agree(t, typ.Elem(), values, path+"{}", seen)
		}
	}
	return n
}
