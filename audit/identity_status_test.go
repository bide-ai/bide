package audit_test

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

const (
	min  = time.Minute
	hour = time.Hour
)

// stFx is a verified chain (root -> inter -> leaf) with the helpers to issue CRLs for the leaf (as
// the intermediate) and for the intermediate (as the root). The leaf is valid from T0-1h to T0+1h.
type stFx struct {
	*fixture
	t    *testing.T
	c    *audit.CertIdentity
	leaf *testca.Leaf
	ac   *audit.ApproverCert
	a    time.Time // A(d)
}

func newStFx(t *testing.T) *stFx { return newStFxAt(t, idT0.Add(5*time.Second)) }

func newStFxAt(t *testing.T, a time.Time) *stFx {
	t.Helper()
	f := newFixture(t)
	c := mustCompile(t, f.policy())
	leaf := f.leaf(t, nil)
	decided := a
	if decided.After(idT0) {
		decided = idT0
	}
	ac, err := c.VerifyChain("alice", leaf.Chain(), decided, a)
	if err != nil {
		t.Fatalf("fixture chain: %v", err)
	}
	return &stFx{fixture: f, t: t, c: c, leaf: leaf, ac: ac, a: a}
}

// lcrl is a CRL by the leaf's issuer (the intermediate), valid for next after this.
func (s *stFx) lcrl(num int64, this time.Time, next time.Duration, entries ...x509.RevocationListEntry) []byte {
	s.t.Helper()
	return s.inter.NewCRL(s.t, testca.CRLOpts{Number: num, ThisUpdate: this, NextUpdate: this.Add(next), Entries: entries})
}

// rcrl is a CRL by the intermediate's issuer (the root).
func (s *stFx) rcrl(num int64, this time.Time, next time.Duration, entries ...x509.RevocationListEntry) []byte {
	s.t.Helper()
	return s.root.NewCRL(s.t, testca.CRLOpts{Number: num, ThisUpdate: this, NextUpdate: this.Add(next), Entries: entries})
}

func (s *stFx) snap(crls ...[]byte) *audit.StatusSnapshot {
	s.t.Helper()
	sn := audit.NewStatusSnapshot()
	for i, der := range crls {
		if _, err := sn.AddCRL(der); err != nil {
			s.t.Fatalf("AddCRL %d: %v", i, err)
		}
	}
	return sn
}

// declared fills in the CA issuance interval the fixtures' CRLs keep (no two more than 40 minutes
// apart) unless the test sets its own; a negative Interval declares none.
func declared(p audit.StatusParams) audit.StatusParams {
	if p.Interval == 0 {
		p.Interval = 40 * min
	}
	return p
}

func (s *stFx) eval(sn *audit.StatusSnapshot, p audit.StatusParams) audit.ChainStatus {
	return s.c.EvaluateStatus(s.ac, s.a, sn, declared(p))
}

// leafStatus evaluates and returns the leaf's own status.
func (s *stFx) leafStatus(sn *audit.StatusSnapshot, p audit.StatusParams) audit.CertStatus {
	s.t.Helper()
	cs := s.eval(sn, p)
	if len(cs.Certs) != 2 {
		s.t.Fatalf("a leaf under one intermediate has 2 certificates to check, got %d", len(cs.Certs))
	}
	return cs.Certs[0]
}

func wantStatusErr(t *testing.T, err error, reason string) {
	t.Helper()
	var se *audit.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not an *audit.StatusError", err)
	}
	if se.Reason != reason || se.Detail == "" {
		t.Fatalf("reason %q detail %q, want reason %q with a detail", se.Reason, se.Detail, reason)
	}
}

func ext(oid asn1.ObjectIdentifier, critical bool, v []byte) pkix.Extension {
	return pkix.Extension{Id: oid, Critical: critical, Value: v}
}

func genTime(t *testing.T, tm time.Time) []byte {
	t.Helper()
	b, err := asn1.MarshalWithParams(tm.UTC(), "generalized")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- CRL intake: what is refused, and why ----

func TestAddCRLAcceptsAPlainCRL(t *testing.T) {
	s := newStFx(t)
	der := s.lcrl(7, idT0, hour, testca.Revoked(s.leaf.Cert, idT0.Add(30*min), 1))
	sn := audit.NewStatusSnapshot()
	info, err := sn.AddCRL(der)
	if err != nil {
		t.Fatal(err)
	}
	if info.Number.Int64() != 7 || info.Entries != 1 || !info.ThisUpdate.Equal(idT0) || !info.NextUpdate.Equal(idT0.Add(hour)) {
		t.Fatalf("info %+v", info)
	}
	// The same CRL again is one CRL, not two.
	if _, err := sn.AddCRL(der); err != nil || sn.Len() != 1 {
		t.Fatalf("second add: %v, %d CRLs", err, sn.Len())
	}
}

func TestAddCRLRefusals(t *testing.T) {
	s := newStFx(t)
	this := idT0
	idp := []byte{0x30, 0x00}
	cases := map[string]testca.CRLOpts{
		"issuing distribution point, critical":     {Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 28}, true, idp)}},
		"issuing distribution point, non-critical": {Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 28}, false, idp)}},
		"unknown critical extension":               {Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 5}, true, []byte{5, 0})}},
		"indirect entry (certificateIssuer)": {Entries: []x509.RevocationListEntry{{
			SerialNumber: big.NewInt(5), RevocationTime: this,
			ExtraExtensions: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 29}, true, []byte{0x30, 0x00})}}}},
		"unknown critical entry extension": {Entries: []x509.RevocationListEntry{{
			SerialNumber: big.NewInt(5), RevocationTime: this,
			ExtraExtensions: []pkix.Extension{ext(asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 6}, true, []byte{5, 0})}}}},
		"removeFromCRL in a full CRL": {Entries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(5), RevocationTime: this, ReasonCode: 8}}},
		"malformed invalidity date": {Entries: []x509.RevocationListEntry{{
			SerialNumber: big.NewInt(5), RevocationTime: this,
			ExtraExtensions: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 24}, false, []byte{0x04, 0x01, 0x00})}}}},
		"delta CRL":       {Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 27}, true, []byte{0x02, 0x01, 0x01})}},
		"sha-1 signature": {SigAlg: x509.ECDSAWithSHA1},
	}
	for name, o := range cases {
		o.Number, o.ThisUpdate, o.NextUpdate = 5, this, this.Add(hour)
		der, err := s.inter.TryCRL(o)
		if err != nil {
			t.Logf("%s: the standard library will not create it: %v", name, err)
			continue
		}
		_, err = audit.NewStatusSnapshot().AddCRL(der)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantStatusErr(t, err, agent.ReasonStatusUnknown)
	}
	// Garbage and trailing bytes.
	good := s.lcrl(1, this, hour)
	for name, der := range map[string][]byte{"empty": nil, "garbage": []byte("nope"), "trailing": append(append([]byte{}, good...), 0)} {
		_, err := audit.NewStatusSnapshot().AddCRL(der)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantStatusErr(t, err, agent.ReasonStatusUnknown)
	}
	// An oversized input is refused before parsing.
	_, err := audit.NewStatusSnapshot().AddCRL(make([]byte, 65<<20))
	wantStatusErr(t, err, agent.ReasonStatusUnknown)
}

// A non-critical extension bide does not process is ignored, as RFC 5280 says.
func TestAddCRLIgnoresUnknownNonCriticalExtensions(t *testing.T) {
	s := newStFx(t)
	der := s.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: idT0, NextUpdate: idT0.Add(hour),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 5}, false, []byte{5, 0})}})
	if _, err := audit.NewStatusSnapshot().AddCRL(der); err != nil {
		t.Fatal(err)
	}
}

// ---- who signed it ----

// A CRL counts only for certificates its signer issued: the signature is checked against the
// issuing certificate of the chain, so a CRL from another CA, or from the root about a serial it did
// not issue, says nothing about the leaf.
func TestCRLFromAnotherIssuerIsIgnored(t *testing.T) {
	s := newStFx(t)
	// The root signs a CRL that lists the leaf's serial. The leaf's issuer is the intermediate.
	byRoot := s.rcrl(1, idT0.Add(10*time.Second), hour, testca.Revoked(s.leaf.Cert, idT0, 1))
	// A stranger CA with the intermediate's name signs one too.
	other := testca.NewRoot(t, "inter", testca.Opts{})
	byOther := other.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: idT0.Add(10 * time.Second), NextUpdate: idT0.Add(hour),
		Entries: []x509.RevocationListEntry{testca.Revoked(s.leaf.Cert, idT0, 1)}})
	st := s.leafStatus(s.snap(byRoot, byOther), audit.StatusParams{})
	if st.Revoked {
		t.Fatal("a CRL by a CA that did not issue the leaf revoked it")
	}
	if st.Tier != audit.TierNoInformation {
		t.Fatalf("tier %v: CRLs by other issuers are no information", st.Tier)
	}
}

