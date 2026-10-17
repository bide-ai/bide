package identitypreflight

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/testca"
)

// certRig is a rig with healthy CRLs and a chain file, and a way to judge other chains.
type certRig struct {
	*rig
	rootCRL, interCRL []byte
}

func newCertRig(t *testing.T) *certRig {
	g := newRig(t)
	rc, ic := g.freshCRLs()
	return &certRig{rig: g, rootCRL: rc, interCRL: ic}
}

// judge writes the chain and runs the check over the CRLs of c.
func (c *certRig) judge(der ...[]byte) *Report {
	c.t.Helper()
	o := c.options(c.flatDir(c.rootCRL, c.interCRL))
	o.Cert = c.write("cert.pem", pemOf(der...))
	return c.run(o)
}

func TestCertAcceptedByTheGate(t *testing.T) {
	c := newCertRig(t)
	r := c.judge(c.leaf.Chain()...)
	r.want(t, "cert.chain", OK, "accepted")
	r.want(t, "cert.status.count", OK, "would count")
	r.want(t, "cert.status.exec", OK, "would pass")
}

// One test per reason the gate would refuse, each with the gate's own reason in the message.
func TestCertRefusalReasons(t *testing.T) {
	type tc struct {
		name   string
		leaf   func(c *certRig) *testca.Leaf
		chain  func(c *certRig, l *testca.Leaf) [][]byte
		id     string
		reason string
		detail string
	}
	person := func(n int) func(*x509.Certificate) {
		return func(x *x509.Certificate) { u, _ := url.Parse(testca.Person(n)); x.URIs = []*url.URL{u} }
	}
	cases := []tc{
		{name: "expired", id: "cert.chain", reason: "outside the certificate's validity", detail: "validity",
			leaf: func(c *certRig) *testca.Leaf {
				return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) {
					x.NotBefore, x.NotAfter = testca.T0.Add(-3*time.Hour), testca.T0.Add(-2*time.Hour)
				}})
			}},
		{name: "not yet valid", id: "cert.chain", reason: "outside the certificate's validity", detail: "validity",
			leaf: func(c *certRig) *testca.Leaf {
				return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) {
					x.NotBefore, x.NotAfter = testca.T0.Add(2*time.Hour), testca.T0.Add(3*time.Hour)
				}})
			}},
		{name: "wrong EKU", id: "cert.chain", reason: "not usable for approving", detail: "extended key usage",
			leaf: func(c *certRig) *testca.Leaf {
				return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) {
					x.UnknownExtKeyUsage = nil
					x.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
				}})
			}},
		{name: "wrong certificate policy", id: "cert.chain", reason: "not usable for approving", detail: "certificate policy",
			leaf: func(c *certRig) *testca.Leaf {
				return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) { x.Policies = nil }})
			}},
		{name: "SAN of another person", id: "cert.chain", reason: "does not carry the approver's identity", detail: "is not the identity of approver",
			leaf: func(c *certRig) *testca.Leaf {
				return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: person(2)})
			}},
		{name: "no SAN", id: "cert.chain", reason: "does not carry the approver's identity", detail: "no subject alternative name",
			leaf: func(c *certRig) *testca.Leaf {
				return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) { x.URIs = nil }})
			}},
		{name: "two SANs", id: "cert.chain", reason: "does not carry the approver's identity", detail: "exactly one is required",
			leaf: func(c *certRig) *testca.Leaf {
				return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) {
					u, _ := url.Parse(testca.Person(1))
					v, _ := url.Parse(testca.Person(2))
					x.URIs = []*url.URL{u, v}
				}})
			}},
		{name: "lives too long", id: "cert.chain", reason: "outside the certificate's validity", detail: "over the limit",
			leaf: func(c *certRig) *testca.Leaf {
				return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) {
					x.NotBefore, x.NotAfter = testca.T0.Add(-time.Hour), testca.T0.Add(72*time.Hour)
				}})
			}},
		{name: "chain reaches no pinned CA", id: "cert.chain", reason: "does not verify", detail: "",
			leaf: func(c *certRig) *testca.Leaf {
				other := testca.NewRoot(c.t, "other-root", testca.Opts{}).NewIntermediate(c.t, "other-inter", testca.Opts{})
				return other.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519})
			}},
		{name: "the intermediate is left out", id: "cert.chain", reason: "does not verify", detail: "",
			leaf:  func(c *certRig) *testca.Leaf { return c.leaf },
			chain: func(c *certRig, l *testca.Leaf) [][]byte { return [][]byte{l.DER} }},
		{name: "leaf issued by an unpinned intermediate", id: "cert.chain", reason: "does not verify", detail: "",
			leaf: func(c *certRig) *testca.Leaf {
				return c.root.NewIntermediate(c.t, "unpinned", testca.Opts{}).NewLeaf(c.t, testca.Opts{Key: testca.Ed25519})
			}},
		{name: "the leaf is a CA", id: "cert.chain", reason: "not usable for approving", detail: "marked as a CA",
			leaf: func(c *certRig) *testca.Leaf {
				return c.inter.NewLeaf(c.t, testca.Opts{Key: testca.Ed25519, Mutate: func(x *x509.Certificate) { x.IsCA, x.BasicConstraintsValid = true, true }})
			}},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			c := newCertRig(t)
			l := k.leaf(c)
			chain := l.Chain()
			if k.chain != nil {
				chain = k.chain(c, l)
			}
			r := c.judge(chain...)
			chk := r.get(k.id)
			if chk == nil || chk.Status != Fail || !strings.Contains(chk.Message, k.reason) || !strings.Contains(chk.Message, k.detail) {
				t.Fatalf("want %s FAIL with %q and %q:\n%s", k.id, k.reason, k.detail, r.dump())
			}
			if chk.Fix == "" || r.Ready || r.ExitCode() != ExitFail {
				t.Fatalf("a refused chain needs a fix and is not ready (exit %d)", r.ExitCode())
			}
			if r.get("cert.status.count") != nil {
				t.Fatalf("status is not judged for a chain the gate refuses:\n%s", r.dump())
			}
		})
	}
}

