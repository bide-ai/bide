package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// -pubkey is the verifier's trust root. A value that is a public key in hex is that key: it is
// never taken as the name of a file, whose content could be another key. Otherwise a producer
// who ships an evidence directory could include a file named after the auditor's key, holding
// the producer's own, and an auditor verifying from inside that directory would check the
// producer's signature under the producer's key.
func TestCLI_HexPublicKeyIsNotAFileName(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bin, err := filepath.Abs(buildCLI(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "run1", "pay", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "pay", Result: json.RawMessage(`{"usd":10}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	auditorPub, _, _ := ed25519.GenerateKey(rand.Reader)
	producerPub, producerPriv, _ := ed25519.GenerateKey(rand.Reader)
	th, _ := audit.NewTreeHead(ctx, store, "run1", 1)
	b, err := audit.ProveToolCall(ctx, store, "run1", "pay", audit.SignTreeHead(th, producerPriv))
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "evidence")
	if err := os.Mkdir(evidence, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(evidence, "bundle.json"), b)
	auditorHex := hex.EncodeToString(auditorPub)
	if err := os.WriteFile(filepath.Join(evidence, auditorHex), []byte(hex.EncodeToString(producerPub)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "verify", "-bundle", "bundle.json", "-pubkey", auditorHex)
	cmd.Dir = evidence
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("verify accepted a bundle signed by another key, read from a file named after the auditor's key:\n%s", out)
	}
}
