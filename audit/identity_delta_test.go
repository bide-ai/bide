package audit_test

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// delta is a delta CRL by the leaf's issuer: number num, naming base number baseNum.
func (s *stFx) delta(num, baseNum int64, this time.Time, next time.Duration, entries ...x509.RevocationListEntry) []byte {
	s.t.Helper()
	return s.deltaBy(s.inter, num, baseNum, this, next, entries...)
}

func (s *stFx) deltaBy(ca *testca.CA, num, baseNum int64, this time.Time, next time.Duration, entries ...x509.RevocationListEntry) []byte {
	s.t.Helper()
	ind, err := asn1.Marshal(big.NewInt(baseNum))
	if err != nil {
		s.t.Fatal(err)
	}
	return ca.NewCRL(s.t, testca.CRLOpts{Number: num, ThisUpdate: this, NextUpdate: this.Add(next), Entries: entries,
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 27}, true, ind)}})
}

func has(hashes [][32]byte, h [32]byte) bool {
	for _, x := range hashes {
		if x == h {
			return true
		}
	}
	return false
}

func hashOf(t *testing.T, der []byte) [32]byte {
	t.Helper()
	info, err := audit.NewStatusSnapshot().AddCRL(der)
	if err != nil {
		t.Fatal(err)
	}
	return info.Hash
}

func TestDeltaIntake(t *testing.T) {
	s := newStFx(t)
	this := idT0
	if _, err := audit.NewStatusSnapshot().AddCRL(s.delta(5, 4, this, hour)); err != nil {
		t.Fatalf("a well-formed delta CRL: %v", err)
	}
	// removeFromCRL is allowed in a delta (and refused in a full CRL: see TestAddCRLRefusals).
	rel := x509.RevocationListEntry{SerialNumber: big.NewInt(9), RevocationTime: this, ReasonCode: 8}
	if _, err := audit.NewStatusSnapshot().AddCRL(s.delta(5, 4, this, hour, rel)); err != nil {
		t.Fatalf("removeFromCRL in a delta: %v", err)
	}
	for name, tc := range map[string]struct {
		crit bool
		val  []byte
	}{
		"non-critical indicator":          {false, []byte{0x02, 0x01, 0x04}},
		"indicator not an integer":        {true, []byte{0x04, 0x01, 0x04}},
		"indicator with trailing bytes":   {true, []byte{0x02, 0x01, 0x04, 0x00}},
		"negative base number":            {true, []byte{0x02, 0x01, 0xff}},
		"indicator not minimally encoded": {true, []byte{0x02, 0x02, 0x00, 0x04}},
		"empty indicator":                 {true, nil},
	} {
		der, err := s.inter.TryCRL(testca.CRLOpts{Number: 5, ThisUpdate: this, NextUpdate: this.Add(hour),
			Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 27}, tc.crit, tc.val)}})
		if err != nil {
			t.Logf("%s: the standard library will not create it: %v", name, err)
			continue
		}
		_, err = audit.NewStatusSnapshot().AddCRL(der)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantStatusErr(t, err, agent.ReasonStatusUnknown)
		if !strings.Contains(err.Error(), "delta") {
			t.Fatalf("%s: refused for another reason: %v", name, err)
		}
	}
	// A delta that also carries an issuing distribution point is refused.
	ind, _ := asn1.Marshal(big.NewInt(4))
	der := s.inter.NewCRL(t, testca.CRLOpts{Number: 5, ThisUpdate: this, NextUpdate: this.Add(hour), Extra: []pkix.Extension{
		ext(asn1.ObjectIdentifier{2, 5, 29, 27}, true, ind), ext(asn1.ObjectIdentifier{2, 5, 29, 28}, true, []byte{0x30, 0x00})}})
	if _, err := audit.NewStatusSnapshot().AddCRL(der); err == nil {
		t.Fatal("a delta with an issuing distribution point was accepted")
	}
}

