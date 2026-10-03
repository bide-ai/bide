package main

import (
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
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// capBundle writes an authentic ProofBundle and its key, for the input-cap tests.
func capBundle(t *testing.T, dir string) (bundlePath, pubHex string) {
	t.Helper()
	ctx := context.Background()
	store := agenttest.MemJournal()
	if _, err := journaltest.Do(ctx, store, "r", "charge", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := audit.ProveRecord(ctx, store, "r", 0, signHead(t, th, audit.Ed25519Signer{Priv: priv}))
	if err != nil {
		t.Fatal(err)
	}
	bundlePath = filepath.Join(dir, "bundle.json")
	writeJSON(t, bundlePath, pb)
	return bundlePath, hex.EncodeToString(pub)
}

// Every file bide-audit reads is capped, 256 MiB by default: a larger input is refused before it is
// read into memory, naming the limit and the flag that raises it.
func TestCLI_InputOverTheDefaultCapIsRefused(t *testing.T) {
	dir := t.TempDir()
	_, pubHex := capBundle(t, dir)
	big := filepath.Join(dir, "big.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(256<<20 + 1); err != nil { // sparse: no disk or time spent on the zeros
		t.Fatal(err)
	}
	f.Close()
	bin := buildCLI(t, dir)
	code, out := exitCode(t, bin, "verify", "-bundle", big, "-pubkey", pubHex)
	if code != 4 || !strings.Contains(out, "-max-input-bytes") {
		t.Fatalf("a 256 MiB + 1 byte bundle: exit %d, output:\n%s\nwant exit 4 (unusable input) naming -max-input-bytes", code, out)
	}
}

// -max-input-bytes sets the cap for every input a verb reads: a bundle, a key file, a digest list,
// and a policy file each fail over it and pass at or under it.
func TestCLI_MaxInputBytesAppliesToEveryInput(t *testing.T) {
	dir := t.TempDir()
	bundle, pubHex := capBundle(t, dir)
	bin := buildCLI(t, dir)
	size := func(p string) string { return itoa(mustSize(t, p)) }
	// The bundle itself.
	if code, out := exitCode(t, bin, "verify", "-max-input-bytes", itoa(mustSize(t, bundle)-1), "-bundle", bundle, "-pubkey", pubHex); code != 4 || !strings.Contains(out, "-max-input-bytes") {
		t.Errorf("bundle over the cap: exit %d, output:\n%s", code, out)
	}
	if code, out := exitCode(t, bin, "verify", "-max-input-bytes", size(bundle), "-bundle", bundle, "-pubkey", pubHex); code != 0 {
		t.Errorf("bundle at the cap: exit %d, output:\n%s", code, out)
	}
	// A key file, read first by verify-quorum. Trailing newlines, which readPubKey trims, make it
	// larger than the cap while the key stays valid.
	keyFile := filepath.Join(dir, "key.hex")
	if err := os.WriteFile(keyFile, []byte(pubHex+strings.Repeat("\n", int(mustSize(t, bundle)))), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := exitCode(t, bin, "verify-quorum", "-max-input-bytes", size(bundle), "-name", "q", "-tally", bundle, "-vote", bundle, "-pubkey", keyFile, "-k", "1"); code != 4 || !strings.Contains(out, "-max-input-bytes") {
		t.Errorf("key file over the cap: exit %d, output:\n%s", code, out)
	}
	// A digest list, read by verify-evidence after the package and the key.
	ctx := context.Background()
	store := agenttest.MemJournal()
	if _, err := journaltest.Do(ctx, store, "r", "charge", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pkg, err := audit.Evidence(ctx, store, "r", audit.Ed25519Signer{Priv: priv}, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "evidence.json")
	writeJSON(t, evidence, pkg)
	digests := filepath.Join(dir, "approved.txt")
	if err := os.WriteFile(digests, []byte(strings.Repeat("# padding\n", int(mustSize(t, evidence)))), 0o600); err != nil {
		t.Fatal(err)
	}
	evCap := size(evidence)
	if code, out := exitCode(t, bin, "verify-evidence", "-max-input-bytes", evCap, "-evidence", evidence, "-pubkey", hex.EncodeToString(pub), "-approved-file", digests); code != 4 || !strings.Contains(out, "-max-input-bytes") {
		t.Errorf("digest list over the cap: exit %d, output:\n%s", code, out)
	}
	// A policy file.
	if code, out := exitCode(t, bin, "verify-governance", "-max-input-bytes", "20", "-policy", digests); code != 4 || !strings.Contains(out, "-max-input-bytes") {
		t.Errorf("policy file over the cap: exit %d, output:\n%s", code, out)
	}
	// A cap below 1 is a usage error.
	if code, out := exitCode(t, bin, "verify", "-max-input-bytes", "0", "-bundle", bundle, "-pubkey", pubHex); code != 2 {
		t.Errorf("-max-input-bytes 0: exit %d, output:\n%s; want 2", code, out)
	}
}

func mustSize(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
