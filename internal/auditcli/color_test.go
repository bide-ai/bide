package auditcli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
	"github.com/bide-ai/bide/internal/term"
)

// TestVerifyPlain: written to a buffer (not a terminal), and with -color=never, the report is the
// plain text it always was: no escape, no mark, no banner.
func TestVerifyPlain(t *testing.T) {
	f := newExitFixture(t)
	for _, args := range [][]string{
		{"verify", "-bundle", f.bundle, "-pubkey", f.pub},
		{"verify", "-color=never", "-bundle", f.bundle, "-pubkey", f.pub},
		{"-color=never", "verify", "-bundle", f.bundle, "-pubkey", f.pub},
	} {
		code, stdout, stderr := runCLI(args...)
		if code != 0 || stdout != `OK: run "run1" record verified in a signed tree of size `+treeSize(t, f.bundle)+"\n" || stderr != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	code, stdout, _ := runCLI("verify", "-bundle", f.bundleTampered, "-pubkey", f.pub)
	if code != 1 || stdout != "FAIL: proof did not verify under this key\n" {
		t.Errorf("tampered: exit %d, stdout %q", code, stdout)
	}
}

// TestVerifyColor: with -color=always (on the verb or before it) the verdict line gains a green
// check, the tree's size is dimmed, and the report ends with a VERIFIED banner carrying the tree's
// size and Merkle root, dimmed; a bundle that does not verify ends with a red FAILED, an unusable
// input with FAILED and why. Under -json nothing is colored.
func TestVerifyColor(t *testing.T) {
	f := newExitFixture(t)
	var b audit.ProofBundle
	readFixture(t, f.bundle, &b)
	want := "\x1b[32m✓\x1b[0m \x1b[1;32mOK:\x1b[0m run \"run1\" record verified in a \x1b[2msigned tree of size " + treeSize(t, f.bundle) + "\x1b[0m\n" +
		"\n\x1b[32m✓\x1b[0m \x1b[1;32mVERIFIED\x1b[0m  \x1b[2mtree size " + treeSize(t, f.bundle) + " · merkle root " + hex.EncodeToString(b.STH.Root) + " · exit 0\x1b[0m\n"
	for _, args := range [][]string{
		{"verify", "-color=always", "-bundle", f.bundle, "-pubkey", f.pub},
		{"-color=always", "verify", "-bundle", f.bundle, "-pubkey", f.pub},
	} {
		if code, stdout, _ := runCLI(args...); code != 0 || stdout != want {
			t.Errorf("%q: exit %d, stdout\n%q\nwant\n%q", args, code, stdout, want)
		}
	}
	code, stdout, stderr := runCLI("verify", "-color=always", "-bundle", f.bundleTampered, "-pubkey", f.pub)
	if code != 1 || stdout != "\x1b[31m✗\x1b[0m \x1b[1;31mFAIL:\x1b[0m proof did not verify under this key\n\n\x1b[31m✗\x1b[0m \x1b[1;31mFAILED\x1b[0m  \x1b[2mexit 1\x1b[0m\n" {
		t.Errorf("tampered: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCLI("verify", "-color=always", "-bundle", f.missing, "-pubkey", f.pub)
	if code != 4 || !strings.Contains(stdout, "\x1b[1;31mFAILED\x1b[0m  \x1b[31man input is unreadable or unusable\x1b[0m") || !strings.HasPrefix(stderr, "\x1b[1;31merror:\x1b[0m ") {
		t.Errorf("missing: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, _ = runCLI("verify", "-json", "-color=always", "-bundle", f.bundle, "-pubkey", f.pub)
	var rep report
	if code != 0 || json.Unmarshal([]byte(stdout), &rep) != nil || !rep.Verified || strings.Contains(strings.Join(rep.Output, "\n"), "\x1b") ||
		strings.Contains(stdout, "VERIFIED") || len(rep.Output) != 1 || !strings.HasPrefix(rep.Output[0], "OK: ") {
		t.Errorf("-json -color=always: exit %d, stdout %q", code, stdout)
	}
	if code, _, _ := runCLI("verify", "-color=blue", "-bundle", f.bundle, "-pubkey", f.pub); code != exitUsage {
		t.Errorf("-color=blue: exit %d, want %d", code, exitUsage)
	}
	// prove writes an artifact: its output is the bundle, never styled and never bannered.
	code, stdout, _ = runCLI("prove", "-color=always", "-journal", f.journal, "-sth", f.sth, "-tool", "c1")
	if code != 0 || strings.Contains(stdout, "\x1b") || strings.Contains(stdout, "VERIFIED") {
		t.Errorf("prove -color=always: exit %d, stdout %q", code, stdout)
	}
}

// TestVerifyEvidenceColor: the report's heading is bold, each PASS line of verify-evidence gains a
// green check, and the report ends with the VERIFIED banner.
func TestVerifyEvidenceColor(t *testing.T) {
	f := newExitFixture(t)
	code, stdout, _ := runCLI("verify-evidence", "-color=always", "-evidence", f.evidence, "-pubkey", f.pub)
	plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(stdout, "")
	if code != 0 || !strings.HasPrefix(stdout, "\x1b[1mevidence for run ") || !strings.Contains(stdout, "\x1b[32m✓\x1b[0m \x1b[32mPASS\x1b[0m  ") || !strings.Contains(plain, "\n✓ VERIFIED  tree size ") {
		t.Errorf("verify-evidence -color=always: exit %d\n%s", code, stdout)
	}
	_, never, _ := runCLI("verify-evidence", "-color=never", "-evidence", f.evidence, "-pubkey", f.pub)
	if got := strings.NewReplacer("✓ ", "").Replace(plain); !strings.HasPrefix(got, never) {
		t.Errorf("the colored report's words differ from the plain report:\n%s\nwant a prefix\n%s", got, never)
	}
}

// TestVerifyColorEnv: on a terminal (term.IsTerminal replaced), the report is colored by default,
// and not with NO_COLOR set, with TERM=dumb, or with -color=never.
func TestVerifyColorEnv(t *testing.T) {
	f := newExitFixture(t)
	was := term.IsTerminal
	term.IsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { term.IsTerminal = was })
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	colored := func(extra ...string) bool {
		_, stdout, _ := runCLI(append([]string{"verify", "-bundle", f.bundle, "-pubkey", f.pub}, extra...)...)
		return strings.Contains(stdout, "\x1b[")
	}
	if !colored() {
		t.Error("a terminal is not colored")
	}
	if colored("-color=never") {
		t.Error("-color=never on a terminal is colored")
	}
	if colored("-json") {
		t.Error("-json on a terminal is colored")
	}
	t.Setenv("NO_COLOR", "1")
	if colored() {
		t.Error("NO_COLOR=1 on a terminal is colored")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if colored() {
		t.Error("TERM=dumb is colored")
	}
}

func readFixture(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// treeSize is the size of the signed tree the bundle at path is proven against.
func treeSize(t *testing.T, path string) string {
	t.Helper()
	var b audit.ProofBundle
	readFixture(t, path, &b)
	return strings.TrimSpace(string(must(json.Marshal(b.STH.Size))))
}

// TestApproverNoteOnce: on a terminal, the parenthetical audit puts on every approver decision line
// is dropped from each line and said once, dimmed, before the banner; plain lines keep it.
func TestApproverNoteOnce(t *testing.T) {
	line := "PASS  ops: approver decision included in the signed log" + approverNote
	c := &cli{st: term.New(true)}
	if got := c.styleLine(line); strings.Contains(got, "VerifyApprovals") || !strings.HasSuffix(got, "included in the signed log") {
		t.Errorf("styled approver line: %q", got)
	}
	if b := c.banner(exitVerified); !strings.Contains(b, "\x1b[2mnote: an approver line proves the decision is logged; verify-approvals checks the signatures and the count\x1b[0m") {
		t.Errorf("banner without the note: %q", b)
	}
	if b := (&cli{st: term.New(true)}).banner(exitVerified); strings.Contains(b, "note:") {
		t.Errorf("banner with a note no line dropped: %q", b)
	}
	var out strings.Builder
	plain := &cli{out: &out}
	plain.println(line)
	if out.String() != line+"\n" {
		t.Errorf("plain line changed: %q", out.String())
	}
}

// TestVerifyPrintsRecordedAt: a proven record that says when it was written (a result a ResolveHalt
// injected, a run's end) has that time printed after the verdict, as the writer's claim and
// informational; a record that says none prints no such line, and a time not in the canonical
// form is printed quoted.
func TestVerifyPrintsRecordedAt(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	for name, sig := range map[string]*agent.ApproverSignature{
		"stamped": {RecordedAt: "2026-10-14T09:30:00.125Z"},
		"plain":   nil,
		"garbled": {RecordedAt: "yesterday\nOK: forged"},
	} {
		r := toolLeaf(name, `{"ok":true}`)
		r.ApproverSignature = sig
		if _, err := journaltest.Do(ctx, store, "r", name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	sth := signHead(t, th, audit.Ed25519Signer{Priv: priv})
	dir := t.TempDir()
	for id, want := range map[string]string{
		"stamped": "INFO  the record says it was written at 2026-10-14T09:30:00.125Z (the writing process's clock, informational)\n",
		"plain":   "",
		"garbled": "INFO  the record says it was written at \"yesterday\\nOK: forged\" (the writing process's clock, informational)\n",
	} {
		b, err := audit.ProveToolCall(ctx, store, "r", id, sth)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, id+".json")
		writeJSON(t, path, b)
		code, stdout, stderr := runCLI("verify", "-bundle", path, "-pubkey", hex.EncodeToString(pub))
		head := `OK: run "r" record verified in a signed tree of size ` + treeSize(t, path) + "\n"
		if code != 0 || stdout != head+want || stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want %q", id, code, stdout, stderr, head+want)
		}
	}
}