// Serial numbers are unique per issuer only: the root's entry for the intermediate's serial does not
// touch a leaf that happens to share that serial.
func TestEntriesAreKeyedByIssuer(t *testing.T) {
	s := newStFx(t)
	leaf2 := s.inter.NewLeaf(t, testca.Opts{Mutate: func(c *x509.Certificate) { c.SerialNumber = s.inter.Cert.SerialNumber }})
	ac2, err := s.c.VerifyChain("alice", leaf2.Chain(), idT0, s.a)
	if err != nil {
		t.Fatal(err)
	}
	// The root revokes the intermediate (serial n); the leaf has serial n too but another issuer.
	sn := s.snap(s.rcrl(1, idT0.Add(10*time.Second), hour), s.lcrl(1, idT0.Add(10*time.Second), hour))
	cs := s.c.EvaluateStatus(ac2, s.a, sn, audit.StatusParams{})
	if cs.Certs[0].Revoked {
		t.Fatal("clean leaf CRL, but the leaf read as revoked")
	}
	snRev := s.snap(s.rcrl(1, idT0.Add(10*time.Second), hour, testca.Revoked(s.inter.Cert, idT0, 1)), s.lcrl(1, idT0.Add(10*time.Second), hour))
	cs = s.c.EvaluateStatus(ac2, s.a, snRev, audit.StatusParams{})
	if cs.Certs[0].Revoked {
		t.Fatal("the root's entry for the intermediate's serial revoked a leaf with the same serial from another issuer")
	}
	if !cs.Certs[1].Revoked {
		t.Fatal("the intermediate's own entry was not read")
	}
}

// ---- E: when a revocation takes effect ----

func TestEffectiveRevocationTime(t *testing.T) {
	a := idT0.Add(5 * time.Second)
	cases := []struct {
		name    string
		entry   func(s *stFx) x509.RevocationListEntry
		grace   time.Duration
		revoked bool
	}{
		{"revoked before the decision", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(-time.Second), 0) }, 0, true},
		{"revoked exactly at A", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a, 0) }, 0, true},
		{"revoked one second after A (times are encoded to the second)", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(time.Second), 0) }, 0, false},
		{"revoked after the decision", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(10*min), 0) }, 0, false},
		{"invalidity date earlier than the revocation", func(s *stFx) x509.RevocationListEntry {
			return testca.WithInvalidity(testca.Revoked(s.leaf.Cert, a.Add(10*min), 0), a.Add(-time.Second))
		}, 0, true},
		{"invalidity date later than the revocation never extends trust", func(s *stFx) x509.RevocationListEntry {
			return testca.WithInvalidity(testca.Revoked(s.leaf.Cert, a.Add(-time.Second), 0), a.Add(10*min))
		}, 0, true},
		{"keyCompromise with no invalidity date is retroactive to notBefore", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(10*min), 1) }, 0, true},
		{"cACompromise with no invalidity date is retroactive", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(10*min), 2) }, 0, true},
		{"aACompromise with no invalidity date is retroactive", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(10*min), 10) }, 0, true},
		{"keyCompromise with a later invalidity date", func(s *stFx) x509.RevocationListEntry {
			return testca.WithInvalidity(testca.Revoked(s.leaf.Cert, a.Add(10*min), 1), a.Add(5*min))
		}, 0, false},
		{"keyCompromise with an earlier invalidity date", func(s *stFx) x509.RevocationListEntry {
			return testca.WithInvalidity(testca.Revoked(s.leaf.Cert, a.Add(10*min), 1), a.Add(-time.Hour))
		}, 0, true},
		// RoutineGrace: for routine reasons a decision with A < E + RoutineGrace still counts.
		{"affiliationChanged inside the grace", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(-30*time.Second), 3) }, min, false},
		{"affiliationChanged at the grace edge", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(-time.Minute), 3) }, min, true},
		{"affiliationChanged outside the grace", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(-2*min), 3) }, min, true},
		{"superseded inside the grace", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(-30*time.Second), 4) }, min, false},
		{"cessationOfOperation inside the grace", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(-30*time.Second), 5) }, min, false},
		{"privilegeWithdrawn inside the grace", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(-30*time.Second), 9) }, min, false},
		{"unspecified never gets grace", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(-30*time.Second), 0) }, min, true},
		{"keyCompromise never gets grace", func(s *stFx) x509.RevocationListEntry {
			return testca.WithInvalidity(testca.Revoked(s.leaf.Cert, a.Add(-30*time.Second), 1), a.Add(-30*time.Second))
		}, min, true},
		{"certificateHold never gets grace", func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, a.Add(-30*time.Second), 6) }, min, true},
	}
	for _, tc := range cases {
		s := newStFx(t)
		sn := s.snap(s.lcrl(1, a.Add(time.Second), hour, tc.entry(s)))
		st := s.leafStatus(sn, audit.StatusParams{RoutineGrace: tc.grace})
		if st.Revoked != tc.revoked {
			t.Fatalf("%s: revoked=%v (E=%v code=%d), want %v", tc.name, st.Revoked, st.E, st.ReasonCode, tc.revoked)
		}
		if tc.revoked && st.E.IsZero() {
			t.Fatalf("%s: revoked with no E", tc.name)
		}
	}
}

func TestEffectiveTimeValuesAndCompromiseFlag(t *testing.T) {
	a := idT0.Add(5 * time.Second)
	s := newStFx(t)
	// Retroactive compromise: E is the leaf's notBefore, and the finding is flagged for dispute.
	st := s.leafStatus(s.snap(s.lcrl(1, a.Add(time.Second), hour, testca.Revoked(s.leaf.Cert, a.Add(10*min), 1))), audit.StatusParams{})
	if !st.Revoked || !st.E.Equal(s.leaf.Cert.NotBefore) || !st.Compromise || st.ReasonCode != 1 {
		t.Fatalf("%+v", st)
	}
	// E is the earlier of the invalidity date and the revocation time.
	inv := a.Add(-20 * min)
	s = newStFx(t)
	st = s.leafStatus(s.snap(s.lcrl(1, a.Add(time.Second), hour, testca.WithInvalidity(testca.Revoked(s.leaf.Cert, a.Add(-10*min), 5), inv))), audit.StatusParams{})
	if !st.E.Equal(inv) || st.Compromise {
		t.Fatalf("E=%v want %v, compromise=%v", st.E, inv, st.Compromise)
	}
	if st.RevokedBy == ([32]byte{}) {
		t.Fatal("the finding does not name the CRL that revoked")
	}
}

// ---- the series: absence, L(C), windows, tiers ----

// Timeline used below (leaf valid T0-1h .. T0+1h, A = T0+5s). Each CRL is issued no later than the
// previous one's nextUpdate, so the series has no gap:
//
//	c1 T0 .. +30m (in force at A)   c2 T0+30m .. +40m   cL T0+70m .. +30m (the first beyond notAfter)
func series(s *stFx) (c1, c2, cL []byte) {
	return s.lcrl(1, idT0, 30*min),
		s.lcrl(2, idT0.Add(30*min), 40*min),
		s.lcrl(3, idT0.Add(70*min), 30*min)
}

func TestFinalNeedsTheCRLAfterNotAfter(t *testing.T) {
	s := newStFx(t)
	c1, c2, cL := series(s)
	// With the CRL at L(C) present, the status is final.
	st := s.leafStatus(s.snap(c1, c2, cL), audit.StatusParams{})
	if st.Tier != audit.TierFinal || st.Revoked || !st.LCKnown || !st.LC.Equal(idT0.Add(70*min)) || !st.S.Equal(idT0.Add(70*min)) {
		t.Fatalf("%+v", st)
	}
	wantW := idT0.Add(70 * min).Sub(s.a)
	if st.Window != wantW {
		t.Fatalf("W' = %v, want L - A = %v (cut from the 24h window by the certificate's life)", st.Window, wantW)
	}
	if len(st.Used) == 0 {
		t.Fatal("no CRL recorded as relied on")
	}
	// Without it the best status is provisional: a later usable one could still exist.
	st = s.leafStatus(s.snap(c1, c2), audit.StatusParams{})
	if st.Tier != audit.TierProvisional || st.LCKnown {
		t.Fatalf("no CRL after notAfter yet: %+v", st)
	}
}

func TestNoInformationWithoutAnyUsableCRL(t *testing.T) {
	s := newStFx(t)
	st := s.leafStatus(s.snap(), audit.StatusParams{})
	if st.Tier != audit.TierNoInformation || st.Revoked {
		t.Fatalf("empty snapshot: %+v", st)
	}
	// A CRL older than the decision is not evidence of absence: the revocation may have come later.
	st = s.leafStatus(s.snap(s.lcrl(1, s.a.Add(-time.Second), hour)), audit.StatusParams{})
	if st.Tier != audit.TierNoInformation {
		t.Fatalf("CRL issued before A(d): %+v", st)
	}
	// Exactly at A it is usable.
	st = s.leafStatus(s.snap(s.lcrl(1, s.a, hour)), audit.StatusParams{})
	if st.Tier == audit.TierNoInformation {
		t.Fatalf("CRL issued at A(d) is usable: %+v", st)
	}
}

