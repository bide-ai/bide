package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// FINDING (CLI end to end): after a tool result is redacted, prove-absent + verify-absent exit 0
// with "OK: tooluse:pay is absent" for a call the anchored journal tree commits.
func Test_R105_CLIVerifyAbsentOnRedactedCall(t *testing.T) {
	ctx := context.Background()
	s := agent.NewMemStore()
	j := agenttest.MustJournal(s)
	if _, err := journaltest.Do(ctx, j, "r", "call:pay", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "pay", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	red := agent.NewMemStore()
	j2 := agenttest.MustJournal(red)
	for e, err := range s.Load(ctx, "r", -1) {
		if err != nil {
			t.Fatal(err)
		}
		d := e.Data
		if e.Name == "call:pay" {
			d = []byte(`{"redacted":{"leaf_hash":"` + hex.EncodeToString(audit.JournalLeafHash(e.Data)) + `","at_ms":1}}`)
		}
		red.Insert(ctx, "r", e.Name, d)
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	signer := audit.Ed25519Signer{Priv: priv}
	ts := time.Now().Add(-time.Hour).UnixNano()
	th, _ := audit.NewTreeHead(ctx, j, "r", ts) // the head anchored before redaction
	recs, _ := j2.History(ctx, "r")
	abs, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, th, signer, ts)
	if err != nil {
		// SignAbsenceRoot refuses a journal holding a redacted record (audit.ErrRedacted). A key
		// holder can still sign the key set it projects by hand; prove-absent must refuse to build
		// a proof from it.
		if !errors.Is(err, audit.ErrRedacted) {
			t.Fatal(err)
		}
		var kept []agent.Record
		for _, r := range recs {
			if !r.Redacted {
				kept = append(kept, r)
			}
		}
		root, size, rerr := audit.AbsenceRoot(kept, audit.ToolUseKeys)
		if rerr != nil {
			t.Fatal(rerr)
		}
		abs, err = audit.SignTreeHead(audit.TreeHead{Kind: audit.TreeToolUse, RunID: "r", Size: size, Root: root,
			TimestampNanos: ts, Journal: &audit.TreeRef{Size: th.Size, Root: th.Root}}, signer)
		if err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "j.json"), exportJournal(t, j2, "r"))
	writeJSON(t, filepath.Join(dir, "abs.json"), abs)
	var o, e bytes.Buffer
	if code := run([]string{"prove-absent", "-journal", filepath.Join(dir, "j.json"), "-sth", filepath.Join(dir, "abs.json"), "-key", "tool:pay", "-out", filepath.Join(dir, "b.json")}, &o, &e); code != 0 {
		t.Logf("prove-absent refused (%d): %s", code, e.String())
		return
	}
	o.Reset()
	code := run([]string{"verify-absent", "-bundle", filepath.Join(dir, "b.json"), "-pubkey", keyText(signer)}, &o, &e)
	if code == 0 && strings.Contains(o.String(), "OK:") {
		t.Fatalf("exit %d: %s", code, strings.TrimSpace(o.String()))
	}
}
