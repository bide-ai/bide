package audit_test

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

var idT0 = testca.T0

// fixture is root -> inter -> (leaf), with a policy that trusts the root, pins the intermediate and
// maps approver "alice" to testca.Person(1).
type fixture struct {
	root, inter *testca.CA
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := testca.NewRoot(t, "root", testca.Opts{})
	return &fixture{root: root, inter: root.NewIntermediate(t, "inter", testca.Opts{})}
}

func (f *fixture) policy() audit.IdentityPolicy {
	return audit.IdentityPolicy{
		Anchors:   []*x509.Certificate{f.root.Cert},
		Inter:     []audit.SPKIHash{audit.SPKIHashOf(f.inter.Cert)},
		Identity:  map[string]string{"alice": testca.Person(1), "bob": testca.Person(2)},
		IDKind:    audit.IDPersonURI,
		EKU:       testca.EKU,
		Policy:    testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
		MaxChain:  4,
	}
}

func (f *fixture) leaf(t *testing.T, mut func(*x509.Certificate)) *testca.Leaf {
	t.Helper()
	return f.inter.NewLeaf(t, testca.Opts{Mutate: mut})
}

func mustCompile(t *testing.T, p audit.IdentityPolicy) *audit.CertIdentity {
	t.Helper()
	c, err := p.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

// verify is VerifyChain at the default times: decided at T0, provable 5 s later.
func verifyAs(c *audit.CertIdentity, id string, l *testca.Leaf) (*audit.ApproverCert, error) {
	return c.VerifyChain(id, l.Chain(), idT0, idT0.Add(5*time.Second))
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var ce *audit.CertError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not an *audit.CertError", err)
	}
	if ce.Reason != reason {
		t.Fatalf("reason = %q (detail %q), want %q", ce.Reason, ce.Detail, reason)
	}
	if ce.Detail == "" {
		t.Fatalf("error for %q has no detail: the cause must be precise", reason)
	}
}

func TestValidChainCounts(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	l := f.leaf(t, nil)
	got, err := verifyAs(c, "alice", l)
	if err != nil {
		t.Fatalf("a valid chain: %v", err)
	}
	if got.Identity != testca.Person(1) || got.Scheme != audit.AlgECDSAP256 {
		t.Fatalf("identity %q scheme %q", got.Identity, got.Scheme)
	}
	if got.SeatKeyID() != "ident:"+testca.Person(1) {
		t.Fatalf("seat key id %q", got.SeatKeyID())
	}
	if len(got.Chain) != 3 || got.Chain[0] != got.Leaf || got.Anchor != audit.SPKIHashOf(f.root.Cert) {
		t.Fatalf("accepted chain has %d certificates, anchor %x", len(got.Chain), got.Anchor)
	}
	// The public key is in the encoding audit.NewVerifier reads, so the later signature check can use it.
	if _, err := audit.NewVerifier(got.Scheme, got.PublicKey); err != nil {
		t.Fatalf("public key does not build a verifier: %v", err)
	}
}

// The scheme is derived from the certificate's key, never from the record (algorithm-confusion guard).
func TestSchemeIsDerivedFromTheCertificateKey(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	for _, tc := range []struct {
		key  testca.KeyKind
		want audit.Alg
	}{{testca.P256, audit.AlgECDSAP256}, {testca.Ed25519, audit.AlgEd25519}, {testca.MLDSA65, audit.AlgMLDSA65}} {
		l := f.inter.NewLeaf(t, testca.Opts{Key: tc.key})
		got, err := verifyAs(c, "alice", l)
		if err != nil {
			t.Fatalf("key kind %d: %v", tc.key, err)
		}
		if got.Scheme != tc.want {
			t.Fatalf("key kind %d: scheme %q, want %q", tc.key, got.Scheme, tc.want)
		}
		if _, err := audit.NewVerifier(got.Scheme, got.PublicKey); err != nil {
			t.Fatalf("key kind %d: NewVerifier: %v", tc.key, err)
		}
	}
}

// Leaf key types a decision scheme cannot verify are refused.
func TestLeafKeyTypesWithoutADecisionSchemeAreRefused(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	for name, k := range map[string]testca.KeyKind{"p384": testca.P384, "p521": testca.P521, "rsa3072": testca.RSA3072} {
		_, err := verifyAs(c, "alice", f.inter.NewLeaf(t, testca.Opts{Key: k}))
		if err == nil {
			t.Fatalf("%s leaf accepted", name)
		}
		wantReason(t, err, agent.ReasonCertUsage)
	}
}

func TestWindow(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	l := f.leaf(t, nil) // valid T0-1h .. T0+1h
	at := func(decided, anchored time.Time) error {
		_, err := c.VerifyChain("alice", l.Chain(), decided, anchored)
		return err
	}
	if err := at(idT0, idT0.Add(time.Second)); err != nil {
		t.Fatalf("inside the window: %v", err)
	}
	for name, tc := range map[string]struct{ decided, anchored time.Time }{
		"anchored before notBefore":  {idT0.Add(-2 * time.Hour), idT0.Add(-2 * time.Hour)},
		"decided before notBefore":   {idT0.Add(-90 * time.Minute), idT0},
		"decided after notAfter":     {idT0.Add(2 * time.Hour), idT0.Add(2 * time.Hour)},
		"decided in the future of A": {idT0.Add(10 * time.Minute), idT0},
		"anchored long after expiry": {idT0.Add(30 * time.Minute), idT0.Add(time.Hour + 2*time.Minute)},
	} {
		err := at(tc.decided, tc.anchored)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantReason(t, err, agent.ReasonCertWindow)
	}
}

