package identitycheck

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/fips140"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// caWith builds a self-signed CA from key with the given signature algorithm and returns a P-256
// leaf signed by it, so the tests below can exercise the CA algorithm allowlist of the design.
// createCertUnenforced and createCRLUnenforced make material the product must refuse (SHA-1
// signatures) with GODEBUG=fips140=only enforcement off; the code under test runs with it on.
func createCertUnenforced(tpl, parent *x509.Certificate, pub, priv any) (der []byte, err error) {
	fips140.WithoutEnforcement(func() { der, err = x509.CreateCertificate(rand.Reader, tpl, parent, pub, priv) })
	return der, err
}

func createCRLUnenforced(tpl *x509.RevocationList, issuer *x509.Certificate, priv crypto.Signer) (der []byte, err error) {
	fips140.WithoutEnforcement(func() { der, err = x509.CreateRevocationList(rand.Reader, tpl, issuer, priv) })
	return der, err
}

func caWith(t *testing.T, key crypto.Signer, alg x509.SignatureAlgorithm) (*x509.Certificate, *x509.Certificate) {
	t.Helper()
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "alg root"},
		NotBefore: t0.Add(-24 * time.Hour), NotAfter: t0.Add(24 * 365 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SignatureAlgorithm: alg,
	}
	der, err := createCertUnenforced(tpl, tpl, key.Public(), key)
	if err != nil {
		t.Fatalf("create CA with %v: %v", alg, err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ltpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "ignored"},
		NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, UnknownExtKeyUsage: nil,
		SignatureAlgorithm: alg,
	}
	ld, err := createCertUnenforced(ltpl, root, leafKey(t), key)
	if err != nil {
		t.Fatalf("create leaf with %v: %v", alg, err)
	}
	leaf, err := x509.ParseCertificate(ld)
	if err != nil {
		t.Fatal(err)
	}
	return root, leaf
}

func verifyAny(root, leaf *x509.Certificate) error {
	pool := x509.NewCertPool()
	pool.AddCert(root)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err
}

// Assumption (design, "CA signature algorithms"): P-384 ECDSA, RSA PKCS#1 v1.5 and RSA-PSS CAs with
// SHA-256 or better verify in Go, so the allowlist is a bide policy choice and not a stdlib limit.
func TestCAAlgorithmsAcceptedByAllowlistVerify(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r3072, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		key  crypto.Signer
		alg  x509.SignatureAlgorithm
	}{
		{"ecdsa-p384-sha384", p384, x509.ECDSAWithSHA384},
		{"rsa3072-pkcs1-sha256", r3072, x509.SHA256WithRSA},
		{"rsa3072-pss-sha256", r3072, x509.SHA256WithRSAPSS},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, leaf := caWith(t, c.key, c.alg)
			if err := verifyAny(root, leaf); err != nil {
				t.Fatalf("ASSUMPTION BROKEN: %s CA did not verify: %v", c.name, err)
			}
			if root.SignatureAlgorithm != c.alg {
				t.Fatalf("signature algorithm %v, want %v", root.SignatureAlgorithm, c.alg)
			}
		})
	}
}

// Assumption: Go refuses a SHA-1 certificate signature in Verify, so "never SHA-1 or MD5" holds even if
// the bide check were missed; the bide check is still needed for CRLs, whose signature goes through
// CheckSignatureFrom (see TestSHA1CRLSignatureHandling).
func TestSHA1CertificateSignatureIsRefused(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root, leaf := caWith(t, k, x509.ECDSAWithSHA1)
	err = verifyAny(root, leaf)
	if err == nil {
		t.Fatal("ASSUMPTION BROKEN: a SHA-1 signed chain verified")
	}
	t.Logf("SHA-1 chain: %v", err)
}

// Assumption: RSA modulus size is not a Go verification limit above 1024 bits (2048 verifies), so the
// 3072-bit minimum and the AllowRSA2048CA exception are enforced by bide from the public key.
func TestRSA2048CAVerifiesInGoSoBideMustEnforceModulus(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	root, leaf := caWith(t, k, x509.SHA256WithRSA)
	if err := verifyAny(root, leaf); err != nil {
		t.Fatalf("RSA-2048 CA: %v", err)
	}
	pub, ok := root.PublicKey.(*rsa.PublicKey)
	if !ok || pub.N.BitLen() != 2048 {
		t.Fatalf("modulus is readable from the certificate: %T", root.PublicKey)
	}
}

// Assumption (design, strict URI profile): Go returns a URI SAN exactly as issued: no case folding, no
// port or percent-decoding normalisation, no trailing-slash removal. Two spellings of one person's
// identifier are two different strings, so bide must refuse all but one spelling.
func TestURISANIsNotNormalisedByGo(t *testing.T) {
	root := newCA(t, nil)
	spellings := []string{
		"https://bank.example/person/0123456789abcdef0123456789abcdef",
		"https://bank.example:443/person/0123456789abcdef0123456789abcdef",
		"https://bank.example/person/0123456789abcdef0123456789abcdef/",
		"https://bank.example/person/0123456789ABCDEF0123456789abcdef",
		"https://bank.example/person/%30123456789abcdef0123456789abcdef",
		"https://user@bank.example/person/0123456789abcdef0123456789abcdef",
		"https://bank.example/person/0123456789abcdef0123456789abcdef?x=1",
	}
	seen := map[string]bool{}
	for _, s := range spellings {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		l := root.leaf(t, leafKey(t), func(c *x509.Certificate) { c.URIs = []*url.URL{u} })
		if len(l.URIs) != 1 {
			t.Fatalf("%s: %d URIs", s, len(l.URIs))
		}
		got := l.URIs[0].String()
		if got != s {
			t.Fatalf("ASSUMPTION BROKEN: Go normalised %q to %q", s, got)
		}
		seen[got] = true
	}
	if len(seen) != len(spellings) {
		t.Fatalf("spellings collapsed: %d of %d", len(seen), len(spellings))
	}
}

// Finding (Go 1.27.0): CheckSignatureFrom accepts a CRL signed with SHA-1, unlike certificate
// verification. The design therefore checks the CRL signature algorithm itself and does not rely on
// Go to refuse it. The test logs the behaviour and fails only if bide's check would become unneeded
// silently, so a Go change is noticed.
func TestSHA1CRLSignatureHandling(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := caWith(t, k, x509.ECDSAWithSHA256)
	tpl := &x509.RevocationList{
		Number: big.NewInt(1), ThisUpdate: t0, NextUpdate: t0.Add(time.Hour),
		SignatureAlgorithm: x509.ECDSAWithSHA1,
	}
	der, err := createCRLUnenforced(tpl, root, k)
	if err != nil {
		t.Logf("Go refuses to create a SHA-1 CRL: %v", err)
		return
	}
	rl, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Logf("Go refuses to parse a SHA-1 CRL: %v", err)
		return
	}
	// This tests Go's own x509 behaviour with SHA-1, which crypto/sha1 refuses under
	// GODEBUG=fips140=only, so the call runs with enforcement off (bide's check does not call it
	// for a SHA-1 CRL: TestAddCRLRefusals in audit covers that with enforcement on).
	var cerr error
	fips140.WithoutEnforcement(func() { cerr = rl.CheckSignatureFrom(root) })
	if err := cerr; err != nil {
		t.Fatalf("Go now refuses SHA-1 CRL signatures (%v): update the design note, bide's own check stays", err)
	}
	if rl.SignatureAlgorithm != x509.ECDSAWithSHA1 {
		t.Fatalf("algorithm %v is not exposed as ECDSAWithSHA1, bide's check cannot read it", rl.SignatureAlgorithm)
	}
}