// The listing in a CRL issued before the decision still revokes: a listing is a revocation from its
// time on, whatever the date of the CRL.
func TestListingBeforeADoesRevoke(t *testing.T) {
	s := newStFx(t)
	old := s.lcrl(1, s.a.Add(-10*min), hour, testca.Revoked(s.leaf.Cert, s.a.Add(-20*min), 0))
	st := s.leafStatus(s.snap(old), audit.StatusParams{})
	if !st.Revoked {
		t.Fatalf("%+v", st)
	}
}

func TestProvisionalWhenTheWindowIsNotCovered(t *testing.T) {
	s := newStFx(t)
	_, c2, _ := series(s)
	// Only c2: S = T0+30m is before A + W' (W' = W = 24h while L is unknown).
	st := s.leafStatus(s.snap(c2), audit.StatusParams{})
	if st.Tier != audit.TierProvisional {
		t.Fatalf("%+v", st)
	}
}

func TestShortWindowIsProvisional(t *testing.T) {
	// A decision a minute before notAfter has about one CRL period of window: below MinW (30 min).
	s := newStFxAt(t, idT0.Add(59*min))
	_, _, cL := series(s)
	pre := s.lcrl(1, idT0.Add(58*min), 12*min) // covers up to cL
	st := s.leafStatus(s.snap(pre, cL), audit.StatusParams{})
	if st.Window >= 30*min {
		t.Fatalf("test setup: W' = %v", st.Window)
	}
	if st.Tier != audit.TierProvisional || !strings.Contains(st.Note, "short window") {
		t.Fatalf("%+v", st)
	}
	// Lowering MinW makes the same evidence final.
	st = s.leafStatus(s.snap(pre, cL), audit.StatusParams{MinW: min})
	if st.Tier != audit.TierFinal {
		t.Fatalf("MinW 1m: %+v", st)
	}
}

func TestWindowParameterCapsW(t *testing.T) {
	s := newStFx(t)
	c1, c2, cL := series(s)
	// W of 10 minutes: W' = min(10m, L - A) = 10m, below the default MinW of 30m.
	st := s.leafStatus(s.snap(c1, c2, cL), audit.StatusParams{W: 10 * min})
	if st.Window != 10*min || st.Tier != audit.TierProvisional {
		t.Fatalf("%+v", st)
	}
	st = s.leafStatus(s.snap(c1, c2, cL), audit.StatusParams{W: 10 * min, MinW: 5 * min})
	if st.Tier != audit.TierFinal {
		t.Fatalf("%+v", st)
	}
}

// Test plan 12, the expiry sequence: the leaf is revoked after the decision and listed in CRL 1; it
// expires; the CRL after the first scheduled issuance beyond notAfter omits it.
func TestExpirySequence(t *testing.T) {
	s := newStFx(t)
	rev := testca.Revoked(s.leaf.Cert, idT0.Add(20*min), 0) // R > A: the decision stands
	crl0 := s.lcrl(1, idT0, 30*min)                         // in force at A
	crl1 := s.lcrl(2, idT0.Add(30*min), 40*min, rev)        // next update T0+70m
	crlL := s.lcrl(3, idT0.Add(70*min), 30*min, rev)        // the first beyond notAfter still lists it
	crl2 := s.lcrl(4, idT0.Add(140*min), 30*min)            // pruned: no longer lists it

	// CRL 2 alone: nothing establishes that it is the first CRL beyond notAfter, so its silence is no
	// information, never "not revoked".
	st := s.leafStatus(s.snap(crl2), audit.StatusParams{})
	if st.Tier != audit.TierNoInformation || st.Revoked {
		t.Fatalf("CRL 2 alone: %+v", st)
	}
	// CRL 1 present: per E versus A. E = R > A, so the decision counts, and the listing is seen.
	st = s.leafStatus(s.snap(crl0, crl1, crl2), audit.StatusParams{})
	if st.Revoked {
		t.Fatalf("E = R > A must not revoke: %+v", st)
	}
	if st.Tier == audit.TierFinal {
		t.Fatalf("a gap between CRL 1 and CRL 2 cannot be final: %+v", st)
	}
	// The complete series: final, and still not revoked because R > A.
	st = s.leafStatus(s.snap(crl0, crl1, crlL, crl2), audit.StatusParams{})
	if st.Revoked || st.Tier != audit.TierFinal {
		t.Fatalf("complete series: %+v", st)
	}
	// Had the revocation come before the decision (E <= A), any CRL that lists it revokes, even after
	// the entry is pruned from later CRLs.
	s2 := newStFx(t)
	early := testca.Revoked(s2.leaf.Cert, s2.a.Add(-time.Minute), 0)
	a1 := s2.lcrl(1, s2.a.Add(time.Second), 30*min, early)
	a2 := s2.lcrl(2, idT0.Add(140*min), 30*min)
	if !s2.leafStatus(s2.snap(a1, a2), audit.StatusParams{}).Revoked {
		t.Fatal("a pruned later CRL undid a listing")
	}
}

// With expiredCertsOnCRL a CRL keeps entries of expired certificates, so a CRL beyond the first
// scheduled one is usable and L(C) is unbounded.
func TestExpiredCertsOnCRLMakesLCUnbounded(t *testing.T) {
	s := newStFx(t)
	retained := testca.CRLOpts{Number: 5, ThisUpdate: idT0.Add(140 * min), NextUpdate: idT0.Add(170 * min),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 60}, false, genTime(t, idT0.Add(-24*hour)))}}
	der := s.inter.NewCRL(t, retained)
	pre := s.lcrl(4, idT0, 3*hour) // in force at A and reaching the retained CRL
	st := s.leafStatus(s.snap(pre, der), audit.StatusParams{W: hour, Interval: 3 * hour})
	if !st.Unbounded || st.Tier != audit.TierFinal {
		t.Fatalf("%+v", st)
	}
	// The retention date must reach back to the certificate's notAfter: a CRL that keeps expired
	// entries only from after this certificate expired says nothing about it.
	retained.Extra = []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 60}, false, genTime(t, idT0.Add(100*min)))}
	der = s.inter.NewCRL(t, retained)
	st = s.leafStatus(s.snap(der), audit.StatusParams{})
	if st.Unbounded || st.Tier == audit.TierFinal {
		t.Fatalf("retention starts after the certificate expired: %+v", st)
	}
	// Contractual retention recorded in the policy has the same effect.
	plain := s.lcrl(5, idT0.Add(140*min), 30*min)
	st = s.leafStatus(s.snap(plain), audit.StatusParams{Retained: true})
	if !st.Unbounded {
		t.Fatalf("%+v", st)
	}
}

// A gap in time coverage between the CRLs from S back to L(C) is not final; a gap in CRL numbers alone is fine.
func TestTimeCoverageNotNumbers(t *testing.T) {
	s := newStFx(t)
	c1 := s.lcrl(10, idT0, 30*min)
	c2 := s.lcrl(50, idT0.Add(30*min), 40*min) // numbers jump: that alone is nothing
	cL := s.lcrl(51, idT0.Add(70*min), 30*min)
	if st := s.leafStatus(s.snap(c1, c2, cL), audit.StatusParams{}); st.Tier != audit.TierFinal {
		t.Fatalf("number gap only: %+v", st)
	}
	// A time gap: c1 covers only to T0+40m but the next CRL we have is at T0+75m.
	cLate := s.lcrl(52, idT0.Add(75*min), 30*min)
	short := s.lcrl(11, idT0, 20*min)
	st := s.leafStatus(s.snap(short, cLate), audit.StatusParams{})
	if st.Tier == audit.TierFinal {
		t.Fatalf("time gap: %+v", st)
	}
}

// certificateHold: listed in one CRL and absent from the next (released): the listing stays.
func TestCertificateHoldStaysRevoked(t *testing.T) {
	s := newStFx(t)
	held := s.lcrl(1, s.a.Add(time.Second), 30*min, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 6))
	released := s.lcrl(2, s.a.Add(20*min), 30*min)
	if !s.leafStatus(s.snap(held, released), audit.StatusParams{}).Revoked {
		t.Fatal("a released hold undid the listing for a decision made during the hold")
	}
}

// Later CRLs can only take trust away: a CRL after L(C) that lists the leaf is a rejection, its silence is not an upgrade.
func TestLaterCRLsOnlyAddRejections(t *testing.T) {
	s := newStFx(t)
	c1, c2, cL := series(s)
	late := s.lcrl(9, idT0.Add(140*min), 30*min, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 0))
	if !s.leafStatus(s.snap(c1, c2, cL, late), audit.StatusParams{}).Revoked {
		t.Fatal("a late CRL listing the certificate with E <= A did not revoke")
	}
	// A late CRL that is silent changes nothing: the result is the same as without it.
	quiet := s.lcrl(9, idT0.Add(140*min), 30*min)
	with := s.leafStatus(s.snap(c1, c2, cL, quiet), audit.StatusParams{})
	without := s.leafStatus(s.snap(c1, c2, cL), audit.StatusParams{})
	if with.Tier != without.Tier || !with.S.Equal(without.S) || !with.LC.Equal(without.LC) {
		t.Fatalf("with %+v, without %+v", with, without)
	}
	// And it never rescues a decision with less evidence.
	if st := s.leafStatus(s.snap(c1, quiet), audit.StatusParams{}); st.Tier == audit.TierFinal {
		t.Fatalf("a late silent CRL upgraded a thin series: %+v", st)
	}
}

