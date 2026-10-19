//go:build !bide_audit_only

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/cmd/bide/internal/samplecmd"
)

// sampleDir writes the sample bide sample makes into a temporary directory.
func sampleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, _, err := samplecmd.WriteFiles(dir, false); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSampleCommands: the sample works out of the box with the commands its README lists: the
// evidence verifies offline, a flow config with a violated assertion fails bide check, and
// a diff that lowers a gate fails -fail-on-weaken.
func TestSampleCommands(t *testing.T) {
	dir := sampleDir(t)
	in := func(name string) string { return filepath.Join(dir, name) }
	for _, tc := range []struct {
		args []string
		want int
		out  string // in stdout+stderr
	}{
		{[]string{"audit", "verify-evidence", "-evidence", in("refund-1041.evidence.json"), "-pubkey", in("log.pub")}, 0, `run "refund-1041" evidence verified`},
		{[]string{"audit", "verify-evidence", "-evidence", in("refund-1049.evidence.json"), "-pubkey", in("log.pub")}, 0, `run "refund-1049" evidence verified`},
		{[]string{"check", in("flows/refund-v1.yaml")}, 0, "4 hold, 0 violated"},
		{[]string{"check", in("flows/refund-v2.yaml")}, 0, "3 hold, 0 violated"},
		{[]string{"check", in("flows/refund-bypass.yaml")}, 1, "counterexample: validate -> lookup -> charge_vip"},
		{[]string{"check", in("flows/refund-v2.yaml"), "-diff", in("flows/refund-v1.yaml"), "-check"}, 0, "approval policy weakened"},
		{[]string{"check", in("flows/refund-v2.yaml"), "-diff", in("flows/refund-v1.yaml"), "-check", "-fail-on-weaken"}, 1, "review: approval 2 of 3 -> 1 of 3"},
		{[]string{"check", in("flows/refund-v1.yaml"), "-diff", in("flows/refund-v2.yaml"), "-check", "-fail-on-weaken"}, 0, "strengthened"},
	} {
		code, stdout, stderr := runBide(tc.args...)
		if code != tc.want || !strings.Contains(stdout+stderr, tc.out) {
			t.Errorf("bide %s: exit %d, want %d, and output containing %q:\n%s%s", strings.Join(tc.args, " "), code, tc.want, tc.out, stdout, stderr)
		}
	}
}

// TestSampleEvidenceDetectsEdit: the sample's evidence is checked, not just read: a changed byte
// fails bide audit verify-evidence.
func TestSampleEvidenceDetectsEdit(t *testing.T) {
	dir := sampleDir(t)
	path := filepath.Join(dir, "refund-1041.evidence.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), `"run_id": "refund-1041"`, `"run_id": "refund-1042"`, 1)
	if edited == string(b) {
		t.Fatal("found nothing to edit in the evidence")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := runBide("audit", "verify-evidence", "-evidence", path, "-pubkey", filepath.Join(dir, "log.pub")); code == 0 {
		t.Fatalf("edited evidence verified:\n%s%s", out, errs)
	}
}