// decided_at <= A + skew: the default skew is 2 minutes, exactly at the edge counts and one
// nanosecond past does not.
func TestSkewEdge(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	l := f.leaf(t, nil)
	a := idT0
	if _, err := c.VerifyChain("alice", l.Chain(), a.Add(2*time.Minute), a); err != nil {
		t.Fatalf("decided_at = A + skew: %v", err)
	}
	_, err := c.VerifyChain("alice", l.Chain(), a.Add(2*time.Minute+time.Nanosecond), a)
	wantReason(t, err, agent.ReasonCertWindow)
}

// A <= notAfter + anchorGrace (default 60 s) and the leaf is checked at min(A, notAfter), so a
// decision anchored inside the grace after notAfter is not rejected by the standard library's own
// validity check, and one past the grace is.
func TestAnchorGraceAfterNotAfter(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	l := f.leaf(t, nil)
	na := l.Cert.NotAfter
	decided := na.Add(-30 * time.Second)
	if _, err := c.VerifyChain("alice", l.Chain(), decided, na.Add(60*time.Second)); err != nil {
		t.Fatalf("anchored exactly at notAfter + grace: %v", err)
	}
	_, err := c.VerifyChain("alice", l.Chain(), decided, na.Add(60*time.Second+time.Nanosecond))
	wantReason(t, err, agent.ReasonCertWindow)
}

// A CA certificate is checked at A, not at min(A, notAfter): a CA that expired before A fails even
// when the leaf is still inside its grace.
func TestIntermediateExpiredAtAnchorTimeFails(t *testing.T) {
	f := newFixture(t)
	inter := f.root.NewIntermediate(t, "short inter", testca.Opts{Mutate: func(c *x509.Certificate) {
		c.NotAfter = idT0.Add(30 * time.Second)
	}})
	p := f.policy()
	p.Inter = []audit.SPKIHash{audit.SPKIHashOf(inter.Cert)}
	c := mustCompile(t, p)
	l := inter.NewLeaf(t, testca.Opts{Mutate: func(c *x509.Certificate) { c.NotAfter = idT0.Add(time.Hour) }})
	if _, err := c.VerifyChain("alice", l.Chain(), idT0, idT0.Add(10*time.Second)); err != nil {
		t.Fatalf("CA still valid at A: %v", err)
	}
	_, err := c.VerifyChain("alice", l.Chain(), idT0, idT0.Add(45*time.Second))
	if err == nil {
		t.Fatal("a CA past its notAfter at A was accepted")
	}
	wantReason(t, err, agent.ReasonCertWindow)
}

// The standard library checks the CA at min(A, leaf notAfter); a CA that expired between the leaf's
// notAfter and A (inside the anchor grace) is caught by bide's own check at A.
func TestIntermediateExpiredInsideTheGraceFails(t *testing.T) {
	f := newFixture(t)
	inter := f.root.NewIntermediate(t, "inter", testca.Opts{Mutate: func(c *x509.Certificate) { c.NotAfter = idT0.Add(20 * time.Second) }})
	p := f.policy()
	p.Inter = []audit.SPKIHash{audit.SPKIHashOf(inter.Cert)}
	c := mustCompile(t, p)
	l := inter.NewLeaf(t, testca.Opts{Mutate: func(c *x509.Certificate) { c.NotAfter = idT0.Add(10 * time.Second) }})
	decided := idT0
	if _, err := c.VerifyChain("alice", l.Chain(), decided, idT0.Add(15*time.Second)); err != nil {
		t.Fatalf("both valid at A: %v", err)
	}
	// A = T0+40s: the leaf is 30 s past notAfter (inside the 60 s grace), the CA 20 s past its own.
	_, err := c.VerifyChain("alice", l.Chain(), decided, idT0.Add(40*time.Second))
	if err == nil {
		t.Fatal("a CA that had expired at A was accepted")
	}
	wantReason(t, err, agent.ReasonCertWindow)
}

