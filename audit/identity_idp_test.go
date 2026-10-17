package audit_test

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// The issuing distribution point (IDP) rule. A CRL whose IDP holds only a distributionPoint fullName of
// URIs is read, but RFC 5280 5.2.5 and 6.3.3 make it cover only the certificates whose CRL
// distribution points extension names that same point, so a certificate gets status from it only when
// one of its distribution points (with no reasons and no cRLIssuer) holds one of the IDP's URIs, byte
// for byte. Every other IDP (a flag, a reasons field, a relative name, a non-URI name, a non-critical
// IDP, an empty one) is still refused. step-ca stamps exactly the accepted shape on every CRL it signs
// (testdata/stepca).

var (
	oidIDP = asn1.ObjectIdentifier{2, 5, 29, 28}
	oidCDP = asn1.ObjectIdentifier{2, 5, 29, 31}
)

// der marshals v or fails the test.
func der(t *testing.T, v any, params string) []byte {
	t.Helper()
	b, err := asn1.MarshalWithParams(v, params)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// uriName is a GeneralName uniformResourceIdentifier ([6], IA5String).
func uriName(u string) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 6, Bytes: []byte(u)}
}

// fullName is DistributionPointName.fullName ([0] GeneralNames) holding names.
func fullName(t *testing.T, names ...asn1.RawValue) asn1.RawValue {
	t.Helper()
	var body []byte
	for _, n := range names {
		body = append(body, der(t, n, "")...)
	}
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: body}
}

// dpName is the [0] distributionPoint field of an IDP or a DistributionPoint, wrapping name.
func dpName(t *testing.T, name asn1.RawValue) asn1.RawValue {
	t.Helper()
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: der(t, name, "")}
}

// seq is a SEQUENCE of the given elements.
func seq(t *testing.T, elems ...asn1.RawValue) []byte {
	t.Helper()
	var body []byte
	for _, e := range elems {
		body = append(body, der(t, e, "")...)
	}
	return der(t, asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: body}, "")
}

// idpOf is the value of an IDP extension holding only the fullName of the given URIs.
func idpOf(t *testing.T, uris ...string) []byte {
	t.Helper()
	var names []asn1.RawValue
	for _, u := range uris {
		names = append(names, uriName(u))
	}
	return seq(t, dpName(t, fullName(t, names...)))
}

func idpExt(critical bool, v []byte) pkix.Extension {
	return pkix.Extension{Id: oidIDP, Critical: critical, Value: v}
}

// idpFx is the status fixture with a leaf that has the given mutation, so its CRL distribution
// points can be set.
func newIDPFx(t *testing.T, mut func(*x509.Certificate)) *stFx {
	t.Helper()
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	leaf := f.leaf(t, mut)
	a := idT0.Add(5 * time.Second)
	ac, err := c.VerifyChain("alice", leaf.Chain(), idT0, a)
	if err != nil {
		t.Fatalf("fixture chain: %v", err)
	}
	return &stFx{fixture: f, t: t, c: c, leaf: leaf, ac: ac, a: a}
}

func withCDP(urls ...string) func(*x509.Certificate) {
	return func(c *x509.Certificate) { c.CRLDistributionPoints = urls }
}

// icrl is a CRL by the leaf's issuer with an IDP extension.
func (s *stFx) icrl(num int64, this time.Time, ext pkix.Extension, entries ...x509.RevocationListEntry) []byte {
	s.t.Helper()
	return s.inter.NewCRL(s.t, testca.CRLOpts{Number: num, ThisUpdate: this, NextUpdate: this.Add(hour), Entries: entries, Extra: []pkix.Extension{ext}})
}

// rootCRL is a plain CRL by the root for the intermediate, so only the leaf's status is in question.
func (s *stFx) rootCRL() []byte { return s.rcrl(1, idT0, 24*hour) }

const (
	urlA = "http://pki.bank.example/approvers/a.crl"
	urlB = "http://pki.bank.example/approvers/b.crl"
)

