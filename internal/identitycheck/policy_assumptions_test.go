package identitycheck

import (
	"crypto/x509"
	"net/url"
	"testing"

	"github.com/bide-ai/bide/internal/testca"
)

func oid(t *testing.T, s string) x509.OID {
	t.Helper()
	o, err := x509.ParseOID(s)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// Assumption (design, "certificate policy along the chain"): Certificate.Verify does NOT enforce
// that a CA restricted to another certificate policy cannot issue a leaf asserting ours, even with
// VerifyOptions.CertificatePolicies set (the RFC 5280 policy tree may end empty without failing the
// path). bide therefore nests policies along the chain itself. If a Go release starts enforcing it,
// this fails to prompt a review of that code.
func TestVerifyDoesNotEnforceCAPolicyRestriction(t *testing.T) {
	P := testca.PolicyOID
	Q := oid(t, "1.3.6.1.4.1.99999.2.9")
	u, _ := url.Parse(testca.Person(1))
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{Mutate: func(c *x509.Certificate) { c.Policies = []x509.OID{Q} }})
	leaf := inter.NewLeaf(t, testca.Opts{Mutate: func(c *x509.Certificate) { c.URIs = []*url.URL{u} }})
	roots := x509.NewCertPool()
	roots.AddCert(root.Cert)
	pool := x509.NewCertPool()
	pool.AddCert(inter.Cert)
	_, err := leaf.Cert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: pool, CurrentTime: testca.T0,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}, CertificatePolicies: []x509.OID{P}})
	if err != nil {
		t.Fatalf("ASSUMPTION CHANGED: Verify now rejects a CA restricted to another policy (%v); review the policy nesting in audit/identity_cert.go", err)
	}
}

// Assumption: two trust anchors that are different certificates for one key give two verified chains.
// bide folds chains that differ only in such a certificate into one.
func TestTwoAnchorCertificatesForOneKeyGiveTwoChains(t *testing.T) {
	key := testca.NewKey(t, testca.P256)
	r1 := testca.NewRoot(t, "root", testca.Opts{KeyWith: key})
	r2 := testca.NewRoot(t, "root", testca.Opts{KeyWith: key})
	inter := r1.NewIntermediate(t, "inter", testca.Opts{})
	u, _ := url.Parse(testca.Person(1))
	leaf := inter.NewLeaf(t, testca.Opts{Mutate: func(c *x509.Certificate) { c.URIs = []*url.URL{u} }})
	roots := x509.NewCertPool()
	roots.AddCert(r1.Cert)
	roots.AddCert(r2.Cert)
	pool := x509.NewCertPool()
	pool.AddCert(inter.Cert)
	chains, err := leaf.Cert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: pool, CurrentTime: testca.T0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	if err != nil || len(chains) != 2 {
		t.Fatalf("chains %d, err %v", len(chains), err)
	}
}
