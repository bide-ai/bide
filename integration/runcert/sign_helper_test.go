package runcert_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/bide-ai/bide/audit"
)

// mustSign signs th with an ed25519 key.
func mustSign(t *testing.T, th audit.TreeHead, priv ed25519.PrivateKey) audit.SignedTreeHead {
	t.Helper()
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	return sth
}
