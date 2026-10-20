// Package safefiletest has the helpers the tests of readers built on safefile share: a named pipe
// to point a reader at, and a deadline that turns a reader waiting for a writer into a failure.
package safefiletest

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/safefile"
)

// Deadline runs f and fails the test if it has not returned in 5 s, which is how a read of a named
// pipe shows: it waits for a writer that never comes. what names the reader in the failure.
func Deadline(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked on a named pipe", what)
	}
}

// KeyText returns a valid public key in the text form bide flags take, "ed25519:<hex>".
func KeyText(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return "ed25519:" + hex.EncodeToString(pub)
}

// Stdin makes s the standard input safefile.ReadOrStdin reads, for the rest of the test.
func Stdin(t *testing.T, s string) {
	t.Helper()
	safefile.ResetStdin()
	safefile.Stdin = strings.NewReader(s)
	t.Cleanup(func() { safefile.Stdin = os.Stdin; safefile.ResetStdin() })
}