// An IDP that holds only the fullName of URIs is accepted, and the CRL records it.
func TestAddCRLAcceptsADistributionPointOnlyIDP(t *testing.T) {
	s := newIDPFx(t, withCDP(urlA))
	for name, uris := range map[string][]string{"one": {urlA}, "two": {urlA, urlB}} {
		t.Run(name, func(t *testing.T) {
			crl := s.icrl(1, idT0, idpExt(true, idpOf(t, uris...)))
			if _, err := audit.NewStatusSnapshot().AddCRL(crl); err != nil {
				t.Fatal(err)
			}
			v, err := audit.VerifyCRL(crl, s.inter.Cert, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(v.DistributionPoints, " "); got != strings.Join(uris, " ") {
				t.Fatalf("DistributionPoints = %q, want %q", got, uris)
			}
		})
	}
	// A CRL without an IDP has none.
	v, err := audit.VerifyCRL(s.lcrl(1, idT0, hour), s.inter.Cert, 1000)
	if err != nil || len(v.DistributionPoints) != 0 {
		t.Fatalf("plain CRL: %v, %v", err, v)
	}
}

// Every other IDP is refused as before: a partition by certificate type or reason, an indirect CRL, a
// name that is not a URI list, an IDP that is not critical, an empty one, a malformed one.
func TestAddCRLRefusesOtherIDPs(t *testing.T) {
	s := newIDPFx(t, withCDP(urlA))
	dp := dpName(t, fullName(t, uriName(urlA)))
	flag := func(tag int) asn1.RawValue { // an implicit BOOLEAN TRUE
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: tag, Bytes: []byte{0xff}}
	}
	falseFlag := func(tag int) asn1.RawValue { // not DER: a DEFAULT FALSE is omitted
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: tag, Bytes: []byte{0x00}}
	}
	reasons := asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 3, Bytes: []byte{0x07, 0x80}}
	dns := asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 2, Bytes: []byte("pki.bank.example")}
	rdn := asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 1, IsCompound: true, Bytes: []byte{0x31, 0x00}}
	cases := map[string]pkix.Extension{
		"empty":                       idpExt(true, []byte{0x30, 0x00}),
		"not critical":                idpExt(false, idpOf(t, urlA)),
		"onlyContainsUserCerts":       idpExt(true, seq(t, dp, flag(1))),
		"onlyContainsCACerts":         idpExt(true, seq(t, dp, flag(2))),
		"onlySomeReasons":             idpExt(true, seq(t, dp, reasons)),
		"indirectCRL":                 idpExt(true, seq(t, dp, flag(4))),
		"onlyContainsAttributeCerts":  idpExt(true, seq(t, dp, flag(5))),
		"a flag encoded FALSE":        idpExt(true, seq(t, dp, falseFlag(1))),
		"flags without a point":       idpExt(true, seq(t, flag(1))),
		"nameRelativeToCRLIssuer":     idpExt(true, seq(t, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: der(t, rdn, "")})),
		"a dNSName in the full name":  idpExt(true, seq(t, dpName(t, fullName(t, dns)))),
		"a URI and a dNSName":         idpExt(true, seq(t, dpName(t, fullName(t, uriName(urlA), dns)))),
		"an empty full name":          idpExt(true, seq(t, dpName(t, fullName(t)))),
		"trailing bytes":              idpExt(true, append(idpOf(t, urlA), 0)),
		"not a sequence":              idpExt(true, []byte{0x04, 0x00}),
		"a URI with a non-ASCII byte": idpExt(true, idpOf(t, "http://pki.bank.example/\xc3\xa9.crl")),
		"an empty URI":                idpExt(true, idpOf(t, "")),
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			crl, err := s.inter.TryCRL(testca.CRLOpts{Number: 1, ThisUpdate: idT0, NextUpdate: idT0.Add(hour), Extra: []pkix.Extension{e}})
			if err != nil {
				t.Skipf("the standard library will not create it: %v", err)
			}
			_, err = audit.NewStatusSnapshot().AddCRL(crl)
			if err == nil {
				t.Fatal("accepted")
			}
			wantStatusErr(t, err, agent.ReasonStatusUnknown)
		})
	}
}