func TestIdentity(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	u := func(s string) *url.URL {
		x, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	for name, tc := range map[string]struct {
		id  string
		mut func(*x509.Certificate)
	}{
		"another person":         {"alice", func(c *x509.Certificate) { c.URIs = []*url.URL{u(testca.Person(2))} }},
		"no san":                 {"alice", func(c *x509.Certificate) { c.URIs = nil }},
		"two uris":               {"alice", func(c *x509.Certificate) { c.URIs = []*url.URL{u(testca.Person(1)), u(testca.Person(2))} }},
		"uri and email":          {"alice", func(c *x509.Certificate) { c.EmailAddresses = []string{"alice@bank.example"} }},
		"uri and dns":            {"alice", func(c *x509.Certificate) { c.DNSNames = []string{"alice.bank.example"} }},
		"uri and ip":             {"alice", func(c *x509.Certificate) { c.IPAddresses = []net.IP{net.ParseIP("192.0.2.1")} }},
		"dns only":               {"alice", func(c *x509.Certificate) { c.URIs = nil; c.DNSNames = []string{"alice.bank.example"} }},
		"email under a uri rule": {"alice", func(c *x509.Certificate) { c.URIs = nil; c.EmailAddresses = []string{"alice@bank.example"} }},
		"unknown approver id":    {"mallory", nil},
		"other approver's id":    {"bob", nil}, // the certificate is alice's
	} {
		_, err := verifyAs(c, tc.id, f.leaf(t, tc.mut))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantReason(t, err, agent.ReasonCertIdentity)
	}
}

// The Subject DN is free text and plays no part: a CN equal to the identifier does not stand in for
// the SAN.
func TestSubjectCommonNameIsNotAnIdentity(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	l := f.leaf(t, func(c *x509.Certificate) {
		c.URIs = nil
		c.Subject = pkix.Name{CommonName: testca.Person(1)}
	})
	_, err := verifyAs(c, "alice", l)
	wantReason(t, err, agent.ReasonCertIdentity)
}

// A SAN of a kind Go does not expose (otherName) still counts as a second name.
func TestOtherNameSANIsRefused(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	other, _ := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true,
		Bytes: append(mustMarshal(t, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 9}),
			mustMarshal(t, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: mustMarshal(t, "x")})...)})
	uri := mustMarshal(t, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 6, Bytes: []byte(testca.Person(1))})
	san := mustMarshal(t, asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: append(uri, other...)})
	l := f.leaf(t, func(c *x509.Certificate) {
		c.URIs = nil
		c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 17}, Value: san}}
	})
	if len(l.Cert.URIs) != 1 {
		t.Fatalf("test setup: the parsed certificate has %d URIs", len(l.Cert.URIs))
	}
	_, err := verifyAs(c, "alice", l)
	wantReason(t, err, agent.ReasonCertIdentity)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := asn1.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// URI identifiers use a strict profile: one spelling per person.
func TestURIProfileRefusesOtherSpellings(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	good := testca.Person(1)
	for name, spelling := range map[string]string{
		"uppercase host":     strings.Replace(good, "idp.bank.example", "IDP.bank.example", 1),
		"uppercase hex":      strings.ToUpper(good[:len("https://idp.bank.example/person/")]) + good[len("https://idp.bank.example/person/"):],
		"explicit port":      strings.Replace(good, "example/", "example:443/", 1),
		"trailing slash":     good + "/",
		"query":              good + "?x=1",
		"fragment":           good + "#x",
		"userinfo":           strings.Replace(good, "https://", "https://u@", 1),
		"percent encoding":   strings.Replace(good, "/person/", "/%70erson/", 1),
		"other host":         strings.Replace(good, "idp.bank.example", "idp.evil.example", 1),
		"other path":         strings.Replace(good, "/person/", "/user/", 1),
		"short id":           good[:len(good)-1],
		"scheme http":        strings.Replace(good, "https", "http", 1),
		"no host (urn)":      "urn:bank:person:1",
		"dot segment":        strings.Replace(good, "/person/", "/person/../person/", 1),
		"double slash":       strings.Replace(good, "/person/", "//person/", 1),
		"non-ascii in path":  good[:len(good)-1] + "é",
		"empty authority":    "https:///person/" + strings.Repeat("0", 31) + "1",
		"id with whitespace": good[:len(good)-1] + " ",
	} {
		l, terr := f.inter.TryLeaf(t, testca.Opts{Mutate: func(c *x509.Certificate) {
			c.URIs = []*url.URL{{Scheme: "x"}}
			c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 17},
				Value: mustMarshal(t, asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true,
					Bytes: mustMarshal(t, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 6, Bytes: []byte(spelling)})})}}
		}})
		if terr != nil {
			t.Logf("%s: the standard library already refuses this spelling: %v", name, terr)
			continue
		}
		_, err := verifyAs(c, "alice", l)
		if err == nil {
			t.Fatalf("%s (%q): accepted", name, spelling)
		}
		// Anything but the exact identifier fails closed. Usually the identity reason; Go itself may
		// refuse a malformed URI earlier, as a chain failure.
		var ce *audit.CertError
		if !errors.As(err, &ce) || (ce.Reason != agent.ReasonCertIdentity && ce.Reason != agent.ReasonCertChain) {
			t.Fatalf("%s: unexpected error %v", name, err)
		}
	}
	// The exact spelling does count.
	if _, err := verifyAs(c, "alice", f.leaf(t, nil)); err != nil {
		t.Fatal(err)
	}
}

func emailPolicy(f *fixture) audit.IdentityPolicy {
	p := f.policy()
	p.IDKind = audit.IDEmail
	p.Identity = map[string]string{"alice": "alice@bank.example", "bob": "Bob@Bank.Example"}
	return p
}

