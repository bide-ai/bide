package audit

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"
)

// A signed tree head's Timestamp is Unix nanoseconds. The rule EvidencePackage.Verify applies to
// every head a package carries: positive, not later than the verifier's clock plus the allowed
// skew, and, where one head extends another, not earlier than the head it extends.

// tsEvidence builds a sealed package over three tool calls in run A at timestamp ts, with the
// options given, and returns it with its store and keys.
func tsEvidence(t *testing.T, ts int64, opts ...EvidenceOption) (EvidencePackage, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv := secKey(t)
	s := secThreeCalls(t, "A")
	pkg, err := Evidence(context.Background(), s, "A", priv, ts, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return pkg, pub, priv
}

func tsOK(t *testing.T, pkg EvidencePackage, pub ed25519.PublicKey) bool {
	t.Helper()
	rep, err := pkg.Verify(pub, WithApprovedPolicies())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return rep.OK
}

func TestEvidence_HeadTimestampMustBePositiveAndNotInTheFuture(t *testing.T) {
	now := time.Now()
	for name, ts := range map[string]int64{
		"zero":                  0,
		"negative":              -5,
		"an hour in the future": now.Add(time.Hour).UnixNano(),
		"a year in the future":  now.AddDate(1, 0, 0).UnixNano(),
	} {
		pkg, pub, _ := tsEvidence(t, ts)
		if tsOK(t, pkg, pub) {
			t.Errorf("a package whose signed head has timestamp %s (%d) verified", name, ts)
		}
	}
	for name, ts := range map[string]int64{
		"an hour ago":                  now.Add(-time.Hour).UnixNano(),
		"a minute ahead (within skew)": now.Add(time.Minute).UnixNano(),
		"1":                            1,
	} {
		pkg, pub, _ := tsEvidence(t, ts)
		if !tsOK(t, pkg, pub) {
			t.Errorf("a package whose signed head has timestamp %s (%d) did not verify", name, ts)
		}
	}
}

// An action's bundle may carry its own signature over the same tree (SameTree ignores the
// timestamp); a future timestamp there fails the item.
func TestEvidence_ActionHeadTimestampIsChecked(t *testing.T) {
	past := time.Now().Add(-time.Hour).UnixNano()
	pkg, pub, priv := tsEvidence(t, past)
	th := pkg.Actions[0].Bundle.STH.TreeHead
	th.Timestamp = time.Now().Add(time.Hour).UnixNano()
	pkg.Actions[0].Bundle.STH = SignTreeHead(th, priv)
	if err := pkg.Seal(priv); err != nil {
		t.Fatal(err)
	}
	if tsOK(t, pkg, pub) {
		t.Fatal("a package whose action bundle is signed at a future time verified")
	}
}

// The consistency proof's earlier head must not be later than the head it extends.
func TestEvidence_ConsistencyHeadsAreInOrder(t *testing.T) {
	pub, priv := secKey(t)
	s := secThreeCalls(t, "A")
	recs, _ := s.History(context.Background(), "A")
	at := time.Now().Add(-time.Hour)
	for name, tc := range map[string]struct {
		from int64
		ok   bool
	}{
		"earlier":    {at.Add(-time.Minute).UnixNano(), true},
		"same time":  {at.UnixNano(), true},
		"later":      {at.Add(time.Minute).UnixNano(), false},
		"zero":       {0, false},
		"the future": {time.Now().Add(time.Hour).UnixNano(), false},
	} {
		early, err := journalHead("A", recs[:1], tc.from)
		if err != nil {
			t.Fatal(err)
		}
		pkg, err := Evidence(context.Background(), s, "A", priv, at.UnixNano(), WithConsistencyFrom(SignTreeHead(early, priv)))
		if err != nil {
			t.Fatal(err)
		}
		if got := tsOK(t, pkg, pub); got != tc.ok {
			t.Errorf("consistency from a head signed %s: verified = %v, want %v", name, got, tc.ok)
		}
	}
}

// A run certificate's heads follow the rule too, and its used-policy head, projected from the
// journal head, is not earlier than it.
func TestEvidence_RunCertificateHeadTimestampsAreChecked(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	for name, tc := range map[string]struct {
		used int64
		ok   bool
	}{
		"same time":  {at.UnixNano(), true},
		"later":      {at.Add(time.Minute).UnixNano(), true},
		"earlier":    {at.Add(-time.Minute).UnixNano(), false},
		"the future": {time.Now().Add(time.Hour).UnixNano(), false},
	} {
		pub, priv := secKey(t)
		s := secThreeCalls(t, "A")
		pkg, err := Evidence(context.Background(), s, "A", priv, at.UnixNano())
		if err != nil {
			t.Fatal(err)
		}
		cert, err := CertifyRun(context.Background(), s, "A", pkg.STH, RunCertSpec{}, priv, tc.used)
		if err != nil {
			t.Fatal(err)
		}
		pkg.RunCertificate = &cert
		if err := pkg.Seal(priv); err != nil {
			t.Fatal(err)
		}
		if got := tsOK(t, pkg, pub); got != tc.ok {
			t.Errorf("a run certificate whose used-policy head is signed %s: verified = %v, want %v", name, got, tc.ok)
		}
	}
}
