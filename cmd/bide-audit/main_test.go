package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// auditBin is the path to build the bide-audit test binary to. On Windows, go
// build writes (and exec expects) a .exe suffix, so add it there.
func auditBin(dir string) string {
	bin := filepath.Join(dir, "bide-audit")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	return bin
}

// fakeChecker writes an executable script that ignores its argument, prints the given
// classification line, and exits with the given code, standing in for the external oracle
// (exit 0 = convergent, 1 = not, as the astchecker does; the printed line is the compensation-free
// verdict).
func fakeChecker(t *testing.T, dir, name string, code int, line string) string {
	t.Helper()
	// The stand-in oracle is a POSIX shell script, which Windows cannot exec
	// directly (a real Windows user would supply a .exe astchecker). The CLI's
	// checker-invocation path is platform-neutral and covered on Linux/macOS;
	// the cryptographic assertions before this point still run on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("external-oracle cross-check uses a POSIX shell script; skipped on Windows")
	}
	path := filepath.Join(dir, name+".sh")
	script := "#!/bin/sh\necho " + line + "\nexit " + map[int]string{0: "0", 1: "1"}[code] + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake checker: %v", err)
	}
	return path
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// signHead signs th with s, failing the test on error.
func signHead(t *testing.T, th audit.TreeHead, s audit.Signer) audit.SignedTreeHead {
	t.Helper()
	sth, err := audit.SignTreeHead(th, s)
	if err != nil {
		t.Fatalf("sign tree head: %v", err)
	}
	return sth
}

// keyText is the -pubkey text of s's public key: "<scheme>:<hex>".
func keyText(s audit.Signer) string { return audit.FormatPublicKey(s.Alg(), s.PublicKey()) }

// exportJournal is the journal export of runID in store, as prove and prove-absent read it.
func exportJournal(t *testing.T, store *agent.Journal, runID string) audit.JournalExport {
	t.Helper()
	x, err := audit.ExportJournal(context.Background(), store, runID)
	if err != nil {
		t.Fatalf("export journal: %v", err)
	}
	return x
}

// rewriteJSON marshals v, replaces old with new exactly once, and writes the result to path, so a
// test can produce an artifact in an older or foreign layout.
func rewriteJSON(t *testing.T, path string, v any, old, new string) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(b, []byte(old)) < 1 {
		t.Fatalf("rewrite %s: %q not in %s", path, old, b)
	}
	b = bytes.Replace(b, []byte(old), []byte(new), 1)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
