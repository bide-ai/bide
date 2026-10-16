package runview

import (
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
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

// TestAttentionCertCommand: a gate with a certificate-bound approver shows the bide approve -cert
// command. When every approver is certificate-bound it is the only command (a bare-key decision does not
// count for them); when the gate has bare-key approvers too, both are shown. A gate of bare keys shows no
// -cert command, and without the hook none is shown.
func TestAttentionCertCommand(t *testing.T) {
	j := approveline.Journal{SQLite: "/var/lib/j.db"}
	bare := func(run, call string, single bool) (string, error) {
		return approveline.Command(j, run, call, "", "", "", single)
	}
	cert := func(run, call string) (string, error) { return approveline.CertCommand(j, run, call, "") }
	item := func(recs []agent.Record, hook func(string, string) (string, error)) AttentionItem {
		t.Helper()
		l := Options{Source: Exports{"r1": recs}, ApproveCommand: bare, ApproveCertCommand: hook, Now: func() time.Time { return attnNow }}.attention(t.Context())
		items := itemsByKind(l.Items, AttentionApproval)
		if len(items) != 1 {
			t.Fatalf("%d approval items", len(items))
		}
		return items[0]
	}
	allBound := identRecs(t, agent.TierLogclockFinal, false, nil).recs
	it := item(allBound, cert)
	if it.Command != "" || !strings.Contains(it.CertCommand, " -cert ") || !strings.Contains(it.CertCommand, "-identity-policy") || strings.Contains(it.CertCommand, "-approver-keys") {
		t.Errorf("all approvers bound: command %q, cert command %q", it.Command, it.CertCommand)
	}
	mixed := identRecs(t, agent.TierLogclockFinal, false, func(_ *agent.ApprovalIdentity, w *agent.ApprovalWait) {
		w.Need, w.Approvers = 2, []string{"alice", "bob"}
	}).recs
	it = item(mixed, cert)
	if !strings.Contains(it.Command, "-approver-keys") || strings.Contains(it.Command, " -cert ") || !strings.Contains(it.CertCommand, " -cert ") {
		t.Errorf("mixed gate: command %q, cert command %q", it.Command, it.CertCommand)
	}
	if it = item(allBound, nil); it.CertCommand != "" || it.Command != "" {
		t.Errorf("without the hook: command %q, cert command %q", it.Command, it.CertCommand)
	}
	if it = item(sampleRecs(t, "refund-1043"), cert); it.CertCommand != "" || it.Command == "" {
		t.Errorf("a bare-key gate: command %q, cert command %q", it.Command, it.CertCommand)
	}
	// The page shows the cert command with its own label, and the real viewer writes it for the journal.
	o := Options{Source: Exports{"r1": allBound}, ApproveCommand: bare, ApproveCertCommand: cert, Now: func() time.Time { return attnNow }}
	if _, page := get(t, Handler(o), "/attention"); !strings.Contains(page, "certificate-bound approver") || !strings.Contains(page, "-cert &#39;&lt;chain.pem&gt;&#39;") {
		t.Errorf("the page does not show the -cert command:\n%s", page)
	}
	// An unsafe id gets no command and the reason.
	bad := Options{Source: Exports{"bad\nid": allBound}, ApproveCommand: bare, ApproveCertCommand: cert, Now: func() time.Time { return attnNow }}.attention(t.Context())
	if items := itemsByKind(bad.Items, AttentionApproval); len(items) != 1 || items[0].CertCommand != "" || items[0].NoCommand != "no command (unsafe id)" {
		t.Errorf("unsafe id: %+v", items)
	}
}
