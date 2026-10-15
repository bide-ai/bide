// Package identitycheck holds tests of the crypto/x509 behaviors the approver identity design
// (docs/design/approver-identity.md) relies on. Each test states one assumption and fails, naming
// it, if a Go release changes it. The package has no non-test code; the build of the feature replaces
// it with the real tests of the identity verifier and keeps these as the assumption checks.
package identitycheck

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"
)

var (
	t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// eku is a bank-assigned private extended key usage OID.
	eku = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 1}
	// oidInvalidityDate and oidDeltaCRLIndicator are the CRL extensions the design reads.
	oidInvalidityDate    = asn1.ObjectIdentifier{2, 5, 29, 24}
	oidDeltaCRLIndicator = asn1.ObjectIdentifier{2, 5, 29, 27}
)

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T, mut func(*x509.Certificate)) *ca {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: t0.Add(-24 * time.Hour), NotAfter: t0.Add(24 * 365 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	if mut != nil {
		mut(tpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &ca{c, k}
}

func (c *ca) leaf(t *testing.T, pub any, mut func(*x509.Certificate)) *x509.Certificate {
	t.Helper()
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "ignored"},
		NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, UnknownExtKeyUsage: []asn1.ObjectIdentifier{eku},
	}
	if mut != nil {
		mut(tpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.cert, pub, c.key)
	if err != nil {
		t.Fatal(err)
	}
	l, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (c *ca) verify(l *x509.Certificate, at time.Time, usages ...x509.ExtKeyUsage) error {
	roots := x509.NewCertPool()
	roots.AddCert(c.cert)
	_, err := l.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: at, KeyUsages: usages})
	return err
}

func leafKey(t *testing.T) *ecdsa.PublicKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &k.PublicKey
}

// Assumption: Certificate.Verify enforces name constraints on email SANs (permitted domain).
func TestEmailNameConstraintsAreEnforced(t *testing.T) {
	root := newCA(t, func(c *x509.Certificate) {
		c.PermittedEmailAddresses = []string{"bank.example"}
		c.PermittedDNSDomainsCritical = true
	})
	ok := root.leaf(t, leafKey(t), func(c *x509.Certificate) { c.EmailAddresses = []string{"alice@bank.example"} })
	if err := root.verify(ok, t0, x509.ExtKeyUsageAny); err != nil {
		t.Fatalf("a permitted email: %v", err)
	}
	bad := root.leaf(t, leafKey(t), func(c *x509.Certificate) { c.EmailAddresses = []string{"alice@other.example"} })
	if err := root.verify(bad, t0, x509.ExtKeyUsageAny); err == nil {
		t.Fatal("ASSUMPTION BROKEN: an email outside the permitted domain verified")
	}
	// A permitted constraint "bank.example" (no leading dot) also permits subdomains in crypto/x509, which
	// is not what RFC 5280 says for an email constraint written that way. The design therefore never
	// relies on a permitted subtree alone to pin a domain: the identity map matches the SAN exactly,
	// and a CA that must exclude subdomains adds an excluded subtree.
	sub := root.leaf(t, leafKey(t), func(c *x509.Certificate) { c.EmailAddresses = []string{"alice@evil.bank.example"} })
	if err := root.verify(sub, t0, x509.ExtKeyUsageAny); err != nil {
		t.Fatalf("ASSUMPTION CHANGED: a subdomain of a permitted email domain no longer verifies: %v (update the design note)", err)
	}
	excl := newCA(t, func(c *x509.Certificate) {
		c.PermittedEmailAddresses = []string{"bank.example"}
		c.ExcludedEmailAddresses = []string{".bank.example"}
	})
	if err := excl.verify(excl.leaf(t, leafKey(t), func(c *x509.Certificate) { c.EmailAddresses = []string{"alice@bank.example"} }), t0, x509.ExtKeyUsageAny); err != nil {
		t.Fatalf("the exact domain under an excluded subdomain subtree: %v", err)
	}
	if err := excl.verify(excl.leaf(t, leafKey(t), func(c *x509.Certificate) { c.EmailAddresses = []string{"alice@evil.bank.example"} }), t0, x509.ExtKeyUsageAny); err == nil {
		t.Fatal("ASSUMPTION BROKEN: an excluded email subdomain verified")
	}
}