// A delta is merged with its base and gives no evidence of absence alone; what it lists still counts.
func TestDeltaAloneGivesNoEvidenceOfAbsence(t *testing.T) {
	s := newStFx(t)
	d := s.delta(2, 1, s.a.Add(time.Second), hour)
	st := s.leafStatus(s.snap(d), audit.StatusParams{})
	if st.Tier != audit.TierNoInformation || st.Revoked {
		t.Fatalf("%+v", st)
	}
	// A listing can only take trust away: a delta without its base that lists the certificate revokes.
	d = s.delta(2, 1, s.a.Add(time.Second), hour, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 0))
	if st := s.leafStatus(s.snap(d), audit.StatusParams{}); !st.Revoked {
		t.Fatalf("a delta without a base that lists the leaf did not revoke: %+v", st)
	}
	// And a revoked certificate never reads final because the base is missing: a clean full CRL beside
	// a delta whose base is lost, the delta listing the leaf.
	clean := s.lcrl(10, s.a.Add(time.Second), hour)
	lost := s.delta(12, 11, s.a.Add(2*time.Second), hour, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 0))
	st = s.leafStatus(s.snap(clean, lost), audit.StatusParams{})
	if !st.Revoked || st.Tier == audit.TierFinal {
		t.Fatalf("%+v", st)
	}
}

// The merged view is as of the delta: a base older than the decision is not evidence by itself, the
// base with a delta issued after the decision is.
func TestBaseAndDeltaGiveStatusAsOfTheDelta(t *testing.T) {
	s := newStFx(t)
	base := s.lcrl(1, idT0, 24*hour)
	d := s.delta(2, 1, idT0.Add(30*min), 30*min)
	if st := s.leafStatus(s.snap(base), audit.StatusParams{}); st.Tier != audit.TierNoInformation {
		t.Fatalf("the base predates A: %+v", st)
	}
	st := s.leafStatus(s.snap(base, d), audit.StatusParams{})
	if st.Tier != audit.TierProvisional || !st.S.Equal(idT0.Add(30*min)) {
		t.Fatalf("%+v", st)
	}
	if !has(st.Used, hashOf(t, base)) || !has(st.Used, hashOf(t, d)) {
		t.Fatal("both the base and the delta are among the status relied on")
	}
}

func TestDeltaListingRevokes(t *testing.T) {
	s := newStFx(t)
	base := s.lcrl(1, idT0, 24*hour)
	d := s.delta(2, 1, s.a.Add(time.Second), hour, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 0))
	if st := s.leafStatus(s.snap(base, d), audit.StatusParams{}); !st.Revoked {
		t.Fatalf("%+v", st)
	}
	// A revocation after the decision does not.
	d = s.delta(3, 1, s.a.Add(time.Second), hour, testca.Revoked(s.leaf.Cert, s.a.Add(time.Hour), 0))
	if st := s.leafStatus(s.snap(base, d), audit.StatusParams{}); st.Revoked {
		t.Fatalf("%+v", st)
	}
}