// ---- CRL number order ----

func TestNumberRollbackIsNotEvidenceOfAbsence(t *testing.T) {
	s := newStFx(t)
	newer := s.lcrl(20, idT0.Add(10*time.Second), 30*min, testca.Revoked(s.leaf.Cert, idT0.Add(20*min), 0))
	// A later-dated CRL with a LOWER number: not a legitimate successor.
	rolled := s.lcrl(19, idT0.Add(20*min), 30*min)
	st := s.leafStatus(s.snap(newer, rolled), audit.StatusParams{})
	for _, h := range st.Used {
		var rolledInfo [32]byte
		info, err := audit.NewStatusSnapshot().AddCRL(rolled)
		if err != nil {
			t.Fatal(err)
		}
		rolledInfo = info.Hash
		if h == rolledInfo {
			t.Fatal("the rolled-back CRL was relied on")
		}
	}
	// Its silence did not become the latest word: S is the number-20 CRL's time.
	if !st.S.Equal(idT0.Add(10 * time.Second)) {
		t.Fatalf("S = %v", st.S)
	}
}

// ---- live freshness ----

func TestLiveFreshness(t *testing.T) {
	s := newStFx(t)
	crl := s.lcrl(1, idT0.Add(10*time.Second), 30*min)
	sn := s.snap(crl)
	live := func(now time.Time, maxAge time.Duration) audit.CertStatus {
		return s.leafStatus(sn, audit.StatusParams{Now: now, MaxStatusAge: maxAge})
	}
	if st := live(idT0.Add(20*time.Second), hour); st.Tier == audit.TierNoInformation || st.Stale {
		t.Fatalf("fresh: %+v", st)
	}
	// Past nextUpdate: stale.
	st := live(idT0.Add(31*min), 2*hour)
	if st.Tier != audit.TierNoInformation || !st.Stale {
		t.Fatalf("past nextUpdate: %+v", st)
	}
	// Older than MaxStatusAge though still before nextUpdate.
	st = live(idT0.Add(20*min), 10*min)
	if st.Tier != audit.TierNoInformation || !st.Stale {
		t.Fatalf("older than MaxStatusAge: %+v", st)
	}
	// Live mode with no age limit is a configuration mistake and fails closed.
	st = live(idT0.Add(20*time.Second), 0)
	if st.Tier != audit.TierNoInformation {
		t.Fatalf("Now without MaxStatusAge: %+v", st)
	}
	// Freshness gates absence, not a listing: a stale CRL that lists the leaf still revokes.
	listed := s.lcrl(2, s.a.Add(time.Second), 10*min, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 0))
	st = s.leafStatus(s.snap(listed), audit.StatusParams{Now: idT0.Add(5 * hour), MaxStatusAge: hour})
	if !st.Revoked {
		t.Fatalf("stale listing ignored: %+v", st)
	}
}

// ---- distrust ----

func TestDistrustedIssuerStatusIsNoInformation(t *testing.T) {
	s := newStFx(t)
	crl := s.lcrl(1, idT0.Add(10*time.Second), 30*min)
	listed := s.lcrl(2, idT0.Add(20*time.Second), 30*min, testca.Revoked(s.leaf.Cert, idT0.Add(-time.Minute), 0))
	ih := audit.SPKIHashOf(s.inter.Cert)
	// Distrust from after the CRL's thisUpdate: unaffected.
	p := s.policy()
	p.Distrust = map[audit.SPKIHash]time.Time{ih: idT0.Add(hour)}
	c := mustCompile(t, p)
	ac, err := c.VerifyChain("alice", s.leaf.Chain(), idT0, s.a)
	if err != nil {
		t.Fatal(err)
	}
	cs := c.EvaluateStatus(ac, s.a, s.snap(crl), audit.StatusParams{})
	if cs.Certs[0].Tier == audit.TierNoInformation {
		t.Fatalf("distrust in the future: %+v", cs.Certs[0])
	}
	// Distrust at or before the CRL's thisUpdate: a clean CRL signed by a compromised key says nothing,
	// and neither does a listing it signs.
	p.Distrust = map[audit.SPKIHash]time.Time{ih: idT0.Add(10 * time.Second)}
	c = mustCompile(t, p)
	ac, err = c.VerifyChain("alice", s.leaf.Chain(), idT0, idT0.Add(5*time.Second)) // chain fine: A < distrust
	if err != nil {
		t.Fatal(err)
	}
	cs = c.EvaluateStatus(ac, s.a, s.snap(crl, listed), audit.StatusParams{})
	if cs.Certs[0].Tier != audit.TierNoInformation || cs.Certs[0].Revoked {
		t.Fatalf("CRL from a distrusted time: %+v", cs.Certs[0])
	}
	// The auditor's overlay applies in addition to the recorded policy.
	c = mustCompile(t, s.policy())
	ac, _ = c.VerifyChain("alice", s.leaf.Chain(), idT0, s.a)
	cs = c.EvaluateStatus(ac, s.a, s.snap(crl), audit.StatusParams{Distrust: map[audit.SPKIHash]time.Time{ih: idT0}})
	if cs.Certs[0].Tier != audit.TierNoInformation {
		t.Fatalf("overlay: %+v", cs.Certs[0])
	}
}

// ---- the whole chain ----

func TestEveryNonAnchorCertificateNeedsStatus(t *testing.T) {
	s := newStFx(t)
	now := idT0.Add(10 * time.Second)
	leafCRL := s.lcrl(1, now, 30*min)
	// No status for the intermediate: the chain has no information although the leaf is good.
	cs := s.eval(s.snap(leafCRL), audit.StatusParams{})
	if cs.Tier != audit.TierNoInformation || cs.Revoked {
		t.Fatalf("%+v", cs)
	}
	if err := cs.Err(); err == nil {
		t.Fatal("no error for a chain without full status")
	} else {
		wantStatusErr(t, err, agent.ReasonStatusUnknown)
	}
	// With the root's CRL for the intermediate, the chain is known.
	cs = s.eval(s.snap(leafCRL, s.rcrl(1, now, 30*min)), audit.StatusParams{})
	if cs.Tier == audit.TierNoInformation || cs.Revoked || cs.Err() != nil {
		t.Fatalf("%+v err %v", cs, cs.Err())
	}
	// The chain's tier is its weakest certificate's.
	if cs.Tier != minTier(cs.Certs[0].Tier, cs.Certs[1].Tier) {
		t.Fatalf("chain tier %v from %v and %v", cs.Tier, cs.Certs[0].Tier, cs.Certs[1].Tier)
	}
}

func minTier(a, b audit.StatusTier) audit.StatusTier {
	if a < b {
		return a
	}
	return b
}

// An intermediate revoked with E_int <= A invalidates the leaf's decisions even when the leaf itself is good.
func TestRevokedIntermediateRevokesTheChain(t *testing.T) {
	s := newStFx(t)
	now := idT0.Add(10 * time.Second)
	cs := s.eval(s.snap(
		s.lcrl(1, now, 30*min),
		s.rcrl(1, now, 30*min, testca.Revoked(s.inter.Cert, s.a.Add(-hour), 2)),
	), audit.StatusParams{})
	if !cs.Revoked || cs.Certs[0].Revoked || !cs.Certs[1].Revoked {
		t.Fatalf("%+v", cs)
	}
	wantStatusErr(t, cs.Err(), agent.ReasonRevoked)
	// An intermediate revoked AFTER the decision does not touch it.
	cs = s.eval(s.snap(
		s.lcrl(1, now, 30*min),
		s.rcrl(1, now, 30*min, testca.Revoked(s.inter.Cert, s.a.Add(hour), 0)),
	), audit.StatusParams{})
	if cs.Revoked {
		t.Fatalf("%+v", cs)
	}
}

// Live: a snapshot with fresh CRLs for both certificates of the chain. Stale status is reported as
// stale, so the gate can say so.
func TestChainErrDistinguishesStaleFromMissing(t *testing.T) {
	s := newStFx(t)
	now := idT0.Add(10 * time.Second)
	sn := s.snap(s.lcrl(1, now, 30*min), s.rcrl(1, now, 30*min))
	cs := s.eval(sn, audit.StatusParams{Now: idT0.Add(3 * hour), MaxStatusAge: hour})
	wantStatusErr(t, cs.Err(), agent.ReasonStatusStale)
	cs = s.eval(audit.NewStatusSnapshot(), audit.StatusParams{Now: idT0, MaxStatusAge: hour})
	wantStatusErr(t, cs.Err(), agent.ReasonStatusUnknown)
}

func TestHashesOfTheStatusRelied(t *testing.T) {
	s := newStFx(t)
	c1, c2, cL := series(s)
	sn := audit.NewStatusSnapshot()
	var hL [32]byte
	for i, der := range [][]byte{c1, c2, cL} {
		info, err := sn.AddCRL(der)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			hL = info.Hash
		}
	}
	st := s.leafStatus(sn, audit.StatusParams{})
	found := false
	for _, h := range st.Used {
		found = found || h == hL
	}
	if !found {
		t.Fatal("the CRL at L(C) is not among the status relied on")
	}
}

