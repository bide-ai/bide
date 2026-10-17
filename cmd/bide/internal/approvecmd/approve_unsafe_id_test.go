package approvecmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
)

// TestUnsafeIDDecidedFromListJSON: a run or call id with a bidirectional override or a line break
// gets no command from -list, and the text says what to do instead; the ids -list -json gives,
// decoded exactly as a tool such as jq prints them, are accepted by -run and -call, and the decision
// is recorded on that call and counts.
func TestUnsafeIDDecidedFromListJSON(t *testing.T) {
	const run, call = "refund‮1049", "c\n2"
	f := newFixtureIDs(t, twoOfThree, refundArgs, audit.AlgEd25519, "", run, call)

	code, out, errs := f.run_("", "-list")
	if code != 0 {
		t.Fatalf("-list = %d: %s", code, errs)
	}
	contains(t, "-list", out, "no command: ", "bide approve -list -json", "-item FILE")
	if strings.Contains(out, "bide approve -db") || strings.ContainsAny(out, "‮") {
		t.Errorf("-list wrote a command or an unescaped id:\n%s", out)
	}

	_, js, _ := f.run_("", "-list", "-json")
	var got []waiting
	if err := json.Unmarshal([]byte(js), &got); err != nil || len(got) != 1 {
		t.Fatalf("%v: %s", err, js)
	}
	if got[0].Run != run || got[0].Call != call {
		t.Fatalf("-list -json ids = %q, %q; want %q, %q", got[0].Run, got[0].Call, run, call)
	}

	// Decide with the ids as the JSON gave them.
	args := []string{"-run", got[0].Run, "-call", got[0].Call, "-as", "ops", "-key", f.keys["ops"], "-approver-keys", f.keysJSON, "-yes"}
	if code, out, errs := f.run_("", args...); code != 0 {
		t.Fatalf("deciding with the ids from -list -json = %d\n%s\n%s", code, out, errs)
	}
	if n := len(f.decisions()); n != 1 {
		t.Fatalf("%d decisions recorded, want 1", n)
	}
}
