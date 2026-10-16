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
	"crypto/sha256"
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

// CRLOpts shapes one CRL signed by a CA.
type CRLOpts struct {
	Number     int64
	ThisUpdate time.Time
	NextUpdate time.Time
	Entries    []x509.RevocationListEntry
	Extra      []pkix.Extension // raw CRL extensions, such as the delta indicator or expiredCertsOnCRL
	SigAlg     x509.SignatureAlgorithm
}

// NewCRL signs a CRL as c and returns its DER.
func (c *CA) NewCRL(tb testing.TB, o CRLOpts) []byte {
	tb.Helper()
	der, err := c.TryCRL(o)
	if err != nil {
		tb.Fatal(err)
	}
	return der
}

// TryCRL is NewCRL that reports an error when the standard library refuses to create the CRL.
func (c *CA) TryCRL(o CRLOpts) ([]byte, error) {
	tpl := &x509.RevocationList{
		Number: big.NewInt(o.Number), ThisUpdate: o.ThisUpdate, NextUpdate: o.NextUpdate,
		RevokedCertificateEntries: o.Entries, ExtraExtensions: o.Extra, SignatureAlgorithm: o.SigAlg,
	}
	der, err := x509.CreateRevocationList(rand.Reader, tpl, c.Cert, c.Key)
	if err != nil {
		return nil, fmt.Errorf("testca: create CRL: %w", err)
	}
	return der, nil
}

// Revoked is an entry for the serial of cert revoked at t with reason code code (0 omits it).
func Revoked(cert *x509.Certificate, t time.Time, code int) x509.RevocationListEntry {
	return x509.RevocationListEntry{SerialNumber: cert.SerialNumber, RevocationTime: t, ReasonCode: code}
}

// WithInvalidity adds an invalidity date (entry extension 2.5.29.24) to e.
func WithInvalidity(e x509.RevocationListEntry, inv time.Time) x509.RevocationListEntry {
	v, err := asn1.MarshalWithParams(inv.UTC(), "generalized")
	if err != nil {
		panic(err)
	}
	e.ExtraExtensions = append(e.ExtraExtensions, pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 24}, Value: v})
	return e
}

// RawEntry is a revoked entry of a hand-built CRL.
type RawEntry struct {
	Serial *big.Int
	Time   time.Time
	Exts   []pkix.Extension
}

// RawCRLOpts shapes a hand-built CRL for inputs the standard library will not create: a missing CRL
// number, missing or backwards times, odd algorithm identifiers, padding. The CRL is signed with the
// CA's key (ECDSA P-256 or Ed25519 CAs only) so it verifies unless the options say otherwise; only
// its content is malformed.
type RawCRLOpts struct {
	Number      *big.Int // nil omits the cRLNumber extension
	This, Next  time.Time
	OmitNext    bool
	OmitVersion bool       // a version 1 CRL (no version field)
	Serials     []*big.Int // revoked at This, no extensions
	Entries     []RawEntry // revoked entries with their own times and extensions
	Extensions  []pkix.Extension
	PadBytes    int // adds a non-critical unknown extension of about this many bytes
	// InnerAlg and OuterAlg override the signature algorithm identifiers in the TBSCertList and in
	// the CertificateList (the signature itself is still the CA key's). OuterParams is the full DER of
	// the outer identifier's parameters (for RSASSA-PSS).
	InnerAlg, OuterAlg asn1.ObjectIdentifier
	OuterParams        []byte
	// Indefinite re-encodes the outer SEQUENCE with an indefinite length (BER).
	Indefinite bool
	// NoAKI omits the authority key identifier extension.
	NoAKI bool
}

// RawCRL builds and signs a CRL by marshalling the TBSCertList itself.
func (c *CA) RawCRL(tb testing.TB, o RawCRLOpts) []byte {
	tb.Helper()
	type algID struct {
		Algorithm  asn1.ObjectIdentifier
		Parameters asn1.RawValue `asn1:"optional"`
	}
	type entry struct {
		Serial *big.Int
		Time   time.Time
		Exts   []pkix.Extension `asn1:"optional"`
	}
	type tbs struct {
		Version int `asn1:"optional"`
		Sig     algID
		Issuer  asn1.RawValue
		This    time.Time
		Next    time.Time        `asn1:"optional"`
		Revoked []entry          `asn1:"optional"`
		Exts    []pkix.Extension `asn1:"tag:0,optional,explicit"`
	}
	var sigAlg asn1.ObjectIdentifier
	var sign func(msg []byte) []byte
	switch k := c.Key.(type) {
	case *ecdsa.PrivateKey:
		sigAlg = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
		sign = func(msg []byte) []byte {
			h := sha256.Sum256(msg)
			sig, err := ecdsa.SignASN1(rand.Reader, k, h[:])
			if err != nil {
				tb.Fatal(err)
			}
			return sig
		}
	case ed25519.PrivateKey:
		sigAlg = asn1.ObjectIdentifier{1, 3, 101, 112}
		sign = func(msg []byte) []byte { return ed25519.Sign(k, msg) }
	default:
		tb.Fatalf("testca: RawCRL cannot sign with %T", c.Key)
	}
	inner, outer := sigAlg, sigAlg
	if o.InnerAlg != nil {
		inner = o.InnerAlg
	}
	if o.OuterAlg != nil {
		outer = o.OuterAlg
	}
	exts := append([]pkix.Extension(nil), o.Extensions...)
	if len(c.Cert.SubjectKeyId) > 0 && !o.NoAKI {
		kid, _ := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, Bytes: c.Cert.SubjectKeyId})
		aki, _ := asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: kid})
		exts = append(exts, pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 35}, Value: aki})
	}
	if o.Number != nil {
		n, _ := asn1.Marshal(o.Number)
		exts = append(exts, pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 20}, Value: n})
	}
	if o.PadBytes > 0 {
		pad, _ := asn1.Marshal(make([]byte, o.PadBytes))
		exts = append(exts, pkix.Extension{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 8}, Value: pad})
	}
	t := tbs{Version: 1, Sig: algID{Algorithm: inner}, Issuer: asn1.RawValue{FullBytes: c.Cert.RawSubject}, This: o.This, Next: o.Next, Exts: exts}
	if o.OmitVersion {
		t.Version = 0
	}
	if o.OmitNext {
		t.Next = time.Time{}
	}
	for _, s := range o.Serials {
		t.Revoked = append(t.Revoked, entry{Serial: s, Time: o.This})
	}
	for _, e := range o.Entries {
		t.Revoked = append(t.Revoked, entry{Serial: e.Serial, Time: e.Time, Exts: e.Exts})
	}
	tbsDER, err := asn1.Marshal(t)
	if err != nil {
		tb.Fatal(err)
	}
	sigBytes := sign(tbsDER)
	list, err := asn1.Marshal(struct {
		TBS asn1.RawValue
		Alg algID
		Sig asn1.BitString
	}{asn1.RawValue{FullBytes: tbsDER}, algID{Algorithm: outer, Parameters: asn1.RawValue{FullBytes: o.OuterParams}}, asn1.BitString{Bytes: sigBytes, BitLength: 8 * len(sigBytes)}})
	if err != nil {
		tb.Fatal(err)
	}
	if o.Indefinite {
		// Strip the definite SEQUENCE header and wrap the content in an indefinite one.
		var rv asn1.RawValue
		if _, err := asn1.Unmarshal(list, &rv); err != nil {
			tb.Fatal(err)
		}
		list = append(append([]byte{0x30, 0x80}, rv.Bytes...), 0x00, 0x00)
	}
	return list
}
