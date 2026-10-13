package auditcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The mistakes a first verification meets say what to do next, and keep their exit status (4: an
// input is unreadable or unusable).
func TestVerifyEvidenceErrorsSayWhatToDoNext(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "evidence.json")
	notJSON := filepath.Join(dir, "run.db")
	if err := os.WriteFile(notJSON, []byte("SQLite format 3\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing evidence", []string{"verify-evidence", "-evidence", missing, "-pubkey", key}, "check the path"},
		{"journal database as evidence", []string{"verify-evidence", "-evidence", notJSON, "-pubkey", key}, "not the journal database"},
		{"bad public key", []string{"verify-evidence", "-evidence", notJSON, "-pubkey", "ab"}, "keys/log.pub"},
	} {
		code, out, errOut := runCLI(tc.args...)
		if code != 4 || !strings.Contains(out+errOut, tc.want) {
			t.Errorf("%s: exit %d, want 4 mentioning %q\n%s%s", tc.name, code, tc.want, out, errOut)
		}
	}
}