func TestEmailKindCanonicalisation(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, emailPolicy(f))
	mail := func(addr string) func(*x509.Certificate) {
		return func(c *x509.Certificate) { c.URIs = nil; c.EmailAddresses = []string{addr} }
	}
	// Case is folded on both sides: the map value "Bob@Bank.Example" and the SAN "bob@bank.example" agree.
	for _, tc := range []struct{ id, san string }{{"alice", "alice@bank.example"}, {"alice", "ALICE@Bank.Example"}, {"bob", "bob@bank.example"}} {
		got, err := verifyAs(c, tc.id, f.leaf(t, mail(tc.san)))
		if err != nil {
			t.Fatalf("%s %q: %v", tc.id, tc.san, err)
		}
		if got.Identity != strings.ToLower(got.Identity) {
			t.Fatalf("identity %q is not folded", got.Identity)
		}
	}
	// No alias folding or trimming: a.smith and asmith are two identities.
	_, err := verifyAs(c, "alice", f.leaf(t, mail("a.lice@bank.example")))
	wantReason(t, err, agent.ReasonCertIdentity)
	// ASCII only is covered by TestCanonicalizer: the standard library will not even encode such a SAN.
}

func TestCanonicalizer(t *testing.T) {
	cz := audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"})
	if cz.Name() == "" || cz.Version() < 1 {
		t.Fatalf("a canonicalizer names itself and its version: %q v%d", cz.Name(), cz.Version())
	}
	for in, want := range map[string]string{"Alice@Bank.Example": "alice@bank.example", "a+b@x.example": "a+b@x.example"} {
		got, err := cz.Canonical(audit.IDEmail, in)
		if err != nil || got != want {
			t.Fatalf("Canonical(email, %q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "alice", "@bank.example", "alice@", "a@b@c", "alice@bank.example ", " alice@bank.example", "al ice@bank.example", "alice@bank..example", "alice@-bank.example", "alicé@bank.example", "alice@bank.example\x00", "<alice@bank.example>", "alice@bank.example\n"} {
		if got, err := cz.Canonical(audit.IDEmail, bad); err == nil {
			t.Fatalf("Canonical(email, %q) = %q, want an error", bad, got)
		}
	}
	if _, err := cz.Canonical(audit.IDKind("dns"), "x"); err == nil {
		t.Fatal("an unknown kind canonicalised")
	}
	// The URI kind is the identity function on the one accepted spelling.
	if got, err := cz.Canonical(audit.IDPersonURI, testca.Person(7)); err != nil || got != testca.Person(7) {
		t.Fatalf("person uri: %q, %v", got, err)
	}
	uuid := "https://idp.bank.example/person/123e4567-e89b-42d3-a456-426614174000"
	if got, err := cz.Canonical(audit.IDPersonURI, uuid); err != nil || got != uuid {
		t.Fatalf("lowercase uuid path: %q, %v", got, err)
	}
	if _, err := cz.Canonical(audit.IDPersonURI, strings.ToUpper(uuid)); err == nil {
		t.Fatal("uppercase uuid accepted")
	}
	// A URI canonicalizer needs its host.
	nohost := audit.NewCanonicalizer(audit.CanonicalConfig{})
	if _, err := nohost.Canonical(audit.IDPersonURI, testca.Person(1)); err == nil {
		t.Fatal("a canonicalizer with no configured host accepted a URI")
	}
	// Idempotent: a canonical form canonicalises to itself.
	once, _ := cz.Canonical(audit.IDEmail, "Mixed@Case.Example")
	twice, _ := cz.Canonical(audit.IDEmail, once)
	if once != twice {
		t.Fatalf("not idempotent: %q then %q", once, twice)
	}
}

func TestNameConstraints(t *testing.T) {
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{Mutate: func(c *x509.Certificate) {
		c.PermittedURIDomains = []string{"idp.bank.example"}
		c.PermittedEmailAddresses = []string{"bank.example"}
		c.PermittedDNSDomains = []string{"bank.example"}
		c.PermittedDNSDomainsCritical = true
	}})
	f := &fixture{root: root, inter: inter}
	c := mustCompile(t, f.policy())
	if _, err := verifyAs(c, "alice", f.leaf(t, nil)); err != nil {
		t.Fatalf("inside the constraints: %v", err)
	}
	// The identity map says evil.example's person 1 is alice; the CA's constraint still stops it.
	p := f.policy()
	p.Identity = map[string]string{"alice": "https://idp.evil.example/person/" + strings.Repeat("0", 31) + "1"}
	p.Canonical = audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.evil.example"})
	c2 := mustCompile(t, p)
	l := f.leaf(t, func(c *x509.Certificate) {
		u, _ := url.Parse(p.Identity["alice"])
		c.URIs = []*url.URL{u}
	})
	_, err := verifyAs(c2, "alice", l)
	wantReason(t, err, agent.ReasonCertChain)
}

func TestKeyUsageAndExtendedKeyUsage(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	for name, mut := range map[string]func(*x509.Certificate){
		"no key usage":          func(c *x509.Certificate) { c.KeyUsage = 0 },
		"key encipherment only": func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageKeyEncipherment },
		"no eku":                func(c *x509.Certificate) { c.UnknownExtKeyUsage = nil },
		"another private eku": func(c *x509.Certificate) {
			c.UnknownExtKeyUsage = []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 99999, 1, 2}}
		},
		"server auth only": func(c *x509.Certificate) {
			c.UnknownExtKeyUsage = nil
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		},
		"any eku is not the oid": func(c *x509.Certificate) {
			c.UnknownExtKeyUsage = nil
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
		},
		"email protection, no flag": func(c *x509.Certificate) {
			c.UnknownExtKeyUsage = nil
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection}
		},
		"leaf marked CA": func(c *x509.Certificate) {
			c.IsCA = true
			c.BasicConstraintsValid = true
			c.KeyUsage |= x509.KeyUsageCertSign
		},
		"no policy": func(c *x509.Certificate) { c.Policies = nil },
		"another policy": func(c *x509.Certificate) {
			o, _ := x509.ParseOID("1.3.6.1.4.1.99999.2.2")
			c.Policies = []x509.OID{o}
		},
	} {
		_, err := verifyAs(c, "alice", f.leaf(t, mut))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantReason(t, err, agent.ReasonCertUsage)
	}
	// Extra EKUs next to the required one do not matter; the required one must be present.
	l := f.leaf(t, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} })
	if _, err := verifyAs(c, "alice", l); err != nil {
		t.Fatalf("required EKU next to clientAuth: %v", err)
	}
}

