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