// A certificate gets status from a CRL with an IDP only when its own distribution points name the IDP's
// URI: the same GeneralName type and the same bytes.
func TestIDPCoversOnlyCertificatesThatNameIt(t *testing.T) {
	covered := func(t *testing.T, mut func(*x509.Certificate), idp []byte) bool {
		t.Helper()
		s := newIDPFx(t, mut)
		st := s.leafStatus(s.snap(s.icrl(1, idT0.Add(10*min), idpExt(true, idp)), s.rootCRL()), audit.StatusParams{})
		return st.Tier != audit.TierNoInformation
	}
	cdpRaw := func(t *testing.T, dps ...[]byte) func(*x509.Certificate) {
		return func(c *x509.Certificate) {
			var body []byte
			for _, d := range dps {
				body = append(body, d...)
			}
			v := der(t, asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: body}, "")
			c.ExtraExtensions = append(c.ExtraExtensions, pkix.Extension{Id: oidCDP, Value: v})
		}
	}
	plainDP := func(t *testing.T, u string) []byte { return seq(t, dpName(t, fullName(t, uriName(u)))) }
	crlIssuer := asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 2, IsCompound: true, Bytes: der(t, uriName("http://other.example/ca"), "")}
	reasons := asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 1, Bytes: []byte{0x07, 0x80}}

	idpA := idpOf(t, urlA)
	for name, tc := range map[string]struct {
		mut  func(*x509.Certificate)
		idp  []byte
		want bool
	}{
		"the same URI":                    {withCDP(urlA), idpA, true},
		"one of several leaf points":      {withCDP(urlB, urlA), idpA, true},
		"one of several IDP names":        {withCDP(urlB), idpOf(t, urlA, urlB), true},
		"another URI":                     {withCDP(urlB), idpA, false},
		"no distribution points":          {nil, idpA, false},
		"a different case":                {withCDP(strings.ToUpper(urlA)), idpA, false},
		"a trailing slash":                {withCDP(urlA + "/"), idpA, false},
		"a prefix":                        {withCDP(urlA + "?x=1"), idpA, false},
		"a point with a cRLIssuer":        {cdpRaw(t, seq(t, dpName(t, fullName(t, uriName(urlA))), crlIssuer)), idpA, false},
		"a point with reasons":            {cdpRaw(t, seq(t, dpName(t, fullName(t, uriName(urlA))), reasons)), idpA, false},
		"a point with only a cRLIssuer":   {cdpRaw(t, seq(t, crlIssuer)), idpA, false},
		"a good point after a bad one":    {cdpRaw(t, seq(t, dpName(t, fullName(t, uriName(urlA))), crlIssuer), plainDP(t, urlA)), idpA, true},
		"the URI as a dNSName":            {cdpRaw(t, seq(t, dpName(t, fullName(t, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 2, Bytes: []byte(urlA)})))), idpA, false},
		"a URI with the host in capitals": {withCDP("http://PKI.bank.example/approvers/a.crl"), idpA, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := covered(t, tc.mut, tc.idp); got != tc.want {
				t.Fatalf("status from the CRL = %v, want %v", got, tc.want)
			}
		})
	}
}

// A CRL without an IDP covers every certificate of its issuer, with or without distribution points of
// their own: nothing changes for a CA that publishes one full CRL.
func TestCRLWithoutAnIDPStillCoversEveryCertificate(t *testing.T) {
	for name, mut := range map[string]func(*x509.Certificate){"no points": nil, "a point": withCDP(urlA)} {
		t.Run(name, func(t *testing.T) {
			s := newIDPFx(t, mut)
			st := s.leafStatus(s.snap(s.lcrl(1, idT0.Add(10*min), hour), s.rootCRL()), audit.StatusParams{})
			if st.Tier == audit.TierNoInformation {
				t.Fatalf("%+v", st)
			}
		})
	}
}