// RFC 5280 6.3.3: which base a delta may be merged with.
func TestDeltaPairingRules(t *testing.T) {
	type tc struct {
		name    string
		baseNum int64     // the base CRL's number
		baseAt  time.Time // and its thisUpdate
		named   int64     // the base number the delta names
		deltaN  int64
		deltaAt time.Time
		paired  bool
	}
	at := func(m time.Duration) time.Time { return idT0.Add(m) }
	for _, c := range []tc{
		{"the named base", 1, at(0), 1, 2, at(30 * min), true},
		{"a newer base than named (the delta is cumulative since the named one)", 3, at(10 * min), 1, 4, at(30 * min), true},
		{"the base's number equals the delta's", 2, at(0), 1, 2, at(30 * min), false},
		{"a delta naming a base number as large as its own", 1, at(0), 5, 5, at(30 * min), false},
		{"the base is older than the one named", 1, at(0), 2, 3, at(30 * min), false},
		{"the delta's number is not above the base's", 2, at(0), 1, 2, at(30 * min), false},
		{"the delta's number is below the base's", 3, at(0), 1, 2, at(30 * min), false},
		{"the delta was issued before the base", 1, at(20 * min), 1, 2, at(10 * min), false},
		{"issued at the same instant", 1, at(10 * min), 1, 2, at(10 * min), true},
	} {
		s := newStFx(t)
		base := s.lcrl(c.baseNum, c.baseAt, 24*hour)
		d := s.delta(c.deltaN, c.named, c.deltaAt, 30*min)
		st := s.leafStatus(s.snap(base, d), audit.StatusParams{})
		got := has(st.Used, hashOf(t, d))
		if got != c.paired {
			t.Fatalf("%s: delta merged=%v (tier %v, used %d), want %v", c.name, got, st.Tier, len(st.Used), c.paired)
		}
		if c.paired && !has(st.Used, hashOf(t, base)) {
			t.Fatalf("%s: the base is missing from the status relied on", c.name)
		}
	}
	// A delta and a base signed by different keys are not a pair; neither is a stranger's base.
	s := newStFx(t)
	stranger := testca.NewRoot(t, "stranger", testca.Opts{})
	base := s.lcrl(1, idT0, 24*hour)
	d := s.deltaBy(stranger, 2, 1, idT0.Add(30*min), 30*min)
	if st := s.leafStatus(s.snap(base, d), audit.StatusParams{}); has(st.Used, hashOf(t, d)) {
		t.Fatal("a delta from another issuer was merged")
	}
	sBase := stranger.NewCRL(t, testca.CRLOpts{Number: 1, ThisUpdate: idT0, NextUpdate: idT0.Add(24 * hour)})
	d = s.delta(2, 1, idT0.Add(30*min), 30*min)
	if st := s.leafStatus(s.snap(sBase, d), audit.StatusParams{}); st.Tier != audit.TierNoInformation {
		t.Fatalf("a base from another issuer: %+v", st)
	}
	// Of several eligible bases the highest numbered (the freshest valid partner) is used, so a
	// lapsed older base does not make a fresh merged view stale.
	b1 := s.lcrl(1, idT0, 20*min)
	b3 := s.lcrl(3, idT0.Add(10*min), 24*hour)
	d = s.delta(4, 1, idT0.Add(30*min), 3*hour)
	st := s.leafStatus(s.snap(b1, b3, d), audit.StatusParams{Now: idT0.Add(40 * min), MaxStatusAge: 5 * hour, CountLive: true})
	if !has(st.Used, hashOf(t, b3)) || has(st.Used, hashOf(t, b1)) || st.Tier == audit.TierNoInformation {
		t.Fatalf("the freshest qualifying base was not the one merged: %+v", st)
	}
}

// The merged nextUpdate is the earlier of the two: a stale base does not make a fresh delta fresh.
func TestMergedNextUpdateIsTheEarlier(t *testing.T) {
	s := newStFx(t)
	base := s.lcrl(1, idT0, 40*min)              // next T0+40m
	d := s.delta(2, 1, idT0.Add(30*min), 3*hour) // next T0+3h30m
	sn := s.snap(base, d)
	live := func(now time.Time) audit.CertStatus {
		return s.leafStatus(sn, audit.StatusParams{Now: now, MaxStatusAge: 5 * hour, CountLive: true})
	}
	if st := live(idT0.Add(35 * min)); st.Tier == audit.TierNoInformation {
		t.Fatalf("both current: %+v", st)
	}
	st := live(idT0.Add(50 * min))
	if st.Tier != audit.TierNoInformation || !st.Stale {
		t.Fatalf("the base had lapsed: %+v", st)
	}
	// And the other way: a lapsed delta with a current base. Live counting may use the base alone
	// (it is fresh), but the lapsed merged view is not relied on.
	base = s.lcrl(1, idT0, 6*hour)
	d = s.delta(2, 1, idT0.Add(30*min), 20*min) // next T0+50m
	sn = s.snap(base, d)
	st = live(idT0.Add(55 * min))
	if has(st.Used, hashOf(t, d)) || !st.S.Equal(idT0) {
		t.Fatalf("the delta had lapsed: %+v", st)
	}
}