// Assumption: a URI SAN with a host is checked against permitted URI domains; a URI with no host
// cannot be matched against constraints and is refused.
func TestURISANConstraints(t *testing.T) {
	root := newCA(t, func(c *x509.Certificate) { c.PermittedURIDomains = []string{"bank.example"} })
	ok := root.leaf(t, leafKey(t), func(c *x509.Certificate) {
		c.URIs = []*url.URL{{Scheme: "spiffe", Host: "bank.example", Path: "/approver/alice"}}
	})
	if err := root.verify(ok, t0, x509.ExtKeyUsageAny); err != nil {
		t.Fatalf("a permitted URI host: %v", err)
	}
	other := root.leaf(t, leafKey(t), func(c *x509.Certificate) {
		c.URIs = []*url.URL{{Scheme: "spiffe", Host: "other.example", Path: "/approver/alice"}}
	})
	if err := root.verify(other, t0, x509.ExtKeyUsageAny); err == nil {
		t.Fatal("ASSUMPTION BROKEN: a URI host outside the permitted domain verified")
	}
	hostless := root.leaf(t, leafKey(t), func(c *x509.Certificate) { c.URIs = []*url.URL{{Scheme: "urn", Opaque: "bide:approver:alice"}} })
	err := root.verify(hostless, t0, x509.ExtKeyUsageAny)
	if err == nil {
		t.Fatal("ASSUMPTION BROKEN: a host-less URI SAN verified under URI name constraints")
	}
	t.Logf("host-less URI under constraints: %v", err)
}

// Assumption: directory-name (Subject DN) constraints are not enforced. A critical name-constraints
// extension holding only a directoryName subtree is built by hand (CreateCertificate has no field for
// one) and the leaf's subject is outside it.
func TestDirectoryNameConstraintsAreNotEnforced(t *testing.T) {
	// NameConstraints ::= SEQUENCE { permittedSubtrees [0] GeneralSubtrees }, GeneralSubtree ::= SEQUENCE { base GeneralName }
	// directoryName is [4] EXPLICIT Name; here the Name is the RDNSequence of O=Bank.
	rdn, err := asn1.Marshal(pkix.Name{Organization: []string{"Bank"}}.ToRDNSequence())
	if err != nil {
		t.Fatal(err)
	}
	dirName := asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 4, IsCompound: true, Bytes: rdn}
	subtree, err := asn1.Marshal(struct{ Base asn1.RawValue }{dirName})
	if err != nil {
		t.Fatal(err)
	}
	permitted := asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: subtree}
	ncBytes, err := asn1.Marshal(struct {
		P asn1.RawValue
	}{permitted})
	if err != nil {
		t.Fatal(err)
	}
	for _, critical := range []bool{false, true} {
		root := newCA(t, func(c *x509.Certificate) {
			c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 30}, Critical: critical, Value: ncBytes}}
		})
		l := root.leaf(t, leafKey(t), func(c *x509.Certificate) {
			c.Subject = pkix.Name{Organization: []string{"Evil"}}
			c.EmailAddresses = []string{"a@b.example"}
		})
		err := root.verify(l, t0, x509.ExtKeyUsageAny)
		t.Logf("directoryName-only constraint (critical=%v), leaf O=Evil: verify error = %v", critical, err)
		if !critical && err != nil {
			t.Fatalf("a non-critical unsupported constraint should be ignored: %v", err)
		}
	}
}

// Assumption: VerifyOptions.CurrentTime is honoured, so a certificate is checked at A(d), not now.
func TestVerifyAtAPastTime(t *testing.T) {
	root := newCA(t, nil)
	l := root.leaf(t, leafKey(t), func(c *x509.Certificate) { c.EmailAddresses = []string{"alice@bank.example"} })
	if err := root.verify(l, t0, x509.ExtKeyUsageAny); err != nil {
		t.Fatal(err)
	}
	if err := root.verify(l, t0.Add(2*time.Hour), x509.ExtKeyUsageAny); err == nil {
		t.Fatal("ASSUMPTION BROKEN: an expired certificate verified at a time after notAfter")
	}
	if err := root.verify(l, t0.Add(-2*time.Hour), x509.ExtKeyUsageAny); err == nil {
		t.Fatal("ASSUMPTION BROKEN: a certificate verified at a time before notBefore")
	}
	// A years-later check at the time of the decision still passes.
	if err := root.verify(l, t0.Add(30*time.Minute), x509.ExtKeyUsageAny); err != nil {
		t.Fatal(err)
	}
}