// ---- concurrency ----

func TestSnapshotIsSafeForConcurrentUse(t *testing.T) {
	s := newStFx(t)
	sn := audit.NewStatusSnapshot()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, _ = sn.AddCRL(s.lcrl(int64(i+1), idT0.Add(time.Duration(i)*min), 30*min))
		}(i)
		go func() {
			defer wg.Done()
			_ = s.eval(sn, audit.StatusParams{})
		}()
	}
	wg.Wait()
}

// ---- CRLs the standard library will not build: marshalled by hand and signed ----

func TestRawCRLBuilderMakesAUsableCRL(t *testing.T) {
	s := newStFx(t)
	raw := s.inter.RawCRL(t, testca.RawCRLOpts{Number: big.NewInt(9), This: s.a.Add(time.Second), Next: s.a.Add(hour)})
	sn := audit.NewStatusSnapshot()
	info, err := sn.AddCRL(raw)
	if err != nil {
		t.Fatalf("a well-formed hand-built CRL: %v", err)
	}
	if info.Number.Int64() != 9 {
		t.Fatalf("number %v", info.Number)
	}
	if st := s.leafStatus(sn, audit.StatusParams{}); st.Tier == audit.TierNoInformation {
		t.Fatalf("the hand-built CRL verifies under its issuer and is usable: %+v", st)
	}
}

func TestAddCRLRefusesMalformedCRLsTheLibraryWillNotBuild(t *testing.T) {
	s := newStFx(t)
	this, next := s.a.Add(time.Second), s.a.Add(hour)
	for name, tc := range map[string]struct {
		o    testca.RawCRLOpts
		want string
	}{
		"no CRL number":                {testca.RawCRLOpts{This: this, Next: next}, "CRL number"},
		"negative CRL number":          {testca.RawCRLOpts{Number: big.NewInt(-1), This: this, Next: next}, "CRL number"},
		"no nextUpdate":                {testca.RawCRLOpts{Number: big.NewInt(1), This: this, OmitNext: true}, "nextUpdate"},
		"zero thisUpdate":              {testca.RawCRLOpts{Number: big.NewInt(1), Next: next}, "thisUpdate"},
		"nextUpdate = thisUpdate":      {testca.RawCRLOpts{Number: big.NewInt(1), This: this, Next: this}, "nextUpdate"},
		"nextUpdate before thisUpdate": {testca.RawCRLOpts{Number: big.NewInt(1), This: this, Next: this.Add(-time.Hour)}, "nextUpdate"},
	} {
		_, err := audit.NewStatusSnapshot().AddCRL(s.inter.RawCRL(t, tc.o))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantStatusErr(t, err, agent.ReasonStatusUnknown)
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: refused for another reason: %v", name, err)
		}
	}
}

func TestAddCRLSizeAndEntryLimits(t *testing.T) {
	s := newStFx(t)
	this, next := s.a.Add(time.Second), s.a.Add(hour)
	// The default ceiling is 16 MiB.
	big17 := s.inter.RawCRL(t, testca.RawCRLOpts{Number: big.NewInt(1), This: this, Next: next, PadBytes: 17 << 20})
	_, err := audit.NewStatusSnapshot().AddCRL(big17)
	wantStatusErr(t, err, agent.ReasonStatusUnknown)
	if !strings.Contains(err.Error(), "bytes, outside") {
		t.Fatalf("refused for another reason than size: %v", err)
	}
	// A policy that needs more sets it.
	if _, err := audit.NewStatusSnapshot(audit.WithLimits(32<<20, 1000)).AddCRL(big17); err != nil {
		t.Fatalf("under a raised limit: %v", err)
	}
	// And one that wants less.
	small := s.inter.RawCRL(t, testca.RawCRLOpts{Number: big.NewInt(1), This: this, Next: next, PadBytes: 2 << 10})
	_, err = audit.NewStatusSnapshot(audit.WithLimits(1<<10, 1000)).AddCRL(small)
	wantStatusErr(t, err, agent.ReasonStatusUnknown)

	three := s.inter.RawCRL(t, testca.RawCRLOpts{Number: big.NewInt(1), This: this, Next: next, Serials: []*big.Int{big.NewInt(1), big.NewInt(2), big.NewInt(3)}})
	_, err = audit.NewStatusSnapshot(audit.WithLimits(1<<20, 2)).AddCRL(three)
	wantStatusErr(t, err, agent.ReasonStatusUnknown)
	if !strings.Contains(err.Error(), "entries") {
		t.Fatalf("refused for another reason than the entry count: %v", err)
	}
	two := s.inter.RawCRL(t, testca.RawCRLOpts{Number: big.NewInt(2), This: this, Next: next, Serials: []*big.Int{big.NewInt(1), big.NewInt(2)}})
	if _, err := audit.NewStatusSnapshot(audit.WithLimits(1<<20, 2)).AddCRL(two); err != nil {
		t.Fatalf("exactly the limit: %v", err)
	}
}

// With issuers configured a CRL is authenticated before its entries are built: a CRL no configured
// issuer signed is refused for its signature even when its entries would also be refused.
func TestSnapshotAuthenticatesBeforeParsingEntries(t *testing.T) {
	s := newStFx(t)
	sn := func() *audit.StatusSnapshot { return audit.NewStatusSnapshot(audit.WithIssuers(s.inter.Cert)) }
	this, next := s.a.Add(time.Second), s.a.Add(hour)
	if _, err := sn().AddCRL(s.lcrl(1, this, hour)); err != nil {
		t.Fatalf("a CRL by the configured issuer: %v", err)
	}
	stranger := testca.NewRoot(t, "inter", testca.Opts{})
	_, err := sn().AddCRL(stranger.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: this, NextUpdate: next}))
	wantStatusErr(t, err, agent.ReasonStatusUnknown)
	if !strings.Contains(err.Error(), "configured issuer") {
		t.Fatalf("%v", err)
	}
	// Signed by a stranger and carrying an entry that would be refused: the signature comes first.
	bad := stranger.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: this, NextUpdate: next,
		Entries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(5), RevocationTime: this, ReasonCode: 8}}})
	_, err = sn().AddCRL(bad)
	if err == nil || !strings.Contains(err.Error(), "configured issuer") {
		t.Fatalf("an unauthenticated CRL was parsed for its entries first: %v", err)
	}
	// The same malformed entry from the right issuer is refused for the entry.
	ok := s.inter.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: this, NextUpdate: next,
		Entries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(5), RevocationTime: this, ReasonCode: 8}}})
	_, err = sn().AddCRL(ok)
	if err == nil || !strings.Contains(err.Error(), "removeFromCRL") {
		t.Fatalf("%v", err)
	}
}

// The snapshot is bounded: beyond WithMaxCRLs the CRL with the earliest thisUpdate goes.
func TestSnapshotEvictsTheOldest(t *testing.T) {
	s := newStFx(t)
	sn := audit.NewStatusSnapshot(audit.WithMaxCRLs(3))
	var oldest []byte
	for i := 0; i < 5; i++ {
		der := s.lcrl(int64(i+1), idT0.Add(time.Duration(i)*min), 30*min)
		if i == 0 {
			oldest = der
		}
		if _, err := sn.AddCRL(der); err != nil {
			t.Fatal(err)
		}
	}
	if sn.Len() != 3 {
		t.Fatalf("%d CRLs held", sn.Len())
	}
	// The evicted one can be added again (and evicts the now-oldest).
	if _, err := sn.AddCRL(oldest); err != nil {
		t.Fatal(err)
	}
	if sn.Len() != 3 {
		t.Fatalf("%d CRLs held after re-adding", sn.Len())
	}
	// What remains is the three latest by thisUpdate: the earliest of the original five are gone.
	st := s.leafStatus(sn, audit.StatusParams{})
	if !st.S.Equal(idT0.Add(4 * min)) {
		t.Fatalf("S = %v", st.S)
	}
}

// ---- refusals that a more general one could mask ----

func TestAddCRLSpecificRefusals(t *testing.T) {
	s := newStFx(t)
	this := idT0
	cases := map[string]struct {
		o    testca.CRLOpts
		want string
	}{
		"non-critical delta indicator": {testca.CRLOpts{Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 27}, false, []byte{0x02, 0x01, 0x01})}}, "delta"},
		"non-critical certificateIssuer": {testca.CRLOpts{Entries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(5), RevocationTime: this,
			ExtraExtensions: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 29}, false, []byte{0x30, 0x00})}}}}, "indirect"},
		"reason code 7":  {testca.CRLOpts{Entries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(5), RevocationTime: this, ReasonCode: 7}}}, "reason code"},
		"reason code 11": {testca.CRLOpts{Entries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(5), RevocationTime: this, ReasonCode: 11}}}, "reason code"},
	}
	for name, tc := range cases {
		tc.o.Number, tc.o.ThisUpdate, tc.o.NextUpdate = 5, this, this.Add(hour)
		der, err := s.inter.TryCRL(tc.o)
		if err != nil {
			t.Logf("%s: the standard library will not create it: %v", name, err)
			continue
		}
		_, err = audit.NewStatusSnapshot().AddCRL(der)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: refused for another reason: %v", name, err)
		}
	}
}

