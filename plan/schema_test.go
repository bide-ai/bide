package plan

// The JSON Schema for flow configs (schema/flow.schema.json) is what editors use to complete and
// check a config as it is typed. These tests keep it the same as what Load reads: every key the
// config structs decode is in the schema and no other is, the enums and the assertion grammar
// are the code's own, and every sample config is valid under it. The core module is stdlib-only,
// so the schema is checked by a small validator and the samples are read by a small YAML reader,
// both below and both limited to what the schema and the samples use.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
)

const (
	flowSchemaFile  = "schema/flow.schema.json"
	flowSamplesGlob = "../examples/plan/flows/*.yaml"
)

type jsonObj = map[string]any

func loadFlowSchema(t *testing.T) jsonObj {
	t.Helper()
	b, err := os.ReadFile(flowSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	var s jsonObj
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", flowSchemaFile, err)
	}
	return s
}

// resolve follows a "#/$defs/name" reference.
func resolve(t testing.TB, root, s jsonObj) jsonObj {
	t.Helper()
	ref, ok := s["$ref"].(string)
	if !ok {
		return s
	}
	name, ok := strings.CutPrefix(ref, "#/$defs/")
	if !ok {
		t.Fatalf("unsupported $ref %q", ref)
	}
	d, ok := root["$defs"].(jsonObj)[name].(jsonObj)
	if !ok {
		t.Fatalf("$ref %q resolves to nothing", ref)
	}
	return d
}

// schemaProps is the property schemas of an object schema, merged across the branches of its
// oneOf (the wiring element is a union of three shapes).
func schemaProps(t testing.TB, root, s jsonObj) map[string]jsonObj {
	t.Helper()
	s = resolve(t, root, s)
	out := map[string]jsonObj{}
	props, _ := s["properties"].(jsonObj)
	for k, v := range props {
		out[k] = v.(jsonObj)
	}
	if one, ok := s["oneOf"].([]any); ok {
		for _, b := range one {
			for k, v := range schemaProps(t, root, b.(jsonObj)) {
				out[k] = v
			}
		}
	}
	return out
}

// schemaObjects lists every object schema of s (s itself, and each oneOf branch).
func schemaObjects(root, s jsonObj) []jsonObj {
	if one, ok := s["oneOf"].([]any); ok {
		var out []jsonObj
		for _, b := range one {
			out = append(out, schemaObjects(root, b.(jsonObj))...)
		}
		return out
	}
	return []jsonObj{s}
}