// Assumption (corrected by this test): VerifyOptions.KeyUsages takes only the predefined ExtKeyUsage
// values. With none given, Verify requires ServerAuth, so a private-OID certificate fails unless the
// caller passes ExtKeyUsageAny. A private OID is therefore checked by hand against
// Certificate.UnknownExtKeyUsage.
func TestPrivateEKUMustBeCheckedByHand(t *testing.T) {
	root := newCA(t, nil)
	l := root.leaf(t, leafKey(t), nil) // EKU: only the private OID
	err := root.verify(l, t0)          // no KeyUsages: Go defaults to ServerAuth
	t.Logf("private-OID-only leaf, default KeyUsages: %v", err)
	if err == nil {
		t.Fatal("ASSUMPTION CHANGED: a leaf without ServerAuth verified under the default key usage")
	}
	if err := root.verify(l, t0, x509.ExtKeyUsageAny); err != nil {
		t.Fatalf("ExtKeyUsageAny: %v", err)
	}
	found := false
	for _, o := range l.UnknownExtKeyUsage {
		found = found || o.Equal(eku)
	}
	if !found {
		t.Fatal("the private EKU is not in UnknownExtKeyUsage")
	}
	// A leaf with no EKU at all passes ExtKeyUsageAny: the by-hand check must insist the OID is present.
	none := root.leaf(t, leafKey(t), func(c *x509.Certificate) { c.UnknownExtKeyUsage = nil })
	if err := root.verify(none, t0, x509.ExtKeyUsageAny); err != nil {
		t.Fatalf("no-EKU leaf under Any: %v", err)
	}
	if len(none.UnknownExtKeyUsage) != 0 || len(none.ExtKeyUsage) != 0 {
		t.Fatal("expected a leaf with no EKU")
	}
}

// Assumption: a leaf with a critical extension Go does not understand is refused.
func TestUnknownCriticalExtensionIsRefused(t *testing.T) {
	root := newCA(t, nil)
	l := root.leaf(t, leafKey(t), func(c *x509.Certificate) {
		c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 9}, Critical: true, Value: []byte{0x05, 0x00}}}
	})
	err := root.verify(l, t0, x509.ExtKeyUsageAny)
	if err == nil {
		t.Fatal("ASSUMPTION BROKEN: an unknown critical extension was accepted")
	}
	t.Logf("unknown critical extension: %v", err)
}

// Assumption: Verify enforces basicConstraints: a certificate that is not a CA cannot be an
// intermediate, even when it signed the leaf.
func TestNonCAIntermediateIsRefused(t *testing.T) {
	root := newCA(t, nil)
	fk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	notCA := root.leaf(t, &fk.PublicKey, func(c *x509.Certificate) { c.UnknownExtKeyUsage = nil })
	lk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "x"}, NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour),
		EmailAddresses: []string{"m@bank.example"}, KeyUsage: x509.KeyUsageDigitalSignature, UnknownExtKeyUsage: []asn1.ObjectIdentifier{eku}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, notCA, &lk.PublicKey, fk)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := x509.ParseCertificate(der)
	inter := x509.NewCertPool()
	inter.AddCert(notCA)
	roots := x509.NewCertPool()
	roots.AddCert(root.cert)
	if _, err := l.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err == nil {
		t.Fatal("ASSUMPTION BROKEN: a chain through a non-CA certificate verified")
	}
}

