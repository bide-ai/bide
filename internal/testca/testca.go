// Package testca is a small certificate authority for the tests of the approver identity feature
// (docs/design/approver-identity.md, "Test plan"). It uses only the standard library, builds roots,
// intermediates and leaves with chosen keys, validity, names and extensions, and is deterministic
// where the tests need it to be: every time is an argument, nothing reads the clock.
//
// It is test support. Nothing outside tests imports it.
package testca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// T0 is the instant the tests treat as "now". Certificates are issued around it.
var T0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

var (
	// EKU is the bank-assigned private extended key usage the approver certificates carry.
	EKU = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 1}
	// PolicyOID is the certificate policy the bank's CA asserts after MFA step-up and a hardware key.
	PolicyOID = mustOID("1.3.6.1.4.1.99999.2.1")
)

func mustOID(s string) x509.OID {
	o, err := x509.ParseOID(s)
	if err != nil {
		panic(err)
	}
	return o
}

// Person returns the opaque person URI the default identity profile accepts for host
// "idp.bank.example" and the 32 hex digits made of n.
func Person(n int) string { return fmt.Sprintf("https://idp.bank.example/person/%032x", n) }

// KeyKind names a key type for a CA or a leaf.
type KeyKind int

// The key kinds.
const (
	P256 KeyKind = iota // ECDSA P-256 (the default)
	P384
	P521
	Ed25519
	MLDSA65
	RSA2048
	RSA3072
)

// NewKey generates a key of kind k.
func NewKey(tb testing.TB, k KeyKind) crypto.Signer {
	tb.Helper()
	var (
		s   crypto.Signer
		err error
	)
	switch k {
	case P256:
		s, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case P384:
		s, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case P521:
		s, err = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	case Ed25519:
		_, s, err = ed25519.GenerateKey(rand.Reader)
	case MLDSA65:
		s, err = mldsa.GenerateKey(mldsa.MLDSA65())
	case RSA2048:
		s, err = rsa.GenerateKey(rand.Reader, 2048)
	case RSA3072:
		s, err = rsa.GenerateKey(rand.Reader, 3072)
	default:
		tb.Fatalf("testca: unknown key kind %d", k)
	}
	if err != nil {
		tb.Fatalf("testca: generate key kind %d: %v", k, err)
	}
	return s
}

// Opts shapes one certificate. The zero value gives the default for the certificate's role.
type Opts struct {
	Key     KeyKind                 // the new certificate's key kind (default P-256)
	KeyWith crypto.Signer           // use this key instead of generating one
	Mutate  func(*x509.Certificate) // last word on the template, before signing
	SigAlg  x509.SignatureAlgorithm // the signature algorithm, when the default is not wanted
}

// CA is a certificate authority: a certificate and the key that signs for it.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	DER  []byte
	// parent is the CA that issued this one (nil for a root); it lets a chain be assembled.
	parent *CA
}

// Leaf is an issued end-entity certificate with its key.
type Leaf struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	DER  []byte
	ca   *CA
}

func serial(tb testing.TB) *big.Int {
	tb.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 150))
	if err != nil {
		tb.Fatal(err)
	}
	return n.Add(n, big.NewInt(1))
}

func tryCreate(tpl, parent *x509.Certificate, pub crypto.PublicKey, signer crypto.Signer) ([]byte, *x509.Certificate, error) {
	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, pub, signer)
	if err != nil {
		return nil, nil, fmt.Errorf("testca: create certificate %q: %w", tpl.Subject.CommonName, err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("testca: parse certificate %q: %w", tpl.Subject.CommonName, err)
	}
	return der, c, nil
}

func create(tb testing.TB, tpl, parent *x509.Certificate, pub crypto.PublicKey, signer crypto.Signer) ([]byte, *x509.Certificate) {
	tb.Helper()
	der, c, err := tryCreate(tpl, parent, pub, signer)
	if err != nil {
		tb.Fatal(err)
	}
	return der, c
}

func caTemplate(tb testing.TB, name string, o Opts) *x509.Certificate {
	tpl := &x509.Certificate{
		SerialNumber: serial(tb), Subject: pkix.Name{CommonName: name},
		NotBefore: T0.Add(-24 * time.Hour), NotAfter: T0.Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SignatureAlgorithm: o.SigAlg,
	}
	if o.Mutate != nil {
		o.Mutate(tpl)
	}
	return tpl
}

// NewRoot returns a self-signed root.
func NewRoot(tb testing.TB, name string, o Opts) *CA {
	tb.Helper()
	key := o.KeyWith
	if key == nil {
		key = NewKey(tb, o.Key)
	}
	tpl := caTemplate(tb, name, o)
	der, c := create(tb, tpl, tpl, key.Public(), key)
	return &CA{Cert: c, Key: key, DER: der}
}

// NewIntermediate returns a CA issued by c.
func (c *CA) NewIntermediate(tb testing.TB, name string, o Opts) *CA {
	tb.Helper()
	key := o.KeyWith
	if key == nil {
		key = NewKey(tb, o.Key)
	}
	tpl := caTemplate(tb, name, o)
	der, cert := create(tb, tpl, c.Cert, key.Public(), c.Key)
	return &CA{Cert: cert, Key: key, DER: der, parent: c}
}

// NewLeaf issues an approver certificate: one URI SAN (Person(1)), digital signature, the bank EKU
// and the bank policy, valid from an hour before T0 to an hour after it. Mutate changes any of it.
func (c *CA) NewLeaf(tb testing.TB, o Opts) *Leaf {
	tb.Helper()
	l, err := c.TryLeaf(tb, o)
	if err != nil {
		tb.Fatal(err)
	}
	return l
}

// TryLeaf is NewLeaf that reports an error when the standard library refuses to issue or to parse
// the certificate, for tests of inputs the library itself may block.
func (c *CA) TryLeaf(tb testing.TB, o Opts) (*Leaf, error) {
	tb.Helper()
	key := o.KeyWith
	if key == nil {
		key = NewKey(tb, o.Key)
	}
	u, err := url.Parse(Person(1))
	if err != nil {
		tb.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(tb), Subject: pkix.Name{CommonName: "ignored by bide"},
		NotBefore: T0.Add(-time.Hour), NotAfter: T0.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, UnknownExtKeyUsage: []asn1.ObjectIdentifier{EKU},
		Policies: []x509.OID{PolicyOID}, URIs: []*url.URL{u}, SignatureAlgorithm: o.SigAlg,
	}
	if o.Mutate != nil {
		o.Mutate(tpl)
	}
	der, cert, err := tryCreate(tpl, c.Cert, key.Public(), c.Key)
	if err != nil {
		return nil, err
	}
	return &Leaf{Cert: cert, Key: key, DER: der, ca: c}, nil
}

// Chain returns the leaf-first DER chain a decision carries: the leaf, then every issuing CA up to
// but excluding the root (the root is the trust anchor the verifier holds).
func (l *Leaf) Chain() [][]byte {
	out := [][]byte{l.DER}
	for ca := l.ca; ca != nil && ca.parent != nil; ca = ca.parent {
		out = append(out, ca.DER)
	}
	return out
}

// ChainWithRoot is Chain plus the root, for tests that show a carried root is ignored.
func (l *Leaf) ChainWithRoot() [][]byte {
	out := l.Chain()
	ca := l.ca
	for ca.parent != nil {
		ca = ca.parent
	}
	return append(out, ca.DER)
}
