package runview

import (
	"encoding/json"
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

// TestAttentionCertCommand: a gate with a pending certificate-bound approver shows the bide approve -cert
// command. When every pending approver is certificate-bound it is the only command (a bare-key decision
// does not count for them); when a bare-key approver is pending too, both are shown. A gate of bare keys
// shows no -cert command, and without the hook none is shown.
func TestAttentionCertCommand(t *testing.T) {
	j := approveline.Journal{SQLite: "/var/lib/j.db"}
	bare := func(run, call string, single bool) (string, error) {
		return approveline.Command(j, run, call, "", "", "", single)
	}
	cert := func(run, call string) (string, error) { return approveline.CertCommand(j, run, call, "") }
	recsOf := func(approvers, bound []string) []agent.Record {
		w, _ := json.Marshal(agent.ApprovalWait{ToolUseID: "c1", ToolName: "refund", Need: 2, Approvers: approvers, IdentityBound: len(bound) > 0, IdentityApprovers: bound, IdentityPolicyDigest: strings.Repeat("a", 64)})
		return []agent.Record{
			{Name: "@llm/0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "refund", Args: json.RawMessage(`{}`)}}}},
			{Name: agent.ApprovalPendingStep("c1"), Kind: agent.StepValue, Result: w},
		}
	}
	opts := func(id string, recs []agent.Record, hook func(string, string) (string, error)) Options {
		return Options{Source: Exports{id: recs}, ApproveCommand: bare, ApproveCertCommand: hook, Now: func() time.Time { return attnNow }}
	}
	item := func(id string, recs []agent.Record, hook func(string, string) (string, error)) AttentionItem {
		t.Helper()
		items := itemsByKind(opts(id, recs, hook).attention(t.Context()).Items, AttentionApproval)
		if len(items) != 1 {
			t.Fatalf("%d approval items", len(items))
		}
		return items[0]
	}
	allBound := recsOf([]string{"ops", "finance"}, []string{"ops", "finance"})
	it := item("r1", allBound, cert)
	if it.Command != "" || !it.CertOnly || !strings.Contains(it.CertCommand, " -cert ") || !strings.Contains(it.CertCommand, "-identity-policy") || strings.Contains(it.CertCommand, "-approver-keys") {
		t.Errorf("all pending approvers bound: %+v", it)
	}
	it = item("r1", recsOf([]string{"ops", "risk"}, []string{"ops"}), cert)
	if !strings.Contains(it.Command, "-approver-keys") || strings.Contains(it.Command, " -cert ") || !strings.Contains(it.CertCommand, " -cert ") || it.CertOnly {
		t.Errorf("mixed gate: %+v", it)
	}
	if it = item("r1", allBound, nil); it.CertCommand != "" {
		t.Errorf("without the hook: %+v", it)
	}
	if it = item("refund-1043", sampleRecs(t, "refund-1043"), cert); it.CertCommand != "" || it.Command == "" {
		t.Errorf("a bare-key gate: %+v", it)
	}
	// The page shows the cert command with its own label.
	if _, page := get(t, Handler(opts("r1", allBound, cert)), "/attention"); !strings.Contains(page, "certificate-bound approver") || !strings.Contains(page, "-cert &#39;&lt;chain.pem&gt;&#39;") {
		t.Errorf("the page does not show the -cert command:\n%s", page)
	}
	// An unsafe id gets no command, and the reason.
	for _, recs := range [][]agent.Record{allBound, recsOf([]string{"ops", "risk"}, []string{"ops"})} {
		it = item("bad\nid", recs, cert)
		if it.CertCommand != "" || it.NoCommand != "no command (unsafe id)" {
			t.Errorf("unsafe id: %+v", it)
		}
	}
}