func TestEmailProtectionEKUOnlyOnADedicatedCA(t *testing.T) {
	f := newFixture(t)
	smime := func(c *x509.Certificate) {
		c.UnknownExtKeyUsage = nil
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection}
	}
	p := f.policy()
	p.AllowEmailProtectionEKU = true
	c := mustCompile(t, p)
	if _, err := verifyAs(c, "alice", f.leaf(t, smime)); err != nil {
		t.Fatalf("emailProtection with the flag: %v", err)
	}
	// An intermediate scoped to something else does not permit it.
	inter := f.root.NewIntermediate(t, "tls only", testca.Opts{Mutate: func(c *x509.Certificate) {
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}})
	p.Inter = []audit.SPKIHash{audit.SPKIHashOf(inter.Cert)}
	_, err := mustCompile(t, p).VerifyChain("alice", inter.NewLeaf(t, testca.Opts{Mutate: smime}).Chain(), idT0, idT0)
	wantReason(t, err, agent.ReasonCertUsage)
}

// Certificate.Verify with ExtKeyUsageAny does not nest EKUs along the chain, so bide does.
func TestIntermediateEKUNesting(t *testing.T) {
	f := newFixture(t)
	for name, tc := range map[string]struct {
		eku  []x509.ExtKeyUsage
		unk  []asn1.ObjectIdentifier
		want string
	}{
		"no eku on the CA":         {nil, nil, ""},
		"any eku on the CA":        {[]x509.ExtKeyUsage{x509.ExtKeyUsageAny}, nil, ""},
		"the oid on the CA":        {nil, []asn1.ObjectIdentifier{testca.EKU}, ""},
		"server auth only":         {[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil, agent.ReasonCertUsage},
		"another private oid":      {nil, []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 99999, 1, 7}}, agent.ReasonCertUsage},
		"server auth plus our oid": {[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []asn1.ObjectIdentifier{testca.EKU}, ""},
	} {
		inter := f.root.NewIntermediate(t, "inter-"+name, testca.Opts{Mutate: func(c *x509.Certificate) {
			c.ExtKeyUsage, c.UnknownExtKeyUsage = tc.eku, tc.unk
		}})
		p := f.policy()
		p.Inter = []audit.SPKIHash{audit.SPKIHashOf(inter.Cert)}
		_, err := mustCompile(t, p).VerifyChain("alice", inter.NewLeaf(t, testca.Opts{}).Chain(), idT0, idT0)
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantReason(t, err, tc.want)
	}
}

func TestTrustAnchorsAndPinnedIntermediates(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	l := f.leaf(t, nil)

	// A root the policy does not trust.
	otherRoot := testca.NewRoot(t, "other root", testca.Opts{})
	otherInter := otherRoot.NewIntermediate(t, "other inter", testca.Opts{})
	_, err := verifyAs(c, "alice", otherInter.NewLeaf(t, testca.Opts{}))
	wantReason(t, err, agent.ReasonCertChain)

	// The right root, but this gate's anchors are another set.
	p := f.policy()
	p.Anchors = []*x509.Certificate{otherRoot.Cert}
	_, err = verifyAs(mustCompile(t, p), "alice", l)
	wantReason(t, err, agent.ReasonCertChain)

	// An intermediate that is not pinned, though it chains to the anchor.
	rogue := f.root.NewIntermediate(t, "rogue", testca.Opts{})
	_, err = verifyAs(c, "alice", rogue.NewLeaf(t, testca.Opts{}))
	wantReason(t, err, agent.ReasonCertChain)

	// A carried root is ignored (the policy's anchors decide); a carried junk certificate is ignored.
	if _, err := c.VerifyChain("alice", l.ChainWithRoot(), idT0, idT0); err != nil {
		t.Fatalf("a carried root: %v", err)
	}
	if _, err := c.VerifyChain("alice", append(l.Chain(), rogue.DER), idT0, idT0); err != nil {
		t.Fatalf("a carried unpinned certificate must be ignored: %v", err)
	}

	// A carried self-signed lookalike root with the same name does not become an anchor.
	fake := testca.NewRoot(t, "root", testca.Opts{})
	fakeInter := fake.NewIntermediate(t, "inter", testca.Opts{})
	pFake := f.policy()
	pFake.Inter = []audit.SPKIHash{audit.SPKIHashOf(fakeInter.Cert)}
	_, err = mustCompile(t, pFake).VerifyChain("alice", fakeInter.NewLeaf(t, testca.Opts{}).ChainWithRoot(), idT0, idT0)
	wantReason(t, err, agent.ReasonCertChain)
}

