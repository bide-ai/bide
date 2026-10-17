package flowedit

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/plan"
)

const assertFlow = `version: 1
flow: refund
entry: validate
nodes:
  - {name: validate, block: validate, safety: readonly}
  - {name: "big spender", block: big, safety: readonly}
  - name: charge
    block: charge
    safety: side_effect
    approval: {need: 2, approvers: [ops, finance, risk]}
  - {name: receipt, block: receipt, safety: idempotent}
wiring:
  - edge: [validate, big spender]
  - edge: [big spender, charge]
  - edge: [charge, receipt]
`

func assertOp(op string, idx int, a *Assertion) Operation {
	return Operation{Op: op, Index: idx, Assertion: a}
}

// Each of the six forms is written as the line plan reads, read back as the same form, and decided
// by the check (not invalid), for node names that are bare, have a space, or have a character that
// is not ASCII.
func TestAssertionFormsRoundTrip(t *testing.T) {
	for _, node := range []string{"charge", "big spender", "überweisung", "a-b_c.d", `my\ node`, `x\n y`, `tab\t here`, `back\\slash x`, `bare\back`} {
		for _, a := range []Assertion{
			{Form: "approval", Node: node, K: 2},
			{Form: "approval", Node: node, K: 2, N: 3},
			{Form: "approval", Node: node, K: 1, N: 1, Excluding: true},
			{Form: "approval", Node: node, K: 3, Excluding: true},
			{Form: "passes", Node: node, Via: "validate"},
			{Form: "passes", Node: "receipt", Via: node},
			{Form: "terminal", Via: node},
			{Form: "no_side_effect", Node: node},
			{Form: "reachable", Node: node},
			{Form: "unreachable", Node: node},
		} {
			text, err := a.Text()
			if err != nil {
				t.Errorf("%+v: %v", a, err)
				continue
			}
			back, ok := ParseAssertion(text)
			if !ok || !reflect.DeepEqual(back, a) {
				t.Errorf("%q reads back as %+v (%v), want %+v", text, back, ok, a)
			}
			// plan reads it: not an invalid line, and the line it reports is the one written.
			cfg := strings.Replace(assertFlow, "wiring:", "assert:\n  - "+yamlString(text)+"\nwiring:", 1)
			cfg = strings.ReplaceAll(cfg, "name: charge", "name: charge")
			data, err := ToJSON([]byte(cfg))
			if err != nil {
				t.Fatalf("%q: %v", text, err)
			}
			// the node under test must be declared for the check to decide it
			r := plan.InspectConfig(data, nil)
			if len(r.Assertions) != 1 || r.Assertions[0].Text != text {
				t.Errorf("%q: plan reports %+v", text, r.Assertions)
				continue
			}
			if r.Assertions[0].Invalid && declared(r, a) {
				t.Errorf("%q: plan says invalid: %s", text, r.Assertions[0].Message)
			}
		}
	}
}

func declared(r plan.ConfigReport, a Assertion) bool {
	have := map[string]bool{}
	for _, n := range r.Topology.Nodes {
		have[n.Name] = true
	}
	return (a.Node == "" || have[a.Node]) && (a.Via == "" || have[a.Via])
}

func yamlString(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a YAML double-quoted scalar
	return string(b)
}

func TestAssertionTextRefusals(t *testing.T) {
	for name, a := range map[string]Assertion{
		"an unknown form":          {Form: "always", Node: "charge"},
		"no form":                  {Node: "charge"},
		"approval with no node":    {Form: "approval", K: 1},
		"K of zero":                {Form: "approval", Node: "charge", K: 0},
		"K above N":                {Form: "approval", Node: "charge", K: 3, N: 2},
		"a negative N":             {Form: "approval", Node: "charge", K: 1, N: -1},
		"a node with a quote":      {Form: "reachable", Node: `a"b`},
		"a node with a tab":        {Form: "reachable", Node: "a\tb"},
		"a node with a no-break":   {Form: "reachable", Node: "a\u00a0b"},
		"a node with two spaces":   {Form: "reachable", Node: "a  b"},
		"a node with a lead space": {Form: "reachable", Node: " a"},
		"passes with no via":       {Form: "passes", Node: "charge"},
		"terminal with no via":     {Form: "terminal"},
		"reachable with no node":   {Form: "reachable"},
	} {
		if _, err := a.Text(); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			var rf *Refusal
			if !errors.As(err, &rf) {
				t.Errorf("%s: %v is not a Refusal", name, err)
			}
		}
	}
}