// tagInfo is a config struct's json keys, and which of them are not omitempty.
func tagInfo(typ reflect.Type) (keys []string, required []string) {
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		keys = append(keys, name)
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	sort.Strings(keys)
	sort.Strings(required)
	return keys, required
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The config structs, and the $defs entry (or the root, "") describing each.
var schemaOf = map[reflect.Type]string{
	reflect.TypeFor[config]():         "",
	reflect.TypeFor[configNode]():     "node",
	reflect.TypeFor[configApproval](): "approval",
	reflect.TypeFor[configWire]():     "wire",
	reflect.TypeFor[configArm]():      "arm",
}

func defFor(t testing.TB, root jsonObj, name string) jsonObj {
	t.Helper()
	if name == "" {
		return root
	}
	d, ok := root["$defs"].(jsonObj)[name].(jsonObj)
	if !ok {
		t.Fatalf("schema has no $defs.%s", name)
	}
	return d
}

// Every key a config struct decodes is a property of its schema and the schema has no other, the
// required keys are the keys without omitempty, every object refuses other keys, and each
// property has the JSON type of its Go field.
func TestFlowSchema_PropertiesMatchConfigStructs(t *testing.T) {
	root := loadFlowSchema(t)
	for typ, def := range schemaOf {
		t.Run(typ.Name(), func(t *testing.T) {
			s := defFor(t, root, def)
			keys, required := tagInfo(typ)
			props := schemaProps(t, root, s)
			if got := sortedKeys(props); !slices.Equal(got, keys) {
				t.Fatalf("schema properties %v, config struct keys %v", got, keys)
			}
			for _, o := range schemaObjects(root, s) {
				if o["additionalProperties"] != false {
					t.Errorf("an object schema allows other keys: %v", o["title"])
				}
				if o["type"] != "object" {
					t.Errorf("an object schema is not type object: %v", o["title"])
				}
			}
			if typ != reflect.TypeFor[configWire]() {
				var got []string
				for _, r := range s["required"].([]any) {
					got = append(got, r.(string))
				}
				sort.Strings(got)
				// Load may need fewer keys than the struct has without omitempty (see
				// TestFlowSchema_RequiredKeysAreNeeded), never more.
				for _, k := range got {
					if !slices.Contains(required, k) {
						t.Errorf("schema requires %q, which the struct marks omitempty", k)
					}
				}
			}
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				p := resolve(t, root, props[name])
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				switch ft.Kind() {
				case reflect.String:
					if ty, ok := p["type"]; ok && ty != "string" {
						t.Errorf("%s: schema type %v, Go string", name, ty)
					}
				case reflect.Int:
					if ty, ok := p["type"]; ok && ty != "integer" {
						t.Errorf("%s: schema type %v, Go int", name, ty)
					}
				case reflect.Slice:
					if p["type"] != "array" {
						t.Errorf("%s: schema type %v, Go slice", name, p["type"])
					}
					items := p["items"].(jsonObj)
					if et := ft.Elem(); et.Kind() == reflect.Struct {
						want, ok := schemaOf[et]
						if !ok || items["$ref"] != "#/$defs/"+want {
							t.Errorf("%s: items %v, want a reference to $defs.%s", name, items, want)
						}
					} else if resolve(t, root, items)["type"] != "string" {
						t.Errorf("%s: items are not strings", name)
					}
				case reflect.Struct:
					if want := schemaOf[ft]; props[name]["$ref"] != "#/$defs/"+want {
						t.Errorf("%s: schema %v, want a reference to $defs.%s", name, props[name], want)
					}
				default:
					t.Errorf("%s: Go kind %s has no schema check", name, ft.Kind())
				}
			}
		})
	}
	// A new config struct must be described, not skipped.
	for _, typ := range []reflect.Type{reflect.TypeFor[config](), reflect.TypeFor[configNode](), reflect.TypeFor[configWire]()} {
		for i := 0; i < typ.NumField(); i++ {
			ft := typ.Field(i).Type
			for ft.Kind() == reflect.Slice || ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if _, ok := schemaOf[ft]; ft.Kind() == reflect.Struct && !ok {
				t.Errorf("%s.%s has struct type %s, which the schema test does not map", typ.Name(), typ.Field(i).Name, ft)
			}
		}
	}
}

// The enums and constants are the values Load accepts.
func TestFlowSchema_EnumsMatchCode(t *testing.T) {
	root := loadFlowSchema(t)

	if v := schemaProps(t, root, root)["version"]["const"]; v != float64(ConfigVersion) {
		t.Errorf("schema version const %v, ConfigVersion %d", v, ConfigVersion)
	}

	want := slices.Sorted(slices.Values(levelNames[:]))
	var got []string
	for _, v := range defFor(t, root, "safety")["enum"].([]any) {
		got = append(got, v.(string))
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("schema safety enum %v, levelNames %v", got, want)
	}
	// Each is accepted by Load, over a block that allows it, and nothing else is.
	base := agent.Safety{ReadOnly: true}
	for _, s := range got {
		if _, err := safetyFromConfig(base, s); err != nil {
			t.Errorf("safety %q is in the schema but refused: %v", s, err)
		}
	}
	for _, s := range []string{"", "retryable", "read_only", "read-only", "side effect", "Idempotent"} {
		if _, err := safetyFromConfig(base, s); err == nil {
			t.Errorf("safety %q is accepted but not in the schema's enum", s)
		}
	}
	// Both safety properties use the one enum.
	for _, ref := range []jsonObj{
		schemaProps(t, root, defFor(t, root, "node"))["safety"],
		schemaProps(t, root, defFor(t, root, "wire"))["safety"],
	} {
		if ref["$ref"] != "#/$defs/safety" {
			t.Errorf("a safety property does not use $defs.safety: %v", ref)
		}
	}
}

// anchored turns a grammar regexp of assert.go into its schema spelling: single spaces are runs of
// whitespace (the grammar collapses them) and the ends may carry some.
func anchored(re string) string {
	body := strings.TrimSuffix(strings.TrimPrefix(re, "^"), "$")
	return `^\s*` + strings.ReplaceAll(body, " ", `\s+`) + `\s*$`
}

