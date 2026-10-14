package pkcs11

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
)

// The DER of the EC parameters that name Ed25519: the OID 1.3.101.112, and the printable string
// "edwards25519" that PKCS#11 v3.0 also allows.
var (
	ed25519OID    = []byte{0x06, 0x03, 0x2b, 0x65, 0x70}
	ed25519Params = append([]byte{0x13, 0x0c}, "edwards25519"...)
)

// isEd25519Params reports whether CKA_EC_PARAMS names the Ed25519 curve.
func isEd25519Params(p []byte) bool {
	return bytes.Equal(p, ed25519OID) || bytes.Equal(p, ed25519Params)
}

// parseECPoint reads a CKA_EC_POINT value of an Ed25519 key: the 32-byte public key either as
// the DER OCTET STRING PKCS#11 specifies (04 20 || key) or, as some tokens return it, raw.
func parseECPoint(v []byte) ([]byte, error) {
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
