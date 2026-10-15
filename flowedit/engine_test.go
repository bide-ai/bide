package flowedit

import (
	"errors"
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

// A comment directly above an entry goes with it; a comment after a blank line stays.
func TestDeleteTakesAttachedComments(t *testing.T) {
	out := apply(t, sample, Op{Kind: Delete, Path: []any{"nodes", 1}})
	if strings.Contains(out, "the gated one") || strings.Contains(out, "# gated") || strings.Contains(out, "name: charge") {
		t.Errorf("the entry or its comments are still there:\n%s", out)
	}
	if !strings.Contains(out, "# nodes, in digest order") || !strings.Contains(out, "# yaml-language-server") {
		t.Errorf("a comment that belongs to the file was removed:\n%s", out)
	}
}

// Blank lines, comments and the spacing before a trailing comment are the original bytes.
func TestUntouchedBytesStay(t *testing.T) {
	r, err := Apply([]byte(sample), Op{Kind: Set, Path: []any{"nodes", 2, "block"}, Value: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(r.Text[:r.Lo]) + string(r.Text[r.Lo+len(r.Frag):]); got != sample[:r.Lo]+sample[r.Hi:] {
		t.Errorf("bytes outside the edit changed")
	}
	if !strings.Contains(string(r.Text), "flow: refund   # the label\n\n# nodes") {
		t.Errorf("spacing or a blank line changed:\n%s", r.Text)
	}
}

// What the engine will not splice, it refuses, and the text is the caller's still.
func TestRefusals(t *testing.T) {
	for name, src := range map[string]string{
		"anchor":       "version: 1\nflow: &f refund\nnodes: []\n",
		"alias":        "a: &x [1]\nversion: 1\nnodes: *x\n",
		"merge key":    "base: &b {a: 1}\nnodes:\n  - <<: *b\n",
		"tag":          "version: 1\nflow: !!str refund\nnodes: []\n",
		"two docs":     "version: 1\nnodes: []\n---\nversion: 1\n",
		"crlf":         "version: 1\r\nflow: a\r\nnodes: []\r\n",
		"bom":          "\xef\xbb\xbfversion: 1\nflow: a\nnodes: []\n",
		"block scalar": "version: 1\nflow: |\n  a\nnodes: []\n",
	} {
		_, err := Apply([]byte(src), Op{Kind: Set, Path: []any{"flow"}, Value: "x"})
		var rf *Refusal
		if !errors.As(err, &rf) {
			t.Errorf("%s: want a refusal, got %v", name, err)
		}
	}
}

const tabJSON = "{\n\t\"version\": 1,\n\t\"flow\": \"f\",\n\t\"nodes\": [\n\t\t{\"name\": \"a\", \"block\": \"a\"},\n\t\t{\"name\": \"b\", \"block\": \"b\"}\n\t]\n}\n"

// A JSON file indented with tabs is edited like any other: a new entry gets a line of its own, with
// the same tabs, and is written as JSON.
func TestTabIndentedJSON(t *testing.T) {
	out := apply(t, tabJSON, Op{Kind: Insert, Path: []any{"nodes"}, Index: 2, Value: Map{{"name", "c"}, {"block", "c"}}})
	want := "\t\t{\"name\": \"b\", \"block\": \"b\"},\n\t\t{\"name\": \"c\", \"block\": \"c\"}\n\t]"
	if !strings.Contains(out, want) {
		t.Errorf("appended entry:\n%s", out)
	}
	out = apply(t, tabJSON, Op{Kind: Insert, Path: []any{"nodes"}, Index: 0, Value: Map{{"name", "c"}, {"block", "c"}}})
	if !strings.Contains(out, "\t\"nodes\": [\n\t\t{\"name\": \"c\", \"block\": \"c\"},\n\t\t{\"name\": \"a\"") {
		t.Errorf("entry at the front:\n%s", out)
	}
	out = apply(t, tabJSON, Op{Kind: Insert, Path: nil, Key: "entry", Value: "a"})
	if !strings.Contains(out, "\t]\n,\n\t\"entry\": \"a\"\n}") && !strings.Contains(out, "\t],\n\t\"entry\": \"a\"\n}") {
		t.Errorf("new key:\n%s", out)
	}
	out = apply(t, tabJSON, Op{Kind: Delete, Path: []any{"nodes", 1}})
	if strings.Contains(out, `"b"`) || !strings.Contains(out, "\t\t{\"name\": \"a\", \"block\": \"a\"}\n\t]") {
		t.Errorf("deleted entry:\n%s", out)
	}
	out = apply(t, tabJSON, Op{Kind: Set, Path: []any{"flow"}, Value: "g"})
	if !strings.Contains(out, "\t\"flow\": \"g\",\n") {
		t.Errorf("set:\n%s", out)
	}
}
