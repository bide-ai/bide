package flowedit

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/plan"
)

const opsFlow = `# the refund flow
version: 1
flow: refund
entry: validate
nodes:
  # reads the request
  - {name: validate, block: validate, safety: readonly}   # no effect

  # the gated one
  - name: charge       # gated
    block: charge
    safety: side_effect
    approval:
      need: 2
      approvers: [ops, finance, risk]
  - {name: receipt, block: receipt}
wiring:
  - edge: [validate, charge]
  - edge: [charge, receipt]
`

func mustApply(t *testing.T, name, src string, op Operation) string {
	t.Helper()
	r, err := ApplyOperation(name, []byte(src), op)
	if err != nil {
		t.Fatalf("%+v: %v", op, err)
	}
	return string(r.Text)
}

// Each operation changes the lines it is about and no others: comments, blank lines, the flow and
// block styles of the siblings and the spacing before a trailing comment stay as they were.
func TestOperationsAreSplices(t *testing.T) {
	for _, c := range []struct {
		name string
		op   Operation
		want string // the text after, as a replacement of a line or lines in opsFlow
		from string
		to   string
	}{
		{"set a node's safety in a flow-style item", Operation{Op: "set_safety", Node: "receipt", Value: "idempotent"},
			"", "  - {name: receipt, block: receipt}\n", "  - {name: receipt, block: receipt, safety: idempotent}\n"},
		{"change a safety", Operation{Op: "set_safety", Node: "validate", Value: "idempotent"},
			"", "safety: readonly}   # no effect", "safety: idempotent}   # no effect"},
		{"remove a safety", Operation{Op: "set_safety", Node: "validate"},
			"", "  - {name: validate, block: validate, safety: readonly}   # no effect\n", "  - {name: validate, block: validate}   # no effect\n"},
		{"set a safety in a block-style item", Operation{Op: "set_safety", Node: "charge", Value: "idempotent"},
			"", "    safety: side_effect\n", "    safety: idempotent\n"},
		{"change a block", Operation{Op: "set_block", Node: "charge", Value: "charge_v2"},
			"", "    block: charge\n", "    block: charge_v2\n"},
		{"raise the need", Operation{Op: "set_approval", Node: "charge", Approval: &Approval{Need: 3, Approvers: []string{"ops", "finance", "risk"}}},
			"", "      need: 2\n", "      need: 3\n"},
		{"add an approver at the end", Operation{Op: "set_approval", Node: "charge", Approval: &Approval{Need: 2, Approvers: []string{"ops", "finance", "risk", "audit"}}},
			"", "[ops, finance, risk]", "[ops, finance, risk, audit]"},
		{"remove an approver", Operation{Op: "set_approval", Node: "charge", Approval: &Approval{Need: 2, Approvers: []string{"ops", "risk"}}},
			"", "[ops, finance, risk]", "[ops, risk]"},
		{"replace an approver", Operation{Op: "set_approval", Node: "charge", Approval: &Approval{Need: 2, Approvers: []string{"ops", "treasury", "risk"}}},
			"", "[ops, finance, risk]", "[ops, treasury, risk]"},
		{"exclude the requester", Operation{Op: "set_approval", Node: "charge", Approval: &Approval{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true}},
			"", "      approvers: [ops, finance, risk]\n", "      approvers: [ops, finance, risk]\n      exclude_principal: true\n"},
		{"set a deadline", Operation{Op: "set_approval", Node: "charge", Approval: &Approval{Need: 2, Approvers: []string{"ops", "finance", "risk"}, Timeout: "48h"}},
			"", "      approvers: [ops, finance, risk]\n", "      approvers: [ops, finance, risk]\n      timeout: 48h\n"},
		{"clear the gate", Operation{Op: "clear_approval", Node: "charge"},
			"", "    approval:\n      need: 2\n      approvers: [ops, finance, risk]\n", ""},
		{"gate a node", Operation{Op: "set_approval", Node: "receipt", Approval: &Approval{Need: 1, Approvers: []string{"ops"}}},
			"", "  - {name: receipt, block: receipt}\n", "  - {name: receipt, block: receipt, approval: {need: 1, approvers: [ops]}}\n"},
	} {
		got := mustApply(t, "f.yaml", opsFlow, c.op)
		want := strings.Replace(opsFlow, c.from, c.to, 1)
		if want == opsFlow && c.from != c.to {
			t.Fatalf("%s: the expectation does not apply to the sample", c.name)
		}
		if got != want {
			t.Errorf("%s:\n--- got\n%s\n--- want\n%s", c.name, got, want)
		}
	}
}

