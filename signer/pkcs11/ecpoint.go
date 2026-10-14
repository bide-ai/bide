package pkcs11

import (
	"bytes"
	"crypto/ed25519"
	"fmt"

	"github.com/bide-ai/bide/audit"
)

// The DER of the EC parameters that name Ed25519: the OID 1.3.101.112, and the printable string
// "edwards25519" that PKCS#11 v3.0 also allows.
var (
	ed25519OID    = []byte{0x06, 0x03, 0x2b, 0x65, 0x70}
	ed25519Params = append([]byte{0x13, 0x0c}, "edwards25519"...)
)

// The DER of the EC parameters that name P-256: the OID 1.2.840.10045.3.1.7 (prime256v1).
var p256OID = []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}

// isP256Params reports whether CKA_EC_PARAMS names P-256.
func isP256Params(p []byte) bool { return bytes.Equal(p, p256OID) }

// isEd25519Params reports whether CKA_EC_PARAMS names the Ed25519 curve.
func isEd25519Params(p []byte) bool {
	return bytes.Equal(p, ed25519OID) || bytes.Equal(p, ed25519Params)
}

// parseECPoint reads a CKA_EC_POINT value of an Ed25519 key: the 32-byte public key either as
// the DER OCTET STRING PKCS#11 specifies (04 20 || key) or, as some tokens return it, raw.
func parseECPoint(v []byte) ([]byte, error) {
	return parsePoint(audit.AlgEd25519, v)
}

// parsePoint reads a CKA_EC_POINT of the scheme's key. An Ed25519 point is the 32-byte key, raw or
// as a DER OCTET STRING (04 20 || key); a P-256 point is the 65-byte uncompressed SEC1 point
// (04 || X || Y), raw or as a DER OCTET STRING (04 41 || point).
func parsePoint(alg audit.Alg, v []byte) ([]byte, error) {
	if alg == audit.AlgECDSAP256 {
		switch {
		case len(v) == 65 && v[0] == 0x04:
			return bytes.Clone(v), nil
		case len(v) == 67 && v[0] == 0x04 && v[1] == 0x41 && v[2] == 0x04:
			return bytes.Clone(v[2:]), nil
		}
		return nil, fmt.Errorf("pkcs11: CKA_EC_POINT is %d bytes, want an uncompressed P-256 point (65 bytes, or 67 as a DER OCTET STRING): %w", len(v), ErrNoP256)
	}
	switch {
	case len(v) == ed25519.PublicKeySize:
		return bytes.Clone(v), nil
	case len(v) == ed25519.PublicKeySize+2 && v[0] == 0x04 && v[1] == ed25519.PublicKeySize:
		return bytes.Clone(v[2:]), nil
	}
	// CKK_EC_EDWARDS also covers Ed448, whose public key is 57 bytes (59 as a DER OCTET STRING).
	if len(v) == 57 || len(v) == 59 {
		return nil, fmt.Errorf("pkcs11: CKA_EC_POINT is %d bytes, an Ed448 key; bide signs with Ed25519: %w", len(v), ErrNoEd25519)
	}
	return nil, fmt.Errorf("pkcs11: CKA_EC_POINT is %d bytes, want an Ed25519 public key (32 bytes, or 34 as a DER OCTET STRING)", len(v))
}