func TestChainLength(t *testing.T) {
	f := newFixture(t)
	i2 := f.inter.NewIntermediate(t, "inter2", testca.Opts{})
	p := f.policy()
	p.Inter = append(p.Inter, audit.SPKIHashOf(i2.Cert))
	l := i2.NewLeaf(t, testca.Opts{}) // chain: leaf, i2, inter (+ root)
	if _, err := mustCompile(t, p).VerifyChain("alice", l.Chain(), idT0, idT0); err != nil {
		t.Fatalf("a four-certificate path inside MaxChain 4: %v", err)
	}
	p.MaxChain = 3
	_, err := mustCompile(t, p).VerifyChain("alice", l.Chain(), idT0, idT0)
	wantReason(t, err, agent.ReasonCertChain)
	// MaxChain also bounds what is carried, so a padded chain is refused before any parsing work.
	p.MaxChain = 2
	_, err = mustCompile(t, p).VerifyChain("alice", append(l.Chain(), l.Chain()...), idT0, idT0)
	wantReason(t, err, agent.ReasonCertChain)
}

// A reissued intermediate for one key, both pinned, gives two valid chains: ambiguity is refused.
func TestTwoValidChainsAreRefused(t *testing.T) {
	root := testca.NewRoot(t, "root", testca.Opts{})
	key := testca.NewKey(t, testca.P256)
	i1 := root.NewIntermediate(t, "inter", testca.Opts{KeyWith: key})
	i2 := root.NewIntermediate(t, "inter", testca.Opts{KeyWith: key}) // same key, new certificate
	f := &fixture{root: root, inter: i1}
	p := f.policy()
	// Pinning is by key, so one pin admits both certificates.
	p.Inter = []audit.SPKIHash{audit.SPKIHashOf(i1.Cert)}
	if audit.SPKIHashOf(i1.Cert) != audit.SPKIHashOf(i2.Cert) {
		t.Fatal("test setup: the two intermediates must share a key")
	}
	l := i1.NewLeaf(t, testca.Opts{})
	chain := [][]byte{l.DER, i1.DER, i2.DER}
	_, err := mustCompile(t, p).VerifyChain("alice", chain, idT0, idT0)
	wantReason(t, err, agent.ReasonCertChain)
	// Carrying only one of them is not ambiguous.
	if _, err := mustCompile(t, p).VerifyChain("alice", [][]byte{l.DER, i1.DER}, idT0, idT0); err != nil {
		t.Fatalf("one certificate for the key: %v", err)
	}
}

func TestDistrustAfter(t *testing.T) {
	f := newFixture(t)
	l := f.leaf(t, nil)
	for name, key := range map[string]*x509.Certificate{"anchor": f.root.Cert, "intermediate": f.inter.Cert} {
		p := f.policy()
		p.Distrust = map[audit.SPKIHash]time.Time{audit.SPKIHashOf(key): idT0.Add(10 * time.Second)}
		c := mustCompile(t, p)
		// A decision first provable before the distrust time is unaffected: history is not voided.
		if _, err := c.VerifyChain("alice", l.Chain(), idT0, idT0.Add(9*time.Second)); err != nil {
			t.Fatalf("%s: before DistrustAfter: %v", name, err)
		}
		// At or after it, the CA no longer supports a counted decision.
		for _, a := range []time.Duration{10 * time.Second, 30 * time.Second} {
			_, err := c.VerifyChain("alice", l.Chain(), idT0, idT0.Add(a))
			if err == nil {
				t.Fatalf("%s: accepted at A = distrust + %v", name, a-10*time.Second)
			}
			wantReason(t, err, agent.ReasonCertChain)
		}
	}
}