// A JSON config is edited too, and stays JSON.
func TestOperationsOnJSON(t *testing.T) {
	src := "{\n  \"version\": 1,\n  \"flow\": \"f\",\n  \"nodes\": [\n    {\"name\": \"a\", \"block\": \"a\"},\n    {\"name\": \"b\", \"block\": \"b\", \"approval\": {\"need\": 1, \"approvers\": [\"x\"]}}\n  ],\n  \"wiring\": []\n}\n"
	got := mustApply(t, "f.json", src, Operation{Op: "set_safety", Node: "a", Value: "readonly"})
	if want := strings.Replace(src, `{"name": "a", "block": "a"}`, `{"name": "a", "block": "a", "safety": "readonly"}`, 1); got != want {
		t.Errorf("safety:\n%s", got)
	}
	got = mustApply(t, "f.json", src, Operation{Op: "set_approval", Node: "b", Approval: &Approval{Need: 2, Approvers: []string{"x", "y"}, Timeout: "1h"}})
	if !json.Valid([]byte(got)) || !strings.Contains(got, `"need": 2`) || !strings.Contains(got, `"y"`) || !strings.Contains(got, `"timeout": "1h"`) {
		t.Errorf("gate:\n%s", got)
	}
	if !json.Valid([]byte(mustApply(t, "f.json", src, Operation{Op: "clear_approval", Node: "b"}))) {
		t.Error("clearing a gate left invalid JSON")
	}
}

// An operation that changes nothing returns the same bytes, and says so.
func TestNoOpIsIdentical(t *testing.T) {
	for _, op := range []Operation{
		{Op: "set_safety", Node: "validate", Value: "readonly"},
		{Op: "set_safety", Node: "receipt"},
		{Op: "set_block", Node: "charge", Value: "charge"},
		{Op: "clear_approval", Node: "validate"},
		{Op: "set_approval", Node: "charge", Approval: &Approval{Need: 2, Approvers: []string{"ops", "finance", "risk"}}},
	} {
		r, err := ApplyOperation("f.yaml", []byte(opsFlow), op)
		if err != nil || r.Changed || string(r.Text) != opsFlow {
			t.Errorf("%+v: changed %v, err %v", op, r != nil && r.Changed, err)
		}
	}
}

// What an operation cannot do is a refusal that names why, and the text is the caller's still.
func TestOperationRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		src string
		op  Operation
	}{
		"an unknown operation":      {opsFlow, Operation{Op: "rename_node", Node: "charge", Value: "x"}},
		"an unknown node":           {opsFlow, Operation{Op: "set_safety", Node: "nope", Value: "readonly"}},
		"an empty node name":        {opsFlow, Operation{Op: "set_safety", Value: "readonly"}},
		"a safety not in the set":   {opsFlow, Operation{Op: "set_safety", Node: "charge", Value: "side effect"}},
		"an upper-case safety":      {opsFlow, Operation{Op: "set_safety", Node: "charge", Value: "READONLY"}},
		"an empty block":            {opsFlow, Operation{Op: "set_block", Node: "charge"}},
		"a block with a line break": {opsFlow, Operation{Op: "set_block", Node: "charge", Value: "a\nb"}},
		"a gate with no value":      {opsFlow, Operation{Op: "set_approval", Node: "charge"}},
		"too many approvers":        {opsFlow, Operation{Op: "set_approval", Node: "charge", Approval: &Approval{Need: 1, Approvers: make([]string, maxApprovers+1)}}},
		"two nodes of one name":     {strings.Replace(opsFlow, "name: receipt", "name: charge", 1), Operation{Op: "set_safety", Node: "charge", Value: "readonly"}},
		"an approval that is null":  {strings.Replace(opsFlow, "    approval:\n      need: 2\n      approvers: [ops, finance, risk]\n", "    approval: null\n", 1), Operation{Op: "set_approval", Node: "charge", Approval: &Approval{Need: 1, Approvers: []string{"a"}}}},
		"an anchored node":          {strings.Replace(opsFlow, "- {name: receipt,", "- &r {name: receipt,", 1), Operation{Op: "set_safety", Node: "receipt", Value: "readonly"}},
		"a second document":         {opsFlow + "---\nversion: 1\n", Operation{Op: "set_safety", Node: "receipt", Value: "readonly"}},
		"text that does not parse":  {"nodes: [", Operation{Op: "set_safety", Node: "a", Value: "readonly"}},
		"CRLF line ends":            {strings.ReplaceAll(opsFlow, "\n", "\r\n"), Operation{Op: "set_safety", Node: "receipt", Value: "readonly"}},
		"a byte-order mark":         {"\xef\xbb\xbf" + opsFlow, Operation{Op: "set_safety", Node: "receipt", Value: "readonly"}},
	} {
		_, err := ApplyOperation("f.yaml", []byte(c.src), c.op)
		var rf *Refusal
		if !errors.As(err, &rf) {
			t.Errorf("%s: want a refusal, got %v", name, err)
		}
	}
}