func schemaAssertionPatterns(t *testing.T, root jsonObj) []string {
	t.Helper()
	var out []string
	for _, b := range defFor(t, root, "assertion")["anyOf"].([]any) {
		out = append(out, b.(jsonObj)["pattern"].(string))
	}
	return out
}

// The schema's assertion patterns are the grammar assert.go decides, and the schema's examples
// and the guide's are in it.
func TestFlowSchema_AssertionGrammarMatchesCode(t *testing.T) {
	root := loadFlowSchema(t)
	var want []string
	for _, re := range []*regexp.Regexp{assertGate, assertTerminal, assertPasses, assertNoSide, assertReach} {
		want = append(want, anchored(re.String()))
	}
	got := schemaAssertionPatterns(t, root)
	sort.Strings(want)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("schema assertion patterns\n%q\nassert.go grammar\n%q", got, want)
	}

	var schemaRes []*regexp.Regexp
	for _, p := range got {
		schemaRes = append(schemaRes, regexp.MustCompile(p))
	}
	goRes := []*regexp.Regexp{assertGate, assertTerminal, assertPasses, assertNoSide, assertReach}
	matchAny := func(res []*regexp.Regexp, s string) bool {
		for _, re := range res {
			if re.MatchString(s) {
				return true
			}
		}
		return false
	}
	var good []string
	for _, e := range defFor(t, root, "assertion")["examples"].([]any) {
		good = append(good, e.(string))
	}
	for _, e := range schemaProps(t, root, root)["assert"]["examples"].([]any)[0].([]any) {
		good = append(good, e.(string))
	}
	// The guide's table.
	good = append(good, "every path to charge passes approval >= 2 of 3", "  every  path to   charge passes approval >= 1 ")
	bad := []string{
		"", "charge", "charge reachables", "every path to charge passes approval > 2",
		"every path to charge passes approval >= x", "no side effects after charge",
		"every path to charge passes", "every path to charge passes approval >= 2 of", "charge Reachable",
	}
	for _, s := range good {
		if !matchAny(schemaRes, s) {
			t.Errorf("schema rejects the assertion %q", s)
		}
		if !matchAny(goRes, strings.Join(strings.Fields(s), " ")) {
			t.Errorf("the code rejects the assertion %q, which the schema's examples or this test call valid", s)
		}
	}
	for _, s := range bad {
		if matchAny(schemaRes, s) {
			t.Errorf("schema accepts the assertion %q", s)
		}
		if matchAny(goRes, strings.Join(strings.Fields(s), " ")) {
			t.Errorf("the code accepts the assertion %q, which this test calls invalid", s)
		}
	}
}

// ---- a validator for the schema keywords the flow schema uses ----