// A CRL with the issuer's name and key identifier but another key is told apart by its signature alone.
func TestForgedCRLWithTheIssuersNameAndKeyIDIsIgnored(t *testing.T) {
	s := newStFx(t)
	forger := testca.NewRoot(t, "inter", testca.Opts{Mutate: func(c *x509.Certificate) {
		c.Subject = s.inter.Cert.Subject
		c.SubjectKeyId = s.inter.Cert.SubjectKeyId
	}})
	forged := forger.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: idT0.Add(10 * time.Second), NextUpdate: idT0.Add(hour),
		Entries: []x509.RevocationListEntry{testca.Revoked(s.leaf.Cert, idT0.Add(-time.Hour), 0)}})
	st := s.leafStatus(s.snap(forged), audit.StatusParams{})
	if st.Revoked || st.Tier != audit.TierNoInformation {
		t.Fatalf("a CRL signed by another key was read: %+v", st)
	}
}

// Two different CRLs under one CRL number: the later is not a legitimate successor.
func TestSameNumberDifferentCRLIsNotEvidence(t *testing.T) {
	s := newStFx(t)
	first := s.lcrl(5, idT0.Add(10*time.Second), 30*min)
	second := s.lcrl(5, idT0.Add(20*min), 30*min)
	st := s.leafStatus(s.snap(first, second), audit.StatusParams{})
	if !st.S.Equal(idT0.Add(10 * time.Second)) {
		t.Fatalf("S = %v: the second CRL under the same number was relied on", st.S)
	}
}

// A live evaluation with no age limit accepts no evidence, even for a CRL issued this instant.
func TestLiveNeedsAPositiveMaxStatusAge(t *testing.T) {
	s := newStFx(t)
	this := idT0.Add(10 * time.Second)
	sn := s.snap(s.lcrl(1, this, 30*min))
	st := s.leafStatus(sn, audit.StatusParams{Now: this})
	if st.Tier != audit.TierNoInformation {
		t.Fatalf("%+v", st)
	}
}

func TestRevokedFindingCarriesNoEvidenceOfAbsence(t *testing.T) {
	s := newStFx(t)
	c1, c2, cL := series(s)
	listed := s.lcrl(9, idT0.Add(20*time.Second), 30*min, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 0))
	st := s.leafStatus(s.snap(c1, c2, cL, listed), audit.StatusParams{})
	if !st.Revoked || st.Tier != audit.TierNoInformation || len(st.Used) != 0 || !st.S.IsZero() {
		t.Fatalf("%+v", st)
	}
}

// The chain's tier is its weakest certificate's, whichever certificate that is.
func TestChainTierIsTheWeakestCertificate(t *testing.T) {
	s := newStFx(t)
	now := idT0.Add(10 * time.Second)
	// Only the intermediate has status: the leaf is the weakest.
	cs := s.eval(s.snap(s.rcrl(1, now, 30*min)), audit.StatusParams{})
	if cs.Certs[0].Tier != audit.TierNoInformation || cs.Certs[1].Tier == audit.TierNoInformation || cs.Tier != audit.TierNoInformation {
		t.Fatalf("%+v", cs)
	}
}

