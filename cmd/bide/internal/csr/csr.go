// Package csr builds PKCS#10 certificate signing requests for a key held by an audit.Signer: a key
// file, or a key in an HSM or a KMS that bide-hsm opens. The key signs the request through
// audit.Signer.Sign and is never read; for a KMS key that is one Sign call.
//
// The request is assembled here, not by x509.CreateCertificateRequest, because that function hashes an
// ECDSA request itself and hands its signer a digest, while an audit.Signer signs the message (it
// hashes inside, as a KMS or a token needs). The signer is given the request's CertificationRequestInfo
// and nothing else.
package csr

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"net/url"
	"slices"

	"github.com/bide-ai/bide/audit"
)

// Names are the names a request asks the CA for. The zero value asks for none: an empty subject and no
// extensions, so the CA or identity provider sets the name and the usage. A CA may ignore or replace
// what is asked; the certificate it issues is what bide approve -cert checks.
type Names struct {
	// Email is the one rfc822Name subject alternative name to ask for (a bare address). It excludes URI.
	Email string
	// URI is the one uniformResourceIdentifier subject alternative name to ask for (an absolute URI with
	// a host and no fragment). It excludes Email.
	URI string
	// EKU are the extended key usages to ask for.
	EKU []asn1.ObjectIdentifier
}

var (
	oidExtensionRequest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 14}
	oidSAN              = asn1.ObjectIdentifier{2, 5, 29, 17}
	oidEKU              = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidEd25519          = asn1.ObjectIdentifier{1, 3, 101, 112}
	oidECDSAWithSHA256  = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
)

// PublicKey decodes the encoded public key of sg into the standard library's key, checking that it is a
// valid key of its scheme (Ed25519 or ECDSA P-256, the schemes a request exists for).
func PublicKey(sg audit.Signer) (crypto.PublicKey, error) {
	switch sg.Alg() {
	case audit.AlgEd25519:
		if _, err := audit.NewVerifier(audit.AlgEd25519, sg.PublicKey()); err != nil {
			return nil, fmt.Errorf("the key's public key: %w", err)
		}
		return ed25519.PublicKey(slices.Clone(sg.PublicKey())), nil
	case audit.AlgECDSAP256:
		k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), sg.PublicKey())
		if err != nil {
			return nil, fmt.Errorf("the key's public key is not a point on P-256: %w", audit.ErrMalformed)
		}
		return k, nil
	}
	return nil, fmt.Errorf("a certificate signing request needs an ed25519 or ecdsa-p256 key, not %s", sg.Alg())
}

// Check reports whether n can be put in a request, so a caller can refuse a name before it opens a key.
// Build checks the same.
func Check(n Names) error {
	_, err := extensions(n)
	return err
}

// Build returns the PEM of a certificate signing request for the key of sg asking for n, signed by the
// key. Before returning, it parses the request and checks its signature, that its public key is the
// one sg reports, and that it carries exactly the names asked for; a request that fails any of them is
// an error and no bytes.
func Build(sg audit.Signer, n Names) ([]byte, error) {
	exts, err := extensions(n)
	if err != nil {
		return nil, err
	}
	pub, err := PublicKey(sg)
	if err != nil {
		return nil, err
	}
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("encode the public key: %w", err)
	}
	sigAlg := pkix.AlgorithmIdentifier{Algorithm: oidEd25519}
	if sg.Alg() == audit.AlgECDSAP256 {
		sigAlg.Algorithm = oidECDSAWithSHA256
	}

	attrs := asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true}
	if len(exts) > 0 {
		extDER, err := asn1.Marshal(exts)
		if err != nil {
			return nil, fmt.Errorf("encode the requested extensions: %w", err)
		}
		attr, err := asn1.Marshal(struct {
			Type   asn1.ObjectIdentifier
			Values asn1.RawValue
		}{oidExtensionRequest, asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: extDER}})
		if err != nil {
			return nil, fmt.Errorf("encode the requested extensions: %w", err)
		}
		attrs.Bytes = attr
	}
	tbs, err := asn1.Marshal(struct {
		Version    int
		Subject    asn1.RawValue
		PublicKey  asn1.RawValue
		Attributes asn1.RawValue
	}{0, asn1.RawValue{FullBytes: []byte{0x30, 0x00}}, asn1.RawValue{FullBytes: spki}, attrs})
	if err != nil {
		return nil, fmt.Errorf("encode the request: %w", err)
	}

	sig, err := sg.Sign(tbs)
	if err != nil {
		return nil, fmt.Errorf("the key could not sign the request: %w", err)
	}
	if sg.Alg() == audit.AlgECDSAP256 {
		if len(sig) != 64 {
			return nil, fmt.Errorf("the key's ecdsa-p256 signature is %d bytes, want 64", len(sig))
		}
		if sig, err = asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])}); err != nil {
			return nil, fmt.Errorf("encode the signature: %w", err)
		}
	}
	der, err := asn1.Marshal(struct {
		TBS       asn1.RawValue
		Algorithm pkix.AlgorithmIdentifier
		Signature asn1.BitString
	}{asn1.RawValue{FullBytes: tbs}, sigAlg, asn1.BitString{Bytes: sig, BitLength: 8 * len(sig)}})
	if err != nil {
		return nil, fmt.Errorf("encode the request: %w", err)
	}
	if err := selfCheck(der, spki, n); err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// selfCheck parses der as a verifier would and checks it against what was asked and the key's own