func validate(t testing.TB, root jsonObj, v any, s jsonObj, path string) []string {
	t.Helper()
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, path+": "+fmt.Sprintf(format, args...)) }
	if _, ok := s["$ref"]; ok {
		errs = append(errs, validate(t, root, v, resolve(t, root, s), path)...)
	}
	if c, ok := s["const"]; ok && !reflect.DeepEqual(c, v) {
		add("want %v", c)
	}
	if e, ok := s["enum"].([]any); ok && !slices.ContainsFunc(e, func(x any) bool { return reflect.DeepEqual(x, v) }) {
		add("%v is not one of %v", v, e)
	}
	if ty, ok := s["type"].(string); ok {
		okType := false
		switch ty {
		case "object":
			_, okType = v.(map[string]any)
		case "array":
			_, okType = v.([]any)
		case "string":
			_, okType = v.(string)
		case "integer":
			f, isNum := v.(float64)
			okType = isNum && f == math.Trunc(f)
		}
		if !okType {
			add("want type %s, got %T", ty, v)
			return errs
		}
	}
	switch x := v.(type) {
	case string:
		if n, ok := s["minLength"].(float64); ok && float64(utf8.RuneCountInString(x)) < n {
			add("shorter than %v", n)
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			add("%q does not match %s", x, p)
		}
	case float64:
		if m, ok := s["minimum"].(float64); ok && x < m {
			add("%v is below %v", x, m)
		}
	case []any:
		if n, ok := s["minItems"].(float64); ok && float64(len(x)) < n {
			add("fewer than %v items", n)
		}
		if n, ok := s["maxItems"].(float64); ok && float64(len(x)) > n {
			add("more than %v items", n)
		}
		if s["uniqueItems"] == true {
			for i := range x {
				for j := i + 1; j < len(x); j++ {
					if reflect.DeepEqual(x[i], x[j]) {
						add("items %d and %d are equal", i, j)
					}
				}
			}
		}
		if it, ok := s["items"].(jsonObj); ok {
			for i, e := range x {
				errs = append(errs, validate(t, root, e, it, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	case map[string]any:
		props, _ := s["properties"].(jsonObj)
		for _, r := range anySlice(s["required"]) {
			if _, ok := x[r.(string)]; !ok {
				add("missing required key %q", r)
			}
		}
		for k, e := range x {
			if p, ok := props[k].(jsonObj); ok {
				errs = append(errs, validate(t, root, e, p, path+"."+k)...)
			} else if s["additionalProperties"] == false {
				add("key %q is not allowed", k)
			}
		}
	}
	if one, ok := s["oneOf"].([]any); ok {
		n := 0
		for _, b := range one {
			if len(validate(t, root, v, b.(jsonObj), path)) == 0 {
				n++
			}
		}
		if n != 1 {
			add("matches %d of the %d forms allowed, want exactly 1", n, len(one))
		}
	}
	if any_, ok := s["anyOf"].([]any); ok {
		n := 0
		for _, b := range any_ {
			if len(validate(t, root, v, b.(jsonObj), path)) == 0 {
				n++
			}
		}
		if n == 0 {
			add("matches none of the %d forms allowed", len(any_))
		}
	}
	return errs
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// ---- a reader for the YAML the samples use ----
//
// Block mappings and sequences, flow {a: b} and [a, b], plain and quoted scalars, and comments;
// nothing else (no anchors, tags, block scalars or multi-line scalars), and it fails on those.

type yamlLine struct {
	indent int
	text   string
	no     int
}

func yamlToValue(src string) (any, error) {
	var lines []yamlLine
	for i, raw := range strings.Split(src, "\n") {
		text := strings.TrimRight(stripYAMLComment(raw), " \t\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		if strings.Contains(raw[:len(raw)-len(strings.TrimLeft(raw, " \t"))], "\t") {
			return nil, fmt.Errorf("line %d: tab indentation", i+1)
		}
		trim := strings.TrimLeft(text, " ")
		if trim == "---" {
			continue
		}
		lines = append(lines, yamlLine{len(text) - len(trim), trim, i + 1})
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	p := &yamlParser{lines: lines}
	v, err := p.block(lines[0].indent)
	if err == nil && p.i < len(p.lines) {
		err = fmt.Errorf("line %d: unexpected indentation", p.lines[p.i].no)
	}
	return v, err
}

// stripYAMLComment drops a # comment that starts a line or follows a space, outside quotes.
func stripYAMLComment(s string) string {
	var q rune
	for i, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			}
		case r == '"' || r == '\'':
			q = r
		case r == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

type yamlParser struct {
	lines []yamlLine
	i     int
}

func (p *yamlParser) block(indent int) (any, error) {
	if l := p.lines[p.i]; l.text == "-" || strings.HasPrefix(l.text, "- ") {
		return p.sequence(indent)
	}
	return p.mapping(indent)
}

func (p *yamlParser) sequence(indent int) (any, error) {
	out := []any{}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if l.indent != indent || !(l.text == "-" || strings.HasPrefix(l.text, "- ")) {
			break
		}
		rest := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
		switch {
		case rest == "":
			p.i++
			if p.i >= len(p.lines) || p.lines[p.i].indent <= indent {
				out = append(out, nil)
				continue
			}
			v, err := p.block(p.lines[p.i].indent)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		case isYAMLKey(rest):
			// "- key: value": the item is a mapping whose first line is the rest of this one.
			col := l.indent + (len(l.text) - len(rest))
			p.lines[p.i] = yamlLine{col, rest, l.no}
			v, err := p.mapping(col)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		default:
			v, err := yamlInline(rest, l.no)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			p.i++
		}
	}
	return out, nil
}

func (p *yamlParser) mapping(indent int) (any, error) {
	out := map[string]any{}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if l.indent != indent || l.text == "-" || strings.HasPrefix(l.text, "- ") {
			break
		}
		if !isYAMLKey(l.text) {
			return nil, fmt.Errorf("line %d: want a key, got %q", l.no, l.text)
		}
		k, rest := splitYAMLKey(l.text)
		key, err := yamlScalar(k, l.no)
		ks, isStr := key.(string)
		if err != nil || !isStr {
			return nil, fmt.Errorf("line %d: bad key %q", l.no, k)
		}
		if _, dup := out[ks]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", l.no, ks)
		}
		p.i++
		if rest == "" {
			if p.i < len(p.lines) && (p.lines[p.i].indent > indent ||
				p.lines[p.i].indent == indent && (p.lines[p.i].text == "-" || strings.HasPrefix(p.lines[p.i].text, "- "))) {
				v, err := p.block(p.lines[p.i].indent)
				if err != nil {
					return nil, err
				}
				out[ks] = v
			} else {
				out[ks] = nil
			}
			continue
		}
		v, err := yamlInline(rest, l.no)
		if err != nil {
			return nil, err
		}
		out[ks] = v
	}
	return out, nil
}

// isYAMLKey reports whether s is "key: value" or "key:" with an unquoted-or-quoted key.
func isYAMLKey(s string) bool {
	if s == "" || strings.ContainsRune("{[", rune(s[0])) {
		return false
	}
	k, _ := splitYAMLKey(s)
	return k != "" && k != s || strings.HasSuffix(s, ":") && k != ""
}

// splitYAMLKey splits at the first ": " (or a final ":") outside quotes.
func splitYAMLKey(s string) (key, rest string) {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case (c == '"' || c == '\'') && i == 0:
			q = c
		case c == ':' && (i == len(s)-1 || s[i+1] == ' '):
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		}
	}
	return s, ""
}

