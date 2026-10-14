//go:build !cgo

// Package pkcs11test needs cgo; in a build without it Run only runs the tests and Require skips.
package pkcs11test

import "testing"

// Token is the SoftHSM2 token a test process shares. Without cgo there is none.
type Token struct {
	// Module is the path of libsofthsm2.
	Module string
	// Label is the token's label.
	Label string
	// PIN is the token's user PIN.
	PIN string
}

// Run runs the tests.
func Run(m *testing.M, _ func(*Token) error) int { return m.Run() }

// Require skips the test: a build without cgo cannot load a PKCS#11 module.
func Require(t testing.TB) *Token {
	t.Helper()
	t.Skip("this build has no cgo")
	return nil
}

// GenerateEd25519 does nothing without cgo.
func (*Token) GenerateEd25519(string, []byte) error { return nil }

// GenerateP256 does nothing without cgo.
func (*Token) GenerateP256(string) error { return nil }

// GenerateEd25519Extractable does nothing without cgo.
func (*Token) GenerateEd25519Extractable(string, []byte) error { return nil }

// GenerateEd25519NoSign does nothing without cgo.
func (*Token) GenerateEd25519NoSign(string, []byte) error { return nil }

// GenerateP256Key does nothing without cgo.
func (*Token) GenerateP256Key(string, []byte) error { return nil }

// GenerateP256Extractable does nothing without cgo.
func (*Token) GenerateP256Extractable(string, []byte) error { return nil }

// GenerateEd25519Remarked does nothing without cgo.
func (*Token) GenerateEd25519Remarked(string, []byte) error { return nil }

// GenerateEd25519AlwaysAuth does nothing without cgo.
func (*Token) GenerateEd25519AlwaysAuth(string, []byte) error { return nil }