// Test plan 13: a certificateHold listed in the base and released by a delta (removeFromCRL) is
// still a revocation for a decision made during the hold; a release never validates it afterwards.
func TestHoldReleasedByDeltaStaysRevoked(t *testing.T) {
	s := newStFx(t)
	base := s.lcrl(1, idT0, 24*hour, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 6))
	rel := testca.Revoked(s.leaf.Cert, s.a.Add(10*min), 8)
	d := s.delta(2, 1, s.a.Add(20*min), 30*min, rel)
	st := s.leafStatus(s.snap(base, d), audit.StatusParams{AllowHolds: true})
	if !st.Revoked || st.ReasonCode != 6 {
		t.Fatalf("%+v", st)
	}
	// removeFromCRL is not itself a revocation, even dated before the decision.
	clean := s.lcrl(1, idT0, 24*hour)
	rel = testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 8)
	d = s.delta(2, 1, s.a.Add(20*min), 30*min, rel)
	if st := s.leafStatus(s.snap(clean, d), audit.StatusParams{AllowHolds: true}); st.Revoked {
		t.Fatalf("removeFromCRL read as a revocation: %+v", st)
	}
}

// The first CRL beyond notAfter can be a merged view (a base issued before notAfter and a delta after
// it): it is L(C) when the CRL before it reaches it in time.
func TestFirstCRLAfterNotAfterMayBeAMergedView(t *testing.T) {
	s := newStFx(t)
	c0 := s.lcrl(1, idT0, 30*min)
	c1 := s.lcrl(2, idT0.Add(30*min), 24*hour) // base issued before notAfter, long period
	d := s.delta(3, 2, idT0.Add(70*min), 30*min)
	st := s.leafStatus(s.snap(c0, c1, d), audit.StatusParams{})
	if st.Tier != audit.TierFinal || !st.LCKnown || !st.LC.Equal(idT0.Add(70*min)) {
		t.Fatalf("%+v", st)
	}
	// The listing that the base carries is not lost to the merge.
	rev := testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 0)
	c1 = s.lcrl(2, idT0.Add(30*min), 24*hour, rev)
	if st := s.leafStatus(s.snap(c0, c1, d), audit.StatusParams{}); !st.Revoked {
		t.Fatalf("%+v", st)
	}
}

// Coverage runs through base and delta: a delta issued after the previous view's nextUpdate is a gap.
func TestCoverageAcrossBaseAndDelta(t *testing.T) {
	s := newStFx(t)
	w := audit.StatusParams{W: 20 * min, MinW: 10 * min}
	base := s.lcrl(1, idT0, 24*hour)
	d1 := s.delta(2, 1, idT0.Add(10*min), 15*min) // T0+10m .. T0+25m
	d2 := s.delta(3, 1, idT0.Add(25*min), 10*min) // contiguous: S = T0+25m >= A + 20m
	if st := s.leafStatus(s.snap(base, d1, d2), w); st.Tier != audit.TierFinal {
		t.Fatalf("contiguous: %+v", st)
	}
	late := s.delta(3, 1, idT0.Add(27*min), 10*min) // beyond the tolerance
	st := s.leafStatus(s.snap(base, d1, late), w)
	if st.Tier != audit.TierProvisional || !st.S.Equal(idT0.Add(10*min)) {
		t.Fatalf("a gap between two deltas: S must stay at the last view the coverage reaches: %+v", st)
	}
	// The base lapsing ends the merged view: with a short-period base the deltas run out with it.
	short := s.lcrl(1, idT0, 12*min)
	if st := s.leafStatus(s.snap(short, d1, d2), w); st.Tier == audit.TierFinal {
		t.Fatalf("deltas outlived their base: %+v", st)
	}
}

// Numbers are one sequence across bases and deltas: a delta dated later with a lower number than a
// CRL before it is a rollback, not evidence.
func TestDeltaNumbersFollowTheSequence(t *testing.T) {
	s := newStFx(t)
	base := s.lcrl(1, idT0, 24*hour)
	newer := s.lcrl(9, idT0.Add(10*min), 24*hour)
	rolled := s.delta(5, 1, idT0.Add(30*min), 30*min) // dated after #9 but numbered below it
	st := s.leafStatus(s.snap(base, newer, rolled), audit.StatusParams{})
	if has(st.Used, hashOf(t, rolled)) {
		t.Fatal("a rolled-back delta was relied on")
	}
}

