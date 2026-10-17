package identitypreflight

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/testca"
)

const (
	cdpA = "http://pki.bank.example/approvers/a.crl"
	cdpB = "http://pki.bank.example/approvers/b.crl"
)

// idpFull is a critical issuing distribution point holding only the fullName of uri.
func idpFull(t *testing.T, uri string) pkix.Extension {
	t.Helper()
	enc := func(v asn1.RawValue) []byte {
		b, err := asn1.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	name := enc(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 6, Bytes: []byte(uri)})
	full := enc(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: name})
	point := enc(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: full})
	seq := enc(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: point})
	return pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 28}, Critical: true, Value: seq}
}

func leafWithPoints(c *certRig, urls ...string) *testca.Leaf {
	return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) { x.CRLDistributionPoints = urls }})
}

// scoped makes the intermediate's CRL carry an issuing distribution point naming uri.
func (c *certRig) scoped(uri string) {
	c.interCRL = c.inter.NewCRL(c.t, testca.CRLOpts{Number: 1, ThisUpdate: c.at.Add(-10 * time.Minute), NextUpdate: c.at.Add(50 * time.Minute),
		Extra: []pkix.Extension{idpFull(c.t, uri)}})
}

// A CRL scoped by an issuing distribution point covers only a leaf that names its URI: a leaf that does not is
// reported with the cause, not only as "no status".
func TestCDPLeafMustNameTheCRLsPoint(t *testing.T) {
	c := newCertRig(t)
	c.scoped(cdpA)
	r := c.judge(leafWithPoints(c, cdpB).Chain()...)
	r.want(t, "cert.cdp", Fail, "none of the certificate's plain CRL distribution points names")
	r.want(t, "cert.status.count", Fail, "")

	c = newCertRig(t)
	c.scoped(cdpA)
	r = c.judge(c.leaf.Chain()...) // no distribution point at all
	r.want(t, "cert.cdp", Fail, "none of the certificate's plain CRL distribution points names")

	c = newCertRig(t)
	c.scoped(cdpA)
	l := leafWithPoints(c, cdpA)
	r = c.judge(l.Chain()...)
	r.want(t, "cert.cdp", OK, "consistent")
	r.want(t, "cert.status.count", OK, "would count")
}

// A CRL without an issuing distribution point covers every certificate: no finding for a leaf without points.
func TestCDPUnscopedCRLNeedsNoPoint(t *testing.T) {
	c := newCertRig(t)
	r := c.judge(c.leaf.Chain()...)
	r.want(t, "cert.cdp", OK, "consistent")
}

// Several plain points that share no name draw a warning: the status is read from a CRL covering any one of
// them, which is for mirrors of one CRL and not for a CA that partitions. Points that share a name do not.
func TestCDPSeveralDisjointPointsWarn(t *testing.T) {
	c := newCertRig(t)
	r := c.judge(leafWithPoints(c, cdpA, cdpB).Chain()...)
	r.want(t, "cert.cdp", Warn, "any one of them")
	r.want(t, "cert.status.count", OK, "would count") // a warning does not fail the gate's verdict

	c = newCertRig(t)
	r = c.judge(leafWithPoints(c, cdpA, cdpA).Chain()...)
	r.want(t, "cert.cdp", OK, "consistent")
}
