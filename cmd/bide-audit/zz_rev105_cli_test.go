package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// FINDING (CLI end to end): after a tool result is redacted, prove-absent + verify-absent exit 0
// with "OK: tooluse:pay is absent" for a call the anchored journal tree commits.
func Test_R105_CLIVerifyAbsentOnRedactedCall(t *testing.T) {
	ctx := context.Background()
	s := agent.NewMemStore()
	if _, err := s.Do(ctx, "r", "call:pay", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "pay", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	red := agent.NewMemStore()
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
	th, _ := audit.NewTreeHead(ctx, s, "r", ts) // the head anchored before redaction
	recs, _ := red.History(ctx, "r")
	abs, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, th, signer, ts)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "j.json"), exportJournal(t, red, "r"))
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
