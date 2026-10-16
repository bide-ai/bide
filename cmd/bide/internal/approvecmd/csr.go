package approvecmd

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/bide-ai/bide/audit"
)

// certificateRequest is a PKCS#10 certificate signing request for the key of sg, as PEM. It is signed
// with the key (proof of possession) and names nothing: the subject is empty and there are no
// extensions, so the CA or identity provider that issues the certificate sets the name and the key
// usage. Only the schemes a certificate-bound approver signs with have one (Ed25519, ECDSA P-256).
func certificateRequest(sg audit.Signer) ([]byte, error) {
	var key crypto.Signer
	switch s := sg.(type) {
	case audit.Ed25519Signer:
		key = s.Priv
	case audit.ECDSAP256Signer:
		key = s.Priv
	default:
		return nil, fmt.Errorf("a certificate signing request needs an ed25519 or ecdsa-p256 key, not %s", sg.Alg())
	}
	if _, ok := key.Public().(ed25519.PublicKey); !ok {
		if _, ok := key.Public().(*ecdsa.PublicKey); !ok {
			return nil, fmt.Errorf("a certificate signing request needs an ed25519 or ecdsa-p256 key, not %s", sg.Alg())
		}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, fmt.Errorf("certificate signing request: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}
