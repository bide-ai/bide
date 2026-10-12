//go:build !bide_audit_only

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/runview"
)

const flowYAML = `version: 1
flow: refund
nodes:
  - {name: validate, block: validate}
  - {name: charge, block: charge, approval: {need: 2, approvers: [ops, finance, risk]}}
  - {name: done, block: done}
wiring:
  - switch: validate
    when: [{pred: large, to: charge}]
    else: done
  - edge: [charge, receipt]
`

// TestCheckYAML: -check reads a YAML config, prints each problem as file:where: message, and exits
// 1 when one is an error; a clean config exits 0; a usage error exits 2.
func TestCheckYAML(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "refund.yaml")
	if err := os.WriteFile(bad, []byte(flowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runview.Main([]string{"-config", bad, "-check"}, &out, &errb, stores); code != 1 {
		t.Fatalf("-check of a config with errors = %d, want 1 (stderr %s)", code, errb.String())
	}
	for _, want := range []string{
		bad + `:charge->receipt: wiring[1] edge "charge" -> "receipt": "receipt" is not a declared node`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("-check output lacks %q:\n%s", want, out.String())
		}
	}
	good := filepath.Join(dir, "ok.yaml")
	// A valid approval gate is no problem: plan flows enforce it.
	clean := strings.Replace(flowYAML, "[charge, receipt]", "[charge, done]", 1)
	if err := os.WriteFile(good, []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := runview.Main([]string{"-config", good, "-check"}, &out, &errb, stores); code != 0 || out.Len() != 0 {
		t.Fatalf("-check of a clean config = %d, output %q", code, out.String())
	}
	// A switch with no else is a warning: lenient -check passes it, -strict fails it.
	warn := filepath.Join(dir, "warn.yaml")
	if err := os.WriteFile(warn, []byte(strings.Replace(clean, "    else: done\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := runview.Main([]string{"-config", warn, "-check"}, &out, &errb, stores); code != 0 || !strings.Contains(out.String(), "warning: switch over") {
		t.Fatalf("-check of a config with a warning = %d, output %q; want 0 and the warning", code, out.String())
	}
	if code := runview.Main([]string{"-config", warn, "-check", "-strict"}, &out, &errb, stores); code != 1 {
		t.Fatalf("-check -strict of a config with a warning = %d, want 1", code)
	}
	if code := runview.Main([]string{"-config", warn, "-strict"}, &out, &errb, stores); code != 2 {
		t.Fatalf("-strict without -check = %d, want 2", code)
	}
	if code := runview.Main([]string{"-config", good, "-db", "x"}, &out, &errb, stores); code != 2 {
		t.Fatalf("-config with a run flag = %d, want 2", code)
	}
	if code := runview.Main([]string{"-check"}, &out, &errb, stores); code != 2 {
		t.Fatalf("-check without -config = %d, want 2", code)
	}
	if code := runview.Main([]string{"-config", filepath.Join(dir, "missing.yaml"), "-check"}, &out, &errb, stores); code != 1 {
		t.Fatalf("-check of a missing file = %d, want 1", code)
	}
	if _, err := yamlToJSON([]byte("a: 1\na: 2\n")); err == nil {
		t.Fatal("a YAML config with a duplicate key was read")
	}
}