func TestCAAlgorithmProfile(t *testing.T) {
	type tc struct {
		name string
		key  testca.KeyKind
		sig  x509.SignatureAlgorithm
		ok   bool
	}
	for _, c := range []tc{
		{"ecdsa p-256", testca.P256, 0, true},
		{"ecdsa p-384 sha-384", testca.P384, x509.ECDSAWithSHA384, true},
		{"ecdsa p-521", testca.P521, 0, false},
		{"ed25519", testca.Ed25519, 0, true},
		{"ml-dsa-65", testca.MLDSA65, 0, true},
		{"rsa 3072 pkcs1 sha-256", testca.RSA3072, x509.SHA256WithRSA, true},
		{"rsa 3072 pss sha-256", testca.RSA3072, x509.SHA256WithRSAPSS, true},
		{"rsa 2048 without the exception", testca.RSA2048, x509.SHA256WithRSA, false},
		{"sha-1 rsa", testca.RSA3072, x509.SHA1WithRSA, false},
	} {
		root := testca.NewRoot(t, "root", testca.Opts{Key: c.key, SigAlg: c.sig})
		inter := root.NewIntermediate(t, "inter", testca.Opts{Key: c.key, SigAlg: c.sig})
		f := &fixture{root: root, inter: inter}
		l := inter.NewLeaf(t, testca.Opts{SigAlg: c.sig})
		cp, err := f.policy().Compile()
		if err != nil {
			if c.ok {
				t.Fatalf("%s: Compile: %v", c.name, err)
			}
			continue // refused already at configuration time
		}
		_, err = verifyAs(cp, "alice", l)
		if c.ok && err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !c.ok {
			if err == nil {
				t.Fatalf("%s: accepted", c.name)
			}
			wantReason(t, err, agent.ReasonCertChain)
		}
	}
}

func TestRSA2048CAOnlyThroughADatedException(t *testing.T) {
	root := testca.NewRoot(t, "root", testca.Opts{Key: testca.RSA2048, SigAlg: x509.SHA256WithRSA})
	inter := root.NewIntermediate(t, "inter", testca.Opts{Key: testca.RSA2048, SigAlg: x509.SHA256WithRSA})
	f := &fixture{root: root, inter: inter}
	l := inter.NewLeaf(t, testca.Opts{SigAlg: x509.SHA256WithRSA})
	p := f.policy()
	p.AllowRSA2048CAUntil = idT0.Add(24 * time.Hour)
	c, err := p.Compile()
	if err != nil {
		t.Fatalf("Compile with the exception: %v", err)
	}
	if _, err := c.VerifyChain("alice", l.Chain(), idT0, idT0); err != nil {
		t.Fatalf("RSA-2048 CA inside the exception: %v", err)
	}
	// The exception has an end date, applied against A: after it the same chain does not count.
	_, err = c.VerifyChain("alice", l.Chain(), idT0, idT0.Add(24*time.Hour+time.Second))
	if err == nil {
		t.Fatal("RSA-2048 CA accepted after the exception ended")
	}
	wantReason(t, err, agent.ReasonCertChain)
}

func TestCarriedGarbage(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	l := f.leaf(t, nil)
	_, err := c.VerifyChain("alice", nil, idT0, idT0)
	wantReason(t, err, agent.ReasonNoCert)
	_, err = c.VerifyChain("alice", [][]byte{}, idT0, idT0)
	wantReason(t, err, agent.ReasonNoCert)
	_, err = c.VerifyChain("alice", [][]byte{nil}, idT0, idT0)
	wantReason(t, err, agent.ReasonCertChain)
	_, err = c.VerifyChain("alice", [][]byte{[]byte("not a certificate")}, idT0, idT0)
	wantReason(t, err, agent.ReasonCertChain)
	_, err = c.VerifyChain("alice", [][]byte{append(append([]byte{}, l.DER...), 0)}, idT0, idT0)
	wantReason(t, err, agent.ReasonCertChain)
	_, err = c.VerifyChain("alice", [][]byte{l.DER, []byte{0x30, 0x00}}, idT0, idT0)
	wantReason(t, err, agent.ReasonCertChain)
	_, err = c.VerifyChain("alice", [][]byte{make([]byte, 1<<20)}, idT0, idT0)
	wantReason(t, err, agent.ReasonCertChain)
}