// A delta whose number is not above its base's, or that was issued before its base, is not merged
// with it: it gives no evidence of absence, but what it lists is still read.
func TestUnpairableDeltaListingsAreStillRead(t *testing.T) {
	rev := func(s *stFx) x509.RevocationListEntry { return testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 0) }
	// Numbered below the only base.
	s := newStFx(t)
	base := s.lcrl(3, idT0, 24*hour)
	d := s.delta(2, 1, s.a.Add(time.Second), 30*min, rev(s))
	if st := s.leafStatus(s.snap(base, d), audit.StatusParams{}); !st.Revoked {
		t.Fatalf("a delta numbered below its base was not read for its listing: %+v", st)
	}
	// Issued before the only base.
	s = newStFx(t)
	base = s.lcrl(1, idT0.Add(20*min), 24*hour)
	d = s.delta(2, 1, idT0.Add(10*min), 30*min, rev(s))
	if st := s.leafStatus(s.snap(base, d), audit.StatusParams{}); !st.Revoked {
		t.Fatalf("a delta issued before its base was not read for its listing: %+v", st)
	}
	// Silent unpaired deltas are never evidence of absence.
	s = newStFx(t)
	base = s.lcrl(3, idT0, 24*hour)
	d = s.delta(2, 1, s.a.Add(time.Second), 30*min)
	st := s.leafStatus(s.snap(base, d), audit.StatusParams{})
	if has(st.Used, hashOf(t, d)) {
		t.Fatalf("a silent unpaired delta was relied on: %+v", st)
	}
}

// removeFromCRL proves a hold existed. A release dated at or after the decision is a hold of unknown
// start that covered it; one dated before the decision leaves the decision to the listings that bound
// the hold.
func TestRemoveFromCRLIsEvidenceOfAHold(t *testing.T) {
	s := newStFx(t)
	clean := s.lcrl(1, idT0, 24*hour)
	during := s.delta(2, 1, s.a.Add(20*min), 30*min, testca.Revoked(s.leaf.Cert, s.a.Add(10*min), 8))
	st := s.leafStatus(s.snap(clean, during), audit.StatusParams{AllowHolds: true})
	if !st.Revoked || st.ReasonCode != 6 || !st.E.Equal(s.leaf.Cert.NotBefore) {
		t.Fatalf("a release dated after the decision: %+v", st)
	}
	after := s.delta(2, 1, s.a.Add(20*min), 30*min, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Minute), 8))
	if st := s.leafStatus(s.snap(clean, after), audit.StatusParams{AllowHolds: true}); st.Revoked {
		t.Fatalf("a release dated before the decision: %+v", st)
	}
	// Exactly at the decision counts as during.
	at := s.delta(2, 1, s.a.Add(20*min), 30*min, testca.Revoked(s.leaf.Cert, s.a, 8))
	if st := s.leafStatus(s.snap(clean, at), audit.StatusParams{AllowHolds: true}); !st.Revoked {
		t.Fatalf("a release at the decision: %+v", st)
	}
	// Read even when the delta cannot be paired.
	lone := s.delta(2, 1, s.a.Add(20*min), 30*min, testca.Revoked(s.leaf.Cert, s.a.Add(10*min), 8))
	if st := s.leafStatus(s.snap(lone), audit.StatusParams{AllowHolds: true}); !st.Revoked {
		t.Fatalf("a lone release: %+v", st)
	}
}

// The merged view keeps the base's retention of expired entries.
func TestMergedViewInheritsRetention(t *testing.T) {
	s := newStFx(t)
	day := 24 * hour
	pre := s.lcrl(1, idT0, 3*hour)
	base := s.inter.NewCRL(t, testca.CRLOpts{Number: 2, ThisUpdate: idT0.Add(140 * min), NextUpdate: idT0.Add(140*min + day),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 60}, false, genTime(t, idT0.Add(-24*hour)))}})
	d := s.delta(3, 2, idT0.Add(150*min), 30*min)
	st := s.leafStatus(s.snap(pre, base, d), audit.StatusParams{CAPeriod: 3 * hour, W: 10 * min, MinW: 5 * min})
	if !st.S.Equal(idT0.Add(150*min)) || !st.Unbounded || st.Tier != audit.TierFinal {
		t.Fatalf("the delta merged with a retaining base: %+v", st)
	}
}

