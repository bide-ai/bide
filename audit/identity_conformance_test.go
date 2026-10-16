package audit_test

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// Conformance against CAs that issue true delta CRLs. The fixtures are captured by the manually
// dispatched workflow .github/workflows/crl-fixtures.yml (EJBCA Community in a container, Microsoft AD
// CS on a Windows Server) and checked in, so these tests run offline. testdata/ejbca/capture.sh and
// testdata/adcs/capture.ps1 are the scripts; VERSION records what produced them.

// capture is one CA's checked-in capture.
type capture struct {
	t    *testing.T
	dir  string
	ca   *x509.Certificate
	snap *audit.StatusSnapshot
	crls map[string]*x509.RevocationList
}

func readPEM(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("%s: no PEM block", path)
	}
	return blk.Bytes
}

func loadCapture(t *testing.T, dir string, crls ...string) *capture {
	t.Helper()
	c := &capture{t: t, dir: filepath.Join("testdata", dir), snap: audit.NewStatusSnapshot(), crls: map[string]*x509.RevocationList{}}
	var err error
	if c.ca, err = x509.ParseCertificate(readPEM(t, filepath.Join(c.dir, "ca.pem"))); err != nil {
		t.Fatal(err)
	}
	for _, n := range crls {
		der := readPEM(t, filepath.Join(c.dir, n+".pem"))
		if _, err := c.snap.AddCRL(der); err != nil {
			t.Fatalf("%s: a CRL the CA wrote was refused: %v", n, err)
		}
		if c.crls[n], err = x509.ParseRevocationList(der); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (c *capture) leaf(name string) *x509.Certificate {
	c.t.Helper()
	cert, err := x509.ParseCertificate(readPEM(c.t, filepath.Join(c.dir, name+".pem")))
	if err != nil {
		c.t.Fatal(err)
	}
	return cert
}

// compiled is a policy over the capture's CA; only its status methods are used here.
func (c *capture) compiled() *audit.CertIdentity {
	c.t.Helper()
	return mustCompile(c.t, audit.IdentityPolicy{
		Anchors: []*x509.Certificate{c.ca}, Identity: map[string]string{"x": testca.Person(1)},
		IDKind: audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	})
}

func (c *capture) status(leaf string, a time.Time, p audit.StatusParams) audit.CertStatus {
	c.t.Helper()
	cert := c.leaf(leaf)
	ac := &audit.ApproverCert{Leaf: cert, Chain: []*x509.Certificate{cert, c.ca}}
	return c.compiled().EvaluateStatus(ac, a, c.snap, p).Certs[0]
}

// entry finds a CRL's entry for a certificate.
func (c *capture) entry(crl, leaf string) (x509.RevocationListEntry, bool) {
	cert := c.leaf(leaf)
	for _, e := range c.crls[crl].RevokedCertificateEntries {
		if e.SerialNumber.Cmp(cert.SerialNumber) == 0 {
			return e, true
		}
	}
	return x509.RevocationListEntry{}, false
}

func deltaBase(t *testing.T, rl *x509.RevocationList) *big.Int {
	t.Helper()
	for _, e := range rl.Extensions {
		if e.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 27}) {
			n := new(big.Int)
			if _, err := asn1.Unmarshal(e.Value, &n); err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	return nil
}

// EJBCA Community: base CRLs hourly, deltas every ten minutes, one shared CRL number sequence, each
// delta naming the latest base.
func TestEJBCAConformance(t *testing.T) {
	c := loadCapture(t, "ejbca", "base1", "delta1", "base2", "delta2", "base3", "delta3")
	order := []string{"base1", "delta1", "base2", "delta2", "base3", "delta3"}
	for i := 1; i < len(order); i++ {
		if c.crls[order[i]].Number.Cmp(c.crls[order[i-1]].Number) <= 0 {
			t.Fatalf("CRL numbers are not one increasing sequence: %s=%v %s=%v", order[i-1], c.crls[order[i-1]].Number, order[i], c.crls[order[i]].Number)
		}
		if !c.crls[order[i]].ThisUpdate.After(c.crls[order[i-1]].ThisUpdate) {
			t.Fatalf("%s is not issued after %s", order[i], order[i-1])
		}
	}
	// A delta names the latest base issued before it, and carries the critical indicator.
	for delta, base := range map[string]string{"delta1": "base1", "delta2": "base2", "delta3": "base3"} {
		if n := deltaBase(t, c.crls[delta]); n == nil || n.Cmp(c.crls[base].Number) != 0 {
			t.Fatalf("%s names base %v, want %s (%v)", delta, n, base, c.crls[base].Number)
		}
	}
	// What the CA records: the hold and its release (removeFromCRL), with the release's own date.
	hold, ok := c.entry("delta1", "u2")
	if !ok || hold.ReasonCode != 6 {
		t.Fatalf("delta1 does not list the hold: %+v", hold)
	}
	rel, ok := c.entry("delta2", "u2")
	if !ok || rel.ReasonCode != 8 {
		t.Fatalf("delta2 does not carry removeFromCRL: %+v", rel)
	}
	if !rel.RevocationTime.After(hold.RevocationTime) {
		t.Fatalf("EJBCA stamps a removeFromCRL entry with %v, not after the hold at %v", rel.RevocationTime, hold.RevocationTime)
	}
	if _, listed := c.entry("base3", "u2"); listed {
		t.Fatal("the released certificate is still on the new base")
	}
	if _, listed := c.entry("base2", "u2"); !listed {
		t.Fatal("the hold is missing from the base issued before the release")
	}

	short := audit.StatusParams{W: 5 * time.Second, MinW: time.Second, CAPeriod: time.Hour}
	b1 := c.crls["base1"].ThisUpdate
	a := b1.Add(time.Second)

	// u4 is never revoked: final, resting on the merged base and delta.
	st := c.status("u4", a, short)
	if !st.IsFinal() || len(st.Used) < 2 {
		t.Fatalf("u4: %+v", st)
	}
	// u1: key compromise with no invalidity date, retroactive to notBefore.
	if st := c.status("u1", a, short); !st.Revoked || !st.Compromise {
		t.Fatalf("u1: %+v", st)
	}
	// u3 (superseded) is revoked from the time EJBCA recorded; a decision before it stands.
	sup, _ := c.entry("delta1", "u3")
	before := c.status("u3", sup.RevocationTime.Add(-time.Second), short)
	if before.Revoked {
		t.Fatalf("u3 before the revocation: %+v", before)
	}
	if st := c.status("u3", sup.RevocationTime.Add(time.Second), short); !st.Revoked || st.ReasonCode != 4 {
		t.Fatalf("u3 after: %+v", st)
	}

	// The hold: refused by default; opted in, only inside [start, release].
	holds := short
	holds.AllowHolds = true
	if st := c.status("u2", hold.RevocationTime.Add(-time.Second), short); !st.Revoked {
		t.Fatalf("u2 by default: %+v", st)
	}
	if st := c.status("u2", hold.RevocationTime.Add(-time.Second), holds); st.Revoked {
		t.Fatalf("u2 before the hold, opted in: %+v", st)
	}
	mid := hold.RevocationTime.Add(rel.RevocationTime.Sub(hold.RevocationTime) / 2)
	if st := c.status("u2", mid, holds); !st.Revoked || st.ReasonCode != 6 {
		t.Fatalf("u2 during the hold, opted in: %+v", st)
	}
	if st := c.status("u2", rel.RevocationTime.Add(time.Second), holds); st.Revoked {
		t.Fatalf("u2 after the release, opted in: %+v", st)
	}
}

// Microsoft AD CS (a standalone root on Windows Server 2025; see testdata/adcs/VERSION and
// DEFAULTS.txt for what produced and configured it). What the capture showed, and these tests pin:
// base and delta CRLs share one number sequence; a delta names an older base (here the CA's first,
// which is not among the fixtures) and is cumulative since it, so every delta also lists everything
// revoked since that base; thisUpdate is back-dated by about ten minutes, so a revocation date can be
// later than the thisUpdate of the CRL that lists it; revocations queued before a publication all carry
// that publication's time; and the removeFromCRL entry's date is the time of the release, not the
// original hold date.
func TestADCSConformance(t *testing.T) {
	c := loadCapture(t, "adcs", "base1", "delta1", "base2", "delta2", "base3", "delta3")
	order := []string{"base1", "delta1", "base2", "delta2", "base3", "delta3"}
	for i := 1; i < len(order); i++ {
		if c.crls[order[i]].Number.Cmp(c.crls[order[i-1]].Number) <= 0 {
			t.Fatalf("CRL numbers are not one increasing sequence: %s=%v %s=%v", order[i-1], c.crls[order[i-1]].Number, order[i], c.crls[order[i]].Number)
		}
	}
	// Every delta names the same, older base.
	first := deltaBase(t, c.crls["delta1"])
	for _, d := range []string{"delta1", "delta2", "delta3"} {
		if n := deltaBase(t, c.crls[d]); n == nil || n.Cmp(first) != 0 || n.Cmp(c.crls["base1"].Number) >= 0 {
			t.Fatalf("%s names base %v; expected the CA's first base %v, below %v", d, n, first, c.crls["base1"].Number)
		}
	}
	// Cumulative: the later deltas still list the revocations of the earlier ones.
	for _, u := range []string{"u1", "u3"} {
		for _, d := range []string{"delta1", "delta2", "delta3"} {
			if _, ok := c.entry(d, u); !ok {
				t.Fatalf("%s does not list %s: a delta is cumulative since its base", d, u)
			}
		}
	}
	hold, ok := c.entry("delta1", "u2")
	if !ok || hold.ReasonCode != 6 {
		t.Fatalf("delta1 does not list the hold: %+v", hold)
	}
	rel, ok := c.entry("delta2", "u2")
	if !ok || rel.ReasonCode != 8 || !rel.RevocationTime.After(hold.RevocationTime) {
		t.Fatalf("delta2 should carry removeFromCRL for u2 dated after the hold (%v): %+v", hold.RevocationTime, rel)
	}
	if _, listed := c.entry("base3", "u2"); listed {
		t.Fatal("the released certificate is still on the base issued after the release")
	}
	// The back-dating: revocations are dated after the thisUpdate of the CRL that first lists them.
	if !hold.RevocationTime.After(c.crls["delta1"].ThisUpdate) {
		t.Fatalf("expected AD CS to back-date thisUpdate (%v) before the revocation (%v)", c.crls["delta1"].ThisUpdate, hold.RevocationTime)
	}

	// The status rule on this output. The revocation of u1, u2, u3 was recorded at hold.RevocationTime.
	r := hold.RevocationTime
	short := audit.StatusParams{W: time.Second, MinW: time.Millisecond, CAPeriod: time.Hour}
	// u4 is never revoked. Decided just after base1's thisUpdate it is final, resting on base and delta.
	a := c.crls["base1"].ThisUpdate.Add(time.Second)
	if st := c.status("u4", a, short); !st.IsFinal() {
		t.Fatalf("u4: %+v", st)
	}
	// u1: key compromise with no invalidity date is retroactive to notBefore.
	if st := c.status("u1", a, short); !st.Revoked || !st.Compromise {
		t.Fatalf("u1: %+v", st)
	}
	// u3 (superseded): revoked from the recorded time.
	if st := c.status("u3", r.Add(-time.Second), short); st.Revoked {
		t.Fatalf("u3 before: %+v", st)
	}
	if st := c.status("u3", r.Add(time.Second), short); !st.Revoked || st.ReasonCode != 4 {
		t.Fatalf("u3 after: %+v", st)
	}
	// The hold: refused by default; opted in, only inside [start, release].
	holds := short
	holds.AllowHolds = true
	if st := c.status("u2", r.Add(-time.Second), short); !st.Revoked || !st.PolicyRefusal {
		t.Fatalf("u2 by default: %+v", st)
	}
	if st := c.status("u2", r.Add(-time.Second), holds); st.Revoked {
		t.Fatalf("u2 before the hold, opted in: %+v", st)
	}
	if st := c.status("u2", r.Add(rel.RevocationTime.Sub(r)/2), holds); !st.Revoked {
		t.Fatalf("u2 during the hold, opted in: %+v", st)
	}
	if st := c.status("u2", rel.RevocationTime.Add(time.Second), holds); st.Revoked {
		t.Fatalf("u2 after the release, opted in: %+v", st)
	}
}