// Distrust from the policy and from the overlay both apply, the earlier winning.
func TestDistrustTakesTheEarlierOfPolicyAndOverlay(t *testing.T) {
	s := newStFx(t)
	ih := audit.SPKIHashOf(s.inter.Cert)
	crl := s.lcrl(1, idT0.Add(30*time.Second), 30*min)
	run := func(policy, overlay time.Time) audit.StatusTier {
		p := s.policy()
		if !policy.IsZero() {
			p.Distrust = map[audit.SPKIHash]time.Time{ih: policy}
		}
		c := mustCompile(t, p)
		ac, err := c.VerifyChain("alice", s.leaf.Chain(), idT0, idT0.Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		var ov map[audit.SPKIHash]time.Time
		if !overlay.IsZero() {
			ov = map[audit.SPKIHash]time.Time{ih: overlay}
		}
		return c.EvaluateStatus(ac, idT0.Add(5*time.Second), s.snap(crl), audit.StatusParams{Distrust: ov}).Certs[0].Tier
	}
	late, early := idT0.Add(hour), idT0.Add(10*time.Second)
	if run(late, early) != audit.TierNoInformation {
		t.Fatal("an earlier overlay did not apply over a later policy date")
	}
	if run(early, late) != audit.TierNoInformation {
		t.Fatal("an earlier policy date did not apply over a later overlay")
	}
	if run(late, late) == audit.TierNoInformation {
		t.Fatal("distrust in the future applied")
	}
}

// ---- finality for a certificate unexpired at S, and the caveat ----

// The user's decision (2026-10-15): for a certificate still unexpired at S, completeness is a usable
// S >= A + W', W' >= MinW and time coverage from A to S; L(C) is needed only when S falls after
// notAfter. An issuing CA certificate, valid for a year, can therefore reach final.
func TestUnexpiredIntermediateReachesFinalWithoutLC(t *testing.T) {
	s := newStFx(t)
	// The root's CRLs for the intermediate: one in force at A, then one a full window later.
	r0 := s.rcrl(1, idT0, 24*hour+min)                     // in force at A, reaching r1
	r1 := s.rcrl(2, s.a.Add(24*hour+time.Second), 24*hour) // S >= A + 24h
	l0 := s.lcrl(1, idT0, 30*min)
	cs := s.eval(s.snap(r0, r1, l0), audit.StatusParams{Interval: 25 * hour})
	inter := cs.Certs[1]
	if inter.Tier != audit.TierFinal || inter.LCKnown || inter.Window != 24*hour {
		t.Fatalf("an unexpired intermediate with status a full window past A: %+v", inter)
	}
	if inter.Caveat != "revocations first reported after S are not covered" {
		t.Fatalf("caveat %q", inter.Caveat)
	}
	// The same evidence under the design note's earlier literal rule is provisional.
	defer audit.SetFinalityNeedsLC(true)()
	inter = s.eval(s.snap(r0, r1, l0), audit.StatusParams{Interval: 25 * hour}).Certs[1]
	if inter.Tier != audit.TierProvisional || !strings.Contains(inter.Note, "L(C)") {
		t.Fatalf("literal rule: %+v", inter)
	}
}

// A leaf is judged on L(C) once S falls after its notAfter: CRLs past notAfter that cannot be shown
// to include the first one beyond it are no evidence (a leaf cannot reach final on them).
func TestLeafPastNotAfterNeedsLC(t *testing.T) {
	s := newStFx(t)
	c1, _, _ := series(s)
	late := s.lcrl(9, idT0.Add(5*hour), 30*min) // far past notAfter, nothing links it to c1
	st := s.leafStatus(s.snap(c1, late), audit.StatusParams{W: 10 * min})
	if st.Tier == audit.TierFinal || st.LCKnown {
		t.Fatalf("%+v", st)
	}
	// With the link (the first CRL beyond notAfter, reached by the one before it) it can be final.
	_, c2, cL := series(s)
	st = s.leafStatus(s.snap(c1, c2, cL), audit.StatusParams{})
	if st.Tier != audit.TierFinal || !st.LCKnown {
		t.Fatalf("%+v", st)
	}
	// A leaf that is still unexpired at S is final without L(C) when the window is short enough.
	st = s.leafStatus(s.snap(c1, c2), audit.StatusParams{W: 20 * min, MinW: 10 * min})
	if st.Tier != audit.TierFinal || st.LCKnown {
		t.Fatalf("unexpired at S: %+v", st)
	}
}

// Time coverage from the CRL in force at A to S: a gap anywhere, including before the first CRL
// issued after A, is not final.
func TestCoverageFromAToS(t *testing.T) {
	s := newStFx(t)
	w := audit.StatusParams{W: 20 * min, MinW: 10 * min}
	inForce := s.lcrl(1, idT0, 10*min)          // T0 .. T0+10m
	next := s.lcrl(2, idT0.Add(10*min), 20*min) // contiguous
	last := s.lcrl(3, idT0.Add(30*min), 30*min) // contiguous: S = T0+30m >= A + 20m
	if st := s.leafStatus(s.snap(inForce, next, last), w); st.Tier != audit.TierFinal {
		t.Fatalf("contiguous: %+v", st)
	}
	// No CRL in force at A (the earliest is issued after A): a gap from A.
	if st := s.leafStatus(s.snap(next, last), w); st.Tier == audit.TierFinal {
		t.Fatalf("no CRL in force at A: %+v", st)
	}
	// The CRL in force at A lapsed before A.
	lapsed := s.lcrl(1, idT0.Add(-time.Hour), 30*min)
	if st := s.leafStatus(s.snap(lapsed, next, last), w); st.Tier == audit.TierFinal {
		t.Fatalf("the CRL before A had lapsed: %+v", st)
	}
	// A gap in the middle.
	gap := s.lcrl(2, idT0.Add(12*min), 20*min)
	if st := s.leafStatus(s.snap(inForce, gap, last), w); st.Tier == audit.TierFinal {
		t.Fatalf("gap between CRLs: %+v", st)
	}
	// The evidence is still usable as provisional, and says why it is not final.
	st := s.leafStatus(s.snap(inForce, gap, last), w)
	if st.Tier != audit.TierProvisional || !strings.Contains(st.Note, "coverage") {
		t.Fatalf("%+v", st)
	}
}

// A CRL issued exactly at A is the one in force at A.
func TestCRLIssuedAtAIsInForceAtA(t *testing.T) {
	s := newStFx(t)
	w := audit.StatusParams{W: 20 * min, MinW: 10 * min}
	atA := s.lcrl(1, s.a, 10*min)
	next := s.lcrl(2, s.a.Add(10*min), 20*min)
	last := s.lcrl(3, s.a.Add(30*min), 30*min)
	if st := s.leafStatus(s.snap(atA, next, last), w); st.Tier != audit.TierFinal {
		t.Fatalf("%+v", st)
	}
}

// ---- review of the first version (pki-audit-325) ----

// H1: overlapping CRLs. The CA issues every 20 minutes and each CRL is valid for 24 hours, so a
// predecessor's nextUpdate "reaches" any later CRL. A producer who omits the first CRL beyond notAfter
// (the one that lists the revoked leaf) must not get a later, pruned CRL accepted as L(C): the
// declared issuance interval, not nextUpdate, shows that nothing lies between.
func overlappingSeries(s *stFx) (all [][]byte, listed, pruned []byte) {
	rev := testca.Revoked(s.leaf.Cert, idT0.Add(50*min), 1) // after the last silent CRL; keyCompromise, no invalidity date: E = notBefore
	day := 24 * hour
	c0 := s.lcrl(1, idT0, day)
	c1 := s.lcrl(2, idT0.Add(20*min), day)
	c2 := s.lcrl(3, idT0.Add(40*min), day)
	c3 := s.lcrl(4, idT0.Add(70*min), day, rev) // the first beyond notAfter (T0+60m) still lists the leaf
	c4 := s.lcrl(5, idT0.Add(90*min), day)      // pruned: the leaf has appeared on one CRL beyond notAfter
	return [][]byte{c0, c1, c2, c3, c4}, c3, c4
}

func TestOverlappingCRLsOmittedFirstCRLBeyondNotAfter(t *testing.T) {
	s := newStFx(t)
	all, _, _ := overlappingSeries(s)
	p := audit.StatusParams{Interval: 30 * min}
	// The complete series: the listing is seen.
	if st := s.leafStatus(s.snap(all...), p); !st.Revoked {
		t.Fatalf("complete series: %+v", st)
	}
	// c3 omitted. nextUpdate (24h) reaches c4 from c2, but c2 and c4 are 50 minutes apart against a
	// declared 30: c4 is not shown to be the first CRL beyond notAfter, so it is no evidence.
	omitted := s.snap(all[0], all[1], all[2], all[4])
	st := s.leafStatus(omitted, p)
	if st.Tier == audit.TierFinal || st.LCKnown {
		t.Fatalf("a revoked leaf read as final after omitting one CRL: %+v", st)
	}
	// The same through the consecutive-number mode: 3 then 5 is not consecutive.
	st = s.leafStatus(omitted, audit.StatusParams{Interval: -1, ConsecutiveNumbers: true})
	if st.Tier == audit.TierFinal || st.LCKnown {
		t.Fatalf("consecutive-number mode: %+v", st)
	}
	// Control: an unlisted certificate with the complete series is final in both modes (no listing).
	clean := newStFx(t)
	var series [][]byte
	for i, at := range []time.Duration{0, 20 * min, 40 * min, 70 * min, 90 * min} {
		series = append(series, clean.lcrl(int64(i+1), idT0.Add(at), 24*hour))
	}
	if st := clean.leafStatus(clean.snap(series...), p); st.Tier != audit.TierFinal {
		t.Fatalf("interval mode, complete: %+v", st)
	}
	if st := clean.leafStatus(clean.snap(series...), audit.StatusParams{Interval: -1, ConsecutiveNumbers: true}); st.Tier != audit.TierFinal {
		t.Fatalf("consecutive mode, complete: %+v", st)
	}
}

// With no declared interval and no consecutive-number mode nothing is final.
func TestNoDeclaredIntervalMeansNeverFinal(t *testing.T) {
	s := newStFx(t)
	c1, c2, cL := series(s)
	sn := s.snap(c1, c2, cL)
	short := audit.StatusParams{W: 10 * min, MinW: 5 * min}
	st := s.c.EvaluateStatus(s.ac, s.a, sn, short).Certs[0]
	if st.Tier != audit.TierProvisional || !strings.Contains(st.Note, "issuance interval") {
		t.Fatalf("%+v", st)
	}
	// The interval can be given per issuing CA.
	per := short
	per.IntervalFor = map[audit.SPKIHash]time.Duration{audit.SPKIHashOf(s.inter.Cert): 40 * min}
	if st := s.c.EvaluateStatus(s.ac, s.a, sn, per).Certs[0]; st.Tier != audit.TierFinal {
		t.Fatalf("IntervalFor: %+v", st)
	}
	// Another CA's entry does not apply to this one.
	other := short
	other.IntervalFor = map[audit.SPKIHash]time.Duration{audit.SPKIHashOf(s.root.Cert): 40 * min}
	if st := s.c.EvaluateStatus(s.ac, s.a, sn, other).Certs[0]; st.Tier == audit.TierFinal {
		t.Fatalf("an interval declared for another CA applied: %+v", st)
	}
}

// Test plan 13 with the interval: a hold placed before A and released before S is seen only by the CRL
// that lists it. Omitting that CRL leaves a gap longer than the declared interval, so S cannot reach
// past it and the verdict is not final.
func TestHoldPlacedAndReleasedBeforeS(t *testing.T) {
	s := newStFx(t)
	hold := testca.Revoked(s.leaf.Cert, idT0.Add(2*time.Second), 6)
	c0 := s.lcrl(1, idT0, 10*min)                   // in force at A
	c1 := s.lcrl(2, idT0.Add(10*min), 10*min, hold) // lists the hold
	c2 := s.lcrl(3, idT0.Add(20*min), 10*min)       // released: absent
	c3 := s.lcrl(4, idT0.Add(30*min), 10*min)
	p := audit.StatusParams{Interval: 10 * min, W: 20 * min, MinW: 10 * min}
	if st := s.leafStatus(s.snap(c0, c1, c2, c3), p); !st.Revoked || st.ReasonCode != 6 {
		t.Fatalf("with the CRL that lists the hold: %+v", st)
	}
	st := s.leafStatus(s.snap(c0, c2, c3), p)
	if st.Revoked || st.Tier == audit.TierFinal || !strings.Contains(st.Note, "coverage") {
		t.Fatalf("the hold's CRL omitted: %+v", st)
	}
}

// M1: expiredCertsOnCRL is judged per CRL. One CRL that carries it does not make a later CRL without
// it evidence of absence.
func TestExpiredCertsOnCRLIsPerCRL(t *testing.T) {
	s := newStFx(t)
	day := 24 * hour
	withExt := s.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: idT0.Add(30 * min), NextUpdate: idT0.Add(30*min + day),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 60}, false, genTime(t, idT0.Add(-24*hour)))}})
	c0 := s.lcrl(1, idT0, day)
	late := s.lcrl(3, idT0.Add(140*min), day) // no extension; nothing links it to the first CRL beyond notAfter
	p := audit.StatusParams{Interval: 40 * min, W: 10 * min, MinW: 5 * min}
	st := s.leafStatus(s.snap(c0, withExt, late), p)
	if st.S.Equal(idT0.Add(140*min)) || !st.S.Equal(idT0.Add(30*min)) {
		t.Fatalf("a later CRL without the extension was trusted because another had it: %+v", st)
	}
	// A CRL beyond notAfter that carries the extension does count, on its own account.
	retained := s.inter.NewCRL(t, testca.CRLOpts{Number: 3, ThisUpdate: idT0.Add(140 * min), NextUpdate: idT0.Add(140*min + day),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 60}, false, genTime(t, idT0.Add(-24*hour)))}})
	st = s.leafStatus(s.snap(c0, retained), audit.StatusParams{Interval: 3 * hour, W: 10 * min, MinW: 5 * min})
	if !st.Unbounded || st.Tier != audit.TierFinal || st.Caveat == "" {
		t.Fatalf("%+v", st)
	}
}

