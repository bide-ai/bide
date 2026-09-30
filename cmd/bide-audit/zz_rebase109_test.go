package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A -pubkey value that is a weak Ed25519 key in hex is a key, not a path: it is refused as
// unusable (exit 4) and never opened as a file, even when a file of that name holds the log's
// real key, which would otherwise let a planted file substitute the key. (Rebase of #105 onto
// #109: ParsePublicKey refuses a weak key, and readPubKey must not fall back to the file.)
func TestRebase109_WeakPubkeyIsNotOpenedAsAFile(t *testing.T) {
	f := newExitFixture(t)
	t.Chdir(f.dir)
	if err := os.WriteFile(filepath.Join(f.dir, f.identityPub), []byte(f.pub), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI("verify", "-bundle", f.bundle, "-pubkey", f.identityPub)
	if out := stdout + stderr; code != 4 || !strings.Contains(out, "weak ed25519 public key") {
		t.Fatalf("exit %d, want 4 refusing the weak key rather than reading the file of its name\n%s", code, out)
	}
}
