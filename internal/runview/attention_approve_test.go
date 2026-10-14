package runview

import (
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/approveline"
)

// TestAttentionApproveCommandQuoting: with the real command writer, a run ID with a quote, a
// semicolon, a command substitution, parentheses or a space is one shell word in the command, and an
// ID with a control or bidirectional character gets no command, and says why.
func TestAttentionApproveCommandQuoting(t *testing.T) {
	recs := sampleRecs(t, "refund-1043")
	j := approveline.Journal{SQLite: "/var/lib/j.db"}
	hook := func(run, call string, single bool) (string, error) {
		return approveline.Command(j, run, call, "", "", "", single)
	}
	attn := func(id string) AttentionList {
		return Options{Source: Exports{id: recs}, ApproveCommand: hook, Now: func() time.Time { return attnNow }}.attention(t.Context())
	}
	safe := map[string]string{
		"it's":              `'it'\''s'`,
		"a;b":               `'a;b'`,
		"$(touch x)":        `'$(touch x)'`,
		"run ( 1 )":         `'run ( 1 )'`,
		"a b":               `'a b'`,
		"x'; $(rm -rf ~) '": `'x'\''; $(rm -rf ~) '\'''`,
	}
	for id, quoted := range safe {
		items := itemsByKind(attn(id).Items, AttentionApproval)
		if len(items) != 1 {
			t.Fatalf("%q: %d approval items", id, len(items))
		}
		if !strings.Contains(items[0].Command, "-run "+quoted+" ") || strings.Contains(items[0].Command, "\n") {
			t.Errorf("%q: command %q lacks the run as one word %s", id, items[0].Command, quoted)
		}
	}
	for _, id := range []string{"bad\nid", "bad\rid", "bad‮id"} {
		items := itemsByKind(attn(id).Items, AttentionApproval)
		if len(items) != 1 || items[0].Command != "" || items[0].NoCommand != "no command (unsafe id)" {
			t.Errorf("%q: items %+v; want no command and the unsafe-id reason", id, items)
			continue
		}
		o := Options{Source: Exports{id: recs}, ApproveCommand: hook, Now: func() time.Time { return attnNow }}
		if _, page := get(t, Handler(o), "/attention"); !strings.Contains(page, "no command (unsafe id)") || strings.Contains(page, "bide approve -db") {
			t.Errorf("%q: the page does not show the fallback instead of a command", id)
		}
	}
}