// M2: live counting takes a fresh CRL issued a moment before A and is provisional; offline status
// needs a CRL at or after A, and final is never reached from status that predates A.
func TestLiveCountingUsesStatusBeforeA(t *testing.T) {
	s := newStFx(t)
	before := s.lcrl(1, s.a.Add(-time.Second), 30*min)
	sn := s.snap(before)
	if st := s.leafStatus(sn, audit.StatusParams{}); st.Tier != audit.TierNoInformation {
		t.Fatalf("offline: %+v", st)
	}
	live := audit.StatusParams{Now: s.a.Add(10 * time.Second), MaxStatusAge: hour}
	st := s.leafStatus(sn, live)
	if st.Tier != audit.TierProvisional || !st.S.Equal(s.a.Add(-time.Second)) {
		t.Fatalf("live: %+v", st)
	}
	// Stale live status before A is still refused.
	stale := audit.StatusParams{Now: s.a.Add(2 * hour), MaxStatusAge: hour}
	if st := s.leafStatus(sn, stale); st.Tier != audit.TierNoInformation || !st.Stale {
		t.Fatalf("stale: %+v", st)
	}
	// A listing before A revokes either way.
	listed := s.lcrl(2, s.a.Add(-time.Second), 30*min, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 0))
	if !s.leafStatus(s.snap(listed), live).Revoked {
		t.Fatal("a live listing did not revoke")
	}
}

// L3: a CRL dated in the future is not fresh.
func TestLiveRefusesFutureDatedCRL(t *testing.T) {
	s := newStFx(t)
	now := idT0.Add(10 * time.Second)
	live := audit.StatusParams{Now: now, MaxStatusAge: 6 * hour}
	future := s.lcrl(1, now.Add(30*min), 30*min) // inside the leaf's life, so only the future date can refuse it
	if st := s.leafStatus(s.snap(future), live); st.Tier != audit.TierNoInformation {
		t.Fatalf("half an hour ahead: %+v", st)
	}
	jitter := s.lcrl(1, now.Add(30*time.Second), 30*min)
	if st := s.leafStatus(s.snap(jitter), live); st.Tier == audit.TierNoInformation {
		t.Fatalf("30 s ahead is within the tolerance: %+v", st)
	}
}

// L2: publication jitter inside the tolerance is not a gap; beyond it is.
func TestCoverageTolerance(t *testing.T) {
	s := newStFx(t)
	p := audit.StatusParams{Interval: 20 * min, W: 5 * min, MinW: 2 * min}
	c0 := s.lcrl(1, idT0, 10*min)
	last := func(delay time.Duration) []byte { return s.lcrl(2, idT0.Add(10*min+delay), 30*min) }
	if st := s.leafStatus(s.snap(c0, last(30*time.Second)), p); st.Tier != audit.TierFinal {
		t.Fatalf("30 s late: %+v", st)
	}
	if st := s.leafStatus(s.snap(c0, last(2*min)), p); st.Tier == audit.TierFinal {
		t.Fatalf("2 minutes late: %+v", st)
	}
	p.CoverageTolerance = 5 * min
	if st := s.leafStatus(s.snap(c0, last(2*min)), p); st.Tier != audit.TierFinal {
		t.Fatalf("a 5 minute tolerance: %+v", st)
	}
	// Exactly interval + tolerance apart is still contiguous (and one second more is not).
	p.CoverageTolerance = time.Minute
	exact := s.lcrl(2, idT0.Add(21*min), 30*min) // 20m interval + 1m tolerance after c0
	if st := s.leafStatus(s.snap(s.lcrl(1, idT0, 25*min), exact), p); st.Tier != audit.TierFinal {
		t.Fatalf("exactly interval + tolerance: %+v", st)
	}
	over := s.lcrl(2, idT0.Add(21*min+time.Second), 30*min)
	if st := s.leafStatus(s.snap(s.lcrl(1, idT0, 25*min), over), p); st.Tier == audit.TierFinal {
		t.Fatalf("a second beyond interval + tolerance: %+v", st)
	}
	// An undeclared interval is not "zero": CRLs seconds apart are not shown contiguous either.
	p.Interval = 0
	near := s.lcrl(2, idT0.Add(10*time.Second), 30*min)
	if st := s.c.EvaluateStatus(s.ac, s.a, s.snap(s.lcrl(1, idT0, 25*min), near), p).Certs[0]; st.Tier == audit.TierFinal {
		t.Fatalf("no declared interval: %+v", st)
	}
}

// L1: the caveat is stated even where expired entries are retained.
func TestCaveatAlwaysStated(t *testing.T) {
	s := newStFx(t)
	c0 := s.lcrl(1, idT0, 3*hour)
	late := s.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: idT0.Add(140 * min), NextUpdate: idT0.Add(170 * min),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 60}, false, genTime(t, idT0.Add(-24*hour)))}})
	st := s.leafStatus(s.snap(c0, late), audit.StatusParams{Interval: 3 * hour, W: 10 * min, MinW: 5 * min})
	if !st.Unbounded || st.Caveat != "revocations first reported after S are not covered" {
		t.Fatalf("%+v", st)
	}
}

// L4: malformed CRLs built by hand. Each is pinned: refused, or read and what that means.
func TestMalformedCRLCatalogue(t *testing.T) {
	s := newStFx(t)
	this, next := s.a.Add(time.Second), s.a.Add(hour)
	base := func() testca.RawCRLOpts { return testca.RawCRLOpts{Number: big.NewInt(1), This: this, Next: next} }
	sha1 := asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 1}
	md5 := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 4}
	badReason := pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 21}, Value: []byte{0x04, 0x01, 0x01}} // OCTET STRING, not ENUMERATED
	utc, _ := asn1.Marshal(this.Add(-time.Hour).UTC())                                                    // UTCTime
	big21, _ := new(big.Int).SetString("1"+strings.Repeat("00", 20), 16)                                  // 21 octets
	for name, tc := range map[string]struct {
		o       testca.RawCRLOpts
		refused bool
	}{
		"sha-1 signature algorithm":         {func() testca.RawCRLOpts { o := base(); o.InnerAlg, o.OuterAlg = sha1, sha1; return o }(), true},
		"md5 signature algorithm":           {func() testca.RawCRLOpts { o := base(); o.InnerAlg, o.OuterAlg = md5, md5; return o }(), true},
		"inner and outer algorithms differ": {func() testca.RawCRLOpts { o := base(); o.InnerAlg = sha1; return o }(), true},
		"indefinite length (BER)":           {func() testca.RawCRLOpts { o := base(); o.Indefinite = true; return o }(), true},
		"duplicate CRL number": {func() testca.RawCRLOpts {
			o := base()
			n, _ := asn1.Marshal(big.NewInt(2))
			o.Extensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 20}, Value: n}}
			return o
		}(), true},
		"version 1 with extensions": {func() testca.RawCRLOpts { o := base(); o.OmitVersion = true; return o }(), true},
		"reason code of the wrong type": {func() testca.RawCRLOpts {
			o := base()
			o.Entries = []testca.RawEntry{{Serial: big.NewInt(5), Time: this, Exts: []pkix.Extension{badReason}}}
			return o
		}(), true},
		"invalidity date as UTCTime": {func() testca.RawCRLOpts {
			o := base()
			o.Entries = []testca.RawEntry{{Serial: big.NewInt(5), Time: this, Exts: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 24}, Value: utc}}}}
			return o
		}(), false},
		"serial of 21 octets": {func() testca.RawCRLOpts {
			o := base()
			o.Entries = []testca.RawEntry{{Serial: big21, Time: this}}
			return o
		}(), false},
		"negative serial": {func() testca.RawCRLOpts {
			o := base()
			o.Entries = []testca.RawEntry{{Serial: big.NewInt(-5), Time: this}}
			return o
		}(), false},
		"critical invalidity date": {func() testca.RawCRLOpts {
			o := base()
			o.Entries = []testca.RawEntry{{Serial: big.NewInt(5), Time: this, Exts: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 24}, Critical: true, Value: utc}}}}
			return o
		}(), false},
		"entry revoked after thisUpdate": {func() testca.RawCRLOpts {
			o := base()
			o.Entries = []testca.RawEntry{{Serial: big.NewInt(5), Time: this.Add(time.Hour)}}
			return o
		}(), false},
		"duplicate entry extension": {func() testca.RawCRLOpts {
			o := base()
			o.Entries = []testca.RawEntry{{Serial: big.NewInt(5), Time: this, Exts: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 21}, Value: []byte{0x0a, 0x01, 0x01}}, {Id: asn1.ObjectIdentifier{2, 5, 29, 21}, Value: []byte{0x0a, 0x01, 0x02}}}}}
			return o
		}(), true},
		"critical freshestCRL": {func() testca.RawCRLOpts {
			o := base()
			o.Extensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 46}, Critical: true, Value: []byte{0x30, 0x00}}}
			return o
		}(), true},
		"no authority key identifier": {func() testca.RawCRLOpts { o := base(); o.NoAKI = true; return o }(), false},
	} {
		_, err := audit.NewStatusSnapshot().AddCRL(s.inter.RawCRL(t, tc.o))
		if tc.refused && err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if !tc.refused && err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
		if err != nil {
			wantStatusErr(t, err, agent.ReasonStatusUnknown)
		}
	}
	// A SHA-1 algorithm is refused for the algorithm, not for a signature that happens not to match.
	o := base()
	o.InnerAlg, o.OuterAlg = sha1, sha1
	_, err := audit.NewStatusSnapshot().AddCRL(s.inter.RawCRL(t, o))
	if err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("sha-1: %v", err)
	}
}
