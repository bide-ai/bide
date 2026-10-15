package flowedit

import (
	"strings"
	"testing"
)

const sample = `# yaml-language-server: $schema=https://example/x.json
version: 1
flow: refund   # the label

# nodes, in digest order
entry: validate
nodes:
  - {name: validate, block: validate, safety: readonly}

  # the gated one
  - name: charge       # gated
    block: charge
    approval: {need: 2, approvers: [ops, finance, risk]}
  - {name: skip, block: skip}
wiring:
  - switch: validate
    when: [{pred: large, to: charge}]
    else: skip
assert:
  - every path to charge passes approval >= 2 of 3
`

func apply(t *testing.T, src string, op Op) string {
	t.Helper()
	r, err := Apply([]byte(src), op)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return string(r.Text)
}

func TestApplyExamples(t *testing.T) {
	cases := []struct {
		name string
		op   Op
		want string
	}{
		{"set scalar keeps the comment", Op{Kind: Set, Path: []any{"flow"}, Value: "order"},
			"flow: order   # the label"},
		{"set a number in a flow map", Op{Kind: Set, Path: []any{"nodes", 1, "approval", "need"}, Value: 3},
			"approval: {need: 3, approvers: [ops, finance, risk]}"},
		{"add an approver in a flow list", Op{Kind: Insert, Path: []any{"nodes", 1, "approval", "approvers"}, Index: 3, Value: "legal"},
			"[ops, finance, risk, legal]}"},
		{"add a key to a block mapping", Op{Kind: Insert, Path: []any{"nodes", 1}, Key: "safety", Value: "idempotent"},
			"    safety: idempotent\n"},
		{"add a key to a flow mapping", Op{Kind: Insert, Path: []any{"nodes", 0}, Key: "approval", Value: Map{{"need", 1}, {"approvers", []any{"ops"}}}},
			"safety: readonly, approval: {need: 1, approvers: [ops]}}"},
		{"add a node in the style of its siblings", Op{Kind: Insert, Path: []any{"nodes"}, Index: 3, Value: Map{{"name", "refund"}, {"block", "refund"}}},
			"  - {name: refund, block: refund}\n"},
		{"add an assertion", Op{Kind: Insert, Path: []any{"assert"}, Index: 1, Value: "credit reachable"},
			"  - credit reachable\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := apply(t, sample, c.op)
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q in:\n%s", c.want, out)
			}
		})
	}
}

