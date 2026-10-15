//go:build !bide_audit_only

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestYAMLToJSON: a YAML flow config becomes the JSON plan reads, nested mappings and lists
// included; a mapping key that is not a string, at any depth, is refused because JSON has no such
// key.
func TestYAMLToJSON(t *testing.T) {
	out, err := yamlToJSON([]byte("flow: refund\nnodes:\n  - {name: charge, approval: {need: 2, approvers: [ops, risk]}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Flow  string `json:"flow"`
		Nodes []struct {
			Name     string `json:"name"`
			Approval struct {
				Need      int      `json:"need"`
				Approvers []string `json:"approvers"`
			} `json:"approval"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	if got.Flow != "refund" || len(got.Nodes) != 1 || got.Nodes[0].Approval.Need != 2 || strings.Join(got.Nodes[0].Approval.Approvers, ",") != "ops,risk" {
		t.Errorf("converted to %s", out)
	}
	for name, doc := range map[string]string{
		"top level": "1: one\n",
		"nested":    "a:\n  2: two\n",
		"in a list": "a:\n  - {3: three}\n",
	} {
		if _, err := yamlToJSON([]byte(doc)); err == nil || !strings.Contains(err.Error(), "not a string") {
			t.Errorf("%s: %v, want a non-string key refused", name, err)
		}
	}
	if _, err := yamlToJSON([]byte("a: [1\n")); err == nil {
		t.Error("malformed YAML was converted")
	}
}

// TestReleaseVersion: a stamped version wins; without one the build's module version is used,
// which is empty for a build from source (go test's binary is "(devel)").
func TestReleaseVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v9.9.9"
	if got := releaseVersion(); got != "v9.9.9" {
		t.Errorf("stamped: %q", got)
	}
	version = ""
	if got := releaseVersion(); got != "" {
		t.Errorf("from source: %q, want empty", got)
	}
}

// TestYAMLToJSONOneDocument: a flow config is one document. A second one is refused, with a
// message that says so, whatever it holds, instead of being read as if it were not there (the
// JSON path refuses data after the value). A leading "---" and a closing "..." are one document.
func TestYAMLToJSONOneDocument(t *testing.T) {
	for name, doc := range map[string]string{
		"two flows":          "version: 1\nflow: a\nnodes: []\n---\nversion: 1\nflow: b\nnodes: []\n",
		"empty second":       "version: 1\nflow: a\nnodes: []\n---\n",
		"second is a scalar": "version: 1\nflow: a\nnodes: []\n---\nx\n",
		"comment then two":   "# c\n---\nflow: a\n---\nflow: b\n",
	} {
		if _, err := yamlToJSON([]byte(doc)); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
			t.Errorf("%s: %v, want a refusal naming the second document", name, err)
		}
	}
	if _, err := yamlToJSON([]byte("version: 1\nflow: a\n---\nflow: [b\n")); err == nil {
		t.Error("a malformed second document was accepted")
	}
	for name, doc := range map[string]string{
		"plain":           "version: 1\nflow: a\nnodes: []\n",
		"start and end":   "---\nversion: 1\nflow: a\nnodes: []\n...\n",
		"comments around": "# head\n---\nversion: 1\nflow: a\nnodes: []\n# tail\n",
	} {
		out, err := yamlToJSON([]byte(doc))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(string(out), `"flow":"a"`) {
			t.Errorf("%s: converted to %s", name, out)
		}
	}
}
