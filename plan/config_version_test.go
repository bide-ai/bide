package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A version 1 config round-trips: parsed, marshaled, and parsed again it is the same config, its
// marshaled form states "version": 1 and spells every key in snake_case, and it loads to the
// same Digest as the text it came from.
func TestConfigV1_RoundTrips(t *testing.T) {
	for name, tc := range map[string]struct {
		data string
		load func([]byte) (string, error)
		keys []string
	}{
		"triage": {triageConfig, func(b []byte) (string, error) {
			f, err := Load[cfgOrder, cfgReceipt](b, triageRegistry(t))
			if err != nil {
				return "", err
			}
			return f.Digest(), nil
		}, []string{`"version":1`, `"when":`, `"else":`}},
		"diamond": {diamondConfig, func(b []byte) (string, error) {
			f, err := Load[int, string](b, diamondRegistry(t))
			if err != nil {
				return "", err
			}
			return f.Digest(), nil
		}, []string{`"version":1`, `"join":`, `"inputs":`, `"merge":`}},
		"loop": {loopConfig, func(b []byte) (string, error) {
			f, err := Load[int, string](b, loopRegistry(t))
			if err != nil {
				return "", err
			}
			return f.Digest(), nil
		}, []string{`"version":1`, `"loop_max":10`}},
	} {
		var first config
		if err := parseConfig([]byte(tc.data), &first); err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		out, err := json.Marshal(first)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		for _, k := range tc.keys {
			if !strings.Contains(string(out), k) {
				t.Errorf("%s: marshaled config lacks %s:\n%s", name, k, out)
			}
		}
		var second config
		if err := parseConfig(out, &second); err != nil {
			t.Fatalf("%s: re-parse of the marshaled config: %v\n%s", name, err, out)
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("%s: config did not round-trip:\n got %#v\nwant %#v", name, second, first)
		}
		d1, err := tc.load([]byte(tc.data))
		if err != nil {
			t.Fatalf("%s: load: %v", name, err)
		}
		d2, err := tc.load(out)
		if err != nil || d1 != d2 {
			t.Errorf("%s: marshaled config loads to %q (%v), want %q", name, d2, err, d1)
		}
	}
}

// A config must state "version": 1. A missing version, another number, or a value that is not the
// number 1 is refused by Load and Validate with an ErrConfig naming the key.
func TestConfig_VersionIsRequired(t *testing.T) {
	const rest = `"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]`
	for name, tc := range map[string]struct{ cfg, want string }{
		"missing":      {`{` + rest + `}`, `config has no "version"; state "version": 1`},
		"zero":         {`{"version":0,` + rest + `}`, `"version": 0, which this release does not read; it reads version 1`},
		"future":       {`{"version":2,` + rest + `}`, `"version": 2, which this release does not read; it reads version 1`},
		"string":       {`{"version":"1",` + rest + `}`, `"version" is "1"; want the number 1`},
		"float":        {`{"version":1.0,` + rest + `}`, `"version" is 1.0; want the number 1`},
		"null":         {`{"version":null,` + rest + `}`, `"version" is null; want the number 1`},
		"negative":     {`{"version":-1,` + rest + `}`, `"version" is -1; want the number 1`},
		"future+field": {`{"version":2,"retries":3,` + rest + `}`, `"version": 2, which this release does not read`},
	} {
		_, lerr := Load[int, int]([]byte(tc.cfg), strictRegistry(t))
		verr := Validate([]byte(tc.cfg), strictRegistry(t))
		for fn, err := range map[string]error{"Load": lerr, "Validate": verr} {
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(fmt.Sprint(err), tc.want) {
				t.Errorf("%s: %s = %v; want an ErrConfig containing %q", name, fn, err, tc.want)
			}
		}
	}
	ok := `{"version":1,` + rest + `}`
	if _, err := Load[int, int]([]byte(ok), strictRegistry(t)); err != nil {
		t.Fatalf("Load of a version 1 config: %v", err)
	}
}

// Every other parse failure also wraps ErrConfig.
func TestConfig_ParseErrorsWrapErrConfig(t *testing.T) {
	for name, cfg := range map[string]string{
		"not json":      `{`,
		"not an object": `[]`,
		"unknown key":   `{"version":1,"flow":"f","nodes":[],"wiring":[],"author":"ops"}`,
	} {
		if _, err := Load[int, int]([]byte(cfg), strictRegistry(t)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: Load = %v; want ErrConfig", name, err)
		}
		if err := Validate([]byte(cfg), strictRegistry(t)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: Validate = %v; want ErrConfig", name, err)
		}
	}
}

