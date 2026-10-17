package approvecmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
)

// itemOf returns the one waiting call of -list -json as JSON: the object a user saves for -item.
func itemOf(t *testing.T, f *fixture) []byte {
	t.Helper()
	code, js, errs := f.run_("", "-list", "-json")
	if code != 0 {
		t.Fatalf("-list -json = %d: %s", code, errs)
	}
	var got []json.RawMessage
	if err := json.Unmarshal([]byte(js), &got); err != nil || len(got) != 1 {
		t.Fatalf("%v: %s", err, js)
	}
	return got[0]
}

// TestItemDecidesTheCall: -item takes run and call exactly from one call of -list -json, from a file,
// from an array of one, and from standard input (with -yes), for an id ending in a line break and
// one with a bidirectional override, which a shell's $(...) or a pasted command line cannot carry.
func TestItemDecidesTheCall(t *testing.T) {
	for _, ids := range []struct{ name, run, call string }{
		{"trailing newline", "refund-1049", "c2\n"},
		{"bidirectional override", "refund‮1049", "c2"},
		{"both", "r\n‮", "c‮2\n"},
	} {
		t.Run(ids.name, func(t *testing.T) {
			f := newFixtureIDs(t, twoOfThree, refundArgs, audit.AlgEd25519, "", ids.run, ids.call)
			item := itemOf(t, f)
			decide := func(approver string, stdin string, extra ...string) {
				t.Helper()
				args := append([]string{"-as", approver, "-key", f.keys[approver], "-approver-keys", f.keysJSON}, extra...)
				if code, out, errs := f.run_(stdin, args...); code != 0 {
					t.Fatalf("%s: %d\n%s\n%s", approver, code, out, errs)
				}
			}

			path := filepath.Join(t.TempDir(), "item.json")
			if err := os.WriteFile(path, item, 0o600); err != nil {
				t.Fatal(err)
			}
			decide("ops", "ops\n", "-item", path) // a file leaves standard input for the confirmation
			if n := len(f.decisions()); n != 1 {
				t.Fatalf("%d decisions after a file item, want 1", n)
			}
			decide("finance", "["+string(item)+"]", "-item", "-", "-yes") // an array of one, on standard input
			if n := len(f.decisions()); n != 2 {
				t.Fatalf("%d decisions after a piped item, want 2", n)
			}
		})
	}
}

// TestItemRefusals: an ambiguous or unreadable -item records nothing and says why.
func TestItemRefusals(t *testing.T) {
	f := newFixtureIDs(t, twoOfThree, refundArgs, audit.AlgEd25519, "", "refund-1049", "c2\n")
	item := string(itemOf(t, f))
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.json", item)
	sign := []string{"-as", "ops", "-key", f.keys["ops"], "-approver-keys", f.keysJSON, "-yes"}
	for _, c := range []struct {
		name  string
		code  int
		want  string
		stdin string
		args  []string
	}{
		{"with -run", 2, "do not also pass -run or -call", "", []string{"-item", good, "-run", "refund-1049"}},
		{"with -call", 2, "do not also pass -run or -call", "", []string{"-item", good, "-call", "c2\n"}},
		{"with -list", 2, "does not go with -list", "", []string{"-item", good, "-list"}},
		{"stdin without -yes", 2, "give -yes", item, []string{"-item", "-", "-as", "ops", "-key", f.keys["ops"], "-approver-keys", f.keysJSON}},
		{"missing file", 1, "-item", "", []string{"-item", filepath.Join(dir, "none.json")}},
		{"two calls", 1, "2 calls; name exactly one", "", []string{"-item", write("two.json", "["+item+","+item+"]")}},
		{"no call", 1, `no "run" or no "call"`, "", []string{"-item", write("nocall.json", `{"run":"refund-1049"}`)}},
		{"repeated name", 1, "-item", "", []string{"-item", write("dup.json", `{"run":"refund-1049","call":"x","call":"c2\n"}`)}},
		{"trailing data", 1, "-item", "", []string{"-item", write("trail.json", item+` {}`)}},
		{"not JSON", 1, "-item", "", []string{"-item", write("junk.json", `refund-1049 c2`)}},
		{"too large", 1, "more than", "", []string{"-item", write("big.json", `{"run":"`+strings.Repeat("a", maxItem)+`"}`)}},
		{"another call", 1, "has no approval gate on call", "", []string{"-item", write("other.json", `{"run":"refund-1049","call":"c9"}`)}},
	} {
		before := len(f.decisions())
		args := append(c.args, sign...)
		if c.name == "with -list" || c.name == "stdin without -yes" {
			args = c.args // these carry their own flags
		}
		code, _, errs := f.run_(c.stdin, args...)
		if code != c.code || !strings.Contains(errs, c.want) {
			t.Errorf("%s: code %d (want %d), stderr %q (want %q)", c.name, code, c.code, errs, c.want)
		}
		if after := len(f.decisions()); after != before {
			t.Errorf("%s: recorded %d decisions", c.name, after-before)
		}
	}
}