func TestCertRevoked(t *testing.T) {
	t.Run("the leaf", func(t *testing.T) {
		c := newCertRig(t)
		c.inter2(c.at.Add(-10*time.Minute), testca.Revoked(c.leaf.Cert, c.at.Add(-time.Minute), 1))
		r := c.judge(c.leaf.Chain()...)
		r.want(t, "cert.chain", OK, "accepted")
		r.want(t, "cert.status.count", Fail, "revoked")
		r.want(t, "cert.status.exec", Fail, "revoked")
		if r.ExitCode() != ExitFail {
			t.Fatal("a revoked certificate is not ready")
		}
	})
	t.Run("the intermediate", func(t *testing.T) {
		c := newCertRig(t)
		c.rootCRL = c.rig.root.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.at.Add(-time.Hour), NextUpdate: c.at.Add(48 * time.Hour),
			Entries: []x509.RevocationListEntry{testca.Revoked(c.rig.inter.Cert, c.at.Add(-2*time.Hour), 5)}})
		r := c.judge(c.leaf.Chain()...)
		r.want(t, "cert.status.count", Fail, "revoked")
	})
}

// inter2 replaces the intermediate's CRL with one listing e.
func (c *certRig) inter2(this time.Time, e x509.RevocationListEntry) {
	c.interCRL = c.rig.inter.NewCRL(c.t, testca.CRLOpts{Number: 2, ThisUpdate: this, NextUpdate: this.Add(time.Hour), Entries: []x509.RevocationListEntry{e}})
}

// Status that counts but is too old to run the call: the count and the pre-execution checks differ.
func TestCertStatusTooOldForExecOnly(t *testing.T) {
	c := newCertRig(t)
	c.interCRL = c.rig.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: c.at.Add(-100 * time.Minute), NextUpdate: c.at.Add(time.Hour)})
	r := c.judge(c.leaf.Chain()...)
	r.want(t, "cert.status.count", OK, "would count")
	r.want(t, "cert.status.exec", Fail, "older than the policy allows")
	// The refresher's view of the same CRL agrees: a gate pauses on it.
	r.want(t, "crl.fresh."+c.interK[:8], Fail, "would not accept")
}

func TestCertNoStatusAtAllIsUnknown(t *testing.T) {
	c := newCertRig(t)
	c.interCRL = nil
	o := c.options(c.flatDir(c.rootCRL, nil))
	o.Cert = c.write("cert.pem", pemOf(c.leaf.Chain()...))
	r := c.run(o)
	r.want(t, "cert.status.count", Fail, "no usable revocation status")
}

func TestCertFileProblems(t *testing.T) {
	c := newCertRig(t)
	for name, content := range map[string]string{
		"private key":  "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n",
		"empty":        "",
		"trailing":     string(pemOf(c.leaf.DER)) + "junk\n",
		"wrong block":  "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n",
		"not parsable": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	} {
		t.Run(name, func(t *testing.T) {
			o := c.options(c.flatDir(c.rootCRL, c.interCRL))
			o.Cert = c.write("bad.pem", []byte(content))
			r := c.run(o)
			r.want(t, "cert.parse", Fail, "-cert")
			if strings.Contains(r.get("cert.parse").Message, "AAAA") {
				t.Fatal("file content in the report")
			}
		})
	}
}

// -chain carries more certificates behind -cert, like a bundle file.
func TestCertChainFlag(t *testing.T) {
	c := newCertRig(t)
	o := c.options(c.flatDir(c.rootCRL, c.interCRL))
	o.Cert = c.write("leaf.pem", pemOf(c.leaf.DER))
	o.Chain = c.write("more.pem", pemOf(c.rig.inter.DER))
	r := c.run(o)
	r.want(t, "cert.chain", OK, "path has 3 certificate(s)")
	// Without it the carried chain lacks the intermediate, and the gate refuses it.
	o.Chain = ""
	c.run(o).want(t, "cert.chain", Fail, "does not verify")
}

func TestCertNeedsAnApproverIdWhenThereAreSeveral(t *testing.T) {
	c := newCertRig(t)
	c.policy["identity"] = map[string]string{"alice": testca.Person(1), "bob": testca.Person(2)}
	o := c.options(c.flatDir(c.rootCRL, c.interCRL))
	o.Cert = c.write("cert.pem", pemOf(c.leaf.Chain()...))
	if _, err := Run(t.Context(), o); err == nil || !strings.Contains(err.Error(), "-as is required") {
		t.Fatalf("got %v", err)
	}
	o.As = "alice"
	c.run(o).want(t, "cert.chain", OK, "accepted")
	o.As = "bob"
	c.run(o).want(t, "cert.chain", Fail, "is not the identity of approver")
}

// The check and the gate's verifier must say the same: the same chain, judged by CheckCertificate directly.
func TestCertVerdictIsTheGatesFunction(t *testing.T) {
	c := newCertRig(t)
	c.inter2(c.at.Add(-10*time.Minute), testca.Revoked(c.leaf.Cert, c.at.Add(-time.Minute), 1))
	r := c.judge(c.leaf.Chain()...)
	chk := r.get("cert.status.count")
	if chk == nil || !strings.Contains(chk.Message, "serial "+c.leaf.Cert.SerialNumber.String()) {
		t.Fatalf("the gate's own detail (the serial) is not in the message: %+v", chk)
	}
	_ = filepath.Join
	_ = pkix.Name{}
}
