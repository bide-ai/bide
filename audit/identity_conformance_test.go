package audit_test

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// Conformance against CAs that issue true delta CRLs. The fixtures are captured by the manually
// dispatched workflow .github/workflows/crl-fixtures.yml (EJBCA Community in a container, Microsoft AD
// CS on a Windows Server) and checked in, so these tests run offline. testdata/ejbca/capture.sh and
// testdata/adcs/capture.ps1 are the scripts; VERSION records what produced them.

// capture is one CA's checked-in capture: a two-tier PKI (an offline root and the issuing CA under it),
// four approver certificates the issuing CA issued, and the CRLs of both CAs.
type capture struct {
	t    *testing.T
	dir  string
	root *x509.Certificate
	ca   *x509.Certificate
	snap *audit.StatusSnapshot
	crls map[string]*x509.RevocationList
	ci   *audit.CertIdentity
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

func parseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	cert, err := x509.ParseCertificate(readPEM(t, path))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return cert
}

// loadCapture reads a capture and compiles the approver policy over it: the root is the anchor, the
// issuing CA is pinned, and the bank's EKU and policy are required, so VerifyChain runs on what the
// real CA wrote. backdate is the notBefore back-dating the CA is declared to apply.
func loadCapture(t *testing.T, dir string, backdate time.Duration, crls ...string) *capture {
	t.Helper()
	c := &capture{t: t, dir: filepath.Join("testdata", dir), snap: audit.NewStatusSnapshot(), crls: map[string]*x509.RevocationList{}}
	c.root = parseCert(t, filepath.Join(c.dir, "root.pem"))
	c.ca = parseCert(t, filepath.Join(c.dir, "ca.pem"))
	for _, n := range append([]string{"rootcrl"}, crls...) {
		der := readPEM(t, filepath.Join(c.dir, n+".pem"))
		if _, err := c.snap.AddCRL(der); err != nil {
			t.Fatalf("%s: a CRL the CA wrote was refused: %v", n, err)
		}
		var err error
		if c.crls[n], err = x509.ParseRevocationList(der); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]string{}
	for n := 1; n <= 4; n++ {
		ids[fmt.Sprintf("u%d", n)] = testca.Person(n)
	}
	c.ci = mustCompile(t, audit.IdentityPolicy{
		Anchors: []*x509.Certificate{c.root}, Inter: []audit.SPKIHash{audit.SPKIHashOf(c.ca)},
		Identity: ids, IDKind: audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical:          audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
		RequirePolicyOnCAs: true, NotBeforeBackdate: backdate,
	})
	return c
}

func (c *capture) leaf(name string) *x509.Certificate {
	c.t.Helper()
	return parseCert(c.t, filepath.Join(c.dir, name+".pem"))
}

// verify runs VerifyChain on the real certificates: the leaf, the issuing CA and the root, as an
// approver would carry them.
func (c *capture) verify(leaf string, a time.Time) (*audit.ApproverCert, error) {
	c.t.Helper()
	return c.ci.VerifyChain(leaf, [][]byte{c.leaf(leaf).Raw, c.ca.Raw, c.root.Raw}, a, a)
}

// chainStatus is the status rule over the whole accepted chain, the leaf and then the issuing CA (whose
// status is the root's CRL).
func (c *capture) chainStatus(leaf string, a time.Time, p audit.StatusParams) audit.ChainStatus {
	c.t.Helper()
	ac, err := c.verify(leaf, a)
	if err != nil {
		c.t.Fatalf("VerifyChain %s at %v: %v", leaf, a, err)
	}
	return c.ci.EvaluateStatus(ac, a, c.snap, p)
}