// ParseAssertion reads what plan reads, in the order plan tries the forms, and nothing else.
func TestParseAssertion(t *testing.T) {
	for line, want := range map[string]*Assertion{
		"every  path to charge   passes approval >= 2 of 3":            {Form: "approval", Node: "charge", K: 2, N: 3},
		`every path to "big spender" passes approval >= 1`:             {Form: "approval", Node: "big spender", K: 1},
		"every path to a terminal passes validate":                     {Form: "terminal", Via: "validate"},
		"every path to charge passes validate":                         {Form: "passes", Node: "charge", Via: "validate"},
		"no side effect after validate":                                {Form: "no_side_effect", Node: "validate"},
		"charge reachable":                                             {Form: "reachable", Node: "charge"},
		`"big spender" unreachable`:                                    {Form: "unreachable", Node: "big spender"},
		"charge sometimes reachable":                                   nil,
		"every path to charge passes approval >= x":                    nil,
		"every path to charge passes approval >= 99999999999999999999": nil,
		"": nil,
	} {
		a, ok := ParseAssertion(line)
		if want == nil && ok || want != nil && (!ok || a != *want) {
			t.Errorf("%q: %+v %v, want %+v", line, a, ok, want)
		}
	}
}

func TestAssertionOperationsAreSplices(t *testing.T) {
	withList := assertFlow + "assert:\n  # the gate\n  - every path to charge passes approval >= 2 of 3\n  - receipt reachable   # tail\n"
	for _, c := range []struct {
		name string
		src  string
		op   Operation
		want string
	}{
		{"add to a list", withList, assertOp("add_assertion", 0, &Assertion{Form: "no_side_effect", Node: "receipt"}),
			withList + "  - no side effect after receipt\n"},
		{"add creates the list", assertFlow, assertOp("add_assertion", 0, &Assertion{Form: "approval", Node: "charge", K: 2, N: 3, Excluding: true}),
			assertFlow + "assert:\n  - every path to charge passes approval >= 2 of 3 excluding the requester\n"},
		{"add a name with a space", assertFlow, assertOp("add_assertion", 0, &Assertion{Form: "reachable", Node: "big spender"}),
			assertFlow + "assert:\n  - '\"big spender\" reachable'\n"},
		{"set a line", withList, assertOp("set_assertion", 0, &Assertion{Form: "approval", Node: "charge", K: 2, N: 3, Excluding: true}),
			strings.Replace(withList, "approval >= 2 of 3\n", "approval >= 2 of 3 excluding the requester\n", 1)},
		{"set the line with a trailing comment", withList, assertOp("set_assertion", 1, &Assertion{Form: "unreachable", Node: "receipt"}),
			strings.Replace(withList, "receipt reachable   # tail", "receipt unreachable   # tail", 1)},
		{"remove a line and its comment", withList, assertOp("remove_assertion", 0, nil),
			strings.Replace(withList, "  # the gate\n  - every path to charge passes approval >= 2 of 3\n", "", 1)},
		{"remove the last line takes the list", assertFlow + "assert:\n  - receipt reachable\n", assertOp("remove_assertion", 0, nil), assertFlow},
		{"a no-op set", withList, assertOp("set_assertion", 1, &Assertion{Form: "reachable", Node: "receipt"}), withList},
	} {
		got := mustApply(t, "f.yaml", c.src, c.op)
		if got != c.want {
			t.Errorf("%s:\n--- got\n%s\n--- want\n%s", c.name, got, c.want)
		}
	}
	// JSON.
	js := `{"version": 1, "nodes": [{"name": "a", "block": "a"}], "wiring": [], "assert": ["a reachable"]}`
	got := mustApply(t, "f.json", js, assertOp("add_assertion", 0, &Assertion{Form: "no_side_effect", Node: "a"}))
	if !strings.Contains(got, `"a reachable", "no side effect after a"`) && !strings.Contains(got, `"no side effect after a"`) {
		t.Errorf("json:\n%s", got)
	}
	got = mustApply(t, "f.json", js, assertOp("remove_assertion", 0, nil))
	if strings.Contains(got, "assert") {
		t.Errorf("json remove:\n%s", got)
	}
	// Flow-style and empty lists.
	for name, src := range map[string]string{
		"a flow-style list": "version: 1\nnodes: [{name: a, block: a}]\nwiring: []\nassert: [\"a reachable\"]\n",
		"an empty list":     "version: 1\nnodes: [{name: a, block: a}]\nwiring: []\nassert: []\n",
		"a null list":       "version: 1\nnodes: [{name: a, block: a}]\nwiring: []\nassert:\n",
	} {
		got := mustApply(t, "f.yaml", src, assertOp("add_assertion", 0, &Assertion{Form: "no_side_effect", Node: "a"}))
		data, err := ToJSON([]byte(got))
		if err != nil {
			t.Errorf("%s: %v\n%s", name, err, got)
			continue
		}
		r := plan.InspectConfig(data, nil)
		if !slices.ContainsFunc(r.Assertions, func(a plan.AssertionResult) bool { return a.Text == "no side effect after a" }) {
			t.Errorf("%s: %+v\n%s", name, r.Assertions, got)
		}
	}
}

