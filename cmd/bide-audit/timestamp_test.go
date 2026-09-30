package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// tsBundle writes a ProofBundle whose signed head carries timestamp ts, and returns its path and
// the key.
func tsBundle(t *testing.T, dir, name string, ts int64) (string, string) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "r", "charge", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, "r", ts)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := audit.ProveRecord(ctx, store, "r", 0, audit.SignTreeHead(th, priv))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	writeJSON(t, path, pb)
	return path, hex.EncodeToString(pub)
}

// Every signed head the CLI reads is held to the timestamp rule: Unix nanoseconds, positive, and
// not later than the clock plus the allowed skew.
func TestCLI_SignedHeadTimestampIsChecked(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	now := time.Now()
	for name, tc := range map[string]struct {
		ts   int64
		code int
	}{
		"an hour ago":                  {now.Add(-time.Hour).UnixNano(), 0},
		"a minute ahead (within skew)": {now.Add(time.Minute).UnixNano(), 0},
		"an hour in the future":        {now.Add(time.Hour).UnixNano(), 1},
		"zero":                         {0, 1},
		"negative":                     {-1, 1},
	} {
		path, pubHex := tsBundle(t, dir, "b.json", tc.ts)
		if code, out := exitCode(t, bin, "verify", "-bundle", path, "-pubkey", pubHex); code != tc.code {
			t.Errorf("a bundle signed %s: exit %d, want %d; output:\n%s", name, code, tc.code, out)
		}
	}
}