// liveParams is the live count at now: the issuing CA's CRLs must be at most an hour old, the offline
// root's (rootAge: its CRL period) may be as old as that period, and skew is the issuing CA's declared
// thisUpdate back-dating.
func (c *capture) liveParams(now time.Time, rootAge, skew time.Duration) audit.StatusParams {
	return audit.StatusParams{
		CAPeriod: 90 * time.Minute, Now: now, MaxStatusAge: time.Hour, CountLive: true,
		ThisUpdateSkewFor: map[audit.SPKIHash]time.Duration{audit.SPKIHashOf(c.ca): skew},
		MaxStatusAgeFor:   map[audit.SPKIHash]time.Duration{audit.SPKIHashOf(c.root): rootAge},
		CAPeriodFor:       map[audit.SPKIHash]time.Duration{audit.SPKIHashOf(c.root): rootAge},
	}
}

func (c *capture) status(leaf string, a time.Time, p audit.StatusParams) audit.CertStatus {
	c.t.Helper()
	return c.chainStatus(leaf, a, p).Certs[0]
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
	c := loadCapture(t, "ejbca", 0, "base1", "delta1", "base2", "delta2", "base3", "delta3")
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

	// The real certificates pass VerifyChain under the approver policy (EKU, policy on the leaf and on both
	// CAs, URI subject alternative name, digitalSignature, 24 hours), with the CA's own ten minute notBefore
	// offset, and the whole chain has status: the leaf from the issuing CA's CRLs, the issuing CA from the
	// root's CRL.
	u4 := c.leaf("u4")
	if span := u4.NotAfter.Sub(u4.NotBefore); span != 24*time.Hour-time.Second {
		t.Fatalf("EJBCA's 24 hour profile gave a certificate valid for %v", span)
	}
	if !u4.NotBefore.Before(b1.Add(-9 * time.Minute)) {
		t.Fatalf("expected the ten minute notBefore offset: notBefore %v, first CRL %v", u4.NotBefore, b1)
	}
	live := c.liveParams(c.crls["delta3"].ThisUpdate.Add(5*time.Minute), 24*time.Hour, 0)
	cs := c.chainStatus("u4", a, live)
	if len(cs.Certs) != 2 || cs.Certs[1].Revoked || cs.Certs[1].Tier == audit.TierNoInformation {
		t.Fatalf("the issuing CA's status from the root's CRL: %+v", cs)
	}

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
	c := loadCapture(t, "adcs", 10*time.Minute, "base1", "delta1", "base2", "delta2", "base3", "delta3")
	order := []string{"base1", "delta1", "base2", "delta2", "base3", "delta3"}
	for i := 1; i < len(order); i++ {
		if c.crls[order[i]].Number.Cmp(c.crls[order[i-1]].Number) <= 0 {
			t.Fatalf("CRL numbers are not one increasing sequence: %s=%v %s=%v", order[i-1], c.crls[order[i-1]].Number, order[i], c.crls[order[i]].Number)
		}
	}
	// Every delta names the same base, the first full CRL after the CA's deltas were switched on, and keeps
	// naming it after later bases are published.
	first := deltaBase(t, c.crls["delta1"])
	for _, d := range []string{"delta1", "delta2", "delta3"} {
		if n := deltaBase(t, c.crls[d]); n == nil || n.Cmp(first) != 0 || n.Cmp(c.crls["base1"].Number) != 0 {
			t.Fatalf("%s names base %v; expected the first captured base %v", d, n, c.crls["base1"].Number)
		}
	}
	if c.crls["delta3"].Number.Cmp(c.crls["base3"].Number) <= 0 {
		t.Fatal("delta3 is not numbered after base3")
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

	// The certificates are valid for 24 hours plus the ten minutes notBefore is back-dated by, which is
	// over the default 24 hour lifetime limit: refused unless the back-dating is declared.
	u4 := c.leaf("u4")
	if span := u4.NotAfter.Sub(u4.NotBefore); span != 24*time.Hour+10*time.Minute {
		t.Fatalf("expected AD CS to back-date notBefore by ten minutes under a 24 hour validity: valid for %v", span)
	}
	strict := loadCapture(t, "adcs", 0, "base1")
	if _, err := strict.verify("u4", c.crls["base1"].ThisUpdate.Add(time.Second)); err == nil {
		t.Fatal("a 24h10m certificate passed with no back-dating declared")
	}
	// The root's CRL was published before any decision and is good for a week: the issuing CA certificate's
	// status is a live count on it, with the root's own age limit, and no better than provisional.
	live := c.liveParams(c.crls["delta3"].ThisUpdate.Add(5*time.Minute), 7*24*time.Hour, 10*time.Minute)
	cs := c.chainStatus("u4", c.crls["base1"].ThisUpdate.Add(time.Second), live)
	if len(cs.Certs) != 2 || cs.Certs[1].Revoked || cs.Certs[1].Tier != audit.TierProvisional {
		t.Fatalf("the issuing CA's status from the root's CRL: %+v", cs)
	}
	if c.crls["rootcrl"].ThisUpdate.After(c.crls["base1"].ThisUpdate) {
		t.Fatal("the root's CRL was expected to predate the issuing CA's first CRL")
	}
	// Offline, with no later root CRL, there is no evidence of absence for the CA certificate.
	off := c.chainStatus("u4", c.crls["base1"].ThisUpdate.Add(time.Second), audit.StatusParams{CAPeriod: time.Hour})
	if off.Certs[1].Tier != audit.TierNoInformation {
		t.Fatalf("offline, the root's CRL predates the decision: %+v", off.Certs[1])
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

// The same captures judged with realistic windows: the compromise-reporting window and the minimum window
// at their defaults (24 hours and 30 minutes), the CA's declared one hour period, and the live count
// with a one hour age limit. The capture spans minutes, not the 24 hours of a certificate's life, so no
// verdict is final: the rule says provisional, with the revocations still found, which is what a bank
// sees for the first day.
func TestEJBCARealisticWindows(t *testing.T) {
	c := loadCapture(t, "ejbca", 0, "base1", "delta1", "base2", "delta2", "base3", "delta3")
	a := c.crls["base1"].ThisUpdate.Add(time.Second)
	last := c.crls["delta3"].ThisUpdate
	p := c.liveParams(last.Add(5*time.Minute), 24*time.Hour, 0)
	p.CAPeriod = time.Hour
	if st := c.status("u4", a, p); st.Revoked || st.Tier != audit.TierProvisional {
		t.Fatalf("u4: %+v", st)
	}
	if st := c.status("u1", a, p); !st.Revoked || !st.Compromise {
		t.Fatalf("u1: %+v", st)
	}
	// An hour and a half later the CRLs are past nextUpdate: nothing counts.
	late := p
	late.Now = c.crls["base3"].NextUpdate.Add(time.Second)
	if st := c.status("u4", a, late); st.Tier != audit.TierNoInformation {
		t.Fatalf("u4 on stale CRLs: %+v", st)
	}
	// Offline (no clock), a decision a day in: the series ends before the certificate does.
	off := audit.StatusParams{CAPeriod: time.Hour}
	if st := c.status("u4", a, off); st.IsFinal() || st.Revoked {
		t.Fatalf("u4 offline: %+v", st)
	}
}

// AD CS with realistic windows and its declared thisUpdate back-dating: the default compromise window and
// minimum window, the live count with a one hour age limit, and the declared ten minute skew, which is
// what lets a CRL stamped ten minutes before it was issued count as fresh for as long as it was issued.
func TestADCSRealisticWindows(t *testing.T) {
	c := loadCapture(t, "adcs", 10*time.Minute, "base1", "delta1", "base2", "delta2", "base3", "delta3")
	a := c.crls["base1"].ThisUpdate.Add(time.Second)
	last := c.crls["delta3"].ThisUpdate
	p := c.liveParams(last.Add(5*time.Minute), 7*24*time.Hour, 10*time.Minute)
	if st := c.status("u4", a, p); st.Revoked || st.Tier != audit.TierProvisional {
		t.Fatalf("u4: %+v", st)
	}
	if st := c.status("u1", a, p); !st.Revoked || !st.Compromise {
		t.Fatalf("u1: %+v", st)
	}
	// A CRL stamped 62 minutes ago was issued 52 minutes ago: fresh against a one hour limit only with the
	// skew declared.
	aged := p
	aged.Now = last.Add(62 * time.Minute)
	if st := c.status("u4", a, aged); st.Tier == audit.TierNoInformation {
		t.Fatalf("u4 with the skew declared: %+v", st)
	}
	aged.ThisUpdateSkewFor = nil
	if st := c.status("u4", a, aged); st.Tier != audit.TierNoInformation {
		t.Fatalf("u4 without the skew: %+v", st)
	}
}

// A real AD CS CA that signs with SHA-1 (installed from a PFX into a legacy provider; testdata/adcs-sha1)
// is refused, certificates and CRLs alike, not accepted with a warning.
func TestADCSSHA1OutputIsRefused(t *testing.T) {
	dir := filepath.Join("testdata", "adcs-sha1")
	root, ca := parseCert(t, filepath.Join(dir, "root.pem")), parseCert(t, filepath.Join(dir, "ca.pem"))
	leaf := parseCert(t, filepath.Join(dir, "u1.pem"))
	if leaf.SignatureAlgorithm != x509.SHA1WithRSA {
		t.Fatalf("the fixture leaf is signed with %v, expected SHA-1", leaf.SignatureAlgorithm)
	}
	ci := mustCompile(t, audit.IdentityPolicy{
		Anchors: []*x509.Certificate{root}, Inter: []audit.SPKIHash{audit.SPKIHashOf(ca)},
		Identity: map[string]string{"u1": testca.Person(1)}, IDKind: audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}), NotBeforeBackdate: 10 * time.Minute,
	})
	_, err := ci.VerifyChain("u1", [][]byte{leaf.Raw, ca.Raw, root.Raw}, leaf.NotBefore.Add(time.Hour), leaf.NotBefore.Add(time.Hour))
	if err == nil {
		t.Fatal("a SHA-1 signed leaf was accepted")
	}
	// The leaf is refused as a chain that does not verify, by Go's own refusal of a SHA-1 signature (the
	// fixture leaf is also RSA, which bide's key profile would refuse): the cause is pinned, not just an error.
	wantReason(t, err, agent.ReasonCertChain)
	if !strings.Contains(err.Error(), "insecure algorithm SHA1-RSA") {
		t.Fatalf("the leaf was refused for another reason: %v", err)
	}
	snap := audit.NewStatusSnapshot()
	for _, n := range []string{"base1", "delta1"} {
		_, err := snap.AddCRL(readPEM(t, filepath.Join(dir, n+".pem")))
		if err == nil {
			t.Fatalf("a SHA-1 signed CRL (%s) was accepted", n)
		}
		wantStatusErr(t, err, agent.ReasonStatusUnknown)
		if !strings.Contains(err.Error(), "signature algorithm") || !strings.Contains(err.Error(), "not in the accepted profile") {
			t.Fatalf("%s: refused for another reason: %v", n, err)
		}
	}
	// The root's own SHA-256 CRL from the same capture is fine: the refusal is the algorithm, not the source.
	if _, err := snap.AddCRL(readPEM(t, filepath.Join(dir, "rootcrl.pem"))); err != nil {
		t.Fatalf("the SHA-256 root CRL: %v", err)
	}
}

// No private key or password may be checked in with the captures: they are certificates, CRLs and
// settings only (the fixture CAs are test anchors that must never be reused).
func TestNoPrivateKeyMaterialInTestdata(t *testing.T) {
	err := filepath.WalkDir("testdata", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch ext := strings.ToLower(filepath.Ext(path)); ext {
		case ".pfx", ".p12", ".key", ".jks", ".keystore", ".rsp", ".req", ".csr":
			t.Errorf("%s: key material or a request file under testdata", path)
		}
		if strings.Contains(strings.ToLower(filepath.Base(path)), "password") {
			t.Errorf("%s: a password file under testdata", path)
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), "PRIVATE KEY") || strings.Contains(string(b), "ENCRYPTED") && strings.Contains(string(b), "-----BEGIN") {
			t.Errorf("%s: private key material", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