func yamlInline(s string, no int) (any, error) {
	if s == "|" || s == ">" || strings.ContainsAny(s[:1], "&*!%@`") || strings.HasPrefix(s, "| ") || strings.HasPrefix(s, "> ") {
		return nil, fmt.Errorf("line %d: %q is YAML this reader does not support", no, s)
	}
	f := &yamlFlow{s: s, no: no}
	v, err := f.value(false)
	if err != nil {
		return nil, err
	}
	if f.skip(); f.i != len(f.s) {
		return nil, fmt.Errorf("line %d: unexpected %q", no, f.s[f.i:])
	}
	return v, nil
}

type yamlFlow struct {
	s  string
	i  int
	no int
}

func (f *yamlFlow) skip() {
	for f.i < len(f.s) && f.s[f.i] == ' ' {
		f.i++
	}
}

func (f *yamlFlow) value(inFlow bool) (any, error) {
	f.skip()
	if f.i >= len(f.s) {
		return nil, nil
	}
	switch f.s[f.i] {
	case '{':
		f.i++
		out := map[string]any{}
		for {
			f.skip()
			if f.i < len(f.s) && f.s[f.i] == '}' {
				f.i++
				return out, nil
			}
			k, err := f.scalarUntil(":")
			if err != nil {
				return nil, err
			}
			ks, ok := k.(string)
			if !ok || f.i >= len(f.s) || f.s[f.i] != ':' {
				return nil, fmt.Errorf("line %d: bad flow mapping in %q", f.no, f.s)
			}
			f.i++
			v, err := f.value(true)
			if err != nil {
				return nil, err
			}
			out[ks] = v
			f.skip()
			if f.i < len(f.s) && f.s[f.i] == ',' {
				f.i++
			} else if f.i >= len(f.s) || f.s[f.i] != '}' {
				return nil, fmt.Errorf("line %d: unterminated flow mapping in %q", f.no, f.s)
			}
		}
	case '[':
		f.i++
		out := []any{}
		for {
			f.skip()
			if f.i < len(f.s) && f.s[f.i] == ']' {
				f.i++
				return out, nil
			}
			v, err := f.value(true)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			f.skip()
			if f.i < len(f.s) && f.s[f.i] == ',' {
				f.i++
			} else if f.i >= len(f.s) || f.s[f.i] != ']' {
				return nil, fmt.Errorf("line %d: unterminated flow sequence in %q", f.no, f.s)
			}
		}
	}
	if inFlow {
		return f.scalarUntil(",]}")
	}
	v, err := yamlScalar(strings.TrimSpace(f.s[f.i:]), f.no)
	f.i = len(f.s)
	return v, err
}