// The overlapping-cadence attack of #325 through deltas: the first merged view beyond notAfter (the
// one that lists the revoked leaf) is omitted and a later, pruned one is presented.
func TestOmittedFirstDeltaBeyondNotAfter(t *testing.T) {
	s := newStFx(t)
	day := 24 * hour
	rev := testca.Revoked(s.leaf.Cert, idT0.Add(50*min), 1)
	base := s.lcrl(1, idT0, day)
	d1 := s.delta(2, 1, idT0.Add(20*min), day)
	d2 := s.delta(3, 1, idT0.Add(40*min), day)
	d3 := s.delta(4, 1, idT0.Add(70*min), day, rev) // the first beyond notAfter (T0+60m) lists the leaf
	d4 := s.delta(5, 1, idT0.Add(90*min), day)      // pruned
	p := audit.StatusParams{CAPeriod: 30 * min}
	if st := s.leafStatus(s.snap(base, d1, d2, d3, d4), p); !st.Revoked {
		t.Fatalf("complete: %+v", st)
	}
	st := s.leafStatus(s.snap(base, d1, d2, d4), p)
	if st.Tier == audit.TierFinal || st.LCKnown {
		t.Fatalf("a revoked leaf read as final after omitting one delta: %+v", st)
	}
}

// Numbers are one sequence across bases and deltas; a CA that numbers them apart looks like a rollback
// and is excluded (a pilot-CA check). Two different bases under one number: the later is excluded.
func TestSeparateNumberSequencesAreRollbacks(t *testing.T) {
	s := newStFx(t)
	base := s.lcrl(10, idT0, 24*hour)
	d := s.delta(3, 10, idT0.Add(30*min), 30*min) // numbered from its own sequence
	st := s.leafStatus(s.snap(base, d), audit.StatusParams{})
	if has(st.Used, hashOf(t, d)) {
		t.Fatal("a delta numbered below its base was merged")
	}
	b1 := s.lcrl(5, idT0, 24*hour)
	b2 := s.lcrl(5, idT0.Add(10*min), 24*hour)
	dd := s.delta(6, 5, idT0.Add(30*min), 30*min)
	st = s.leafStatus(s.snap(b1, b2, dd), audit.StatusParams{})
	if has(st.Used, hashOf(t, b2)) || !has(st.Used, hashOf(t, b1)) {
		t.Fatalf("two bases under one number: %+v", st.Used)
	}
}