func TestAssertionOperationRefusals(t *testing.T) {
	withList := assertFlow + "assert:\n  - receipt reachable\n"
	for name, c := range map[string]struct {
		src string
		op  Operation
		why string
	}{
		"add with no assertion":   {withList, Operation{Op: "add_assertion"}, "needs an assertion"},
		"an unknown node":         {withList, assertOp("add_assertion", 0, &Assertion{Form: "reachable", Node: "nope"}), "no node is named"},
		"an unknown via":          {withList, assertOp("add_assertion", 0, &Assertion{Form: "passes", Node: "charge", Via: "nope"}), "no node is named"},
		"a duplicate":             {withList, assertOp("add_assertion", 0, &Assertion{Form: "reachable", Node: "receipt"}), "already has"},
		"a set to another's text": {withList + "  - charge reachable\n", assertOp("set_assertion", 1, &Assertion{Form: "reachable", Node: "receipt"}), "already has"},
		"set out of range":        {withList, assertOp("set_assertion", 5, &Assertion{Form: "reachable", Node: "charge"}), "no assertion at position"},
		"set a negative index":    {withList, assertOp("set_assertion", -1, &Assertion{Form: "reachable", Node: "charge"}), "no assertion at position"},
		"remove out of range":     {withList, assertOp("remove_assertion", 1, nil), "no assertion at position"},
		"remove from no list":     {assertFlow, assertOp("remove_assertion", 0, nil), "no assertion at position"},
		"bad parameters":          {withList, assertOp("add_assertion", 0, &Assertion{Form: "approval", Node: "charge", K: 0}), "1 <= K"},
		"an assert that is a map": {assertFlow + "assert: {a: b}\n", assertOp("add_assertion", 0, &Assertion{Form: "reachable", Node: "charge"}), "not a list of strings"},
	} {
		_, err := ApplyOperation("f.yaml", []byte(c.src), c.op)
		var rf *Refusal
		if !errors.As(err, &rf) {
			t.Errorf("%s: want a refusal, got %v", name, err)
			continue
		}
		if !strings.Contains(rf.Reason, c.why) {
			t.Errorf("%s: %q, want it to say %q", name, rf.Reason, c.why)
		}
	}
}

// The view carries each assert line as a form, aligned with the verdicts, so the page can edit it.
func TestViewCarriesAssertionForms(t *testing.T) {
	v := BuildView("f.yaml", []byte(assertFlow+"assert:\n  - every path to charge passes approval >= 2 of 3\n  - charge sometimes reachable\n  - receipt reachable\n"))
	if len(v.AssertionForms) != 3 || v.AssertionForms[0] == nil || v.AssertionForms[0].K != 2 || v.AssertionForms[1] != nil || v.AssertionForms[2] == nil || v.AssertionForms[2].Form != "reachable" {
		t.Errorf("%+v", v.AssertionForms)
	}
}

// The line the editor writes for a node whose name has a backslash and a space is read by plan as that
// node (the quoted name has no escapes), through the whole path: operation, text, loader, check.
func TestAssertionOnABackslashNodeReadsAsThatNode(t *testing.T) {
	for _, name := range []string{`my\ node`, `x\n y`} {
		src := "version: 1\nflow: f\nentry: " + yamlString(name) + "\nnodes:\n  - {name: " + yamlString(name) + ", block: b, safety: readonly}\nwiring: []\n"
		got := mustApply(t, "f.yaml", src, assertOp("add_assertion", 0, &Assertion{Form: "reachable", Node: name}))
		data, err := ToJSON([]byte(got))
		if err != nil {
			t.Fatal(err)
		}
		r := plan.InspectConfig(data, nil)
		if len(r.Assertions) != 1 || r.Assertions[0].Invalid || !r.Assertions[0].Holds {
			t.Errorf("%q: %+v\n%s", name, r.Assertions, got)
		}
		if a, ok := ParseAssertion(r.Assertions[0].Text); !ok || a.Node != name {
			t.Errorf("%q reads back as %+v", name, a)
		}
	}
}