func TestSerialNumberLength(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	for name, tc := range map[string]struct {
		serial *big.Int
		ok     bool
	}{
		"20 octets": {new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 159), big.NewInt(1)), true},
		"21 octets": {new(big.Int).Lsh(big.NewInt(1), 8*20+1), false},
	} {
		tpl := &x509.Certificate{
			SerialNumber: tc.serial, Subject: pkix.Name{CommonName: "x"},
			NotBefore: idT0.Add(-time.Hour), NotAfter: idT0.Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, UnknownExtKeyUsage: []asn1.ObjectIdentifier{testca.EKU},
			Policies: []x509.OID{testca.PolicyOID}, URIs: []*url.URL{mustURL(t, testca.Person(1))},
		}
		key := testca.NewKey(t, testca.P256)
		der, err := x509.CreateCertificate(rand.Reader, tpl, f.inter.Cert, key.Public(), f.inter.Key)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, err = c.VerifyChain("alice", [][]byte{der, f.inter.DER}, idT0, idT0)
		if tc.ok && err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The error text is for operators and logs: precise, and without secret material.
func TestErrorsNameTheCauseAndNoKeyMaterial(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	l := f.leaf(t, func(c *x509.Certificate) { c.UnknownExtKeyUsage = nil })
	_, err := verifyAs(c, "alice", l)
	if err == nil {
		t.Fatal("accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "1.3.6.1.4.1.99999.1.1") {
		t.Fatalf("the error does not name the missing extended key usage: %q", msg)
	}
	for _, secret := range []string{"PRIVATE", "BEGIN"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error mentions %q: %q", secret, msg)
		}
	}
}

// Two certificates for one key give the same KeyHash, so the later dynamic one-key-one-seat check
// can see a key under two identities.
func TestKeyHashIsStableAcrossCertificates(t *testing.T) {
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	key := testca.NewKey(t, testca.P256)
	a, err := verifyAs(c, "alice", f.inter.NewLeaf(t, testca.Opts{KeyWith: key}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := verifyAs(c, "alice", f.inter.NewLeaf(t, testca.Opts{KeyWith: key}))
	if err != nil {
		t.Fatal(err)
	}
	if a.KeyHash != b.KeyHash || a.LeafHash == b.LeafHash {
		t.Fatalf("key hash equal=%v, leaf hash equal=%v", a.KeyHash == b.KeyHash, a.LeafHash == b.LeafHash)
	}
	other, _ := verifyAs(c, "alice", f.leaf(t, nil))
	if other.KeyHash == a.KeyHash {
		t.Fatal("different keys, same hash")
	}
}

func TestCompileRefusesBadPolicies(t *testing.T) {
	f := newFixture(t)
	oid, _ := x509.ParseOID("1.2.3")
	_ = oid
	mut := map[string]func(*audit.IdentityPolicy){
		"no anchors":           func(p *audit.IdentityPolicy) { p.Anchors = nil },
		"non-CA anchor":        func(p *audit.IdentityPolicy) { p.Anchors = []*x509.Certificate{f.leaf(t, nil).Cert} },
		"nil anchor":           func(p *audit.IdentityPolicy) { p.Anchors = []*x509.Certificate{nil} },
		"no identity":          func(p *audit.IdentityPolicy) { p.Identity = nil },
		"empty identity value": func(p *audit.IdentityPolicy) { p.Identity = map[string]string{"alice": ""} },
		"empty approver id":    func(p *audit.IdentityPolicy) { p.Identity = map[string]string{"": testca.Person(1)} },
		"non-canonical identity": func(p *audit.IdentityPolicy) {
			p.Identity = map[string]string{"alice": strings.ToUpper(testca.Person(1))}
		},
		"one person, two ids": func(p *audit.IdentityPolicy) {
			p.Identity = map[string]string{"alice": testca.Person(1), "alice2": testca.Person(1)}
		},
		"unknown kind":               func(p *audit.IdentityPolicy) { p.IDKind = "dn" },
		"no kind":                    func(p *audit.IdentityPolicy) { p.IDKind = "" },
		"no eku":                     func(p *audit.IdentityPolicy) { p.EKU = nil },
		"no policy oid":              func(p *audit.IdentityPolicy) { p.Policy = x509.OID{} },
		"uri kind, no canonicalizer": func(p *audit.IdentityPolicy) { p.Canonical = nil },
		"max chain too large":        func(p *audit.IdentityPolicy) { p.MaxChain = 9 },
		"negative max chain":         func(p *audit.IdentityPolicy) { p.MaxChain = -1 },
		"negative skew":              func(p *audit.IdentityPolicy) { p.Skew = -time.Second },
		"negative anchor grace":      func(p *audit.IdentityPolicy) { p.AnchorGrace = -time.Second },
		"unpinned: no intermediates and a weak anchor key": func(p *audit.IdentityPolicy) {
			weak := testca.NewRoot(t, "weak", testca.Opts{Key: testca.RSA2048, SigAlg: x509.SHA256WithRSA})
			p.Anchors = []*x509.Certificate{weak.Cert}
		},
	}
	for name, m := range mut {
		p := f.policy()
		m(&p)
		_, err := p.Compile()
		if err == nil {
			t.Fatalf("%s: Compile accepted", name)
		}
		if !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("%s: error %v is not ErrConfig", name, err)
		}
	}
	// A policy with no Inter is valid: the chain then runs leaf to anchor directly.
	p := f.policy()
	p.Inter = nil
	if _, err := p.Compile(); err != nil {
		t.Fatalf("no pinned intermediates: %v", err)
	}
	// Defaults: MaxChain 0 means 4.
	p = f.policy()
	p.MaxChain = 0
	if _, err := p.Compile(); err != nil {
		t.Fatalf("MaxChain 0: %v", err)
	}
}

// Case-only differences in an email map are one person.
func TestCompileFoldsEmailCaseForInjectivity(t *testing.T) {
	f := newFixture(t)
	p := emailPolicy(f)
	p.Identity = map[string]string{"alice": "Alice@bank.example", "alice-2": "alice@BANK.example"}
	_, err := p.Compile()
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("two spellings of one mailbox: %v", err)
	}
}

func TestPolicyIsSnapshottedAtCompile(t *testing.T) {
	f := newFixture(t)
	p := f.policy()
	c := mustCompile(t, p)
	// Mutating the caller's maps and slices after Compile changes nothing.
	p.Identity["alice"] = testca.Person(2)
	p.Identity["bob"] = testca.Person(1)
	p.Inter[0] = audit.SPKIHash{}
	if _, err := verifyAs(c, "alice", f.leaf(t, nil)); err != nil {
		t.Fatalf("compiled policy followed a later mutation: %v", err)
	}
}