// A pre-v1 camelCase key, or a key in another case, is refused with an error that names the
// snake_case key to write and where the key is. A config without a version gets the key hint
// first, since the key is the first thing a pre-v1 config has to change.
func TestConfig_CamelCaseKeysNameTheSnakeCaseKey(t *testing.T) {
	loop := strings.Replace(loopConfig, `"loop_max"`, `"loopMax"`, 1)
	if loop == loopConfig {
		t.Fatal("loopConfig has no loop_max to rewrite")
	}
	for name, tc := range map[string]struct {
		cfg  string
		reg  *Registry
		want string
	}{
		"loopMax": {loop, loopRegistry(t),
			`$.wiring[2].when[0]: "loopMax" is not a config key; version 1 keys are snake_case, write "loop_max"`},
		"loopMax without version": {strings.Replace(loop, `"version": 1,`, ``, 1), loopRegistry(t),
			`"loopMax" is not a config key; version 1 keys are snake_case, write "loop_max"`},
		"Safety": {`{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","Safety":"readonly"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, strictRegistry(t),
			`$.nodes[0]: "Safety" is not a config key; version 1 keys are snake_case, write "safety"`},
		"Version": {`{"Version":1,"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, strictRegistry(t),
			`$: "Version" is not a config key; version 1 keys are snake_case, write "version"`},
		"approval.Need": {`{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","approval":{"Need":1,"approvers":["ops"]}},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`, strictRegistry(t),
			`$.nodes[0].approval: "Need" is not a config key; version 1 keys are snake_case, write "need"`},
	} {
		err := Validate([]byte(tc.cfg), tc.reg)
		if !errors.Is(err, agent.ErrConfig) || !strings.Contains(fmt.Sprint(err), tc.want) {
			t.Errorf("%s: Validate = %v; want an ErrConfig containing %q", name, err, tc.want)
		}
	}
}

// snakeCase maps the spellings a pre-v1 or hand-written config uses to the v1 key.
func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{
		"loopMax":   "loop_max",
		"loop_max":  "loop_max",
		"Safety":    "safety",
		"inTypes":   "in_types",
		"loopBack":  "loop_back",
		"inURL":     "in_url",
		"URLPath":   "url_path",
		"step2Name": "step2_name",
		"flow":      "flow",
	} {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every JSON key the config reads and the Topology writes is snake_case.
func TestConfigAndTopologyKeysAreSnakeCase(t *testing.T) {
	snake := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	seen := map[reflect.Type]bool{}
	var bad []string
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !snake.MatchString(name) {
				bad = append(bad, typ.Name()+"."+f.Name+" "+strconv.Quote(name))
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeFor[config]())
	walk(reflect.TypeFor[Topology]())
	if len(bad) > 0 {
		t.Errorf("keys not snake_case: %v", bad)
	}
	if !seen[reflect.TypeFor[configArm]()] || !seen[reflect.TypeFor[TopologyBranchArm]()] {
		t.Fatal("walk did not reach the nested types")
	}
}

// The Topology's JSON spells every key in snake_case and every kind as its TopologyNodeKind.
func TestTopology_JSONKeys(t *testing.T) {
	data, err := json.Marshal(topologyFixtureFlow().Topology())
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"flow":"generate","entry":"parse","in":"string","out":"string",` +
		`"nodes":[{"name":"parse","kind":"step","in":"string","out":"int"},{"name":"gen","kind":"model","in":"int","out":"int"},` +
		`{"name":"gate","kind":"switch","in":"int","out":"int"},{"name":"reject","kind":"tool","in":"int","out":"string"},` +
		`{"name":"aux","kind":"step","in":"int","out":"bool"},{"name":"merge","kind":"join","in":"","out":"string"}],` +
		`"edges":[{"from":"parse","to":"gen"},{"from":"gen","to":"gate"},{"from":"gen","to":"merge","type":"int"},{"from":"aux","to":"merge","type":"bool"}],` +
		`"branches":[{"over":"gate","arms":[{"target":"gen","loop_back":true,"loop_max":3},{"target":"reject","else":true}],"else":"reject"}],` +
		`"joins":[{"name":"merge","inputs":["gen","aux"],"in_types":["int","bool"],"out":"string"}],` +
		`"loops":[{"head":"gen","over":"gate","body":["gen","gate"],"max":3}]}`
	if string(data) != want {
		t.Errorf("Topology JSON:\n got %s\nwant %s", data, want)
	}
}

// Every node kind the builder produces has a TopologyNodeKind.
func TestNodeKindName_CoversEveryKind(t *testing.T) {
	want := map[nodeKind]TopologyNodeKind{
		kindStep: NodeKindStep, kindTool: NodeKindTool, kindModel: NodeKindModel,
		kindSwitch: NodeKindSwitch, kindJoin: NodeKindJoin,
	}
	for k, w := range want {
		if got := nodeKindName(k); got != w {
			t.Errorf("nodeKindName(%d) = %q, want %q", k, got, w)
		}
	}
	if got := nodeKindName(kindJoin + 1); got != "" {
		t.Errorf("nodeKindName(out of range) = %q, want empty", got)
	}
}

// With several such keys the error names the same one every time: the first in sorted order.
func TestConfig_KeyHintIsDeterministic(t *testing.T) {
	cfg := `{"version":1,"flow":"f","nodes":[{"name":"a","Safety":"readonly","Block":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`
	const want = `$.nodes[0]: "Block" is not a config key; version 1 keys are snake_case, write "block"`
	for range 50 {
		if err := Validate([]byte(cfg), strictRegistry(t)); !strings.Contains(fmt.Sprint(err), want) {
			t.Fatalf("Validate = %v; want an error containing %q", err, want)
		}
	}
}