// public key.
func selfCheck(der, spki []byte, n Names) error {
	req, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return fmt.Errorf("the request built does not parse: %w", err)
	}
	if err := req.CheckSignature(); err != nil {
		return fmt.Errorf("the key's signature does not verify under its public key (a faulty token or KMS, or a key replaced behind the reference): %w", err)
	}
	if !slices.Equal(req.RawSubjectPublicKeyInfo, spki) {
		return errors.New("the request's public key is not the key's")
	}
	wantEmail, wantURI := []string(nil), []string(nil)
	if n.Email != "" {
		wantEmail = []string{n.Email}
	}
	if n.URI != "" {
		wantURI = []string{n.URI}
	}
	var gotURI []string
	for _, u := range req.URIs {
		gotURI = append(gotURI, u.String())
	}
	if !slices.Equal(req.EmailAddresses, wantEmail) || !slices.Equal(gotURI, wantURI) || len(req.DNSNames) != 0 || len(req.IPAddresses) != 0 {
		return errors.New("the request does not carry the names asked for")
	}
	return nil
}

type extension struct {
	ID    asn1.ObjectIdentifier
	Value []byte
}

// extensions checks n and returns the extensionRequest's extensions.
func extensions(n Names) ([]extension, error) {
	var exts []extension
	if n.Email != "" && n.URI != "" {
		return nil, errors.New("a request asks for one subject alternative name: an email or a URI, not both")
	}
	var san *asn1.RawValue
	switch {
	case n.Email != "":
		if err := checkEmail(n.Email); err != nil {
			return nil, err
		}
		san = &asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 1, Bytes: []byte(n.Email)}
	case n.URI != "":
		if err := checkURI(n.URI); err != nil {
			return nil, err
		}
		san = &asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 6, Bytes: []byte(n.URI)}
	}
	if san != nil {
		v, err := asn1.Marshal([]asn1.RawValue{*san})
		if err != nil {
			return nil, fmt.Errorf("encode the subject alternative name: %w", err)
		}
		exts = append(exts, extension{oidSAN, v})
	}
	if len(n.EKU) > 0 {
		for i, o := range n.EKU {
			if len(o) < 3 {
				return nil, fmt.Errorf("extended key usage %d is not an object identifier", i+1)
			}
			if slices.IndexFunc(n.EKU[:i], o.Equal) >= 0 {
				return nil, fmt.Errorf("extended key usage %s is asked for twice", o)
			}
		}
		v, err := asn1.Marshal(n.EKU)
		if err != nil {
			return nil, fmt.Errorf("encode the extended key usages: %w", err)
		}
		exts = append(exts, extension{oidEKU, v})
	}
	return exts, nil
}

// plainASCII reports whether s is printable ASCII without a space.
func plainASCII(s string) bool {
	for _, r := range s {
		if r <= ' ' || r >= 0x7f {
			return false
		}
	}
	return s != ""
}

func checkEmail(s string) error {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" || !plainASCII(s) {
		return fmt.Errorf("%q is not a bare ASCII email address (no display name, no spaces)", s)
	}
	return nil
}

func checkURI(s string) error {
	u, err := url.Parse(s)
	if err != nil || !plainASCII(s) || u.Scheme == "" || u.Host == "" || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" {
		return fmt.Errorf("%q is not an absolute ASCII URI with a host and no fragment", s)
	}
	return nil
}
