package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// A proof bundle in the layout before formats (no "format", "Index"/"Size"/"Path" in Go case) is
// refused with a message naming the format this version reads, not with an unknown-field error;
// the same bundle in the current layout verifies.
func TestVerifyCLI_RefusesAnOldFormatBundle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "r", "c1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	th, err := audit.NewTreeHead(ctx, store, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := audit.ProveRecord(ctx, store, "r", 1, signHead(t, th, audit.Ed25519Signer{Priv: priv})) // leaf 0 is the journal header
	if err != nil {
		t.Fatal(err)
	}
	current, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(current, []byte(`"format":"`+audit.ProofFormat+`",`), nil, 1)
	old = bytes.Replace(old, []byte(`"inclusion":{"index":1,"size":2,"path":`), []byte(`"inclusion":{"Index":1,"Size":2,"Path":`), 1)
	if bytes.Equal(old, current) || bytes.Contains(old, []byte(`"index"`)) {
		t.Fatalf("the old layout was not produced from %s", current)
	}
	bin := buildCLI(t, dir)
	pubHex := hex.EncodeToString(pub)
	for name, b := range map[string][]byte{"current": current, "old": old} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		code, out := exitCode(t, bin, "verify", "-bundle", path, "-pubkey", pubHex)
		switch name {
		case "current":
			if code != 0 {
				t.Fatalf("verify rejected the current bundle: exit %d\n%s", code, out)
			}
		case "old":
			if code != 4 || !strings.Contains(out, "has no format") || !strings.Contains(out, audit.ProofFormat) || strings.Contains(out, "unknown field") {
				t.Fatalf("verify of an old-format bundle: exit %d, want 4 (unusable input) naming format %s\n%s", code, audit.ProofFormat, out)
			}
		}
	}
}