// The gate the page asked for is the gate the check reads back: for every real config in the
// corpus, every node, a handful of gates (new, changed, cleared) lower to the Topology gate that
// was asked for, the rest of the config's problems are the ones it had, and the node's text changes
// and nothing else does.
func TestGatesReadBackThroughTheCheck(t *testing.T) {
	gates := []Approval{
		{Need: 1, Approvers: []string{"ops"}},
		{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true},
		{Need: 2, Approvers: []string{"ops", "finance", "risk"}, Timeout: "48h"},
		{Need: 3, Approvers: []string{"a", "b", "c", "d"}, ExcludePrincipal: true, Timeout: "90m"},
		{Need: 1, Approvers: []string{"yes", "no", "1", "null", "a,b", "[x]", "#c"}}, // ids YAML would read as another type, or split, or start a comment
	}
	tested, gateBacks := 0, 0
	for _, f := range collect(t) {
		if !f.real {
			continue
		}
		name := "x.yaml"
		if json.Valid([]byte(f.text)) {
			name = "x.json"
		}
		data := []byte(f.text)
		if name == "x.yaml" {
			var err error
			if data, err = ToJSON(data); err != nil {
				continue
			}
		}
		before := plan.InspectConfig(data, nil)
		if len(before.Topology.Nodes) == 0 {
			continue
		}
		for _, n := range before.Topology.Nodes {
			if n.Kind == plan.NodeKindJoin || n.Kind == plan.NodeKindSwitch {
				continue
			}
			for _, g := range gates {
				r, err := ApplyOperation(name, []byte(f.text), Operation{Op: "set_approval", Node: n.Name, Approval: &g})
				if err != nil {
					var rf *Refusal
					if !errors.As(err, &rf) {
						t.Errorf("%s %s: %v", f.name, n.Name, err)
					}
					continue // the engine refuses what it cannot splice; the corpus test counts those
				}
				tested++
				after := readReport(t, name, r.Text)
				var got *plan.TopologyGate
				for _, an := range after.Topology.Nodes {
					if an.Name == n.Name {
						got = an.Gate
					}
				}
				want := &plan.TopologyGate{Need: g.Need, Approvers: g.Approvers, ExcludePrincipal: g.ExcludePrincipal}
				if g.Timeout != "" {
					d, _ := time.ParseDuration(g.Timeout)
					want.TimeoutMS = d.Milliseconds()
				}
				if got == nil || !reflect.DeepEqual(*got, *want) {
					// A node whose block declares a gate in Go cannot be given another one; the
					// check says so as a problem on the node, which the structural check cannot see.
					t.Errorf("%s %s: gate %+v read back as %+v", f.name, n.Name, g, got)
				}
				if a, b := problemKeys(before, n.Name), problemKeys(after, n.Name); !slices.Equal(a, b) {
					t.Errorf("%s %s: problems elsewhere changed\n before %q\n after  %q", f.name, n.Name, a, b)
				}
				// Clearing it restores the node: the text is the one the operation started from, when
				// the node had no gate of its own to begin with.
				if n.Gate != nil && !n.Gate.Single && len(before.Problems) == 0 {
					// Put the node's own gate back: the text is the one the edit started from.
					orig := Approval{Need: n.Gate.Need, Approvers: n.Gate.Approvers, ExcludePrincipal: n.Gate.ExcludePrincipal}
					if n.Gate.TimeoutMS > 0 {
						orig.Timeout = n.Gate.TimeoutText()
					}
					back, err := ApplyOperation(name, r.Text, Operation{Op: "set_approval", Node: n.Name, Approval: &orig})
					switch {
					case err != nil:
						t.Errorf("%s %s: putting the gate back: %v", f.name, n.Name, err)
					case string(back.Text) != f.text:
						t.Errorf("%s %s: the gate put back is not byte-identical:\n%s", f.name, n.Name, diffLines(f.text, string(back.Text)))
					default:
						gateBacks++
					}
				}
				if n.Gate == nil {
					back, err := ApplyOperation(name, r.Text, Operation{Op: "clear_approval", Node: n.Name})
					if err != nil || string(back.Text) != f.text {
						t.Errorf("%s %s: set then clear does not restore the text (%v)\n%s", f.name, n.Name, err, diffLines(f.text, string(back.Text)))
					}
				}
			}
		}
	}
	if gateBacks < 20 {
		t.Errorf("only %d gates were put back: the corpus has too few gated nodes", gateBacks)
	}
	if tested < 200 {
		t.Errorf("only %d gate edits were measured: the corpus is not being read", tested)
	}
}

func readReport(t *testing.T, name string, text []byte) plan.ConfigReport {
	t.Helper()
	data := text
	if name == "x.yaml" {
		var err error
		if data, err = ToJSON(text); err != nil {
			t.Fatalf("the edited text does not parse: %v", err)
		}
	}
	return plan.InspectConfig(data, nil)
}

// problemKeys are the report's problems except those on the node that was edited, which an edit of
// its gate may rightly fix or cause, and the assertion verdicts.
func problemKeys(r plan.ConfigReport, edited string) []string {
	var out []string
	for _, p := range r.Problems {
		if p.Node == edited || strings.HasPrefix(p.Message, "assert[") {
			continue // a gate changes what assertions hold: that is the point of one, not a defect of the edit
		}
		out = append(out, p.Severity+"|"+p.Node+"|"+p.From+"|"+p.To+"|"+p.Message)
	}
	slices.Sort(out)
	return out
}

// diffLines is the first differing line of a and b, for a failure message.
func diffLines(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return "line " + string(rune('0'+i%10)) + ":\n  " + x[i] + "\n  " + y[i]
		}
	}
	return "lengths differ"
}
