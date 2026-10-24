package auditcli

import (
	"bytes"
	"strings"
	"testing"
)

// A verb run without its required flags prints one line that names the missing ones and says
// where the verb's usage is, and exits 2: not the usage of every verb, which buried the answer.
func TestMissingRequiredFlagsIsOneLine(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range []struct {
		verb    string
		args    []string
		missing string
	}{
		{"prove", nil, "-journal, -sth"},
		{"verify", nil, "-bundle, -pubkey"},
		{"verify-governance", nil, "-policy"},
		{"verify-governed-action", nil, "-action, -policy-bundle, -pubkey"},
		{"verify-convergence", nil, "-cert-bundle, -policy-bundle, -pubkey"},
		{"verify-quorum", nil, "-name, -tally, -vote, -pubkey, -k"},
		{"verify-run", nil, "-cert, -pubkey, -approved or -approved-file"},
		{"verify-evidence", nil, "-evidence, -pubkey"},
		{"verify-approvals", []string{"-evidence", "e.json", "-pubkey", "k", "-approver-keys", "a.json"}, "-call, -need, -approvers"},
		{"notifications", nil, "-trail and -journal, or -db, or -postgres-env"},
		{"redactions", nil, "-trail, or -db, or -postgres-env"},
		{"caps", nil, "-db or -postgres-env"},
		{"stops", nil, "-trail, or -db, or -postgres-env"},
		{"verify-receipts", nil, "-receipt, -journal, -heads, -pubkey, -approver-keys or -identity-policy"},
		{"verify-decision-coverage", nil, "-journal, -pubkey, -heads"},
		{"prove-absent", nil, "-journal, -sth, -key"},
		{"verify-absent", nil, "-bundle, -pubkey"},
	} {
		seen[tc.verb] = true
		var out, errb bytes.Buffer
		code := Main("", append([]string{tc.verb}, tc.args...), &out, &errb)
		got := errb.String()
		want := "bide audit " + tc.verb + ": missing " + tc.missing + "; run `bide audit " + tc.verb + " -h` for usage"
		if code != 2 || strings.Count(got, "\n") != 1 || !strings.Contains(got, want) {
			t.Errorf("%s: exit %d, stderr (%d lines):\n%s\nwant one line containing: %s", tc.verb, code, strings.Count(got, "\n"), got, want)
		}
	}
	for _, v := range Verbs() {
		if !seen[v] {
			t.Errorf("verb %s is not covered by this test: add its required flags", v)
		}
	}
}
