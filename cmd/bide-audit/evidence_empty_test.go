package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// verify-evidence on a package that proves nothing must exit 1, not print "PASS (0 items)".
func TestVerifyEvidenceCLI_EmptyPackageFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "r", "note", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pkg, err := audit.Evidence(ctx, store, "r", audit.Ed25519Signer{Priv: priv}, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "evidence.json")
	writeJSON(t, path, pkg)
	bin := buildCLI(t, dir)
	code, out := exitCode(t, bin, "verify-evidence", "-evidence", path, "-pubkey", hex.EncodeToString(pub))
	if code != 1 || strings.Contains(out, "PASS: run") {
		t.Fatalf("verify-evidence on an empty package: exit %d, output:\n%s\nwant exit 1 and no PASS verdict", code, out)
	}
}
