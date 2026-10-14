package jsonfields

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func names(t *testing.T, v any) []string {
	t.Helper()
	fs, err := Of(reflect.TypeOf(v))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

type Inner struct {
	A int
	B string `json:"b"`
}

type Deep struct{ Z int }

type Mid struct {
	Deep
	Y int
}

type tagged struct {
	Plain    int
	Renamed  string  `json:"renamed"`
	Skipped  int     `json:"-"`
	Opt      int     `json:"opt,omitempty"`
	OptZero  int     `json:"oz,omitzero"`
	Ptr      *int    `json:"ptr"`
	Quoted   int     `json:"q,string"`
	QuotedS  string  `json:",string"`
	QuotedB  bool    `json:"qb,string"`
	QuotedF  float64 `json:"qf,string"`
	QuotedU  uint8   `json:"qu,string"`
	QuotedPS *string `json:"qps,string"`
	NotQ     []int   `json:"nq,string"`
	private  int
	Dash     int `json:"-,"`
}

// Of lists exactly the keys encoding/json writes for a fully populated value.
func TestOf_MatchesEncodingJSONKeys(t *testing.T) {
	str := "s"
	one := 1
	type embedsPtr struct {
		*Inner
		C int
	}
	type tagEmbed struct {
		Inner `json:"in"`
		C     int
	}
	type shadow struct {
		Inner
		A string // shallower than Inner.A: wins
	}
	type tie1 struct{ X int }
	type tie2 struct{ X int }
	type tieStruct struct {
		tie1
		tie2
	}
	type taggedTie1 struct {
		X int `json:"X"`
	}
	type tagWins struct {
		taggedTie1
		tie2
	}
	type viaMid struct {
		Mid
		W int
	}
	cases := map[string]any{
		"tags": tagged{Ptr: &one, QuotedPS: &str},
		"embedded": struct {
			Inner
			C int
		}{},
		"embedded ptr":    embedsPtr{Inner: &Inner{}},
		"tagged embedded": tagEmbed{},
		"shadowed":        shadow{},
		"ambiguous":       tieStruct{},
		"tag wins tie":    tagWins{},
		"two levels":      viaMid{},
	}
	for name, v := range cases {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var want []string
		for k := range m {
			want = append(want, k)
		}
		got := names(t, v)
		slices.Sort(want)
		slices.Sort(got)
		// omitempty and omitzero fields are written only when set; compare the rest.
		fs, _ := Of(reflect.TypeOf(v))
		var req []string
		for _, f := range fs {
			if !f.Optional {
				req = append(req, f.Name)
			}
		}
		slices.Sort(req)
		if name != "tags" && !slices.Equal(got, want) {
			t.Errorf("%s: Of names %v, encoding/json writes %v", name, got, want)
		}
		for _, k := range req {
			if _, ok := m[k]; !ok {
				t.Errorf("%s: non-optional field %q is not written by encoding/json (keys %v)", name, k, want)
			}
		}
	}
}

func TestOf_TagsAndOptions(t *testing.T) {
	fs, err := Of(reflect.TypeOf(tagged{}))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Field{}
	for _, f := range fs {
		by[f.Name] = f
	}
	for _, gone := range []string{"Skipped", "private"} {
		if _, ok := by[gone]; ok {
			t.Errorf("%s should not be a field", gone)
		}
	}
	if f, ok := by["-"]; !ok || !f.Tagged {
		t.Errorf(`tag "-," names the field "-": %+v`, f)
	}
	for name, tc := range map[string]struct{ tagged, optional, quoted, required bool }{
		"Plain":   {false, false, false, true},
		"renamed": {true, false, false, true},
		"opt":     {true, true, false, false},
		"oz":      {true, true, false, false},
		"ptr":     {true, false, false, false},
		"q":       {true, false, true, true},
		"QuotedS": {false, false, true, true},
		"qb":      {true, false, true, true},
		"qf":      {true, false, true, true},
		"qu":      {true, false, true, true},
		"qps":     {true, false, true, false}, // a pointer to a string: the pointer is followed
		"nq":      {true, false, false, true}, // ",string" on a slice is ignored
	} {
		f, ok := by[name]
		if !ok {
			t.Errorf("%s missing from %v", name, names(t, tagged{}))
			continue
		}
		if f.Tagged != tc.tagged || f.Optional != tc.optional || f.Quoted != tc.quoted || f.Required() != tc.required {
			t.Errorf("%s: tagged=%v optional=%v quoted=%v required=%v; want %+v", name, f.Tagged, f.Optional, f.Quoted, f.Required(), tc)
		}
	}
	if got := by["qps"].Type.Kind(); got != reflect.String {
		t.Errorf("qps Type = %v, want the pointer followed to string", by["qps"].Type)
	}
}

func TestOf_PromotesEmbeddedFieldsWithIndexPaths(t *testing.T) {
	type outer struct {
		Mid
		W int
	}
	fs, err := Of(reflect.TypeOf(outer{}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]int{}
	for _, f := range fs {
		got[f.Name] = f.Index
	}
	want := map[string][]int{"Z": {0, 0, 0}, "Y": {0, 1}, "W": {1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("index paths = %v, want %v", got, want)
	}
	if fs[0].Name != "Z" || fs[1].Name != "Y" || fs[2].Name != "W" {
		t.Errorf("fields are not in index order: %v", names(t, outer{}))
	}
}

// Go's dominance rules: the shallower name wins, a tag breaks a tie at one depth, any other tie
// removes the name.
func TestOf_Dominance(t *testing.T) {
	type e1 struct{ X int }
	type e2 struct{ X int }
	type t1 struct {
		X int `json:"X"`
	}
	type shallow struct {
		e1
		X string
	}
	type tie struct {
		e1
		e2
		K int
	}
	type tagTie struct {
		t1
		e2
	}
	if got := names(t, shallow{}); !slices.Equal(got, []string{"X"}) {
		t.Errorf("shallow: %v", got)
	}
	fs, _ := Of(reflect.TypeOf(shallow{}))
	if len(fs[0].Index) != 1 || fs[0].Type.Kind() != reflect.String {
		t.Errorf("the shallow X must win: %+v", fs[0])
	}
	if got := names(t, tie{}); !slices.Equal(got, []string{"K"}) {
		t.Errorf("untagged tie must remove X: %v", got)
	}
	if got := names(t, tagTie{}); !slices.Equal(got, []string{"X"}) {
		t.Errorf("tagged beats untagged at one depth: %v", got)
	}
}

// An embedded struct reached through two paths at one depth is a tie, so its fields drop out.
func TestOf_DiamondEmbeddingRemovesTheName(t *testing.T) {
	type base struct{ D int }
	type l struct{ base }
	type r struct{ base }
	type top struct {
		l
		r
		K int
	}
	if got := names(t, top{}); !slices.Equal(got, []string{"K"}) {
		t.Errorf("diamond: %v", got)
	}
	raw, _ := json.Marshal(top{})
	if string(raw) != `{"K":0}` {
		t.Errorf("encoding/json disagrees: %s", raw)
	}
}

func TestOf_UnexportedEmbeddedPointerIsFlagged(t *testing.T) {
	type hidden struct{ H int }
	type viaPtr struct {
		*hidden
		V int
	}
	type nested struct {
		viaPtr
	}
	type viaValue struct {
		hidden
	}
	type plain int
	type embedsNonStruct struct {
		plain
		V int
	}
	for name, tc := range map[string]struct {
		typ  reflect.Type
		want map[string]string
	}{
		"pointer":    {reflect.TypeOf(viaPtr{}), map[string]string{"H": "jsonfields.hidden", "V": ""}},
		"inherited":  {reflect.TypeOf(nested{}), map[string]string{"H": "jsonfields.hidden", "V": ""}},
		"value":      {reflect.TypeOf(viaValue{}), map[string]string{"H": ""}},
		"non-struct": {reflect.TypeOf(embedsNonStruct{}), map[string]string{"V": ""}},
	} {
		fs, err := Of(tc.typ)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := map[string]string{}
		for _, f := range fs {
			got[f.Name] = f.ViaUnexportedPtr
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: ViaUnexportedPtr = %v, want %v", name, got, tc.want)
		}
	}
}

func TestOf_RejectsTagNamesEncodingJSONDoesNotAccept(t *testing.T) {
	type quote struct {
		F int `json:"a\"b"`
	}
	type backslash struct {
		F int `json:"a\\b"`
	}
	type comma struct {
		F int `json:"a b,omitempty"`
	}
	type ctl struct {
		F int `json:"a\tb"`
	}
	for name, typ := range map[string]reflect.Type{
		"quote": reflect.TypeOf(quote{}), "backslash": reflect.TypeOf(backslash{}), "control": reflect.TypeOf(ctl{}),
	} {
		_, err := Of(typ)
		var te *TagNameError
		if !errors.As(err, &te) {
			t.Errorf("%s: err = %v, want *TagNameError", name, err)
			continue
		}
		if te.Field != "F" || te.Struct != typ || te.Name == "" {
			t.Errorf("%s: %+v", name, te)
		}
		if want := "field F has the json tag name"; !strings.Contains(te.Error(), want) {
			t.Errorf("%s: Error() = %q, want it to contain %q", name, te.Error(), want)
		}
	}
	// Punctuation and spaces are accepted.
	if got := names(t, comma{}); !slices.Equal(got, []string{"a b"}) {
		t.Errorf("accepted tag: %v", got)
	}
}

func TestValidTagName(t *testing.T) {
	for s, want := range map[string]bool{
		"": false, "a": true, "A1": true, "a-b_c.d": true, "é": true, "a b": true, "$#%": true,
		`a"b`: false, `a\b`: false, "a\x00": false, "a\n": false,
	} {
		if got := validTagName(s); got != want {
			t.Errorf("validTagName(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestHasOption(t *testing.T) {
	for _, tc := range []struct {
		opts, opt string
		want      bool
	}{
		{"", "string", false}, {"string", "string", true}, {"omitempty,string", "string", true},
		{"omitempty,string,x", "string", true}, {"omitempty", "string", false}, {"strings", "string", false},
	} {
		if got := hasOption(tc.opts, tc.opt); got != tc.want {
			t.Errorf("hasOption(%q, %q) = %v", tc.opts, tc.opt, got)
		}
	}
}

func TestField_Required(t *testing.T) {
	ptr := reflect.TypeOf((*int)(nil))
	for name, f := range map[string]struct {
		f    Field
		want bool
	}{
		"plain":     {Field{Field: reflect.StructField{Type: reflect.TypeOf(0)}}, true},
		"optional":  {Field{Optional: true, Field: reflect.StructField{Type: reflect.TypeOf(0)}}, false},
		"pointer":   {Field{Field: reflect.StructField{Type: ptr}}, false},
		"optionalp": {Field{Optional: true, Field: reflect.StructField{Type: ptr}}, false},
	} {
		if got := f.f.Required(); got != f.want {
			t.Errorf("%s: Required() = %v", name, got)
		}
	}
}
