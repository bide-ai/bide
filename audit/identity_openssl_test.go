package audit_test

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// The fixtures in testdata/openssl are produced by OpenSSL (`openssl ca`, `openssl ca -gencrl`; see
// gen.sh there): an issuing CA under a root, approver certificates issued with the bank's EKU,
// policy and a person-URI SAN, and the CRLs the CA software wrote for them, with real encodings of
// the CRL number, authority key identifier, reason codes, invalidity dates, a hold, a compromise
// with and without a date, and expiredCertsOnCRL. The CA issues every 30 minutes and each CRL is
// valid for 24 hours, so the CRLs overlap, as real CAs do. They show the verifier and the status
// rule read what a CA actually emits, not only what the standard library emits. OpenSSL cannot issue
// true delta CRLs (it writes the indicator only), so delta conformance against a CA that can
// (EJBCA, AD CS) is a separate check (docs/design/approver-identity.md, test plan item 32).

func fixturePEM(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "openssl", name+".pem"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("%s: no PEM block", name)
	}
	return blk.Bytes
}

func fixtureCert(t *testing.T, name string) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(fixturePEM(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type osslFx struct {
	t       *testing.T
	c       *audit.CertIdentity
	issuing *x509.Certificate
}

func newOSSLFx(t *testing.T) *osslFx {
	t.Helper()
	root, issuing := fixtureCert(t, "root"), fixtureCert(t, "issuing")
	p := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{root}, Inter: []audit.SPKIHash{audit.SPKIHashOf(issuing)},
		Identity: map[string]string{"good": testca.Person(1), "compromised": testca.Person(2), "held": testca.Person(3),
			"superseded": testca.Person(4), "nodate": testca.Person(5), "late": testca.Person(6)},
		IDKind: audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical:          audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
		RequirePolicyOnCAs: true,
	}
	return &osslFx{t: t, c: mustCompile(t, p), issuing: issuing}
}

func osslAt(h, m int) time.Time {
	return time.Date(2026, 10, 1, h, m, 0, 0, time.UTC)
}

func (f *osslFx) snap(names ...string) *audit.StatusSnapshot {
	f.t.Helper()
	sn := audit.NewStatusSnapshot()
	for _, n := range names {
		if _, err := sn.AddCRL(fixturePEM(f.t, n)); err != nil {
			f.t.Fatalf("%s: a CRL OpenSSL wrote was refused: %v", n, err)
		}
	}
	return sn
}

func (f *osslFx) verify(cert, id string, a time.Time) *audit.ApproverCert {
	f.t.Helper()
	ac, err := f.c.VerifyChain(id, [][]byte{fixturePEM(f.t, cert), fixturePEM(f.t, "issuing")}, a.Add(-5*time.Second), a)
	if err != nil {
		f.t.Fatalf("%s: the chain OpenSSL issued does not verify: %v", cert, err)
	}
	return ac
}

func (f *osslFx) leaf(sn *audit.StatusSnapshot, cert, id string, a time.Time, p audit.StatusParams) audit.CertStatus {
	f.t.Helper()
	return f.c.EvaluateStatus(f.verify(cert, id, a), a, sn, p).Certs[0]
}

func TestOpenSSLFixtures(t *testing.T) {
	f := newOSSLFx(t)
	sn := f.snap("crl1", "crl2", "crl3", "crl4", "crl5", "crl6", "rootcrl")
	short := audit.StatusParams{W: 30 * time.Minute, MinW: 10 * time.Minute, Interval: 30 * time.Minute}
	a := osslAt(12, 0).Add(5 * time.Second)

	// An unrevoked certificate: final, over coverage from the CRL in force at A (crl1) to crl5 (13:40).
	st := f.leaf(sn, "good", "good", a, short)
	if st.Revoked || st.Tier != audit.TierFinal || !st.S.Equal(osslAt(13, 40)) || st.Caveat == "" {
		t.Fatalf("good: %+v", st)
	}

	// Compromise with an invalidity date: effective at the earlier date, flagged.
	st = f.leaf(sn, "compromised", "compromised", a, short)
	if !st.Revoked || !st.E.Equal(osslAt(11, 30)) || st.ReasonCode != 1 || !st.Compromise {
		t.Fatalf("compromised: %+v", st)
	}
	// ...but a decision provable before the invalidity date is not touched by it.
	if st := f.leaf(sn, "compromised", "compromised", osslAt(11, 25), short); st.Revoked {
		t.Fatalf("compromised, earlier decision: %+v", st)
	}
	// Compromise with no invalidity date: retroactive to notBefore.
	st = f.leaf(sn, "nodate", "nodate", osslAt(11, 0), short)
	if !st.Revoked || !st.E.Equal(osslAt(0, 0)) || !st.Compromise {
		t.Fatalf("nodate: %+v", st)
	}
	// A hold counts as a revocation from its time on.
	if st := f.leaf(sn, "held", "held", a, short); !st.Revoked || st.ReasonCode != 6 {
		t.Fatalf("held: %+v", st)
	}
	if st := f.leaf(sn, "held", "held", osslAt(11, 35), short); st.Revoked {
		t.Fatalf("held, decision before the hold: %+v", st)
	}
	// A routine reason inside RoutineGrace still counts; outside it does not.
	withGrace := short
	withGrace.RoutineGrace = 30 * time.Minute
	if st := f.leaf(sn, "superseded", "superseded", a, withGrace); st.Revoked {
		t.Fatalf("superseded inside the grace: %+v", st)
	}
	withGrace.RoutineGrace = 10 * time.Minute
	if st := f.leaf(sn, "superseded", "superseded", a, withGrace); !st.Revoked || st.ReasonCode != 4 {
		t.Fatalf("superseded outside the grace: %+v", st)
	}

	// The whole chain: the issuing CA has status from the root's CRL, and nothing is revoked.
	cs := f.c.EvaluateStatus(f.verify("good", "good", a), a, sn, short)
	if cs.Revoked || len(cs.Certs) != 2 {
		t.Fatalf("chain: %+v", cs)
	}
}

// The overlap scenario with real CRLs: "late" (valid 11:00 to 13:00) is revoked at 12:55 for key
// compromise with no invalidity date (retroactive to notBefore). The CRL at 13:10, the first beyond its
// notAfter, lists it; the CA prunes the entry from the one at 13:40. A producer who omits the 13:10 CRL
// and presents 13:40 must not get a final verdict for a decision made at 12:00.
func TestOpenSSLOverlappingCRLsWithAnOmittedCRL(t *testing.T) {
	f := newOSSLFx(t)
	a := osslAt(12, 0).Add(5 * time.Second)
	p := audit.StatusParams{Interval: 30 * time.Minute} // default W and MinW
	if st := f.leaf(f.snap("crl1", "crl2", "crl3", "crl4", "crl5"), "late", "late", a, p); !st.Revoked || !st.Compromise {
		t.Fatalf("complete series: %+v", st)
	}
	st := f.leaf(f.snap("crl1", "crl2", "crl3", "crl5"), "late", "late", a, p)
	if st.Revoked {
		t.Fatalf("nothing in the series lists it: %+v", st)
	}
	if st.Tier == audit.TierFinal || st.LCKnown {
		t.Fatalf("a retroactively revoked leaf read as final after one CRL was omitted: %+v", st)
	}
	// crl6 keeps expired entries and lists it: seen even with the others omitted.
	if st := f.leaf(f.snap("crl1", "crl2", "crl3", "crl6"), "late", "late", a, p); !st.Revoked {
		t.Fatalf("crl6: %+v", st)
	}
}

// CRLs OpenSSL wrote that a verifier must refuse, and the real encoding of a delta indicator.
func TestOpenSSLRefusalsAndTheDeltaIndicatorEncoding(t *testing.T) {
	for _, name := range []string{"bad-sha1", "bad-idp", "bad-removefromcrl"} {
		if _, err := os.Stat(filepath.Join("testdata", "openssl", name+".pem")); err != nil {
			t.Fatalf("%s: %v (gen.sh writes it; a missing SHA-1 CRL means OpenSSL would not write one)", name, err)
		}
		if _, err := audit.NewStatusSnapshot().AddCRL(fixturePEM(t, name)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A CRL carrying a critical delta CRL indicator as OpenSSL encodes it is read as a delta: it
	// merges with the base it names (number 1) and gives status as of its own time.
	f := newOSSLFx(t)
	a := osslAt(11, 45)
	sn := f.snap("crl1", "pseudo-delta")
	st := f.leaf(sn, "good", "good", a, audit.StatusParams{Interval: 30 * time.Minute})
	if !has(st.Used, hashOf(t, fixturePEM(t, "crl1"))) || !has(st.Used, hashOf(t, fixturePEM(t, "pseudo-delta"))) {
		t.Fatalf("base and delta should both be relied on: %+v", st)
	}
	// Without its base it is no evidence of absence.
	if st := f.leaf(f.snap("pseudo-delta"), "good", "good", a, audit.StatusParams{Interval: 30 * time.Minute}); st.Tier != audit.TierNoInformation {
		t.Fatalf("delta alone: %+v", st)
	}
}