// scalarUntil reads a scalar up to one of the stop bytes (outside quotes); ":" stops only before a
// space or at the end.
func (f *yamlFlow) scalarUntil(stops string) (any, error) {
	f.skip()
	start := f.i
	var q byte
	for ; f.i < len(f.s); f.i++ {
		c := f.s[f.i]
		if q != 0 {
			if c == q {
				q = 0
			}
			continue
		}
		if (c == '"' || c == '\'') && f.i == start {
			q = c
			continue
		}
		if c == ':' && strings.Contains(stops, ":") && (f.i+1 == len(f.s) || f.s[f.i+1] == ' ') {
			break
		}
		if c != ':' && strings.IndexByte(stops, c) >= 0 {
			break
		}
	}
	return yamlScalar(strings.TrimSpace(f.s[start:f.i]), f.no)
}

func yamlScalar(s string, no int) (any, error) {
	switch {
	case s == "" || s == "null" || s == "~":
		return nil, nil
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	case s[0] == '"':
		u, err := strconv.Unquote(s)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad quoted string %s", no, s)
		}
		return u, nil
	case s[0] == '\'':
		if len(s) < 2 || s[len(s)-1] != '\'' {
			return nil, fmt.Errorf("line %d: bad quoted string %s", no, s)
		}
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return float64(n), nil
	}
	return s, nil
}

func TestYAMLReader(t *testing.T) {
	src := "# c\nversion: 1\nflow: f # trailing\nnodes:\n  - {name: a, block: a, approval: {need: 2, approvers: [x, \"y z\"]}}\n  - name: b\n    block: b\nassert:\n- credit reachable\n- 'n: \"q r\" reachable'\nempty:\n"
	got, err := yamlToValue(src)
	if err != nil {
		t.Fatal(err)
	}
	want := jsonObj{
		"version": 1.0, "flow": "f", "empty": nil,
		"nodes": []any{
			jsonObj{"name": "a", "block": "a", "approval": jsonObj{"need": 2.0, "approvers": []any{"x", "y z"}}},
			jsonObj{"name": "b", "block": "b"},
		},
		"assert": []any{"credit reachable", `n: "q r" reachable`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
	for _, bad := range []string{"a: |\n  x\n", "a: &x 1\n", "a: 1\na: 2\n", "a: [1, 2\n", "a:\n\tb: 1\n"} {
		if v, err := yamlToValue(bad); err == nil {
			t.Errorf("%q read as %v, want an error", bad, v)
		}
	}
}

// Every sample config is valid under the schema (so it uses only keys the schema allows), is
// strictly readable by Load's decoder, and starts with the line that points an editor at the schema.
func TestFlowSchema_SampleConfigsAreValid(t *testing.T) {
	root := loadFlowSchema(t)
	files, err := filepath.Glob(flowSamplesGlob)
	if err != nil || len(files) < 6 {
		t.Fatalf("sample configs %v (%v), want at least the six in examples/plan/flows", files, err)
	}
	id := root["$id"].(string)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if want := "# yaml-language-server: $schema=" + id; !strings.HasPrefix(string(b), want+"\n") {
				t.Errorf("first line is not %q", want)
			}
			v, err := yamlToValue(string(b))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range validate(t, root, v, root, "$") {
				t.Error(e)
			}
			js, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			var cfg config
			if err := parseConfig(js, &cfg); err != nil {
				t.Errorf("Load's decoder refuses the sample: %v", err)
			}
		})
	}
}