// The fail-open case: a CA partitions by distribution point. A certificate in partition B must not take
// "not revoked" from partition A's CRL, and its own partition's listing must revoke it.
func TestAPartitionIsNotEvidenceForAnotherPartition(t *testing.T) {
	s := newIDPFx(t, withCDP(urlB))
	clean := s.icrl(1, idT0.Add(10*min), idpExt(true, idpOf(t, urlA)))
	if st := s.leafStatus(s.snap(clean, s.rootCRL()), audit.StatusParams{}); st.Tier != audit.TierNoInformation || st.Revoked {
		t.Fatalf("partition A's clean CRL gave partition B's certificate status: %+v", st)
	}
	listing := s.icrl(2, idT0.Add(20*min), idpExt(true, idpOf(t, urlB)), testca.Revoked(s.leaf.Cert, idT0.Add(15*min), 1))
	st := s.leafStatus(s.snap(clean, listing, s.rootCRL()), audit.StatusParams{})
	if !st.Revoked {
		t.Fatalf("partition B lists the certificate and it is not revoked: %+v", st)
	}
	// And with B's own clean CRL it has status, from B's CRL.
	cleanB := s.icrl(3, idT0.Add(10*min), idpExt(true, idpOf(t, urlB)))
	if st := s.leafStatus(s.snap(clean, cleanB, s.rootCRL()), audit.StatusParams{}); st.Tier == audit.TierNoInformation || st.Revoked {
		t.Fatalf("%+v", st)
	}
}

// A listing is a revocation whatever the partition: a CRL the issuer signed that lists the certificate
// revokes it even when the CRL's IDP does not match the certificate's distribution points, or the
// certificate has none. The matching rule limits evidence of absence only.
func TestAListingInAnyPartitionRevokes(t *testing.T) {
	for name, mut := range map[string]func(*x509.Certificate){"another point": withCDP(urlB), "no points": nil} {
		t.Run(name, func(t *testing.T) {
			s := newIDPFx(t, mut)
			listing := s.icrl(1, idT0.Add(10*min), idpExt(true, idpOf(t, urlA)), testca.Revoked(s.leaf.Cert, idT0.Add(5*min), 1))
			st := s.leafStatus(s.snap(listing, s.rootCRL()), audit.StatusParams{})
			if !st.Revoked {
				t.Fatalf("a listing in a non-matching partition did not revoke: %+v", st)
			}
			// It gives no evidence of absence for another certificate's partition: with the listing
			// removed the same snapshot has no status for this certificate.
			clean := s.icrl(2, idT0.Add(10*min), idpExt(true, idpOf(t, urlA)))
			if st := s.leafStatus(s.snap(clean, s.rootCRL()), audit.StatusParams{}); st.Tier != audit.TierNoInformation || st.Revoked {
				t.Fatalf("%+v", st)
			}
		})
	}
}