// Assumption: the path-length constraint of the root bounds the chain (MaxPathLen 0: no intermediate).
func TestPathLenIsEnforced(t *testing.T) {
	root := newCA(t, func(c *x509.Certificate) { c.MaxPathLen = 0; c.MaxPathLenZero = true })
	ik, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	itpl := &x509.Certificate{SerialNumber: big.NewInt(5), Subject: pkix.Name{CommonName: "inter"}, NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	ider, err := x509.CreateCertificate(rand.Reader, itpl, root.cert, &ik.PublicKey, root.key)
	if err != nil {
		t.Fatal(err)
	}
	icert, _ := x509.ParseCertificate(ider)
	inter := &ca{icert, ik}
	l := inter.leaf(t, leafKey(t), func(c *x509.Certificate) { c.EmailAddresses = []string{"a@bank.example"} })
	roots := x509.NewCertPool()
	roots.AddCert(root.cert)
	pool := x509.NewCertPool()
	pool.AddCert(icert)
	if _, err := l.Verify(x509.VerifyOptions{Roots: roots, Intermediates: pool, CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err == nil {
		t.Fatal("ASSUMPTION BROKEN: an intermediate under a MaxPathLen 0 root verified")
	}
}

// Assumption: ParseRevocationList reads entries (serial, revocation time, reason), this-update,
// next-update and the extension list; CheckSignatureFrom verifies the issuer; the invalidity date
// (entry extension 2.5.29.24) and the delta CRL indicator (2.5.29.27) are visible only as raw
// extensions, which the design parses itself.
func TestRevocationListParsing(t *testing.T) {
	root := newCA(t, nil)
	invalidity, err := asn1.Marshal(t0.Add(-3 * time.Hour).UTC())
	if err != nil {
		t.Fatal(err)
	}
	// GeneralizedTime is what RFC 5280 requires for the invalidity date.
	invalidity, err = asn1.MarshalWithParams(t0.Add(-3*time.Hour).UTC(), "generalized")
	if err != nil {
		t.Fatal(err)
	}
	delta, _ := asn1.Marshal(big.NewInt(41))
	tpl := &x509.RevocationList{
		Number: big.NewInt(42), ThisUpdate: t0, NextUpdate: t0.Add(24 * time.Hour),
		RevokedCertificateEntries: []x509.RevocationListEntry{{
			SerialNumber: big.NewInt(7), RevocationTime: t0.Add(-time.Hour), ReasonCode: 1, // keyCompromise
			ExtraExtensions: []pkix.Extension{{Id: oidInvalidityDate, Value: invalidity}},
		}},
		ExtraExtensions: []pkix.Extension{{Id: oidDeltaCRLIndicator, Critical: true, Value: delta}},
	}
	der, err := x509.CreateRevocationList(rand.Reader, tpl, root.cert, root.key)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := rl.CheckSignatureFrom(root.cert); err != nil {
		t.Fatalf("issuer signature: %v", err)
	}
	other := newCA(t, nil)
	if err := rl.CheckSignatureFrom(other.cert); err == nil {
		t.Fatal("ASSUMPTION BROKEN: a CRL verified under a CA that did not sign it")
	}
	if len(rl.RevokedCertificateEntries) != 1 {
		t.Fatalf("entries: %d", len(rl.RevokedCertificateEntries))
	}
	e := rl.RevokedCertificateEntries[0]
	if e.SerialNumber.Int64() != 7 || e.ReasonCode != 1 || !e.RevocationTime.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("entry: %+v", e)
	}
	var gotInvalidity time.Time
	for _, x := range e.Extensions {
		if x.Id.Equal(oidInvalidityDate) {
			if _, err := asn1.Unmarshal(x.Value, &gotInvalidity); err != nil {
				t.Fatalf("invalidity date: %v", err)
			}
		}
	}
	if !gotInvalidity.Equal(t0.Add(-3 * time.Hour)) {
		t.Fatalf("invalidity date %v", gotInvalidity)
	}
	isDelta := false
	for _, x := range rl.Extensions {
		isDelta = isDelta || x.Id.Equal(oidDeltaCRLIndicator)
	}
	if !isDelta {
		t.Fatal("ASSUMPTION BROKEN: the delta CRL indicator is not visible in RevocationList.Extensions, so a delta CRL cannot be refused")
	}
	if !rl.ThisUpdate.Equal(t0) || !rl.NextUpdate.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("times: %v %v", rl.ThisUpdate, rl.NextUpdate)
	}
	// The stdlib does not apply the CRL to a certificate: Verify never consults it.
	l := root.leaf(t, leafKey(t), func(c *x509.Certificate) {
		c.SerialNumber = big.NewInt(7)
		c.EmailAddresses = []string{"a@bank.example"}
	})
	if err := root.verify(l, t0, x509.ExtKeyUsageAny); err != nil {
		t.Fatalf("Verify must not look at revocation: %v", err)
	}
}

// Assumption: a CRL signer must be a CA with the CRL-sign key usage; a leaf cannot issue one.
func TestCRLIssuerNeedsCRLSign(t *testing.T) {
	root := newCA(t, func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageCertSign }) // no CRLSign
	tpl := &x509.RevocationList{Number: big.NewInt(1), ThisUpdate: t0, NextUpdate: t0.Add(time.Hour)}
	if _, err := x509.CreateRevocationList(rand.Reader, tpl, root.cert, root.key); err == nil {
		t.Fatal("ASSUMPTION CHANGED: a CRL was created by a CA without the CRLSign key usage")
	}
}

// Assumption: Ed25519 leaf keys verify and sign certificates and CRLs the same way (the leaf key
// scheme is the decision's alg).
func TestEd25519Leaf(t *testing.T) {
	root := newCA(t, nil)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	l := root.leaf(t, pub, func(c *x509.Certificate) { c.EmailAddresses = []string{"alice@bank.example"} })
	if err := root.verify(l, t0, x509.ExtKeyUsageAny); err != nil {
		t.Fatal(err)
	}
	if got, ok := l.PublicKey.(ed25519.PublicKey); !ok || !got.Equal(pub) {
		t.Fatal("leaf public key is not the Ed25519 key")
	}
}

// Assumption: the SAN email is returned as issued, so the design's normalization (lowercase domain)
// is the verifier's job.
func TestEmailSANIsReturnedAsIssued(t *testing.T) {
	root := newCA(t, nil)
	l := root.leaf(t, leafKey(t), func(c *x509.Certificate) { c.EmailAddresses = []string{"Alice@Bank.Example"} })
	if len(l.EmailAddresses) != 1 || !strings.EqualFold(l.EmailAddresses[0], "alice@bank.example") || l.EmailAddresses[0] == "alice@bank.example" {
		t.Fatalf("emails: %v", l.EmailAddresses)
	}
}