// The schema refuses what Load refuses at parse and shape level, and accepts what it accepts: the
// same table is run through both.
func TestFlowSchema_AgreesWithLoad(t *testing.T) {
	root := loadFlowSchema(t)
	// Valid, but Load needs more than the config's shape for these (an approver verifier, a loop
	// built with its bound).
	schemaOnly := map[string]bool{"all node keys": true, "switch and loop": true}
	const nodes = `"nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}]`
	cases := []struct {
		name, cfg string
		ok        bool
	}{
		{"minimal", `{"version":1,"flow":"f",` + nodes + `,"wiring":[{"edge":["a","b"]}]}`, true},
		{"all node keys", `{"version":1,"flow":"f","in":"int","out":"int","entry":"a","nodes":[{"name":"a","block":"a","safety":"idempotent","approval":{"need":1,"approvers":["ops"]}},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}],"assert":["b reachable"]}`, true},
		{"switch and loop", `{"version":1,"flow":"f",` + nodes + `,"wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"again","to":"a","loop_max":3}],"else":"b"}]}`, true},
		{"no version", `{"flow":"f",` + nodes + `,"wiring":[{"edge":["a","b"]}]}`, false},
		{"version 2", `{"version":2,"flow":"f",` + nodes + `,"wiring":[{"edge":["a","b"]}]}`, false},
		{"misspelled approval", `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","aproval":{"need":1,"approvers":["ops"]}},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, false},
		{"misspelled entry", `{"version":1,"flow":"f","entrypoint":"b",` + nodes + `,"wiring":[{"edge":["a","b"]}]}`, false},
		{"pre-v1 loopMax", `{"version":1,"flow":"f",` + nodes + `,"wiring":[{"edge":["a","b"]},{"switch":"b","when":[{"pred":"again","to":"a","loopMax":3}]}]}`, false},
		{"unknown top level", `{"version":1,"flow":"f",` + nodes + `,"wiring":[{"edge":["a","b"]}],"author":"ops"}`, false},
		{"retryable safety", `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","safety":"retryable"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, false},
		{"hyphenated safety", `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","safety":"read-only"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, false},
		{"edge and switch", `{"version":1,"flow":"f",` + nodes + `,"wiring":[{"edge":["a","b"],"switch":"a"}]}`, false},
		{"empty wiring element", `{"version":1,"flow":"f",` + nodes + `,"wiring":[{}]}`, false},
		{"edge of three", `{"version":1,"flow":"f",` + nodes + `,"wiring":[{"edge":["a","b","a"]}]}`, false},
		{"approval need 0", `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","approval":{"need":0,"approvers":["ops"]}},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, false},
		{"approval no approvers", `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","approval":{"need":1,"approvers":[]}},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, false},
		{"repeated approver", `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","approval":{"need":1,"approvers":["ops","ops"]}},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, false},
		{"node without block", `{"version":1,"flow":"f","nodes":[{"name":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(c.cfg), &v); err != nil {
				t.Fatal(err)
			}
			schemaErrs := validate(t, root, v, root, "$")
			if got := len(schemaErrs) == 0; got != c.ok {
				t.Errorf("schema accepts = %v, want %v: %v", got, c.ok, schemaErrs)
			}
			if schemaOnly[c.name] {
				return
			}
			_, err := Load[int, int]([]byte(c.cfg), strictRegistry(t))
			if got := err == nil; got != c.ok {
				t.Errorf("Load accepts = %v, want %v: %v", got, c.ok, err)
			}
		})
	}
}

// A key the schema requires is one Load needs, and one it leaves out is one Load can do without:
// dropping each key from a loadable config fails Load exactly for the required ones. (A schema that
// required more would flag configs that load.)
func TestFlowSchema_RequiredKeysAreNeeded(t *testing.T) {
	root := loadFlowSchema(t)
	reg := NewRegistry()
	if err := reg.RegisterStep("a", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly()); err != nil {
		t.Fatal(err)
	}
	const base = `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"}],"wiring":[]}`
	keys, _ := tagInfo(reflect.TypeFor[config]())
	for _, key := range keys {
		var m jsonObj
		_ = json.Unmarshal([]byte(base), &m)
		if _, ok := m[key]; !ok {
			continue // not in the base, so it is optional there
		}
		delete(m, key)
		b, _ := json.Marshal(m)
		_, err := Load[int, int](b, reg)
		required := slices.Contains(root["required"].([]any), any(key))
		if required != (err != nil) {
			t.Errorf("key %q: schema requires it = %v, Load without it fails = %v (%v)", key, required, err != nil, err)
		}
	}
}