// The evidence the verdict records (the hashes of the CRLs relied on) never names a CRL that did not
// cover the certificate.
func TestUsedCRLsAreOnlyThoseThatCovered(t *testing.T) {
	s := newIDPFx(t, withCDP(urlB))
	other := s.icrl(1, idT0.Add(10*min), idpExt(true, idpOf(t, urlA)))
	own := s.icrl(2, idT0.Add(10*min), idpExt(true, idpOf(t, urlB)))
	st := s.leafStatus(s.snap(other, own, s.rootCRL()), audit.StatusParams{})
	otherView, err := audit.VerifyCRL(other, s.inter.Cert, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range st.Used {
		if h == otherView.Hash {
			t.Fatal("the verdict relies on a CRL of another partition")
		}
	}
	if len(st.Used) == 0 {
		t.Fatal("no CRL relied on")
	}
}

// The CRL step-ca 0.30.2 signed for a revoked certificate, and the leaf its OIDC provisioner issued:
// the IDP, the leaf's distribution point and the revocation entry as they are on the wire.
func TestStepCAFixtures(t *testing.T) {
	crlDER, err := os.ReadFile("testdata/stepca/crl.der")
	if err != nil {
		t.Fatal(err)
	}
	inter := readCert(t, "testdata/stepca/intermediate.pem")
	leaf := readCert(t, "testdata/stepca/leaf.pem")
	v, err := audit.VerifyCRL(crlDER, inter, 1000)
	if err != nil {
		t.Fatalf("step-ca's CRL: %v", err)
	}
	if len(v.DistributionPoints) != 1 || len(leaf.CRLDistributionPoints) != 1 || v.DistributionPoints[0] != leaf.CRLDistributionPoints[0] {
		t.Fatalf("IDP %v, leaf CDP %v", v.DistributionPoints, leaf.CRLDistributionPoints)
	}
	found := false
	for _, s := range v.Serials() {
		found = found || s == leaf.SerialNumber.String()
	}
	if !found {
		t.Fatalf("the CRL does not list the leaf %s: %v", leaf.SerialNumber, v.Serials())
	}
}

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("%s: no PEM block", path)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// An unscoped delta (a delta has no IDP) is not merged with a partition's base: it says nothing changed in
// the whole CA since base N, which is no statement about one partition. It is no evidence of absence for the
// partition, and the mirror case (a delta whose base has another partition's IDP) is the same.
func TestUnscopedDeltaIsNotMergedWithAPartitionBase(t *testing.T) {
	for name, idp := range map[string]string{"the cert's partition": urlA, "another partition": urlB} {
		t.Run(name, func(t *testing.T) {
			s := newIDPFx(t, withCDP(urlA))
			base := s.icrl(1, idT0.Add(10*min), idpExt(true, idpOf(t, idp)))
			d := s.delta(2, 1, idT0.Add(40*min), hour)
			st := s.leafStatus(s.snap(base, d, s.rootCRL()), audit.StatusParams{})
			dv, err := audit.VerifyCRL(d, s.inter.Cert, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(st.Used, dv.Hash) || st.S.Equal(idT0.Add(40*min)) {
				t.Fatalf("the delta was used as evidence for a partition's base: S=%v", st.S)
			}
		})
	}
	// Control: the same delta pairs with a base that has no IDP.
	s := newIDPFx(t, withCDP(urlA))
	base := s.lcrl(1, idT0.Add(10*min), hour)
	d := s.delta(2, 1, idT0.Add(40*min), hour)
	st := s.leafStatus(s.snap(base, d, s.rootCRL()), audit.StatusParams{})
	dv, _ := audit.VerifyCRL(d, s.inter.Cert, 1000)
	if !slices.Contains(st.Used, dv.Hash) {
		t.Fatalf("control: the delta did not pair with an unscoped base: %+v", st)
	}
}

// A certificate that names several distribution points (separate points, as AD CS writes its LDAP and HTTP
// publication points of one CRL) takes absence from a CRL that covers any one of them: the points are
// alternative places for the same scope (RFC 5280 section 4.2.1.13). The consequence for a CA that
// partitions and puts every partition's point in each certificate is stated in the guide: such a CA must
// not do that, a certificate names its own partition's point only.
func TestSeveralPointsAreAlternatives(t *testing.T) {
	s := newIDPFx(t, withCDP(urlA, urlB))
	cleanB := s.icrl(1, idT0.Add(10*min), idpExt(true, idpOf(t, urlB)))
	st := s.leafStatus(s.snap(cleanB, s.rootCRL()), audit.StatusParams{})
	if st.Tier == audit.TierNoInformation {
		t.Fatalf("a CRL covering one of the certificate's points gave no status: %+v", st)
	}
	// A listing in the other point's CRL still revokes.
	listA := s.icrl(2, idT0.Add(20*min), idpExt(true, idpOf(t, urlA)), testca.Revoked(s.leaf.Cert, idT0.Add(15*min), 1))
	if st := s.leafStatus(s.snap(cleanB, listA, s.rootCRL()), audit.StatusParams{}); !st.Revoked {
		t.Fatalf("a listing in the certificate's other point did not revoke: %+v", st)
	}
}

// A URI with an embedded NUL, in the CRL's IDP, is refused.
func TestIDPRefusesANulInTheURI(t *testing.T) {
	s := newIDPFx(t, withCDP(urlA))
	crl, err := s.inter.TryCRL(testca.CRLOpts{Number: 1, ThisUpdate: idT0, NextUpdate: idT0.Add(hour),
		Extra: []pkix.Extension{idpExt(true, idpOf(t, "http://pki.bank.example/a\x00.crl"))}})
	if err != nil {
		t.Skip(err)
	}
	if _, err := audit.NewStatusSnapshot().AddCRL(crl); err == nil {
		t.Fatal("accepted")
	}
}
