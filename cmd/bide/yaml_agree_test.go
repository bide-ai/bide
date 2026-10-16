//go:build !bide_audit_only

package main

import (
	"bytes"
	"testing"

	"github.com/bide-ai/bide/flowedit"
)

// bide check and bide view -config convert YAML with yamlToJSON, and bide edit with flowedit.ToJSON,
// which the editor's check needs inside the flowedit module (the root module takes no dependency).
// The two must read every document the same way, or the editor would show a verdict bide check does
// not give: the same JSON, or an error from both.
func TestYAMLConvertersAgree(t *testing.T) {
	for name, src := range map[string]string{
		"a flow":              "version: 1\nflow: f\nnodes:\n  - {name: a, block: a}\nwiring: []\n",
		"block style":         "version: 1\nnodes:\n  - name: a\n    block: a\n    approval:\n      need: 1\n      approvers: [x]\nwiring: []\n",
		"an empty file":       "",
		"only a comment":      "# nothing\n",
		"two documents":       "version: 1\n---\nversion: 2\n",
		"a trailing document": "version: 1\n--- \n",
		"a duplicate key":     "version: 1\nversion: 2\n",
		"a non-string key":    "? [a]\n: 1\n",
		"an integer key":      "1: a\n",
		"an anchor and alias": "a: &x [1]\nb: *x\n",
		"a merge key":         "base: &b {a: 1}\nc:\n  <<: *b\n",
		"a tag":               "a: !!str 1\n",
		"not YAML":            "nodes: [",
		"a tab":               "a:\n\t- b\n",
		"JSON":                `{"version": 1, "nodes": []}`,
		"tab-indented JSON":   "{\n\t\"version\": 1\n}\n",
		"unicode":             "name: \"überweisung\"\n",
		"a scalar document":   "just a string\n",
	} {
		a, errA := yamlToJSON([]byte(src))
		b, errB := flowedit.ToJSON([]byte(src))
		if (errA == nil) != (errB == nil) {
			t.Errorf("%s: yamlToJSON error %v, flowedit.ToJSON error %v", name, errA, errB)
			continue
		}
		if errA == nil && !bytes.Equal(a, b) {
			t.Errorf("%s: yamlToJSON %s, flowedit.ToJSON %s", name, a, b)
		}
	}
}