// Deltas carry the same refusals as full CRLs: a critical extension bide does not process.
func TestDeltaRefusalsMatchFullCRLs(t *testing.T) {
	s := newStFx(t)
	ind, _ := asn1.Marshal(big.NewInt(1))
	for name, extra := range map[string][]pkix.Extension{
		"critical freshestCRL": {ext(asn1.ObjectIdentifier{2, 5, 29, 27}, true, ind), ext(asn1.ObjectIdentifier{2, 5, 29, 46}, true, []byte{0x30, 0x00})},
		"unknown critical":     {ext(asn1.ObjectIdentifier{2, 5, 29, 27}, true, ind), ext(asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 5}, true, []byte{5, 0})},
		"duplicate indicator":  {ext(asn1.ObjectIdentifier{2, 5, 29, 27}, true, ind), ext(asn1.ObjectIdentifier{2, 5, 29, 27}, true, ind)},
	} {
		der, err := s.inter.TryCRL(testca.CRLOpts{Number: 2, ThisUpdate: idT0, NextUpdate: idT0.Add(hour), Extra: extra})
		if err != nil {
			t.Logf("%s: the library will not create it: %v", name, err)
			continue
		}
		if _, err := audit.NewStatusSnapshot().AddCRL(der); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// The pairing is indexed by number, so a long series of bases and deltas stays quick to evaluate.
func TestPairingAtScale(t *testing.T) {
	s := newStFx(t)
	sn := audit.NewStatusSnapshot(audit.WithMaxCRLs(5000))
	n := 0
	for i := 0; i < 400; i++ {
		base := s.lcrl(int64(2*i+1), idT0.Add(time.Duration(i)*min), 24*hour)
		d := s.delta(int64(2*i+2), int64(2*i+1), idT0.Add(time.Duration(i)*min+30*time.Second), 24*hour)
		for _, der := range [][]byte{base, d} {
			if _, err := sn.AddCRL(der); err != nil {
				t.Fatal(err)
			}
			n++
		}
	}
	start := time.Now()
	st := s.leafStatus(sn, audit.StatusParams{CAPeriod: 2 * min, W: 5 * min, MinW: 2 * min})
	if time.Since(start) > 5*time.Second {
		t.Fatalf("evaluating %d CRLs took %v", n, time.Since(start))
	}
	if st.Revoked {
		t.Fatalf("%+v", st)
	}
}

// Deltas are not bases: a run of deltas with no base pairs with nothing.
func TestDeltasAreNotBases(t *testing.T) {
	s := newStFx(t)
	d1 := s.delta(2, 1, s.a.Add(time.Second), hour)
	d2 := s.delta(3, 2, s.a.Add(2*time.Second), hour)
	st := s.leafStatus(s.snap(d1, d2), audit.StatusParams{})
	if st.Tier != audit.TierNoInformation || len(st.Used) != 0 {
		t.Fatalf("%+v", st)
	}
}

// Holds are off by default. A removeFromCRL date is the CA's own and RFC 5280 does not define it as
// the release time, so a policy that has not opted in refuses any certificate that was ever on hold.
func TestHoldsAreRefusedByDefault(t *testing.T) {
	s := newStFx(t)
	// A hold that begins after the decision: a revocation like any other would not touch it; under the
	// default policy a hold is refused wholesale.
	later := s.lcrl(1, idT0, 24*hour, testca.Revoked(s.leaf.Cert, s.a.Add(30*min), 6))
	st := s.leafStatus(s.snap(later), audit.StatusParams{})
	if !st.Revoked || !strings.Contains(st.Note, "AllowHolds") || st.Final() {
		t.Fatalf("default: %+v", st)
	}
	if st := s.leafStatus(s.snap(later), audit.StatusParams{AllowHolds: true}); st.Revoked {
		t.Fatalf("opted in, a hold after the decision: %+v", st)
	}
	// The fail-open case: the listing CRLs are omitted and the CA stamped the release entry with the
	// original hold date, before the decision. Opted in, the entry reads as an old release and the
	// decision passes; by default the entry alone refuses the certificate.
	base := s.lcrl(1, idT0, 24*hour)
	d := s.delta(2, 1, s.a.Add(20*min), 30*min, testca.Revoked(s.leaf.Cert, s.a.Add(-time.Hour), 8))
	st = s.leafStatus(s.snap(base, d), audit.StatusParams{})
	if !st.Revoked || !strings.Contains(st.Note, "AllowHolds") {
		t.Fatalf("default, release entry alone: %+v", st)
	}
	if st := s.leafStatus(s.snap(base, d), audit.StatusParams{AllowHolds: true}); st.Revoked {
		t.Fatalf("opted in, release entry dated before the decision: %+v", st)
	}
	// A certificate that was never on hold is unaffected.
	if st := s.leafStatus(s.snap(s.lcrl(1, idT0, 24*hour)), audit.StatusParams{}); st.Revoked {
		t.Fatalf("%+v", st)
	}
	// The chain's error names the reason.
	cs := s.eval(s.snap(later), audit.StatusParams{})
	wantStatusErr(t, cs.Err(), agent.ReasonRevoked)
}

// Where a listing bounds the hold's start, only decisions inside [start, release] are refused: not
// everything from notBefore, and not everything after.
func TestHoldIntervalIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      time.Duration
		revoked bool
	}{
		{"before the hold", 5 * min, false},
		{"during the hold", 20 * min, true},
		{"at the release", 30 * min, true},
		{"after the release", 40 * min, false},
	} {
		s := newStFxAt(t, idT0.Add(tc.at))
		base := s.lcrl(1, idT0.Add(12*min), 24*hour, testca.Revoked(s.leaf.Cert, idT0.Add(10*min), 6))
		rel := s.delta(2, 1, idT0.Add(31*min), 30*min, testca.Revoked(s.leaf.Cert, idT0.Add(30*min), 8))
		st := s.leafStatus(s.snap(base, rel), audit.StatusParams{AllowHolds: true})
		if st.Revoked != tc.revoked {
			t.Fatalf("%s: revoked=%v %+v", tc.name, st.Revoked, st)
		}
	}
	// A second hold with no release is open-ended; the release of the first does not close it.
	s := newStFxAt(t, idT0.Add(50*min))
	base := s.lcrl(1, idT0.Add(12*min), 24*hour,
		testca.Revoked(s.leaf.Cert, idT0.Add(10*min), 6), testca.Revoked(s.leaf.Cert, idT0.Add(40*min), 6))
	rel := s.delta(2, 1, idT0.Add(31*min), 30*min, testca.Revoked(s.leaf.Cert, idT0.Add(30*min), 8))
	if st := s.leafStatus(s.snap(base, rel), audit.StatusParams{AllowHolds: true}); !st.Revoked {
		t.Fatalf("second, open hold: %+v", st)
	}
	// A release with no listing bounds nothing at the start: refused from notBefore to its date.
	s = newStFxAt(t, idT0.Add(5*min))
	only := s.delta(2, 1, idT0.Add(31*min), 30*min, testca.Revoked(s.leaf.Cert, idT0.Add(30*min), 8))
	if st := s.leafStatus(s.snap(s.lcrl(1, idT0, 24*hour), only), audit.StatusParams{AllowHolds: true}); !st.Revoked {
		t.Fatalf("release only: %+v", st)
	}
}

// The merged view keeps the base's retention of expired entries, not the delta's alone.
func TestMergedRetentionComesFromTheBase(t *testing.T) {
	s := newStFx(t)
	day := 24 * hour
	pre := s.lcrl(1, idT0, 3*hour)
	base := s.lcrl(2, idT0.Add(140*min), day) // no expiredCertsOnCRL
	ind, _ := asn1.Marshal(big.NewInt(2))
	d := s.inter.NewCRL(t, testca.CRLOpts{Number: 3, ThisUpdate: idT0.Add(150 * min), NextUpdate: idT0.Add(180 * min),
		Extra: []pkix.Extension{ext(asn1.ObjectIdentifier{2, 5, 29, 27}, true, ind),
			ext(asn1.ObjectIdentifier{2, 5, 29, 60}, false, genTime(t, idT0.Add(-24*hour)))}})
	st := s.leafStatus(s.snap(pre, base, d), audit.StatusParams{CAPeriod: 3 * hour, W: 10 * min, MinW: 5 * min})
	if st.S.Equal(idT0.Add(150*min)) || st.Unbounded {
		t.Fatalf("only the delta claimed retention: %+v", st)
	}
}

// A release that predates a hold does not close it.
func TestEarlierReleaseDoesNotCloseALaterHold(t *testing.T) {
	s := newStFxAt(t, idT0.Add(25*min))
	base := s.lcrl(1, idT0.Add(22*min), 24*hour, testca.Revoked(s.leaf.Cert, idT0.Add(20*min), 6))
	early := s.delta(2, 1, idT0.Add(23*min), 30*min, testca.Revoked(s.leaf.Cert, idT0.Add(10*min), 8))
	if st := s.leafStatus(s.snap(base, early), audit.StatusParams{AllowHolds: true}); !st.Revoked {
		t.Fatalf("a release dated before the hold closed it: %+v", st)
	}
}

// The same hold is listed by the base and by every delta since: it is one hold, closed by one release.
func TestSameHoldListedByBaseAndDeltasIsOneHold(t *testing.T) {
	for _, at := range []time.Duration{5 * min, 40 * min} { // before the hold, after the release
		sameHoldAt(t, at)
	}
}

func sameHoldAt(t *testing.T, at time.Duration) {
	t.Helper()
	s := newStFxAt(t, idT0.Add(at))
	hold := testca.Revoked(s.leaf.Cert, idT0.Add(10*min), 6)
	base := s.lcrl(1, idT0.Add(12*min), 24*hour, hold)
	d1 := s.delta(2, 1, idT0.Add(20*min), 30*min, hold)
	d2 := s.delta(3, 1, idT0.Add(31*min), 30*min, hold, testca.Revoked(s.leaf.Cert, idT0.Add(30*min), 8))
	d3 := s.delta(4, 1, idT0.Add(32*min), 30*min, testca.Revoked(s.leaf.Cert, idT0.Add(30*min), 8))
	if st := s.leafStatus(s.snap(base, d1, d2, d3), audit.StatusParams{AllowHolds: true}); st.Revoked {
		t.Fatalf("a decision %v from T0 was refused: %+v", at, st)
	}
}
